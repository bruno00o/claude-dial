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
#include <Preferences.h>
#include <time.h>
#include <ctype.h>

#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"

// ── BLE UUIDs ────────────────────────────────────────────────────────────────
#define SVC_UUID  "12345678-1234-1234-1234-123456789ABC"
#define RX_UUID   "12345678-1234-1234-1234-123456789ABD"
#define TX_UUID   "12345678-1234-1234-1234-123456789ABE"

// ── Colour palette ── amber phosphor "terminal" theme (matches the simulator) ──
// Physical object is orange/grey/black: amber-orange = active, grey = idle,
// black = background. The 16-bit sprite expects RGB565: LovyanGFX treats a plain
// integer as a raw 565 value (NOT RGB888), so we convert at compile time — a
// bare 0xRRGGBB hex renders as the wrong colour (a dark value comes out blue).
#define RGB565(r, g, b) ((uint16_t)((((r) & 0xF8) << 8) | (((g) & 0xFC) << 3) | ((b) >> 3)))

#define COL_BG          RGB565(0x0A, 0x08, 0x05)   // warm near-black
#define COL_INK         RGB565(0xE9, 0xE2, 0xD6)   // selected row / command text
#define COL_DIM         RGB565(0x8A, 0x81, 0x75)   // headers, footers, hints
#define COL_GRAY        RGB565(0x6F, 0x69, 0x5F)   // idle rows
#define COL_AMBER       RGB565(0xFF, 0xA6, 0x2B)   // active / accent (the orange)
#define COL_AMBER_HOT   RGB565(0xFF, 0xC4, 0x6B)   // waiting rows, warnings
#define COL_RED         RGB565(0xFF, 0x5B, 0x34)   // reject
#define COL_RING        RGB565(0x2A, 0x23, 0x18)   // dim bezel ring
#define COL_ARC_OFF     RGB565(0x14, 0x0F, 0x08)   // spent countdown-arc dots
#define COL_CONFIRM_BG  COL_BG

// ── Types ────────────────────────────────────────────────────────────────────
enum AppState { IDLE, SESSION_LIST, PERMISSION, CONFIRMING, MODE_MENU, BRIGHTNESS, CLOCK };

struct Session {
  char  session_id[40];
  char  project[40];    // human-readable name (basename of cwd), for the roster
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
static void drawSessionList();
static void drawPermission();
static void drawConfirming();
static void drawModeMenu();
static void drawBrightness();
static void drawClock();
static void redraw();
static void handleEncoder(int delta);
static void handlePress();

// ── App state (all mutated only from loop() context) ─────────────────────────
static AppState appState = IDLE;

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

// Display brightness (0-255), adjustable from the menu, persisted in NVS.
static uint8_t   brightness = 180;
static Preferences prefs;

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

// homeView is the base monitor screen: the roster when any agent exists, else
// the idle clock. settleHome applies it, but only from a base view — it never
// pulls you out of a takeover (PERMISSION/CONFIRMING) or a settings screen
// (MODE_MENU/BRIGHTNESS/CLOCK). One place owns the "roster is home" rule.
static AppState homeView() { return activeSessions() > 0 ? SESSION_LIST : IDLE; }
static void settleHome() {
  if (appState == IDLE || appState == SESSION_LIST) appState = homeView();
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
  const char* proj  = doc["project"]    | "";
  const char* state = doc["state"]      | "";
  const char* tool  = doc["tool_name"]  | "";
  const char* cmd   = doc["command"]    | "";
  if (!sid[0]) return;

  if (strcmp(state, "closed") == 0 || strcmp(state, "done") == 0) {
    removeSession(sid);
    settleHome();   // roster ↔ clock as the agent count changes
    needsRedraw = true;
    return;
  }

  int idx = newSession(sid);
  if (idx < 0) return;
  if (proj[0]) strlcpy(sessions[idx].project, proj, sizeof(sessions[idx].project));
  strlcpy(sessions[idx].state,     state, sizeof(sessions[idx].state));
  strlcpy(sessions[idx].tool_name, tool,  sizeof(sessions[idx].tool_name));
  strlcpy(sessions[idx].command,   cmd,   sizeof(sessions[idx].command));

  if (strcmp(state, "permission_request") == 0) {
    bool isNew = !permInQueue(sid) && strcmp(currentPermSid, sid) != 0;
    permEnqueue(sid);
    if (isNew) buzzPending = true;
    if (currentPermSid[0] == 0) permShowNext();
  } else {
    settleHome();   // a live session appeared/changed → show the roster
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
// Spinner frames for "working" rows — ASCII so they render in the mono font
// (the built-in fonts are ASCII-only; no braille glyphs on-device).
static const char SPINNER[4] = { '|', '/', '-', '\\' };
static uint8_t spinFrame = 0;

// A short, human label for a session: the project name if we have one, else a
// truncated session id.
static void sessionLabel(const Session& s, char* out, size_t n) {
  if (s.project[0]) { strlcpy(out, s.project, n); return; }
  strlcpy(out, s.session_id, n);
  if (strlen(s.session_id) > 12 && n > 11) { out[10] = '.'; out[11] = '.'; out[12] = 0; }
}

static void drawBase() {
  canvas.fillScreen(COL_BG);
  canvas.fillCircle(CX, CY, CR - 1, COL_BG);
  canvas.drawCircle(CX, CY, CR - 1, COL_RING);   // dim bezel only — no CRT gloss
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
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  canvas.drawString(tBuf, CX, CY - 12);

  int act = activeSessions();
  char status[40];
  if (!bleConnected)      strlcpy(status, "waiting for claude", sizeof(status));
  else if (act > 0)       snprintf(status, sizeof(status), "%d agent%s", act, act > 1 ? "s" : "");
  else                    strlcpy(status, "waiting for claude", sizeof(status));

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(status, CX, CY + 30);

  canvas.fillCircle(CX, CY + 56, 3, bleConnected ? COL_AMBER : COL_RING);
  canvas.pushSprite(0, 0);
}

static void drawSessionList() {
  drawBase();

  int active[MAX_SESSIONS], n = 0, waits = 0;
  for (int i = 0; i < MAX_SESSIONS; i++) {
    if (!sessions[i].active) continue;
    active[n++] = i;
    if (strcmp(sessions[i].state, "blocked") == 0 ||
        strcmp(sessions[i].state, "permission_request") == 0) waits++;
  }

  // header — "N agents" (dim)
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_DIM, COL_BG);
  char hdr[24];
  snprintf(hdr, sizeof(hdr), "%d agent%s", n, n == 1 ? "" : "s");
  canvas.drawString(hdr, CX, 40);

  if (n == 0) {                 // no agents — the daemon switches us back to the clock
    canvas.pushSprite(0, 0);
    return;
  }

  const int visible = 4;
  if (listScrollOffset > n - visible) listScrollOffset = n - visible;
  if (listScrollOffset < 0) listScrollOffset = 0;

  const int startY = 72, rowH = 32;
  for (int row = 0; row < visible && (listScrollOffset + row) < n; row++) {
    Session& s = sessions[active[listScrollOffset + row]];
    int y = startY + row * rowH;

    bool working = strcmp(s.state, "working") == 0;
    bool waiting = strcmp(s.state, "blocked") == 0 ||
                   strcmp(s.state, "permission_request") == 0;
    uint32_t col = working ? COL_AMBER : waiting ? COL_AMBER_HOT : COL_GRAY;

    char glyph[2] = { working ? SPINNER[spinFrame] : waiting ? '*' : '.', 0 };
    char label[16];
    sessionLabel(s, label, sizeof(label));

    canvas.setTextColor(col, COL_BG);
    canvas.setTextDatum(middle_left);
    canvas.drawString(glyph, CX - 88, y);
    canvas.drawString(label, CX - 70, y);
  }

  // footer — only shown when something actually needs you; silence otherwise
  if (waits > 0) {
    canvas.setTextDatum(middle_center);
    char ft[20]; snprintf(ft, sizeof(ft), "%d waiting", waits);
    canvas.setTextColor(COL_AMBER_HOT, COL_BG);
    canvas.drawString(ft, CX, 206);
  }

  // scroll dots
  if (n > visible) {
    for (int i = 0; i < n; i++) {
      uint32_t dc = (i >= listScrollOffset && i < listScrollOffset + visible) ? COL_AMBER : COL_RING;
      canvas.fillCircle(CX - (n * 6) / 2 + i * 6 + 3, 226, 2, dc);
    }
  }
  canvas.pushSprite(0, 0);
}

static const char* choiceLabels[3] = { "allow once", "always allow", "reject" };

static void drawPermission() {
  int idx = findSession(currentPermSid);
  if (idx < 0 || !sessions[idx].active) { permShowNext(); return; }
  Session& s = sessions[idx];

  drawBase();
  canvas.setFont(&fonts::FreeMono9pt7b);

  // countdown arc around the rim (subtle ambient timer)
  long remaining = permTimeout - millis();
  if (remaining < 0) remaining = 0;
  int arcR = 114, arcSteps = (int)((remaining / 120000.0f) * 180);
  for (int i = 0; i < 180; i++) {
    float a = (i / 180.0f) * 360.0f - 90.0f;
    int ax = CX + (int)(arcR * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(arcR * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 1, (i < arcSteps) ? COL_AMBER : COL_ARC_OFF);
  }

  // project (who) + queue badge
  char who[16]; sessionLabel(s, who, sizeof(who));
  canvas.setTextDatum(middle_left);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(who, CX - 86, 38);
  canvas.setTextDatum(middle_right);
  canvas.setTextColor(COL_DIM, COL_BG);
  if (permQueueCount > 0) {
    char more[14]; snprintf(more, sizeof(more), "<%d more>", permQueueCount);
    canvas.drawString(more, CX + 86, 38);
  } else {
    canvas.drawString("last", CX + 86, 38);
  }

  // eyebrow
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_AMBER_HOT, COL_BG);
  canvas.drawString("permission", CX, 62);

  // "$ tool command" wrapped to two lines
  char full[240];
  char tool[40]; strlcpy(tool, s.tool_name, sizeof(tool));
  for (char* p = tool; *p; p++) *p = tolower(*p);
  snprintf(full, sizeof(full), "$ %s %s", tool, s.command);
  const int wrap = 19;
  char line1[24] = {0}, line2[24] = {0};
  if ((int)strlen(full) <= wrap) {
    strlcpy(line1, full, sizeof(line1));
  } else {
    int cut = wrap;
    while (cut > 0 && full[cut] != ' ') cut--;
    if (cut == 0) cut = wrap;
    strlcpy(line1, full, cut + 1);
    strlcpy(line2, full + cut + 1, sizeof(line2));
  }
  canvas.setTextColor(COL_INK, COL_BG);
  canvas.drawString(line1, CX, 90);
  if (line2[0]) canvas.drawString(line2, CX, 108);

  // choices — mono-aligned, caret ">" on the selected one
  int btnY[3] = { 150, 176, 202 };
  for (int i = 0; i < 3; i++) {
    bool sel = (i == permChoice);
    uint32_t col = !sel ? COL_GRAY : (i == 2 ? COL_RED : COL_AMBER);
    char row[20];
    snprintf(row, sizeof(row), "%s%s", sel ? "> " : "  ", choiceLabels[i]);
    canvas.setTextColor(col, COL_BG);
    canvas.drawString(row, CX, btnY[i]);
  }
  canvas.pushSprite(0, 0);
}

static void drawConfirming() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  bool rejected = (strcmp(confirmMsg, "reject") == 0);
  canvas.setTextColor(rejected ? COL_RED : COL_AMBER, COL_BG);
  canvas.drawString("sent", CX, CY - 12);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(confirmMsg, CX, CY + 24);
  canvas.pushSprite(0, 0);
}

static const char* menuLabels[] = { "monitor", "brightness", "clock" };
static const int MENU_N = 3;

static void drawModeMenu() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("settings", CX, 60);

  for (int i = 0; i < MENU_N; i++) {
    bool sel = (i == menuChoice);
    int  y   = CY - (MENU_N - 1) * 15 + i * 30;   // centered, 30px apart
    char row[24];
    snprintf(row, sizeof(row), "%s%s", sel ? "> " : "  ", menuLabels[i]);
    canvas.setTextColor(sel ? COL_AMBER : COL_GRAY, COL_BG);
    canvas.drawString(row, CX, y);
  }
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("turn / press", CX, 200);
  canvas.pushSprite(0, 0);
}

static void drawBrightness() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("brightness", CX, 74);

  char pct[8]; snprintf(pct, sizeof(pct), "%d%%", (brightness * 100) / 255);
  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(pct, CX, CY);

  int bw = 150, bh = 8, bx = CX - bw / 2, by = 158;
  canvas.drawRoundRect(bx, by, bw, bh, 4, COL_RING);
  canvas.fillRoundRect(bx + 1, by + 1, (bw - 2) * brightness / 255, bh - 2, 3, COL_AMBER);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("turn / press", CX, 196);
  canvas.pushSprite(0, 0);
}

// A dedicated clock face (date + a minute marker on the rim), distinct from the
// idle-monitor screen which shows "waiting for claude".
static void drawClock() {
  drawBase();
  char tBuf[12], dBuf[20];
  getTimeStr(tBuf, dBuf);

  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(tBuf, CX, CY - 6);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(dBuf, CX, CY + 30);

  auto dt = M5Dial.Rtc.getDateTime();
  float angle = (dt.time.minutes / 60.0f) * 360.0f - 90.0f;
  int ax = CX + (int)((CR - 6) * cosf(angle * DEG_TO_RAD));
  int ay = CY + (int)((CR - 6) * sinf(angle * DEG_TO_RAD));
  canvas.fillCircle(ax, ay, 4, COL_AMBER);
  canvas.pushSprite(0, 0);
}

static void redraw() {
  needsRedraw = false;
  switch (appState) {
    case IDLE:         drawIdle();         break;
    case SESSION_LIST: drawSessionList(); break;
    case PERMISSION:   drawPermission();  break;
    case CONFIRMING:   drawConfirming();  break;
    case MODE_MENU:    drawModeMenu();    break;
    case BRIGHTNESS:   drawBrightness();  break;
    case CLOCK:        drawClock();       break;
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
      menuChoice = (menuChoice + (delta > 0 ? 1 : -1) + MENU_N) % MENU_N;
      needsRedraw = true;
      break;
    case BRIGHTNESS: {
      int b = (int)brightness + (delta > 0 ? 12 : -12);
      if (b < 20)  b = 20;
      if (b > 255) b = 255;
      brightness = (uint8_t)b;
      M5Dial.Display.setBrightness(brightness);
      needsRedraw = true;
      break;
    }
    default: break;
  }
}

static void handlePress() {
  switch (appState) {
    case IDLE:
    case SESSION_LIST:
      // Monitor views have nothing to confirm — the roster auto-shows when
      // agents exist, and permission requests take over the screen on their own.
      // (Long-press still opens the mode menu.)
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
      if (menuChoice == 0)                          // monitor — back to the main view
        appState = homeView();
      else if (menuChoice == 1)                     // brightness
        appState = BRIGHTNESS;
      else                                          // clock
        appState = CLOCK;
      needsRedraw = true;
      break;

    case BRIGHTNESS:                               // confirm — persist and back to menu
      prefs.putUChar("bright", brightness);
      appState    = MODE_MENU;
      menuChoice  = 1;
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

  prefs.begin("cdial", false);
  brightness = prefs.getUChar("bright", 180);
  M5Dial.Display.setBrightness(brightness);

  canvas.setColorDepth(16);          // RGB565 — set depth before allocating
  canvas.createSprite(240, 240);

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

  // If the host link drops, the session view is no longer trustworthy: the
  // daemon can't tell us a session went idle while we're disconnected, so a
  // frozen "working" roster (spinner still turning) would lie. Drop it and fall
  // back to the clock; a reconnect resyncs the full state within a sweep tick.
  static bool wasConnected = false;
  if (wasConnected && !bleConnected) {
    memset(sessions, 0, sizeof(sessions));
    sessionCount   = 0;
    permQueueCount = 0;
    currentPermSid[0] = 0;
    if (appState != MODE_MENU && appState != BRIGHTNESS && appState != CLOCK)
      appState = homeView();   // sessions cleared → clock, unless a settings screen is up
    needsRedraw = true;
  }
  wasConnected = bleConnected;

  // Encoder: accumulate counts, step once per detent
  static long encAccum = 0;
  long pos   = M5Dial.Encoder.read();
  long delta = pos - lastEncoderPos;
  if (delta != 0) {
    lastEncoderPos = pos;
    encAccum += delta;
    const long ENC_DETENT = 4;   // this encoder emits 4 counts per physical detent
    while (encAccum >=  ENC_DETENT) { handleEncoder(+1); encAccum -= ENC_DETENT; }
    while (encAccum <= -ENC_DETENT) { handleEncoder(-1); encAccum += ENC_DETENT; }
  }

  // Button: short click vs long hold, cleanly separated
  if (M5Dial.BtnA.wasClicked()) {
    handlePress();
  }
  if (M5Dial.BtnA.wasHold()) {
    if (appState != MODE_MENU && appState != BRIGHTNESS) {
      appState    = MODE_MENU;
      menuChoice  = 0;
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

  // Periodic redraws: idle clock (10s), permission countdown arc (1s), and the
  // roster spinner (~150ms, only while a session is "working").
  static unsigned long lastSlow = 0, lastFast = 0;
  if ((appState == IDLE || appState == CLOCK) && millis() - lastSlow > 10000) {
    lastSlow = millis(); needsRedraw = true;
  }
  if (appState == PERMISSION && millis() - lastFast > 1000) {
    lastFast = millis(); needsRedraw = true;
  }
  if (appState == SESSION_LIST && millis() - lastFast > 150) {
    lastFast = millis();
    bool anyWorking = false;
    for (int i = 0; i < MAX_SESSIONS; i++)
      if (sessions[i].active && strcmp(sessions[i].state, "working") == 0) anyWorking = true;
    if (anyWorking) { spinFrame = (spinFrame + 1) & 3; needsRedraw = true; }
  }

  if (needsRedraw) redraw();
  delay(10);
}
