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
#include <Update.h>
#include <time.h>
#include <ctype.h>

#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"

// ── BLE UUIDs ────────────────────────────────────────────────────────────────
#define SVC_UUID  "12345678-1234-1234-1234-123456789ABC"
#define RX_UUID   "12345678-1234-1234-1234-123456789ABD"
#define TX_UUID   "12345678-1234-1234-1234-123456789ABE"
// OTA firmware update: JSON control in, raw image bytes in, JSON status out.
#define OTA_CTRL_UUID    "12345678-1234-1234-1234-123456789AC0"  // WRITE  (begin/end/abort)
#define OTA_DATA_UUID    "12345678-1234-1234-1234-123456789AC1"  // WRITE  (raw chunks)
#define OTA_STATUS_UUID  "12345678-1234-1234-1234-123456789AC2"  // NOTIFY (ready/progress/done/error)

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
enum AppState { IDLE, SESSION_LIST, PERMISSION, CONFIRMING, MODE_MENU, BRIGHTNESS, CLOCK, OTA, OTA_PROMPT, FIRMWARE_INFO, SOUND };

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
static void sendHello();
static void sendOtaStatus(const char* state, const char* msg, uint8_t pct);
static void sendOtaConfirm();
static void drawOtaProgress();
static void drawOtaPrompt();
static void drawFirmwareInfo();

// Firmware version, announced to the host on connect so the bridge can flag an
// available OTA update. CI injects the exact release version via a generated
// version_override.h (see the firmware release job) so a published image always
// reports its true version; the fallback below is for local/dev builds and is
// kept current by release-please.
#if defined(__has_include)
#  if __has_include("version_override.h")
#    include "version_override.h"
#  endif
#endif
#ifndef CLAUDE_DIAL_FW_VERSION
#  define CLAUDE_DIAL_FW_VERSION "0.12.0"  // x-release-please-version
#endif
static const char* FW_VERSION = CLAUDE_DIAL_FW_VERSION;

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
static void drawSound();
static void drawClock();
static void redraw();
static void handleEncoder(int delta);
static void handlePress();
static void handleTouch(int x, int y);
static void commitDecision();

// ── App state (all mutated only from loop() context) ─────────────────────────
static AppState appState = IDLE;

static Session sessions[MAX_SESSIONS];
static int     sessionCount = 0;

// Permission FIFO
// PERM_TIMEOUT_MS matches the daemon's default --timeout (90s). If the two
// drift apart the Dial keeps showing a prompt the host has already abandoned
// (or vice-versa); either way an unanswered permission falls back to "ask".
static const unsigned long PERM_TIMEOUT_MS = 90000UL;
static char  permQueue[MAX_SESSIONS][40];
static int   permQueueCount = 0;
static char  currentPermSid[40] = "";     // "" = nothing shown
static long  permTimeout = 0;
static int   permChoice = 0;              // 0 allow once | 1 always | 2 reject
// Long-command reader (P2): tap the command to read it full-screen, encoder
// scrolls. permCmdOverflow is set each render so touch knows there's more to see.
static bool  permCmdView     = false;
static int   permCmdScroll    = 0;
static bool  permCmdOverflow = false;

static int   menuChoice = 0;
static long  lastEncoderPos = 0;
static int   listScrollOffset = 0;

// Display brightness (0-255), adjustable from the menu, persisted in NVS.
static uint8_t   brightness = 180;
static Preferences prefs;

// Speaker volume (0-255, 0 = muted), adjustable from the menu, persisted in NVS.
static uint8_t   soundVol = 128;

// One recognizable sound per event, all routed through playEarcon so volume/mute
// applies uniformly and the palette stays consistent. Sequenced with short
// delays (loop context) — each stays under ~300ms so the UI never visibly stalls.
enum Earcon {
  SND_BOOT, SND_NEEDS_YOU, SND_TICK, SND_ALLOW, SND_REJECT,
  SND_UNDO, SND_DONE, SND_OTA_DONE, SND_ERROR,
};

static void playEarcon(Earcon e) {
  if (soundVol == 0) return;                 // muted → stay silent (and skip the delays)
  auto& spk = M5Dial.Speaker;
  switch (e) {
    case SND_BOOT:      spk.tone(880, 80);  delay(95);  spk.tone(1320, 90);  break;
    case SND_NEEDS_YOU: spk.tone(1047, 110); delay(120); spk.tone(1568, 140); break;  // rising "ding-dong up"
    case SND_TICK:      spk.tone(1500, 25);                                    break;  // soft "registered"
    case SND_ALLOW:     spk.tone(1568, 60);  delay(70);  spk.tone(2093, 90);  break;  // bright up  G6→C7
    case SND_REJECT:    spk.tone(784, 70);   delay(80);  spk.tone(523, 110);  break;  // low down   G5→C5
    case SND_UNDO:      spk.tone(784, 40);   delay(55);  spk.tone(587, 55);   break;  // short descend
    case SND_DONE:      spk.tone(1047, 70); delay(80); spk.tone(1319, 70); delay(80); spk.tone(1568, 130); break;  // C-E-G "all done"
    case SND_OTA_DONE:  spk.tone(1047, 60); delay(70); spk.tone(1319, 60); delay(70);
                        spk.tone(1568, 60); delay(70); spk.tone(2093, 140); break;    // C-E-G-C jingle
    case SND_ERROR:     spk.tone(400, 90);   delay(100); spk.tone(300, 130);  break;  // soft low blip
  }
}

// Decision grace window: a picked choice is NOT sent immediately. It waits
// CONFIRM_MS on the CONFIRMING screen, where a tap/press undoes it; only when
// the window elapses does the decision actually go to the host. That's what
// makes undo real — nothing was transmitted yet, so there's nothing to walk back
// on Claude Code's side.
static const unsigned long CONFIRM_MS = 1500;
static unsigned long confirmStart  = 0;
static int           confirmChoice = 0;   // the deferred permChoice awaiting commit

static bool needsRedraw = true;
static bool buzzPending = false;

// ── BLE / queue handles ──────────────────────────────────────────────────────
static NimBLECharacteristic* txChar  = nullptr;

// OTA state. The write callbacks feed Update directly (the host paces with
// with-response writes, so no buffering is needed); loop() finalizes + reboots
// off the BLE task. otaWritten/otaTotal drive the progress screen.
static NimBLECharacteristic* otaStatusChar = nullptr;
static volatile bool     otaActive   = false;
static volatile bool     otaFinish   = false;   // ota_end received -> end()+reboot in loop()
static volatile uint32_t otaTotal    = 0;
static volatile uint32_t otaWritten  = 0;
static AppState          otaPrevState = IDLE;    // where to return if the OTA aborts

// OTA "update available" tactile prompt (phase 2b).
static char          otaAvailVersion[16]   = "";  // version the host offers ("" = none)
static char          otaTargetVersion[16]  = "";  // version being installed (shown on progress)
static int           otaPromptChoice       = 0;   // 0 = install now, 1 = later
static bool          otaStarting           = false;
static unsigned long otaPromptStartedAt    = 0;
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
      permChoice    = 0;
      permCmdView   = false;
      permCmdScroll = 0;
      permTimeout   = millis() + PERM_TIMEOUT_MS;
      appState      = PERMISSION;
      needsRedraw   = true;
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
    sendHello();  // piggyback our version now that the host is subscribed
    return;
  }

  // Control: the host has a newer firmware and offers a tactile install.
  if (strcmp(type, "ota_available") == 0) {
    const char* v = doc["version"] | "";
    strlcpy(otaAvailVersion, v, sizeof(otaAvailVersion));
    if (v[0]) {
      // Don't hijack an active permission or an in-flight update.
      if (appState != PERMISSION && appState != OTA && appState != OTA_PROMPT) {
        otaPromptChoice = 0; otaStarting = false;
        appState = OTA_PROMPT; needsRedraw = true;
      }
    } else if (appState == OTA_PROMPT) {   // update no longer offered → dismiss
      otaStarting = false;
      appState = homeView(); needsRedraw = true;
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

// Announce our firmware version to the host. Sent in reply to set_time, which
// the host writes right after subscribing to TX — so the notify is guaranteed
// to be received (no connect-time race on the subscription).
static void sendHello() {
  if (!txChar || !bleConnected) return;
  JsonDocument doc;
  doc["type"] = "hello";
  doc["fw"]   = FW_VERSION;
  doc["ota"]  = true;   // this firmware accepts BLE OTA updates
  char buf[64];
  size_t n = serializeJson(doc, buf, sizeof(buf));
  txChar->setValue((uint8_t*)buf, n);
  txChar->notify();
}

// Report OTA progress/result to the host. pct is only meaningful for "progress".
static void sendOtaStatus(const char* state, const char* msg, uint8_t pct) {
  if (!otaStatusChar) return;
  JsonDocument doc;
  doc["ota"] = state;
  if (msg && msg[0])                 doc["msg"] = msg;
  if (strcmp(state, "progress") == 0) doc["pct"] = pct;
  char buf[96];
  size_t n = serializeJson(doc, buf, sizeof(buf));
  otaStatusChar->setValue((uint8_t*)buf, n);
  otaStatusChar->notify();
}

// Tell the host the user chose to install: it replies with ota_begin + the stream.
static void sendOtaConfirm() {
  if (!txChar || !bleConnected) return;
  JsonDocument doc;
  doc["type"] = "ota_confirm";
  char buf[48];
  size_t n = serializeJson(doc, buf, sizeof(buf));
  txChar->setValue((uint8_t*)buf, n);
  txChar->notify();
}

// OTA control: begin (size) opens the inactive slot, end finalizes+reboots (done
// in loop()), abort discards. Update writes to the *inactive* partition and only
// switches the boot slot on a verified end(), so an interrupted transfer never
// touches the running firmware — the Dial can't be bricked mid-update.
class OtaCtrlCallback : public NimBLECharacteristicCallbacks {
  void onWrite(NimBLECharacteristic* pChar) override {
    std::string val = pChar->getValue();
    JsonDocument doc;
    if (deserializeJson(doc, val.data(), val.size())) return;
    const char* type = doc["type"] | "";
    if (strcmp(type, "ota_begin") == 0) {
      uint32_t size = doc["size"] | 0u;
      if (size == 0 || !Update.begin(size)) {
        sendOtaStatus("error", "begin failed", 0);
        return;
      }
      strlcpy(otaTargetVersion, doc["version"] | "", sizeof(otaTargetVersion));
      otaTotal = size; otaWritten = 0; otaFinish = false; otaActive = true;
      otaStarting = false;
      otaPrevState = (appState == OTA || appState == OTA_PROMPT) ? IDLE : appState;
      appState = OTA; needsRedraw = true;
      sendOtaStatus("ready", "", 0);
    } else if (strcmp(type, "ota_end") == 0) {
      otaFinish = true;                 // finalize + reboot in loop()
    } else if (strcmp(type, "ota_abort") == 0) {
      if (otaActive) { Update.abort(); otaActive = false; appState = otaPrevState; needsRedraw = true; }
      sendOtaStatus("aborted", "", 0);
    }
  }
};

// OTA data: raw image bytes, fed straight to Update. The host writes with
// response, so returning from onWrite acks the chunk and paces the next one.
class OtaDataCallback : public NimBLECharacteristicCallbacks {
  void onWrite(NimBLECharacteristic* pChar) override {
    if (!otaActive) return;
    std::string val = pChar->getValue();
    if (Update.write((uint8_t*)val.data(), val.size()) != val.size()) {
      Update.abort(); otaActive = false;
      sendOtaStatus("error", "write failed", 0);
      appState = otaPrevState; needsRedraw = true;
      return;
    }
    otaWritten += val.size();
    static uint8_t lastPct = 255;
    uint8_t pct = otaTotal ? (uint8_t)((uint64_t)otaWritten * 100 / otaTotal) : 0;
    if (pct != lastPct) {
      lastPct = pct;
      needsRedraw = true;
      if (pct % 5 == 0) sendOtaStatus("progress", "", pct);
    }
  }
};

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

// Roster priority, mirroring the daemon's prioritize(): needs-you → working →
// idle. The daemon already sorts the snapshot, but the Dial re-buckets sessions
// into its own slots by id, so it re-applies the same ranking at render time.
static int sessionRank(const Session& s) {
  if (strcmp(s.state, "blocked") == 0 ||
      strcmp(s.state, "permission_request") == 0) return 0;   // needs you
  if (strcmp(s.state, "working") == 0)            return 1;
  return 2;                                                    // idle
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

  // Stable insertion sort by priority — keeps slot order within a tier so rows
  // don't jump around, and n <= MAX_SESSIONS (8) so this is trivially cheap.
  for (int i = 1; i < n; i++) {
    int v = active[i], r = sessionRank(sessions[v]), j = i - 1;
    while (j >= 0 && sessionRank(sessions[active[j]]) > r) { active[j + 1] = active[j]; j--; }
    active[j + 1] = v;
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
    uint32_t col;
    if (working) {
      col = COL_AMBER;                    // busy: the warm orange
    } else if (waiting) {
      // needs-you: a warm-white row that breathes, so the eye lands on it and it
      // never reads like the orange "working" state.
      float ph = (millis() % 1100) / 1100.0f;
      float k  = ph < 0.5f ? ph * 2.0f : (1.0f - ph) * 2.0f;   // triangle 0..1..0
      uint8_t v = 120 + (uint8_t)(k * 135.0f);                 // 120..255
      col = RGB565(v, v, (uint8_t)(v * 0.90f));                // slightly warm white
    } else {
      col = COL_GRAY;                     // idle
    }

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
    canvas.setTextColor(COL_INK, COL_BG);   // match the white "needs you" rows
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

// isDangerous flags commands worth a second look (P5): destructive, privilege-
// escalating, or pipe-to-shell. A curated, low-false-positive list — "> /dev/
// null" is fine, "of=/dev/" and "> /dev/sd" are not; curl/wget only count when
// piped into a shell. Just drives the red styling; it never changes the choice.
static bool isDangerous(const char* cmd) {
  char c[220];
  strlcpy(c, cmd, sizeof(c));
  for (char* p = c; *p; p++) *p = tolower((unsigned char)*p);
  static const char* pats[] = {
    "rm -rf", "rm -fr", "rm -r ", "sudo ", "chmod 777", "mkfs", "dd if=",
    ":(){", "of=/dev/", "> /dev/sd", "git push --force", "push -f", "eval ",
  };
  for (unsigned i = 0; i < sizeof(pats) / sizeof(pats[0]); i++)
    if (strstr(c, pats[i])) return true;
  bool pipeSh = strstr(c, "| sh") || strstr(c, "|sh") ||
                strstr(c, "| bash") || strstr(c, "|bash");
  return (strstr(c, "curl") || strstr(c, "wget")) && pipeSh;
}

// wrapAll breaks s into out[][22] at the given width, preferring space breaks
// and hard-cutting a word with no space in the window. Returns the line count
// (capped at maxLines). 14 lines at width 19 covers the full command buffer.
static int wrapAll(const char* s, char out[][22], int maxLines, int width) {
  int n = 0, len = (int)strlen(s), pos = 0;
  while (pos < len && n < maxLines) {
    int remain = len - pos;
    if (remain <= width) { strlcpy(out[n++], s + pos, remain + 1); break; }
    int cut = width;
    while (cut > 0 && s[pos + cut] != ' ') cut--;
    if (cut <= 0) cut = width;                  // no space in window → hard cut
    strlcpy(out[n++], s + pos, cut + 1);
    pos += cut;
    while (pos < len && s[pos] == ' ') pos++;   // swallow the break space(s)
  }
  return n;
}

static void drawPermission() {
  int idx = findSession(currentPermSid);
  if (idx < 0 || !sessions[idx].active) { permShowNext(); return; }
  Session& s = sessions[idx];

  drawBase();
  canvas.setFont(&fonts::FreeMono9pt7b);

  // Compose "$ tool command", wrap it fully, and flag risky commands (P5).
  bool danger = isDangerous(s.command);
  uint32_t cmdCol = danger ? COL_RED : COL_INK;
  char full[248];
  char tool[40]; strlcpy(tool, s.tool_name, sizeof(tool));
  for (char* p = tool; *p; p++) *p = tolower(*p);
  snprintf(full, sizeof(full), "$ %s %s", tool, s.command);
  char lines[14][22];
  int total = wrapAll(full, lines, 14, 19);
  permCmdOverflow = (total > 3);

  // countdown arc around the rim (ambient timer) — red when the command is risky
  long remaining = permTimeout - millis();
  if (remaining < 0) remaining = 0;
  int arcR = 114, arcSteps = (int)(((float)remaining / PERM_TIMEOUT_MS) * 180);
  uint32_t arcCol = danger ? COL_RED : COL_AMBER;
  for (int i = 0; i < 180; i++) {
    float a = (i / 180.0f) * 360.0f - 90.0f;
    int ax = CX + (int)(arcR * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(arcR * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 1, (i < arcSteps) ? arcCol : COL_ARC_OFF);
  }

  // ── P2: full-screen command reader — scroll the whole command with the encoder
  if (permCmdView) {
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(danger ? COL_RED : COL_DIM, COL_BG);
    canvas.drawString(danger ? "! command" : "command", CX, 34);

    const int visible = 6, top = 62, lh = 20;
    if (permCmdScroll > total - visible) permCmdScroll = total - visible;
    if (permCmdScroll < 0) permCmdScroll = 0;
    canvas.setTextDatum(middle_left);
    canvas.setTextColor(cmdCol, COL_BG);
    for (int r = 0; r < visible && (permCmdScroll + r) < total; r++)
      canvas.drawString(lines[permCmdScroll + r], 22, top + r * lh);

    canvas.setTextDatum(middle_center);
    canvas.setTextColor(COL_DIM, COL_BG);
    if (total > visible) {
      char sc[16]; snprintf(sc, sizeof(sc), "%d/%d", permCmdScroll + 1, total - visible + 1);
      canvas.drawString(sc, CX, 196);
    }
    canvas.drawString("press: back", CX, 216);
    canvas.pushSprite(0, 0);
    return;
  }

  // ── choices view ──
  // project (who) + queue badge
  char who[16]; sessionLabel(s, who, sizeof(who));
  canvas.setTextDatum(middle_left);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(who, CX - 86, 34);
  canvas.setTextDatum(middle_right);
  canvas.setTextColor(COL_DIM, COL_BG);
  if (permQueueCount > 0) {
    char more[14]; snprintf(more, sizeof(more), "<%d more>", permQueueCount);
    canvas.drawString(more, CX + 86, 34);
  } else {
    canvas.drawString("last", CX + 86, 34);
  }

  // eyebrow — warns when risky
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(danger ? COL_RED : COL_AMBER_HOT, COL_BG);
  canvas.drawString(danger ? "! caution" : "permission", CX, 58);

  // up to 3 command lines; when it overflows show 2 + a "tap to read" hint
  int shown = permCmdOverflow ? 2 : total;
  canvas.setTextColor(cmdCol, COL_BG);
  for (int r = 0; r < shown; r++) canvas.drawString(lines[r], CX, 84 + r * 18);
  if (permCmdOverflow) {
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("... tap to read", CX, 84 + 2 * 18);
  }

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
  bool rejected = (confirmChoice == 2);
  uint32_t accent = rejected ? COL_RED : COL_AMBER;

  // Depleting grace-window arc on the rim: when it empties the decision commits.
  long remaining = (long)(confirmStart + CONFIRM_MS) - (long)millis();
  if (remaining < 0) remaining = 0;
  int arcR = 114, arcSteps = (int)(((float)remaining / CONFIRM_MS) * 180);
  for (int i = 0; i < 180; i++) {
    float a = (i / 180.0f) * 360.0f - 90.0f;
    int ax = CX + (int)(arcR * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(arcR * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 1, (i < arcSteps) ? accent : COL_ARC_OFF);
  }

  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("sending", CX, 88);

  canvas.setFont(&fonts::FreeMonoBold12pt7b);
  canvas.setTextColor(accent, COL_BG);
  canvas.drawString(choiceLabels[confirmChoice], CX, 118);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_INK, COL_BG);
  canvas.drawString("tap to undo", CX, 156);
  canvas.pushSprite(0, 0);
}

static const char* menuLabels[] = { "monitor", "brightness", "sound", "clock", "firmware" };
static const int MENU_N = 5;

static void drawModeMenu() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("settings", CX, 34);

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

static void drawSound() {
  drawBase();
  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("sound", CX, 74);

  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  if (soundVol == 0) {
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.drawString("muted", CX, CY);
  } else {
    char pct[8]; snprintf(pct, sizeof(pct), "%d%%", (soundVol * 100) / 255);
    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString(pct, CX, CY);
  }

  int bw = 150, bh = 8, bx = CX - bw / 2, by = 158;
  canvas.drawRoundRect(bx, by, bw, bh, 4, COL_RING);
  if (soundVol) canvas.fillRoundRect(bx + 1, by + 1, (bw - 2) * soundVol / 255, bh - 2, 3, COL_AMBER);

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
    case SOUND:        drawSound();       break;
    case CLOCK:        drawClock();       break;
    case OTA:          drawOtaProgress(); break;
    case OTA_PROMPT:   drawOtaPrompt();   break;
    case FIRMWARE_INFO: drawFirmwareInfo(); break;
  }
}

// Full-screen "installing firmware" takeover: a filling arc + big percentage.
static void drawOtaProgress() {
  drawBase();
  uint8_t pct = otaTotal ? (uint8_t)((uint64_t)otaWritten * 100 / otaTotal) : 0;

  int arcR = 114, arcSteps = (int)((pct / 100.0f) * 180);
  for (int i = 0; i < 180; i++) {
    float a = (i / 180.0f) * 360.0f - 90.0f;
    int ax = CX + (int)(arcR * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(arcR * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 1, (i < arcSteps) ? COL_AMBER : COL_ARC_OFF);
  }

  canvas.setTextDatum(middle_center);
  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  char head[24];
  if (otaTargetVersion[0]) snprintf(head, sizeof(head), "installing %s", otaTargetVersion);
  else                     snprintf(head, sizeof(head), "updating firmware");
  canvas.drawString(head, CX, 92);

  char pctStr[8]; snprintf(pctStr, sizeof(pctStr), "%u%%", pct);
  canvas.setFont(&fonts::FreeMonoBold18pt7b);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(pctStr, CX, 128);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("keep the dial close", CX, 162);

  canvas.pushSprite(0, 0);
}

// "Firmware X available — install now / later", chosen with the encoder.
static void drawOtaPrompt() {
  drawBase();
  canvas.setTextDatum(middle_center);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("firmware update", CX, 58);

  canvas.setFont(&fonts::FreeMonoBold12pt7b);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(otaAvailVersion, CX, 88);

  if (otaStarting) {
    canvas.setFont(&fonts::FreeMono9pt7b);
    canvas.setTextColor(COL_INK, COL_BG);
    canvas.drawString("starting…", CX, 140);
    canvas.pushSprite(0, 0);
    return;
  }

  const char* opts[2] = { "install now", "later" };
  for (int i = 0; i < 2; i++) {
    bool sel = (otaPromptChoice == i);
    canvas.setFont(&fonts::FreeMono9pt7b);
    canvas.setTextColor(sel ? COL_INK : COL_GRAY, COL_BG);
    char row[24]; snprintf(row, sizeof(row), "%s%s", sel ? "> " : "  ", opts[i]);
    canvas.drawString(row, CX, 130 + i * 26);
  }
  canvas.pushSprite(0, 0);
}

// Settings > firmware: the running version and whether an update is offered.
static void drawFirmwareInfo() {
  drawBase();
  canvas.setTextDatum(middle_center);

  canvas.setFont(&fonts::FreeMono9pt7b);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("firmware", CX, 62);

  char v[24]; snprintf(v, sizeof(v), "v%s", FW_VERSION);
  canvas.setFont(&fonts::FreeMonoBold12pt7b);
  canvas.setTextColor(COL_INK, COL_BG);
  canvas.drawString(v, CX, 92);

  canvas.setFont(&fonts::FreeMono9pt7b);
  if (otaAvailVersion[0]) {
    char u[28]; snprintf(u, sizeof(u), "update %s", otaAvailVersion);
    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString(u, CX, 132);
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("press to install", CX, 158);
  } else {
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.drawString("up to date", CX, 138);
  }
  canvas.pushSprite(0, 0);
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
      if (permCmdView) {                        // reader: scroll the command (clamped in draw)
        permCmdScroll += (delta > 0) ? 1 : -1;
        if (permCmdScroll < 0) permCmdScroll = 0;
      } else {
        permChoice = (permChoice + (delta > 0 ? 1 : -1) + 3) % 3;
      }
      needsRedraw = true;
      break;
    case OTA_PROMPT:
      if (!otaStarting) {
        otaPromptChoice = (otaPromptChoice + (delta > 0 ? 1 : -1) + 2) % 2;
        needsRedraw = true;
      }
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
    case SOUND: {
      int v = (int)soundVol + (delta > 0 ? 16 : -16);
      if (v < 0)   v = 0;                     // 0 = muted
      if (v > 255) v = 255;
      soundVol = (uint8_t)v;
      M5Dial.Speaker.setVolume(soundVol);
      playEarcon(SND_TICK);                   // hear the new level (silent if muted)
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
      if (permCmdView) { permCmdView = false; permCmdScroll = 0; needsRedraw = true; break; }
      int idx = findSession(currentPermSid);
      if (idx < 0 || !sessions[idx].active) { permShowNext(); break; }
      // Don't send yet — open the grace window. currentPermSid stays set so we
      // can either commit or undo. commitDecision() (loop) sends when it elapses.
      confirmChoice = permChoice;
      playEarcon(SND_TICK);
      appState     = CONFIRMING;
      confirmStart = millis();
      needsRedraw  = true;
      break;
    }

    case CONFIRMING:                         // undo — nothing was sent, back to the choice
      playEarcon(SND_UNDO);
      permChoice  = confirmChoice;           // keep the selection they had
      appState    = PERMISSION;
      needsRedraw = true;
      break;

    case OTA_PROMPT:
      if (otaStarting) break;
      if (otaPromptChoice == 0) {           // install now → ask the host to begin
        sendOtaConfirm();
        playEarcon(SND_TICK);
        otaStarting = true;
        otaPromptStartedAt = millis();
      } else {                              // later → dismiss, but keep the offer
        appState = homeView();              // (still reachable via settings > firmware)
      }
      needsRedraw = true;
      break;

    case MODE_MENU:
      switch (menuChoice) {
        case 0: appState = homeView();    break;   // monitor — back to the main view
        case 1: appState = BRIGHTNESS;    break;
        case 2: appState = SOUND;         break;
        case 3: appState = CLOCK;         break;
        case 4: appState = FIRMWARE_INFO; break;
      }
      needsRedraw = true;
      break;

    case FIRMWARE_INFO:
      if (otaAvailVersion[0]) {            // an update is offered → go to install/later
        otaPromptChoice = 0; otaStarting = false;
        appState = OTA_PROMPT;
      } else {
        appState = MODE_MENU; menuChoice = 4;
      }
      needsRedraw = true;
      break;

    case BRIGHTNESS:                               // confirm — persist and back to menu
      prefs.putUChar("bright", brightness);
      appState    = MODE_MENU;
      menuChoice  = 1;
      needsRedraw = true;
      break;

    case SOUND:                                    // confirm — persist and back to menu
      prefs.putUChar("vol", soundVol);
      appState    = MODE_MENU;
      menuChoice  = 2;
      needsRedraw = true;
      break;
  }
}

// Touch: tap a choice directly instead of turn-then-click. Maps the tap's Y to
// the on-screen row, sets the same selection the encoder would, then reuses
// handlePress() so touch and encoder+button share one code path. Only the
// decision/selection screens react; monitor screens ignore taps so a stray
// touch can never approve or reject anything.
static void handleTouch(int x, int y) {
  (void)x;   // vertical lists: Y alone picks the row (X reserved for future swipes)
  switch (appState) {
    case PERMISSION:
      if (permCmdView) {                               // reading → any tap closes the reader
        permCmdView = false; permCmdScroll = 0; needsRedraw = true; break;
      }
      if (y < 138) {                                   // tap the command → read it in full (if there's more)
        if (permCmdOverflow) { permCmdView = true; permCmdScroll = 0; needsRedraw = true; }
        break;
      }
      permChoice = (y < 164) ? 0 : (y < 190) ? 1 : 2;  // rows at 150 / 176 / 202
      needsRedraw = true;
      handlePress();
      break;

    case MODE_MENU: {
      if (y < 45) break;                               // title zone
      int base = CY - (MENU_N - 1) * 15;               // first entry's y
      int c = (y - base + 15) / 30;                    // 30px rows, nearest
      if (c < 0) c = 0;
      if (c >= MENU_N) c = MENU_N - 1;
      menuChoice = c;
      needsRedraw = true;
      handlePress();
      break;
    }

    case OTA_PROMPT:
      if (otaStarting || y < 118) break;               // options at 130 / 156
      otaPromptChoice = (y < 143) ? 0 : 1;
      needsRedraw = true;
      handlePress();
      break;

    case FIRMWARE_INFO:
    case CONFIRMING:
      handlePress();                                   // tap anywhere = the button action
      break;

    default: break;                                    // idle / clock / roster: ignore taps
  }
}

// commitDecision fires when the grace window elapses without an undo: only now
// is the decision actually sent to the host. Optimistically updates the local
// row state, plays the "sent" earcon, then advances to the next pending prompt.
static void commitDecision() {
  int idx = findSession(currentPermSid);
  if (idx >= 0 && sessions[idx].active) {
    const char* dec = (confirmChoice == 0) ? "allow_once"
                    : (confirmChoice == 1) ? "always_allow" : "reject";
    sendDecision(currentPermSid, dec);
    strlcpy(sessions[idx].state, (confirmChoice == 2) ? "idle" : "working",
            sizeof(sessions[idx].state));
    playEarcon(confirmChoice == 2 ? SND_REJECT : SND_ALLOW);
  }
  permShowNext();
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
  soundVol = prefs.getUChar("vol", 128);
  M5Dial.Speaker.setVolume(soundVol);

  canvas.setColorDepth(16);          // RGB565 — set depth before allocating
  canvas.createSprite(240, 240);

  M5Dial.Rtc.begin();
  memset(sessions, 0, sizeof(sessions));

  rxQueue = xQueueCreate(8, sizeof(RxMsg));

  NimBLEDevice::init("Claude-Dial");
  NimBLEDevice::setPower(ESP_PWR_LVL_P9);
  NimBLEDevice::setMTU(517);   // 512-byte payloads keep OTA (~0.7 MB) to a few thousand writes

  NimBLEServer* pServer = NimBLEDevice::createServer();
  pServer->setCallbacks(new ServerCallbacks());

  NimBLEService* pService = pServer->createService(SVC_UUID);

  NimBLECharacteristic* rxChar = pService->createCharacteristic(
      RX_UUID, NIMBLE_PROPERTY::WRITE | NIMBLE_PROPERTY::WRITE_NR);
  rxChar->setCallbacks(new RxCallback());

  txChar = pService->createCharacteristic(TX_UUID, NIMBLE_PROPERTY::NOTIFY);

  // OTA characteristics on the same service.
  NimBLECharacteristic* otaCtrl = pService->createCharacteristic(
      OTA_CTRL_UUID, NIMBLE_PROPERTY::WRITE);
  otaCtrl->setCallbacks(new OtaCtrlCallback());
  NimBLECharacteristic* otaData = pService->createCharacteristic(
      OTA_DATA_UUID, NIMBLE_PROPERTY::WRITE);
  otaData->setCallbacks(new OtaDataCallback());
  otaStatusChar = pService->createCharacteristic(OTA_STATUS_UUID, NIMBLE_PROPERTY::NOTIFY);

  pService->start();

  NimBLEAdvertising* pAdv = NimBLEDevice::getAdvertising();
  pAdv->addServiceUUID(SVC_UUID);
  pAdv->setScanResponse(true);
  pAdv->setMinPreferred(0x06);
  NimBLEDevice::startAdvertising();

  playEarcon(SND_BOOT);

  lastEncoderPos = M5Dial.Encoder.read();
  needsRedraw    = true;
}

// ─────────────────────────────────────────────────────────────────────────────
// loop()
// ─────────────────────────────────────────────────────────────────────────────
void loop() {
  M5Dial.update();

  // Deferred "needs you" earcon (set from handleRxMessage, played off the BLE task)
  if (buzzPending) {
    buzzPending = false;
    playEarcon(SND_NEEDS_YOU);
  }

  // Drain inbound BLE messages — this is the only place state mutates
  RxMsg msg;
  while (rxQueue && xQueueReceive(rxQueue, &msg, 0) == pdTRUE) {
    handleRxMessage(msg.data, msg.len);
  }

  // Finalize an OTA off the BLE task: verify the image, switch the boot slot,
  // and reboot into the new firmware. A failed verify leaves the running slot
  // untouched (Update never committed it).
  if (otaFinish) {
    otaFinish = false;
    if (Update.end(true)) {
      sendOtaStatus("done", "", 100);
      playEarcon(SND_OTA_DONE);
      delay(400);
      ESP.restart();
    } else {
      otaActive = false;
      sendOtaStatus("error", "verify failed", 0);
      playEarcon(SND_ERROR);
      appState = otaPrevState; needsRedraw = true;
    }
  }

  // If the host link drops, the session view is no longer trustworthy: the
  // daemon can't tell us a session went idle while we're disconnected, so a
  // frozen "working" roster (spinner still turning) would lie. Drop it and fall
  // back to the clock; a reconnect resyncs the full state within a sweep tick.
  static bool wasConnected = false;
  if (wasConnected && !bleConnected) {
    if (otaActive) { Update.abort(); otaActive = false; }  // link died mid-update: discard
    memset(sessions, 0, sizeof(sessions));
    sessionCount   = 0;
    permQueueCount = 0;
    currentPermSid[0] = 0;
    if (appState != MODE_MENU && appState != BRIGHTNESS && appState != SOUND && appState != CLOCK)
      appState = homeView();   // sessions cleared → clock, unless a settings screen is up
    needsRedraw = true;
  }
  wasConnected = bleConnected;

  // "All done" chime: when the last busy session goes quiet, a gentle cue tells
  // you it's your turn without looking. Only on the busy→idle edge, and only
  // while connected (a disconnect clears sessions but musn't ring — busyPrev
  // resets to false so reconnecting doesn't ring either).
  static bool busyPrev = false;
  bool anyBusy = false;
  for (int i = 0; i < MAX_SESSIONS; i++) {
    if (!sessions[i].active) continue;
    const char* st = sessions[i].state;
    if (strcmp(st, "working") == 0 || strcmp(st, "blocked") == 0 ||
        strcmp(st, "permission_request") == 0) { anyBusy = true; break; }
  }
  if (bleConnected && busyPrev && !anyBusy) playEarcon(SND_DONE);
  busyPrev = anyBusy;

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
    if (appState != MODE_MENU && appState != BRIGHTNESS && appState != SOUND) {
      appState    = MODE_MENU;
      menuChoice  = 0;
      needsRedraw = true;
    }
  }

  // Touch: a tap (finger lifted) selects and fires the choice under it.
  auto tp = M5Dial.Touch.getDetail();
  if (tp.wasClicked()) handleTouch(tp.x, tp.y);

  // Grace window elapsed with no undo -> the decision is finally sent.
  if (appState == CONFIRMING && (millis() - confirmStart) > CONFIRM_MS) {
    commitDecision();
  }

  // OTA install requested but the host never started the stream -> back to choice.
  if (appState == OTA_PROMPT && otaStarting && (millis() - otaPromptStartedAt) > 20000) {
    otaStarting = false; needsRedraw = true;
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
  if (appState == CONFIRMING && millis() - lastFast > 100) {   // animate the undo arc
    lastFast = millis(); needsRedraw = true;
  }
  if (appState == SESSION_LIST && millis() - lastFast > 150) {
    lastFast = millis();
    bool anyWorking = false, anyWaiting = false;
    for (int i = 0; i < MAX_SESSIONS; i++) {
      if (!sessions[i].active) continue;
      const char* st = sessions[i].state;
      if (strcmp(st, "working") == 0) anyWorking = true;
      else if (strcmp(st, "blocked") == 0 || strcmp(st, "permission_request") == 0) anyWaiting = true;
    }
    if (anyWorking) spinFrame = (spinFrame + 1) & 3;      // advance the spinner
    if (anyWorking || anyWaiting) needsRedraw = true;     // spinner and/or the needs-you pulse
  }

  if (needsRedraw) redraw();
  delay(10);
}
