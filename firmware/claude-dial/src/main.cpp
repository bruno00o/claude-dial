/*
 * Claude Code Monitor — M5Stack Dial firmware  (clean / FreeRTOS-queue build)
 *
 * What changed vs the Schematik output:
 *   - The BLE write callback no longer parses JSON or touches shared state.
 *     It only copies the raw bytes into a FreeRTOS queue. loop() drains the
 *     queue and is the ONLY writer of sessions[]/appState. No data race, no
 *     blocking delay/tone inside the NimBLE task. (fixes correctif 3, properly)
 *   - Permissions are a FIFO, not a single pendingIdx: several parallel
 *     sessions queue up and are shown one after another. (fixes correctif 5)
 *   - Long-press uses wasClicked()/wasHold() so it no longer also fires the
 *     short-press action. (correctif 2)
 *   - Encoder rotation is accumulated and stepped per detent. (correctif 4)
 *   - set_time control message lets the host set the RTC. (bonus A)
 *   - Permission timeout sends "ask" (fall back to terminal prompt), not
 *     "reject". (bonus B)
 *
 * BLE GATT service:
 *   Service UUID : 12345678-1234-1234-1234-123456789ABC
 *   RX char UUID : 12345678-1234-1234-1234-123456789ABD  (WRITE / WRITE_NR)
 *   TX char UUID : 12345678-1234-1234-1234-123456789ABE  (NOTIFY)
 *
 * Host -> device (RX): {"session_id":"sid","state":"permission_request",
 *                        "tool_name":"Bash","command":"rm -rf /tmp/x"}
 *                      {"type":"set_time","epoch":1782765000,"tz_offset":7200}
 * Device -> host (TX): {"session_id":"sid","decision":"allow_once"}
 *                      decisions: allow_once | always_allow | reject | ask
 */

#include <M5Dial.h>
#include <NimBLEDevice.h>
#include <ArduinoJson.h>
#include <time.h>

#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"

// ── BLE UUIDs ────────────────────────────────────────────────────────────────
#define SVC_UUID  "12345678-1234-1234-1234-123456789ABC"
#define RX_UUID   "12345678-1234-1234-1234-123456789ABD"
#define TX_UUID   "12345678-1234-1234-1234-123456789ABE"

// ── Colour palette ───────────────────────────────────────────────────────────
#define COL_BG          0x0A0A0A
#define COL_RING        0x1E3A5F
#define COL_ACCENT      0x00BFFF
#define COL_WHITE       0xFFFFFF
#define COL_GRAY        0x808080
#define COL_WORKING     0x00C896
#define COL_IDLE_SES    0x4488FF
#define COL_NEEDS_INPUT 0xFFAA00
#define COL_ALLOW       0x22EE66
#define COL_REJECT      0xFF3333
#define COL_ALWAYS      0x44AAFF
#define COL_CONFIRM_BG  0x003300

// ── Types ────────────────────────────────────────────────────────────────────
enum AppState { IDLE, SESSION_LIST, PERMISSION, CONFIRMING, MODE_MENU };
enum AppMode  { MODE_CLAUDE, MODE_CLOCK };

struct Session {
  char  session_id[40];
  char  state[24];      // working | idle | blocked | permission_request
  char  tool_name[40];
  char  command[200];
  bool  active;
};

// Raw inbound BLE message, copied verbatim out of the callback.
struct RxMsg {
  char     data[480];
  uint16_t len;
};

#define MAX_SESSIONS 8

// ── Forward declarations ─────────────────────────────────────────────────────
static int  findSession(const char* sid);
static int  newSession(const char* sid);
static void removeSession(const char* sid);
static int  activeSessions();
static void sendDecision(const char* sid, const char* decision);

static bool permInQueue(const char* sid);
static void permEnqueue(const char* sid);
static void permRemoveFromQueue(const char* sid);
static void permShowNext();

static void handleRxMessage(const char* data, uint16_t len);

static void drawBase();
static void getTimeStr(char* tBuf, char* dBuf);
static void drawIdle();
static void drawClockMode();
static void drawSessionList();
static void drawPermission();
static void drawConfirming();
static void drawModeMenu();
static void redraw();
static void handleEncoder(int delta);
static void handlePress();

// ── App state (all mutated only from loop() context) ─────────────────────────
static AppState appState = IDLE;
static AppMode  appMode  = MODE_CLAUDE;

static Session sessions[MAX_SESSIONS];
static int     sessionCount = 0;

// Permission FIFO
static char  permQueue[MAX_SESSIONS][40];
static int   permQueueCount = 0;
static char  currentPermSid[40] = "";     // "" = nothing shown
static long  permTimeout = 0;
static int   permChoice = 0;              // 0 allow once | 1 always | 2 reject

static int   menuChoice = 0;
static long  lastEncoderPos = 0;
static int   listScrollOffset = 0;

static unsigned long confirmStart = 0;
static char          confirmMsg[32] = "";

static bool needsRedraw = true;
static bool buzzPending = false;

// ── BLE / queue handles ──────────────────────────────────────────────────────
static NimBLECharacteristic* txChar  = nullptr;
static bool                  bleConnected = false;
static QueueHandle_t         rxQueue = nullptr;

// ── Display sprite ───────────────────────────────────────────────────────────
static M5Canvas canvas(&M5Dial.Display);
static const int CX = 120, CY = 120, CR = 120;

// ─────────────────────────────────────────────────────────────────────────────
// Session helpers
// ─────────────────────────────────────────────────────────────────────────────
static int findSession(const char* sid) {
  for (int i = 0; i < MAX_SESSIONS; i++)
    if (sessions[i].active && strcmp(sessions[i].session_id, sid) == 0) return i;
  return -1;
}

static int newSession(const char* sid) {
  int idx = findSession(sid);
  if (idx >= 0) return idx;
  for (int i = 0; i < MAX_SESSIONS; i++) {
    if (!sessions[i].active) {
      memset(&sessions[i], 0, sizeof(Session));
      strlcpy(sessions[i].session_id, sid, sizeof(sessions[i].session_id));
      sessions[i].active = true;
      sessionCount++;
      return i;
    }
  }
  return -1;
}

static void removeSession(const char* sid) {
  int idx = findSession(sid);
  if (idx >= 0) {
    sessions[idx].active = false;
    if (sessionCount > 0) sessionCount--;
  }
  permRemoveFromQueue(sid);
  if (strcmp(currentPermSid, sid) == 0) {
    currentPermSid[0] = 0;
    if (appState == PERMISSION) permShowNext();
  }
}

static int activeSessions() {
  int n = 0;
  for (int i = 0; i < MAX_SESSIONS; i++) if (sessions[i].active) n++;
  return n;
}

// ─────────────────────────────────────────────────────────────────────────────
// Permission FIFO
// ─────────────────────────────────────────────────────────────────────────────
static bool permInQueue(const char* sid) {
  for (int i = 0; i < permQueueCount; i++)
    if (strcmp(permQueue[i], sid) == 0) return true;
  return false;
}

static void permEnqueue(const char* sid) {
  if (permInQueue(sid)) return;
  if (permQueueCount >= MAX_SESSIONS) return;
  strlcpy(permQueue[permQueueCount], sid, 40);
  permQueueCount++;
}

static void permRemoveFromQueue(const char* sid) {
  for (int i = 0; i < permQueueCount; i++) {
    if (strcmp(permQueue[i], sid) == 0) {
      for (int j = i; j < permQueueCount - 1; j++)
        strlcpy(permQueue[j], permQueue[j + 1], 40);
      permQueueCount--;
      return;
    }
  }
}

// Pop the next valid pending request and show it. If none, leave the screen.
static void permShowNext() {
  currentPermSid[0] = 0;
  while (permQueueCount > 0) {
    char sid[40];
    strlcpy(sid, permQueue[0], 40);
    for (int j = 0; j < permQueueCount - 1; j++)
      strlcpy(permQueue[j], permQueue[j + 1], 40);
    permQueueCount--;

    int idx = findSession(sid);
    if (idx >= 0 && sessions[idx].active &&
        strcmp(sessions[idx].state, "permission_request") == 0) {
      strlcpy(currentPermSid, sid, 40);
      permChoice  = 0;
      permTimeout = millis() + 120000UL;
      appState    = PERMISSION;
      needsRedraw = true;
      return;
    }
    // stale entry, skip
  }
  appState    = (activeSessions() > 0) ? SESSION_LIST : IDLE;
  needsRedraw = true;
}

// ─────────────────────────────────────────────────────────────────────────────
// BLE callbacks  (run in the NimBLE task — keep them trivial)
// ─────────────────────────────────────────────────────────────────────────────
class ServerCallbacks : public NimBLEServerCallbacks {
  void onConnect(NimBLEServer*) override    { bleConnected = true;  needsRedraw = true; }
  void onDisconnect(NimBLEServer*) override {
    bleConnected = false; needsRedraw = true;
    NimBLEDevice::startAdvertising();
  }
};

class RxCallback : public NimBLECharacteristicCallbacks {
  void onWrite(NimBLECharacteristic* pChar) override {
    std::string val = pChar->getValue();
    if (val.empty() || val.size() >= sizeof(((RxMsg*)0)->data)) return;
    RxMsg msg;
    memcpy(msg.data, val.data(), val.size());
    msg.data[val.size()] = 0;
    msg.len = (uint16_t)val.size();
    if (rxQueue) xQueueSend(rxQueue, &msg, 0);   // non-blocking; drop if full
  }
};

// ─────────────────────────────────────────────────────────────────────────────
// Inbound message handling  (called from loop(), single-threaded)
// ─────────────────────────────────────────────────────────────────────────────
static void handleRxMessage(const char* data, uint16_t len) {
  JsonDocument doc;
  if (deserializeJson(doc, data, len)) return;

  // Control: set the RTC clock from the host
  const char* type = doc["type"] | "";
  if (strcmp(type, "set_time") == 0) {
    long long epoch = doc["epoch"]     | 0LL;
    long      tzoff = doc["tz_offset"] | 0;
    if (epoch > 0) {
      time_t t = (time_t)(epoch + tzoff);
      struct tm* g = gmtime(&t);
      m5::rtc_datetime_t dt;
      dt.date.year    = g->tm_year + 1900;
      dt.date.month   = g->tm_mon + 1;
      dt.date.date    = g->tm_mday;
      dt.time.hours   = g->tm_hour;
      dt.time.minutes = g->tm_min;
      dt.time.seconds = g->tm_sec;
      M5Dial.Rtc.setDateTime(dt);
      needsRedraw = true;
    }
    return;
  }

  const char* sid   = doc["session_id"] | "";
  const char* state = doc["state"]      | "";
  const char* tool  = doc["tool_name"]  | "";
  const char* cmd   = doc["command"]    | "";
  if (!sid[0]) return;

  if (strcmp(state, "closed") == 0 || strcmp(state, "done") == 0) {
    removeSession(sid);
    needsRedraw = true;
    return;
  }

  int idx = newSession(sid);
  if (idx < 0) return;
  strlcpy(sessions[idx].state,     state, sizeof(sessions[idx].state));
  strlcpy(sessions[idx].tool_name, tool,  sizeof(sessions[idx].tool_name));
  strlcpy(sessions[idx].command,   cmd,   sizeof(sessions[idx].command));

  if (strcmp(state, "permission_request") == 0) {
    bool isNew = !permInQueue(sid) && strcmp(currentPermSid, sid) != 0;
    permEnqueue(sid);
    if (isNew) buzzPending = true;
    if (currentPermSid[0] == 0) permShowNext();
  } else if (appState == IDLE && appMode == MODE_CLAUDE && activeSessions() > 0) {
    // stay on idle; status line stays accurate
  }
  needsRedraw = true;
}

// ─────────────────────────────────────────────────────────────────────────────
// Send decision back to host
// ─────────────────────────────────────────────────────────────────────────────
static void sendDecision(const char* sid, const char* decision) {
  if (!txChar || !bleConnected) return;
  JsonDocument doc;
  doc["session_id"] = sid;
  doc["decision"]   = decision;
  char buf[128];
  size_t n = serializeJson(doc, buf, sizeof(buf));
  txChar->setValue((uint8_t*)buf, n);
  txChar->notify();
}

// ─────────────────────────────────────────────────────────────────────────────
// Drawing
// ─────────────────────────────────────────────────────────────────────────────
static void drawBase() {
  canvas.fillScreen(COL_BG);
  canvas.fillCircle(CX, CY, CR - 1, COL_BG);
  canvas.drawCircle(CX, CY, CR - 1, COL_RING);
  canvas.drawCircle(CX, CY, CR - 2, COL_RING);
  canvas.drawCircle(CX, CY, CR - 3, COL_ACCENT);
}

static void getTimeStr(char* tBuf, char* dBuf) {
  auto dt = M5Dial.Rtc.getDateTime();
  snprintf(tBuf, 12, "%02d:%02d", dt.time.hours, dt.time.minutes);
  snprintf(dBuf, 20, "%04d-%02d-%02d", dt.date.year, dt.date.month, dt.date.date);
}

static void drawIdle() {
  drawBase();
  char tBuf[12], dBuf[20];
  getTimeStr(tBuf, dBuf);

  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_WHITE, COL_BG);
  canvas.setFont(&fonts::Orbitron_Light_32);
  canvas.setTextSize(1);
  canvas.drawString(tBuf, CX, CY - 14);

  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_GRAY, COL_BG);
  canvas.drawString(dBuf, CX, CY + 20);

  int act = activeSessions();
  char status[40];
  if (!bleConnected)      strlcpy(status, "Waiting for Claude", sizeof(status));
  else if (act > 0)       snprintf(status, sizeof(status), "%d session%s", act, act > 1 ? "s" : "");
  else                    strlcpy(status, "BLE connected", sizeof(status));

  canvas.setTextColor(bleConnected ? COL_ACCENT : COL_GRAY, COL_BG);
  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.drawString(status, CX, CY + 50);

  canvas.fillCircle(CX, CY + 72, 4, bleConnected ? COL_ACCENT : 0x333333);
  canvas.pushSprite(0, 0);
}

static void drawClockMode() {
  drawBase();
  char tBuf[12], dBuf[20];
  getTimeStr(tBuf, dBuf);

  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::Orbitron_Light_32);
  canvas.setTextColor(COL_WHITE, COL_BG);
  canvas.drawString(tBuf, CX, CY - 6);

  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_GRAY, COL_BG);
  canvas.drawString(dBuf, CX, CY + 30);

  auto dt = M5Dial.Rtc.getDateTime();
  float angle = (dt.time.minutes / 60.0f) * 360.0f - 90.0f;
  int ax = CX + (int)((CR - 6) * cosf(angle * DEG_TO_RAD));
  int ay = CY + (int)((CR - 6) * sinf(angle * DEG_TO_RAD));
  canvas.fillCircle(ax, ay, 5, COL_ACCENT);
  canvas.pushSprite(0, 0);
}

static void drawSessionList() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_ACCENT, COL_BG);
  canvas.drawString("Claude Sessions", CX, 20);

  int active[MAX_SESSIONS], n = 0;
  for (int i = 0; i < MAX_SESSIONS; i++) if (sessions[i].active) active[n++] = i;

  if (n == 0) {
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.drawString("No active sessions", CX, CY);
    canvas.pushSprite(0, 0);
    return;
  }

  int visible = 4;
  if (listScrollOffset > n - visible) listScrollOffset = n - visible;
  if (listScrollOffset < 0) listScrollOffset = 0;

  int startY = 42, rowH = 40;
  for (int row = 0; row < visible && (listScrollOffset + row) < n; row++) {
    Session& s = sessions[active[listScrollOffset + row]];
    int y = startY + row * rowH;

    uint32_t rowCol;
    if      (strcmp(s.state, "working")            == 0) rowCol = COL_WORKING;
    else if (strcmp(s.state, "blocked")            == 0) rowCol = COL_NEEDS_INPUT;
    else if (strcmp(s.state, "permission_request") == 0) rowCol = COL_NEEDS_INPUT;
    else                                                 rowCol = COL_IDLE_SES;

    int pw = 160, ph = 30, px = CX - pw / 2, py = y - ph / 2;
    canvas.fillRoundRect(px, py, pw, ph, 8, rowCol);

    char shortId[18];
    strlcpy(shortId, s.session_id, sizeof(shortId));
    if (strlen(s.session_id) > 10) { shortId[8] = '.'; shortId[9] = '.'; shortId[10] = 0; }

    canvas.setTextColor(COL_BG, rowCol);
    canvas.setTextDatum(middle_left);
    canvas.drawString(shortId, px + 8, y);
    canvas.setTextDatum(middle_right);
    canvas.drawString(s.state, px + pw - 6, y);
  }

  if (n > visible) {
    for (int i = 0; i < n; i++) {
      uint32_t dc = (i == listScrollOffset) ? COL_ACCENT : COL_GRAY;
      canvas.fillCircle(CX - (n * 6) / 2 + i * 6 + 3, 228, 3, dc);
    }
  }
  canvas.pushSprite(0, 0);
}

static const char* choiceLabels[3] = { "Allow once", "Always allow", "Reject" };
static const uint32_t choiceColors[3] = { COL_ALLOW, COL_ALWAYS, COL_REJECT };

static void drawPermission() {
  int idx = findSession(currentPermSid);
  if (idx < 0 || !sessions[idx].active) { permShowNext(); return; }
  Session& s = sessions[idx];

  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_NEEDS_INPUT, COL_BG);
  canvas.drawString("Permission request", CX, 18);

  // "+N" badge if more are queued behind this one
  if (permQueueCount > 0) {
    char more[12];
    snprintf(more, sizeof(more), "+%d", permQueueCount);
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.setTextDatum(middle_right);
    canvas.drawString(more, CX + 96, 18);
    canvas.setTextDatum(middle_center);
  }

  canvas.setTextColor(COL_WHITE, COL_BG);
  canvas.drawString(s.tool_name, CX, 38);

  char line1[23] = {0}, line2[23] = {0};
  int cmdLen = strlen(s.command);
  if (cmdLen <= 22) {
    strlcpy(line1, s.command, 23);
  } else {
    int cut = 22;
    while (cut > 0 && s.command[cut] != ' ') cut--;
    if (cut == 0) cut = 22;
    strlcpy(line1, s.command, cut + 1);
    strlcpy(line2, s.command + cut + 1, 23);
  }
  canvas.setTextColor(COL_GRAY, COL_BG);
  canvas.drawString(line1, CX, 58);
  if (line2[0]) canvas.drawString(line2, CX, 74);

  long remaining = permTimeout - millis();
  if (remaining < 0) remaining = 0;
  float frac = remaining / 120000.0f;
  int arcR = 112, arcSteps = (int)(frac * 200);
  for (int i = 0; i < 200; i++) {
    float a = (i / 200.0f) * 360.0f - 90.0f;
    int ax = CX + (int)(arcR * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(arcR * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 2, (i < arcSteps) ? COL_ACCENT : 0x1A1A1A);
  }

  int btnY[3] = { 102, 136, 170 };
  for (int i = 0; i < 3; i++) {
    bool sel = (i == permChoice);
    uint32_t bg = sel ? choiceColors[i] : 0x1E1E2E;
    uint32_t fg = sel ? COL_BG          : choiceColors[i];
    int bw = 170, bh = 26, bx = CX - bw / 2, by = btnY[i] - bh / 2;
    if (sel) canvas.fillRoundRect(bx, by, bw, bh, 7, bg);
    else     canvas.drawRoundRect(bx, by, bw, bh, 7, fg);
    canvas.setTextColor(fg, sel ? bg : COL_BG);
    canvas.setTextDatum(middle_center);
    canvas.drawString(choiceLabels[i], CX, btnY[i]);
  }

  canvas.setTextColor(COL_GRAY, COL_BG);
  canvas.drawString("rotate / push", CX, 210);
  canvas.pushSprite(0, 0);
}

static void drawConfirming() {
  canvas.fillScreen(COL_CONFIRM_BG);
  canvas.fillCircle(CX, CY, CR - 1, COL_CONFIRM_BG);
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::Orbitron_Light_32);
  canvas.setTextColor(COL_ALLOW, COL_CONFIRM_BG);
  canvas.drawString("Sent!", CX, CY - 12);
  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_WHITE, COL_CONFIRM_BG);
  canvas.drawString(confirmMsg, CX, CY + 22);
  canvas.pushSprite(0, 0);
}

static const char* menuLabels[2] = { "Claude Monitor", "Clock" };
static void drawModeMenu() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeSans9pt7b);
  canvas.setTextColor(COL_ACCENT, COL_BG);
  canvas.drawString("Mode", CX, 22);

  int btnY[2] = { CY - 20, CY + 20 };
  for (int i = 0; i < 2; i++) {
    bool sel = (i == menuChoice);
    uint32_t bg = sel ? COL_ACCENT : 0x1E1E2E;
    uint32_t fg = sel ? COL_BG     : COL_WHITE;
    int bw = 180, bh = 30, bx = CX - bw / 2, by = btnY[i] - bh / 2;
    if (sel) canvas.fillRoundRect(bx, by, bw, bh, 8, bg);
    else     canvas.drawRoundRect(bx, by, bw, bh, 8, COL_GRAY);
    canvas.setTextColor(fg, sel ? bg : COL_BG);
    canvas.drawString(menuLabels[i], CX, btnY[i]);
  }
  canvas.setTextColor(COL_GRAY, COL_BG);
  canvas.drawString("rotate / push", CX, 210);
  canvas.pushSprite(0, 0);
}

static void redraw() {
  needsRedraw = false;
  switch (appState) {
    case IDLE:         (appMode == MODE_CLOCK) ? drawClockMode() : drawIdle(); break;
    case SESSION_LIST: drawSessionList(); break;
    case PERMISSION:   drawPermission();  break;
    case CONFIRMING:   drawConfirming();  break;
    case MODE_MENU:    drawModeMenu();    break;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Input
// ─────────────────────────────────────────────────────────────────────────────
static void handleEncoder(int delta) {
  switch (appState) {
    case SESSION_LIST:
      listScrollOffset += (delta > 0) ? 1 : -1;
      if (listScrollOffset < 0) listScrollOffset = 0;
      needsRedraw = true;
      break;
    case PERMISSION:
      permChoice = (permChoice + (delta > 0 ? 1 : -1) + 3) % 3;
      needsRedraw = true;
      break;
    case MODE_MENU:
      menuChoice = (menuChoice + (delta > 0 ? 1 : -1) + 2) % 2;
      needsRedraw = true;
      break;
    default: break;
  }
}

static void handlePress() {
  switch (appState) {
    case IDLE:
    case SESSION_LIST:
      if (appMode == MODE_CLAUDE) {
        appState    = (appState == IDLE) ? SESSION_LIST : IDLE;
        needsRedraw = true;
      }
      break;

    case PERMISSION: {
      int idx = findSession(currentPermSid);
      if (idx < 0 || !sessions[idx].active) { permShowNext(); break; }
      const char* dec = (permChoice == 0) ? "allow_once"
                      : (permChoice == 1) ? "always_allow" : "reject";
      sendDecision(currentPermSid, dec);

      M5Dial.Speaker.tone(2000, 60); delay(80); M5Dial.Speaker.tone(2400, 60);

      snprintf(confirmMsg, sizeof(confirmMsg), "%s", choiceLabels[permChoice]);
      strlcpy(sessions[idx].state, (permChoice == 2) ? "idle" : "working",
              sizeof(sessions[idx].state));
      currentPermSid[0] = 0;
      appState     = CONFIRMING;
      confirmStart = millis();
      needsRedraw  = true;
      break;
    }

    case CONFIRMING:
      permShowNext();
      break;

    case MODE_MENU:
      appMode     = (menuChoice == 0) ? MODE_CLAUDE : MODE_CLOCK;
      appState    = IDLE;
      needsRedraw = true;
      break;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// setup()
// ─────────────────────────────────────────────────────────────────────────────
void setup() {
  pinMode(46, OUTPUT);
  digitalWrite(46, HIGH);          // POWER_HOLD: keep on when on battery/DC

  auto cfg = M5.config();
  M5Dial.begin(cfg, true, false);  // encoder on, RFID off
  M5Dial.Display.setBrightness(200);

  canvas.createSprite(240, 240);
  canvas.setColorDepth(16);

  M5Dial.Rtc.begin();
  memset(sessions, 0, sizeof(sessions));

  rxQueue = xQueueCreate(8, sizeof(RxMsg));

  NimBLEDevice::init("Claude-Dial");
  NimBLEDevice::setPower(ESP_PWR_LVL_P9);

  NimBLEServer* pServer = NimBLEDevice::createServer();
  pServer->setCallbacks(new ServerCallbacks());

  NimBLEService* pService = pServer->createService(SVC_UUID);

  NimBLECharacteristic* rxChar = pService->createCharacteristic(
      RX_UUID, NIMBLE_PROPERTY::WRITE | NIMBLE_PROPERTY::WRITE_NR);
  rxChar->setCallbacks(new RxCallback());

  txChar = pService->createCharacteristic(TX_UUID, NIMBLE_PROPERTY::NOTIFY);

  pService->start();

  NimBLEAdvertising* pAdv = NimBLEDevice::getAdvertising();
  pAdv->addServiceUUID(SVC_UUID);
  pAdv->setScanResponse(true);
  pAdv->setMinPreferred(0x06);
  NimBLEDevice::startAdvertising();

  M5Dial.Speaker.tone(880, 80); delay(100); M5Dial.Speaker.tone(1320, 80);

  lastEncoderPos = M5Dial.Encoder.read();
  needsRedraw    = true;
}

// ─────────────────────────────────────────────────────────────────────────────
// loop()
// ─────────────────────────────────────────────────────────────────────────────
void loop() {
  M5Dial.update();

  // Deferred buzz (set from handleRxMessage, played here outside the BLE task)
  if (buzzPending) {
    buzzPending = false;
    M5Dial.Speaker.tone(1200, 150);
    delay(180);
    M5Dial.Speaker.tone(1600, 80);
  }

  // Drain inbound BLE messages — this is the only place state mutates
  RxMsg msg;
  while (rxQueue && xQueueReceive(rxQueue, &msg, 0) == pdTRUE) {
    handleRxMessage(msg.data, msg.len);
  }

  // Encoder: accumulate counts, step once per detent
  static long encAccum = 0;
  long pos   = M5Dial.Encoder.read();
  long delta = pos - lastEncoderPos;
  if (delta != 0) {
    lastEncoderPos = pos;
    encAccum += delta;
    const long ENC_DETENT = 2;   // counts per click; bump to 4 if it scrolls 2 per click
    while (encAccum >=  ENC_DETENT) { handleEncoder(+1); encAccum -= ENC_DETENT; }
    while (encAccum <= -ENC_DETENT) { handleEncoder(-1); encAccum += ENC_DETENT; }
  }

  // Button: short click vs long hold, cleanly separated
  if (M5Dial.BtnA.wasClicked()) {
    handlePress();
  }
  if (M5Dial.BtnA.wasHold()) {
    if (appState != MODE_MENU) {
      appState    = MODE_MENU;
      menuChoice  = (appMode == MODE_CLAUDE) ? 0 : 1;
      needsRedraw = true;
    }
  }

  // Confirming auto-dismiss -> show next pending (or fall back)
  if (appState == CONFIRMING && (millis() - confirmStart) > 1500) {
    permShowNext();
  }

  // Permission timeout -> "ask" (host falls back to normal terminal prompt)
  if (appState == PERMISSION && millis() > permTimeout) {
    if (currentPermSid[0]) {
      sendDecision(currentPermSid, "ask");
      int idx = findSession(currentPermSid);
      if (idx >= 0) strlcpy(sessions[idx].state, "idle", sizeof(sessions[idx].state));
      currentPermSid[0] = 0;
    }
    permShowNext();
  }

  // Periodic clock tick on idle/clock
  static unsigned long lastTick = 0;
  if ((appState == IDLE || appState == PERMISSION) && (millis() - lastTick > 10000)) {
    lastTick = millis(); needsRedraw = true;
  }
  if (appState == PERMISSION && (millis() - lastTick > 1000)) {
    lastTick = millis(); needsRedraw = true;
  }

  if (needsRedraw) redraw();
  delay(10);
}
