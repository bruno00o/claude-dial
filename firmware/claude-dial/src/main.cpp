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

// ── Premium anti-aliased type: JetBrains Mono embedded as VLW fonts ──────────
// Three sizes replace the aliased built-in FreeMono, matching the web simulator.
// Loaded ONCE in setup() into persistent VLWfont instances; each PointerWrapper
// must outlive its font (VLWfont streams glyph bitmaps lazily from the flash
// array on every drawChar). If a load ever fails, useS/M/L fall back to FreeMono
// so a font problem can never blank the screen.
#include "jbmono_s.h"   // 16 px
#include "jbmono_m.h"   // 24 px
#include "jbmono_l.h"   // 36 px
static lgfx::VLWfont      fontS, fontM, fontL;     // 16 / 24 / 36 px
static lgfx::PointerWrapper wrapS, wrapM, wrapL;
static bool g_fontOK = false;
static char g_fontMsg[48] = "fonts not init";
static void initFonts() {
  wrapS.set(jbmono_s, jbmono_s_len);
  wrapM.set(jbmono_m, jbmono_m_len);
  wrapL.set(jbmono_l, jbmono_l_len);
  bool s = fontS.loadFont(&wrapS);
  bool m = fontM.loadFont(&wrapM);
  bool l = fontL.loadFont(&wrapL);
  g_fontOK = s && m && l;
  snprintf(g_fontMsg, sizeof(g_fontMsg), "VLW load S=%d M=%d L=%d", s, m, l);
}
// Font selectors (defined after `canvas` below): premium VLW when loaded,
// graceful FreeMono fallback otherwise, so a font problem never blanks the screen.
static inline void useS();
static inline void useM();
static inline void useL();

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
// black = background. Colours are RGB888 (0xRRGGBB) but MUST be typed uint32_t:
// LovyanGFX picks the colour format from the ARGUMENT TYPE — uint32_t → RGB888,
// but a bare hex literal is int32_t, which it reads as RGB565 (colortype.hpp
// convert_to_rgb888). A plain `0xFFB000` therefore came out violet, not amber.
// The (uint32_t) cast forces the 888 path. Verified on-device (colour bug hunt).
#define COL_BG          ((uint32_t)0x0A0805)   // warm near-black
#define COL_INK         ((uint32_t)0xE9E2D6)   // selected row / command text
#define COL_DIM         ((uint32_t)0x8A8175)   // headers, footers, hints
#define COL_GRAY        ((uint32_t)0x6F695F)   // idle rows
#define COL_AMBER       ((uint32_t)0xFFB000)   // working / primary accent (A+C)
#define COL_HOT         ((uint32_t)0xFF7A18)   // needs-you / urgent — the 2nd temperature
#define COL_AMBER_HOT   ((uint32_t)0xFFC46B)   // soft warning / usage mid-range
#define COL_RED         ((uint32_t)0xFF5B34)   // reject
#define COL_RING        ((uint32_t)0x2A2318)   // dim bezel ring
#define COL_ARC_OFF     ((uint32_t)0x140F08)   // spent countdown-arc dots
#define COL_CONFIRM_BG  COL_BG

// ── Types ────────────────────────────────────────────────────────────────────
enum AppState { IDLE, SESSION_LIST, DETAIL, PERMISSION, CONFIRMING, MODE_MENU, BRIGHTNESS, CLOCK, OTA, OTA_PROMPT, FIRMWARE_INFO, SOUND, CONNECTION, RESET_CONFIRM };

struct Session {
  char  session_id[40];
  char  project[40];    // human-readable name (basename of cwd), for the roster
  char  state[24];      // working | idle | blocked | permission_request
  char  tool_name[40];
  char  command[200];
  long  total_tokens;   // cumulative "work" tokens for this conversation (0 = unknown)
  long  context_tokens; // tokens resident in the context window now (0 = unknown)
  int   context_pct;    // context as a % of the model max (for the rim, 0 = unknown)
  int   sub_agents;     // Task sub-agents this conversation has spawned
  float cost_usd;       // cumulative USD cost for this conversation
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
#  define CLAUDE_DIAL_FW_VERSION "1.1.0"  // x-release-please-version
#endif
static const char* FW_VERSION = CLAUDE_DIAL_FW_VERSION;

static bool permInQueue(const char* sid);
static void permEnqueue(const char* sid);
static void permRemoveFromQueue(const char* sid);
static void permShowNext();
static bool isDangerous(const char* cmd);

static void handleRxMessage(const char* data, uint16_t len);

static void drawBase();
static void getTimeStr(char* tBuf, char* dBuf);
static void drawIdle();
static void drawSessionList();
static void drawDetail();
static void drawPermission();
static void drawConfirming();
static void drawModeMenu();
static void drawBrightness();
static void drawSound();
static void drawConnection();
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
static int   rosterSel = 0;            // selected roster row (index into the sorted list)
static char  rosterSelSid[40] = {0};  // session id under the caret (set each roster draw)
static char  detailSid[40] = {0};     // session shown in the DETAIL drill-in

// Name of the bridge machine we're connected to (from set_time), shown on idle
// so you can see which computer the Dial is driving. Cleared on disconnect.
static char  hostName[24] = "";
// This Dial's own BLE address, set once at boot — a stable id shown on the
// connection screen to tell multiple Dials apart.
static char  dialId[20] = "";
// How full the 5h usage window is (0..100), pushed by the bridge — fills the
// rim gauge. Cleared on disconnect.
static int   usagePct = 0;

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
  SND_TICK_CW, SND_TICK_CCW,   // per-detent encoder ticks, pitched by direction
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
    case SND_TICK_CW:   spk.tone(4500, 10);                                    break;  // detent, one way…
    case SND_TICK_CCW:  spk.tone(3500, 10);                                    break;  // …lower pitch the other
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
// True once the bridge has connected at least once since boot. Lets the idle
// screen tell a first-run "never paired" Dial apart from a temporary drop, so it
// can show one-time setup guidance out of the box and nothing after.
static bool                  hasEverConnected = false;
static QueueHandle_t         rxQueue = nullptr;

// ── Display sprite ───────────────────────────────────────────────────────────
static M5Canvas canvas(&M5Dial.Display);

// Font selectors — premium VLW when loaded, graceful FreeMono fallback otherwise.
static inline void useS() { if (g_fontOK) canvas.setFont(&fontS); else canvas.setFont(&fonts::FreeMono9pt7b);  }
static inline void useM() { if (g_fontOK) canvas.setFont(&fontM); else canvas.setFont(&fonts::FreeMono12pt7b); }
static inline void useL() { if (g_fontOK) canvas.setFont(&fontL); else canvas.setFont(&fonts::FreeMono18pt7b); }
static const int CX = 120, CY = 120, CR = 120;

// A quick colour circle grows from the center to full screen — a physical "look
// here" wipe when a permission takes over. ~8 frames, no easing library.
static void wipeIn(uint32_t color) {
  for (int r = 8; r <= CR; r += 15) {
    canvas.fillScreen(COL_BG);
    canvas.fillCircle(CX, CY, r, color);
    canvas.pushSprite(0, 0);
    delay(9);
  }
}

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
  AppState prev = appState;   // to wipe only on a real takeover, not between queued prompts
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
      // Wipe in only when the prompt actually takes over the screen (from the
      // roster/clock), not when flipping between queued prompts. Red if risky.
      if (prev != PERMISSION && prev != CONFIRMING)
        wipeIn(isDangerous(sessions[idx].command) ? COL_RED : COL_AMBER);
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
  void onConnect(NimBLEServer*) override    { bleConnected = true;  hasEverConnected = true; needsRedraw = true; }
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
    strlcpy(hostName, doc["host"] | "", sizeof(hostName));
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

  // Control: the 5h usage gauge (0..100), for the rim ring.
  if (strcmp(type, "usage") == 0) {
    usagePct = doc["pct"] | 0;
    if (usagePct < 0) usagePct = 0;
    if (usagePct > 100) usagePct = 100;
    needsRedraw = true;
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
  sessions[idx].total_tokens   = doc["total_tokens"]   | 0L;   // omitted on the wire → 0 (unknown)
  sessions[idx].context_tokens = doc["context_tokens"] | 0L;
  sessions[idx].context_pct    = doc["context_pct"]    | 0;
  sessions[idx].sub_agents     = doc["sub_agents"]     | 0;
  sessions[idx].cost_usd       = doc["cost_usd"]       | 0.0f;

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

// Compact token count for the tiny screen: 216k, 1.2M. Mirrors the web's ftok().
static void fmtTokens(long v, char* out, size_t n) {
  if (v >= 1000000)   snprintf(out, n, "%.1fM", v / 1000000.0);
  else if (v >= 1000) snprintf(out, n, "%ldk", (v + 500) / 1000);
  else                snprintf(out, n, "%ld", v);
}

static void drawBase() {
  canvas.fillScreen(COL_BG);
  canvas.fillCircle(CX, CY, CR - 1, COL_BG);
  canvas.fillArc(CX, CY, CR - 3, CR - 1, 0, 360, COL_RING);   // smooth AA bezel
}

// ── Round-screen text fitting ──────────────────────────────────────────────
// The glass is a disc: a line far from the vertical centre has much less usable
// width than 240px. Truncate dynamic strings (host, project, labels) to the
// chord available at their height so nothing runs off the curved edge.
static const int RSAFE = 116;   // safe radius, a hair inside the bezel ring

// Truncate s into out to fit the chord at height y for the CURRENT font's cell.
// datum: 'C' = centred on CX, 'L' = left edge at x.
static void fitChord(const char* s, char* out, size_t outsz, int x, int y, char datum) {
  int cw = canvas.textWidth("0"); if (cw < 1) cw = 1;
  float dy = (float)(y - CY);
  float hh = (float)RSAFE * RSAFE - dy * dy;
  int h = hh > 0 ? (int)sqrtf(hh) : 0;
  int maxW = (datum == 'C') ? (2 * h - 4) : (CX + h - x);
  int maxN = maxW / cw; if (maxN < 1) maxN = 1;
  size_t lim = (size_t)maxN + 1;
  strlcpy(out, s, lim < outsz ? lim : outsz);
}

// A Mac's hostname is often too long for the disc — drop a trailing .local/.lan
// (the full name still shows on the wider connection screen).
static void hostShort(const char* h, char* out, size_t outsz) {
  strlcpy(out, h, outsz);
  char* dot = strstr(out, ".local"); if (!dot) dot = strstr(out, ".lan");
  if (dot) *dot = '\0';
}

// The recurring "dial" motif: a ring of small dots around the rim. The first
// `frac` (0..1) of them, clockwise from 12 o'clock, are drawn in `on`, the rest
// in `off`. Shared by the idle bezel, the clock, and the countdown arcs — and
// the vehicle for the future usage gauge (light the ring by quota consumed).
static void drawDotRing(float frac, uint32_t on, uint32_t off, int count, int radius) {
  if (frac < 0) frac = 0;
  if (frac > 1) frac = 1;
  int lit = (int)(frac * count + 0.5f);
  for (int i = 0; i < count; i++) {
    float a = (i / (float)count) * 360.0f - 90.0f;
    int ax = CX + (int)(radius * cosf(a * DEG_TO_RAD));
    int ay = CY + (int)(radius * sinf(a * DEG_TO_RAD));
    canvas.fillCircle(ax, ay, 1, (i < lit) ? on : off);
  }
}

// Bold usage ring: a thick solid band from 12 o'clock clockwise — `frac` in `on`,
// the rest in `off`. Same proven angle math as drawDotRing (0deg = top, clockwise)
// but drawn as dense radial ticks so it reads as one solid arc, like the web rim.
static void drawUsageArcBold(float frac, uint32_t on, uint32_t off) {
  if (frac < 0) frac = 0;
  if (frac > 1) frac = 1;
  const int r0 = CR - 8, r1 = CR - 4;               // slim 4px band near the edge
  // Anti-aliased ring via fillArc. Convention (fill_arc_helper, LGFXBase.cpp):
  // 0deg = 3 o'clock, -90 = 12 o'clock, angle increases CLOCKWISE. So fill from
  // the top clockwise. One AA call replaces 360 aliased radial ticks — smoother
  // AND cheaper. (If a flash ever shows it running CCW, swap to 270-360*frac..270.)
  canvas.fillArc(CX, CY, r0, r1, -90.0f, 270.0f, off);              // full dim track
  if (frac > 0.0001f)
    canvas.fillArc(CX, CY, r0, r1, -90.0f, -90.0f + frac * 360.0f, on);
}

// Context-gauge colour by fill: amber → warm → red as a conversation nears its
// context limit (same temperature language as the rest of the UI).
static uint32_t ctxColor(int pct) {
  if (pct >= 85) return COL_RED;
  if (pct >= 70) return COL_AMBER_HOT;
  return COL_AMBER;
}

// Night mode: auto-dim the backlight during night hours (22:00–07:00) so the
// object isn't a lamp on the desk overnight. It only scales down whatever
// brightness the user set — never brighter — and only once the bridge has set
// the clock (otherwise the RTC hour can't be trusted).
static const int NIGHT_START = 22, NIGHT_END = 7;
static bool isNightHour() {
  if (!hasEverConnected) return false;
  int h = M5Dial.Rtc.getDateTime().time.hours;
  return h >= NIGHT_START || h < NIGHT_END;   // window wraps midnight
}
static void applyBrightness() {
  int b = brightness;
  if (isNightHour()) b = b * 35 / 100;        // dim to ~35% at night
  if (b < 8) b = 8;
  M5Dial.Display.setBrightness((uint8_t)b);
}

// Gauge colour by how close the 5h window is to the cap: amber → hot → red.
static uint32_t usageColor() {
  if (usagePct >= 85) return COL_RED;
  if (usagePct >= 70) return COL_AMBER_HOT;
  return COL_AMBER;
}

static void getTimeStr(char* tBuf, char* dBuf) {
  auto dt = M5Dial.Rtc.getDateTime();
  snprintf(tBuf, 12, "%02d:%02d", dt.time.hours, dt.time.minutes);
  snprintf(dBuf, 20, "%04d-%02d-%02d", dt.date.year, dt.date.month, dt.date.date);
}

static void drawIdle() {
  drawBase();
  // Rim gauge: fills with the 5h usage window (all-dim ambient bezel at 0%).
  drawUsageArcBold(usagePct / 100.0f, usageColor(), COL_ARC_OFF);   // smooth AA ring
  if (usagePct > 0) {   // label the rim so it isn't mistaken for a context gauge
    useS();
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(COL_DIM, COL_BG);
    char rimLbl[12]; snprintf(rimLbl, sizeof(rimLbl), "5h %d%%", usagePct);
    canvas.drawString(rimLbl, CX, 30);
  }

  // Fresh out of the box: never paired since boot. Show how to set up the Mac
  // bridge instead of a clock stuck on the wrong time. Disappears for good once
  // the bridge connects the first time (see hasEverConnected).
  if (!bleConnected && !hasEverConnected) {
    canvas.setTextDatum(middle_center);
    useM();
    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString("claude-dial", CX, CY - 58);

    useS();
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("not paired yet", CX, CY - 30);
    canvas.drawString("set up on your Mac", CX, CY - 6);

    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString("github.com/bruno00o/", CX, CY + 22);
    canvas.drawString("claude-dial", CX, CY + 42);
    canvas.pushSprite(0, 0);
    return;
  }

  char tBuf[12], dBuf[20];
  getTimeStr(tBuf, dBuf);

  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_AMBER, COL_BG);
  useL();                            // premium VLW clock (de-risk: this screen first)
  canvas.drawString(tBuf, CX, CY - 14);

  int act = activeSessions();
  char status[40];
  if (!bleConnected)      strlcpy(status, "waiting for claude", sizeof(status));
  else if (act > 0)       snprintf(status, sizeof(status), "%d session%s", act, act > 1 ? "s" : "");
  else                    strlcpy(status, "waiting for claude", sizeof(status));

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(status, CX, CY + 26);

  canvas.fillCircle(CX, CY + 50, 3, bleConnected ? COL_AMBER : COL_RING);
  if (bleConnected && hostName[0]) {           // which machine we're driving
    useS();
    canvas.setTextColor(COL_DIM, COL_BG);
    char hs[40], hf[40];
    hostShort(hostName, hs, sizeof(hs));
    fitChord(hs, hf, sizeof(hf), CX, CY + 70, 'C');
    canvas.drawString(hf, CX, CY + 70);
  }
  canvas.pushSprite(0, 0);
}

// Scale an RGB565 colour toward black by num/den — for the round-screen edge
// fade (M5's trick: rows dim with distance from center, reading as a CRT
// vignette that happens to suit the terminal look).
// Scale an RGB888 colour toward black by num/den (per-channel).
static uint32_t dimColor(uint32_t c, int num, int den) {
  int r = (c >> 16) & 0xFF, g = (c >> 8) & 0xFF, b = c & 0xFF;
  r = r * num / den; g = g * num / den; b = b * num / den;
  return (uint32_t)((r << 16) | (g << 8) | b);
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

  // Clamp the selection and remember which session is under the caret, so a press
  // can open its detail view (handlePress needs the sorted selection).
  if (rosterSel >= n) rosterSel = n - 1;
  if (rosterSel < 0)  rosterSel = 0;
  if (n > 0) strlcpy(rosterSelSid, sessions[active[rosterSel]].session_id, sizeof(rosterSelSid));

  // Rim gauge: the SELECTED conversation's context fill (out of the model max) —
  // a meaningful per-session gauge that follows the caret, not a global quota.
  int selCtx = (n > 0) ? sessions[active[rosterSel]].context_pct : 0;
  drawUsageArcBold(selCtx / 100.0f, ctxColor(selCtx), COL_ARC_OFF);
  if (n > 0 && selCtx > 0) {
    useS();
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(COL_DIM, COL_BG);
    char rimLbl[16]; snprintf(rimLbl, sizeof(rimLbl), "ctx %d%%", selCtx);
    canvas.drawString(rimLbl, CX, 30);
  }

  // header — masthead like the web roster: count (amber) + "session(s)" (dim) on
  // the left, the clock (dim) on the right, with a hairline rule beneath. "claude ›"
  // is dropped: the round chord can't hold branding + clock on one line.
  useS();
  const int hy = 50, hcw = canvas.textWidth("0");
  char cnt[8]; snprintf(cnt, sizeof(cnt), "%d", n);
  canvas.setTextDatum(middle_left);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(cnt, CX - 88, hy);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(n == 1 ? " session" : " sessions", CX - 88 + (int)strlen(cnt) * hcw, hy);
  char tb[8]; { auto dt = M5Dial.Rtc.getDateTime(); snprintf(tb, sizeof(tb), "%02d:%02d", dt.time.hours, dt.time.minutes); }
  canvas.setTextDatum(middle_right);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(tb, CX + 88, hy);
  canvas.drawFastHLine(CX - 84, hy + 14, 168, COL_RING);

  if (n == 0) {                 // no agents — the daemon switches us back to the clock
    canvas.pushSprite(0, 0);
    return;
  }

  const int visible = 4;
  // Keep the selected row on screen (scroll follows the caret).
  if (rosterSel < listScrollOffset) listScrollOffset = rosterSel;
  if (rosterSel >= listScrollOffset + visible) listScrollOffset = rosterSel - visible + 1;
  if (listScrollOffset > n - visible) listScrollOffset = n - visible;
  if (listScrollOffset < 0) listScrollOffset = 0;

  const int startY = 74, rowH = 32;
  useS();   // rows in body size (labels + spinner)
  for (int row = 0; row < visible && (listScrollOffset + row) < n; row++) {
    Session& s = sessions[active[listScrollOffset + row]];
    int y = startY + row * rowH;

    bool working = strcmp(s.state, "working") == 0;
    bool waiting = strcmp(s.state, "blocked") == 0 ||
                   strcmp(s.state, "permission_request") == 0;
    uint32_t col;
    if (working) {
      col = COL_AMBER;                    // busy: the amber (temperature 1)
    } else if (waiting) {
      // needs-you: hot orange (temperature 2) that breathes, so the eye lands on
      // it and it never reads like the amber "working" state.
      float ph = (millis() % 1100) / 1100.0f;
      float k  = ph < 0.5f ? ph * 2.0f : (1.0f - ph) * 2.0f;   // triangle 0..1..0
      col = dimColor(COL_HOT, 150 + (int)(k * 105.0f), 255);     // pulse 150..255
    } else {
      col = COL_GRAY;                     // idle
    }

    bool sel = (listScrollOffset + row) == rosterSel;
    // Round-screen vignette: fade rows toward the rim by distance from center —
    // but never the needs-you rows, and never the selected row (both stay bright).
    if (!waiting && !sel) {
      int f = 255 - abs(y - CY) * 2;
      if (f < 150) f = 150;
      col = dimColor(col, f, 255);
    }

    // per-conversation total tokens, right-aligned + dim (mirrors the web roster)
    char tok[12] = "";
    if (s.total_tokens > 0) fmtTokens(s.total_tokens, tok, sizeof(tok));
    int cw = canvas.textWidth("0"); if (cw < 1) cw = 1;
    const int tokRight = CX + 86;                    // right anchor, inside the chord on every row
    int labelRight = tok[0] ? tokRight - (int)strlen(tok) * cw - 8 : CX + 90;

    // project label, clipped to stop before the token column (round-screen room)
    char label[24], lf[24];
    sessionLabel(s, label, sizeof(label));
    int maxN = (labelRight - (CX - 70)) / cw; if (maxN < 1) maxN = 1;
    if ((size_t)maxN + 1 > sizeof(lf)) maxN = (int)sizeof(lf) - 1;
    strlcpy(lf, label, (size_t)maxN + 1);

    canvas.setTextColor(col, COL_BG);
    canvas.setTextDatum(middle_left);
    if (waiting) {                                   // needs-you: a hot filled triangle
      canvas.fillTriangle(CX - 90, y - 5, CX - 90, y + 5, CX - 82, y, col);
    } else if (working) {                            // working: a smooth spinning C-arc
      int base = spinFrame * 90;                       // advances each 150ms tick
      canvas.fillArc(CX - 88, y, 3, 6, base, base + 270, col);
    } else {                                          // idle: a soft AA dot
      canvas.fillSmoothCircle(CX - 88, y, 2, col);
    }
    canvas.drawString(lf, CX - 70, y);
    if (tok[0]) {                                    // dim per-conversation token count
      canvas.setTextDatum(middle_right);
      canvas.setTextColor(COL_DIM, COL_BG);
      canvas.drawString(tok, tokRight, y);
    }
    if (sel)                                         // selection caret (press to drill in)
      canvas.fillTriangle(CX - 103, y - 4, CX - 103, y + 4, CX - 97, y, COL_AMBER);
  }

  // footer — hint to drill in, or the count of sessions needing you
  useS();
  canvas.setTextDatum(middle_center);
  if (waits > 0) {
    char ft[20]; snprintf(ft, sizeof(ft), "%d waiting", waits);
    canvas.setTextColor(COL_HOT, COL_BG);
    canvas.drawString(ft, CX, 208);
  } else {
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("press: details", CX, 208);
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

// One "label ....... value" row in the detail view: label dim-left, value right.
static void detailStat(int y, const char* label, const char* value, uint32_t vcol) {
  useS();
  canvas.setTextDatum(middle_left);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString(label, CX - 82, y);
  canvas.setTextDatum(middle_right);
  canvas.setTextColor(vcol, COL_BG);
  canvas.drawString(value, CX + 82, y);
}

// DETAIL: the per-session drill-in reached by pressing a roster row. Shows what
// that one conversation is doing — its context fill (also on the rim), spend, and
// the sub-agents it has spawned — the "select a session, see its agents" view.
static void drawDetail() {
  int idx = findSession(detailSid);
  if (idx < 0 || !sessions[idx].active) {   // the session ended while we were in here
    appState = homeView();
    needsRedraw = true;
    return;
  }
  Session& s = sessions[idx];

  drawBase();
  drawUsageArcBold(s.context_pct / 100.0f, ctxColor(s.context_pct), COL_ARC_OFF);  // this session's context

  // title — project name (medium, amber, clipped to the chord)
  useM();
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_AMBER, COL_BG);
  char title[24], tf[24];
  sessionLabel(s, title, sizeof(title));
  fitChord(title, tf, sizeof(tf), CX, 46, 'C');
  canvas.drawString(tf, CX, 46);

  // state (+ tool in flight) — coloured by state
  bool working = strcmp(s.state, "working") == 0;
  bool waiting = strcmp(s.state, "blocked") == 0 || strcmp(s.state, "permission_request") == 0;
  uint32_t stCol = working ? COL_AMBER : (waiting ? COL_HOT : COL_GRAY);
  char st[48], stf[32];
  if (s.tool_name[0]) {
    char tl[24]; strlcpy(tl, s.tool_name, sizeof(tl));
    for (char* p = tl; *p; p++) *p = tolower(*p);
    snprintf(st, sizeof(st), "%s - %s", s.state, tl);
  } else {
    strlcpy(st, s.state, sizeof(st));
  }
  useS();
  fitChord(st, stf, sizeof(stf), CX, 72, 'C');
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(stCol, COL_BG);
  canvas.drawString(stf, CX, 72);

  canvas.drawFastHLine(CX - 74, 88, 148, COL_RING);

  // stats: context / total tokens / dollar cost / sub-agents
  char cbuf[16], tbuf[16], dbuf[16], abuf[12];
  if (s.context_tokens > 0) fmtTokens(s.context_tokens, cbuf, sizeof(cbuf)); else strlcpy(cbuf, "-", sizeof(cbuf));
  if (s.total_tokens   > 0) fmtTokens(s.total_tokens,   tbuf, sizeof(tbuf)); else strlcpy(tbuf, "-", sizeof(tbuf));
  if (s.cost_usd > 0)       snprintf(dbuf, sizeof(dbuf), "$%.2f", s.cost_usd); else strlcpy(dbuf, "-", sizeof(dbuf));
  snprintf(abuf, sizeof(abuf), "%d", s.sub_agents);
  detailStat(106, "context", cbuf, COL_INK);
  detailStat(128, "total",   tbuf, COL_INK);
  detailStat(150, "cost",    dbuf, COL_AMBER_HOT);
  detailStat(172, "agents",  abuf, s.sub_agents > 0 ? COL_AMBER : COL_GRAY);

  useS();
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("press: back", CX, 200);
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
  useS();

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
    useS();
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(danger ? COL_RED : COL_DIM, COL_BG);
    canvas.drawString(danger ? "! command" : "command", CX, 32);

    const int visible = 6, top = 58, lh = 20;
    if (permCmdScroll > total - visible) permCmdScroll = total - visible;
    if (permCmdScroll < 0) permCmdScroll = 0;
    useS();
    canvas.setTextDatum(middle_left);
    canvas.setTextColor(cmdCol, COL_BG);
    for (int r = 0; r < visible && (permCmdScroll + r) < total; r++)
      canvas.drawString(lines[permCmdScroll + r], 22, top + r * lh);

    useS();
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(COL_DIM, COL_BG);
    if (total > visible) {
      char sc[16]; snprintf(sc, sizeof(sc), "%d/%d", permCmdScroll + 1, total - visible + 1);
      canvas.drawString(sc, CX, 196);
    }
    canvas.drawString("press: back", CX, 214);
    canvas.pushSprite(0, 0);
    return;
  }

  // ── choices view ──
  // project (who) + queue badge. The who-label is hard-capped so wider VLW glyphs
  // can never collide with the right-side badge (ends by ~CX+16).
  useS();
  char who[16], wf[20]; sessionLabel(s, who, sizeof(who));
  int cwv = canvas.textWidth("0"); if (cwv < 1) cwv = 1;
  int whoCap = ((CX + 16) - (CX - 80)) / cwv; if (whoCap < 1) whoCap = 1;
  if ((size_t)whoCap + 1 > sizeof(wf)) whoCap = sizeof(wf) - 1;
  strlcpy(wf, who, (size_t)whoCap + 1);
  canvas.setTextDatum(middle_left);
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(wf, CX - 80, 44);
  canvas.setTextDatum(middle_right);
  canvas.setTextColor(COL_DIM, COL_BG);
  if (permQueueCount > 0) {
    char more[14]; snprintf(more, sizeof(more), "<%d more>", permQueueCount);
    canvas.drawString(more, CX + 82, 44);
  } else {
    canvas.drawString("last", CX + 82, 44);
  }

  // eyebrow — warns when risky (small)
  canvas.setTextDatum(middle_center);
  canvas.setTextColor(danger ? COL_RED : COL_AMBER_HOT, COL_BG);
  canvas.drawString(danger ? "! caution" : "permission", CX, 66);

  // up to 3 command lines (body); when it overflows show 2 + a "tap to read" hint
  useS();
  int shown = permCmdOverflow ? 2 : total;
  canvas.setTextColor(cmdCol, COL_BG);
  for (int r = 0; r < shown; r++) canvas.drawString(lines[r], CX, 90 + r * 20);
  if (permCmdOverflow) {
    useS();
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString(".. tap to read", CX, 130);
  }

  // per-conversation context fill — small + dim. Shown only when the command fits
  // in <=2 lines, so it never collides with a 3rd command line.
  if (s.context_tokens > 0 && total <= 2) {
    char cn[24], ctxLine[32];
    fmtTokens(s.context_tokens, cn, sizeof(cn));
    snprintf(ctxLine, sizeof(ctxLine), "ctx %s", cn);
    useS();
    canvas.setTextDatum(middle_center);
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString(ctxLine, CX, 134);
  }

  // choices — a filled AA pill on the selected one (red for reject), like the menu
  useS();
  canvas.setTextDatum(middle_center);
  int btnY[3] = { 156, 180, 204 };
  for (int i = 0; i < 3; i++) {
    bool sel = (i == permChoice);
    if (sel) {
      uint32_t pill = (i == 2) ? COL_RED : COL_AMBER;
      canvas.fillSmoothRoundRect(CX - 78, btnY[i] - 11, 156, 22, 11, pill);
      canvas.setTextColor(COL_BG, pill);      // text blends to the pill, not the bg
    } else {
      canvas.setTextColor(COL_GRAY, COL_BG);
    }
    canvas.drawString(choiceLabels[i], CX, btnY[i]);
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
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("sending", CX, 86);

  useM();
  canvas.setTextColor(accent, COL_BG);
  canvas.drawString(choiceLabels[confirmChoice], CX, 116);

  useS();
  canvas.setTextColor(COL_INK, COL_BG);
  canvas.drawString("tap to undo", CX, 152);
  canvas.pushSprite(0, 0);
}

static const char* menuLabels[] = { "monitor", "brightness", "sound", "clock", "connection", "firmware", "reset" };
static const int MENU_N   = 7;
static const int MENU_GAP = 26;   // row pitch; tightened so 7 entries clear the title

// Draw a monospace string centred on cx with `extra` px between glyphs — the
// wide-tracking "readout" look for the clock and uppercase labels. Font + colour
// are set by the caller.
static void drawTracked(const char* s, int cx, int y, int extra) {
  int n = strlen(s);
  if (n == 0) return;
  int cw = canvas.textWidth("0");            // monospace: every glyph is this wide
  int total = n * cw + (n - 1) * extra;
  int x = cx - total / 2;
  canvas.setTextDatum(middle_left);
  for (int i = 0; i < n; i++) {
    char ch[2] = { s[i], 0 };
    canvas.drawString(ch, x, y);
    x += cw + extra;
  }
}

// Tiny settings-menu glyphs, all drawn from primitives (line/circle/arc/triangle)
// so they cost nothing on-device. Centred on (x, y), ~11px, in colour `col`.
static void drawMenuIcon(int k, int x, int y, uint32_t col) {
  switch (k) {
    case 0:  // monitor — a stacked list
      for (int i = -1; i <= 1; i++) canvas.drawLine(x - 5, y + i * 4, x + 5, y + i * 4, col);
      break;
    case 1:  // brightness — a sun
      canvas.drawCircle(x, y, 3, col);
      for (int a = 0; a < 360; a += 90) {
        float r = a * DEG_TO_RAD;
        canvas.drawLine(x + 5 * cosf(r), y + 5 * sinf(r), x + 7 * cosf(r), y + 7 * sinf(r), col);
      }
      break;
    case 2:  // sound — a speaker + wave
      canvas.fillTriangle(x - 1, y - 5, x - 1, y + 5, x + 4, y, col);
      canvas.fillRect(x - 5, y - 2, 4, 5, col);
      canvas.drawArc(x + 4, y, 4, 5, -45, 45, col);
      break;
    case 3:  // clock — a face with hands
      canvas.drawCircle(x, y, 5, col);
      canvas.drawLine(x, y, x, y - 3, col);
      canvas.drawLine(x, y, x + 2, y + 1, col);
      break;
    case 4:  // connection — a signal dot with arcs
      canvas.fillCircle(x, y, 2, col);
      canvas.drawArc(x, y, 5, 6, 40, 140, col);
      canvas.drawArc(x, y, 5, 6, 220, 320, col);
      break;
    case 5:  // firmware — an up chevron on a stem
      canvas.drawLine(x - 4, y + 2, x, y - 4, col);
      canvas.drawLine(x, y - 4, x + 4, y + 2, col);
      canvas.drawLine(x, y - 4, x, y + 5, col);
      break;
    case 6:  // reset — a refresh arc with an arrowhead
      canvas.drawArc(x, y, 4, 5, 300, 200, col);
      canvas.drawLine(x + 4, y - 3, x + 7, y - 2, col);
      canvas.drawLine(x + 5, y + 1, x + 7, y - 2, col);
      break;
  }
}

static void drawModeMenu() {
  drawBase();
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  drawTracked("SETTINGS", CX, 22, 3);

  useS();
  for (int i = 0; i < MENU_N; i++) {
    bool sel = (i == menuChoice);
    int  y   = CY - (MENU_N - 1) * (MENU_GAP / 2) + i * MENU_GAP;   // vertically centered
    if (sel) canvas.fillSmoothRoundRect(CX - 78, y - 12, 156, 24, 11, COL_AMBER);  // AA selection pill
    uint32_t fg = sel ? COL_BG : COL_GRAY;
    drawMenuIcon(i, CX - 58, y, fg);
    canvas.setTextColor(fg, sel ? COL_AMBER : COL_BG);   // opaque text over the pill
    canvas.setTextDatum(middle_left);
    canvas.drawString(menuLabels[i], CX - 42, y);
  }
  canvas.pushSprite(0, 0);
}

// Factory reset: wipe persisted settings (the NVS "cdial" namespace) and reboot.
// It's the only in-field recovery once the Dial is unplugged, so it lives in the
// menu — but behind a two-option confirm defaulting to "cancel", so no single
// press can ever trigger it.
static int resetChoice = 0;   // 0 = cancel (default), 1 = reset

static void doFactoryReset() {
  prefs.clear();              // clears the whole namespace (bright, vol) -> defaults
  delay(60);                  // let the flash write settle before the reboot
  ESP.restart();
}

static void drawReset() {
  drawBase();
  canvas.setTextDatum(middle_center);

  useM();
  canvas.setTextColor(COL_RED, COL_BG);
  canvas.drawString("factory reset", CX, 64);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("wipes settings,", CX, 94);
  canvas.drawString("then restarts", CX, 112);

  useS();
  const char* opts[2] = { "cancel", "reset" };
  for (int i = 0; i < 2; i++) {
    bool sel = (i == resetChoice);
    int  y   = 148 + i * 26;
    char row[20];
    snprintf(row, sizeof(row), "%s%s", sel ? "> " : "  ", opts[i]);
    uint32_t col = sel ? (i == 1 ? COL_RED : COL_AMBER) : COL_GRAY;
    canvas.setTextColor(col, COL_BG);
    canvas.drawString(row, CX, y);
  }
  canvas.pushSprite(0, 0);
}

static void drawBrightness() {
  drawBase();
  canvas.setTextDatum(middle_center);
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("brightness", CX, 72);

  char pct[8]; snprintf(pct, sizeof(pct), "%d%%", (brightness * 100) / 255);
  useL();
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(pct, CX, CY - 6);

  int bw = 150, bh = 8, bx = CX - bw / 2, by = 150;
  canvas.drawRoundRect(bx, by, bw, bh, 4, COL_RING);
  canvas.fillRoundRect(bx + 1, by + 1, (bw - 2) * brightness / 255, bh - 2, 3, COL_AMBER);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("turn / press", CX, 190);
  canvas.pushSprite(0, 0);
}

static void drawSound() {
  drawBase();
  canvas.setTextDatum(middle_center);
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("sound", CX, 72);

  useL();
  if (soundVol == 0) {
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.drawString("muted", CX, CY - 6);
  } else {
    char pct[8]; snprintf(pct, sizeof(pct), "%d%%", (soundVol * 100) / 255);
    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString(pct, CX, CY - 6);
  }

  int bw = 150, bh = 8, bx = CX - bw / 2, by = 150;
  canvas.drawRoundRect(bx, by, bw, bh, 4, COL_RING);
  if (soundVol) canvas.fillRoundRect(bx + 1, by + 1, (bw - 2) * soundVol / 255, bh - 2, 3, COL_AMBER);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("turn / press", CX, 190);
  canvas.pushSprite(0, 0);
}

// Settings > connection: link status, which machine drives us, and this Dial's
// own BLE id (to tell several Dials apart).
static void drawConnection() {
  drawBase();
  canvas.setTextDatum(middle_center);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("connection", CX, 40);

  useM();
  canvas.setTextColor(bleConnected ? COL_AMBER : COL_GRAY, COL_BG);
  canvas.drawString(bleConnected ? "connected" : "scanning...", CX, 72);

  if (bleConnected && hostName[0]) {
    useS();
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("host", CX, 104);
    useS();
    canvas.setTextColor(COL_INK, COL_BG);
    char hs[40], hf[40];
    hostShort(hostName, hs, sizeof(hs));
    fitChord(hs, hf, sizeof(hf), CX, 122, 'C');
    canvas.drawString(hf, CX, 122);
  }

  // this Dial's id (BLE address) — dim, at the bottom; short tail to fit the disc
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  char idbuf[16];
  if (dialId[0]) {
    size_t dl = strlen(dialId);
    snprintf(idbuf, sizeof(idbuf), "id %s", dl > 5 ? dialId + dl - 5 : dialId);
  } else {
    strlcpy(idbuf, "no id", sizeof(idbuf));
  }
  canvas.drawString(idbuf, CX, 196);

  canvas.pushSprite(0, 0);
}

// A dedicated clock face (date + a minute marker on the rim), distinct from the
// idle-monitor screen which shows "waiting for claude".
// Sakamoto's algorithm: day of week (0 = Sunday) from a Gregorian date. The RTC's
// own weekday field isn't reliably set by the bridge, so we derive it.
static int dayOfWeek(int y, int m, int d) {
  static const int t[] = { 0, 3, 2, 5, 0, 3, 5, 1, 4, 6, 2, 4 };
  if (m < 3) y -= 1;
  return (y + y / 4 - y / 100 + y / 400 + t[m - 1] + d) % 7;
}

static void drawClock() {
  drawBase();
  auto dt = M5Dial.Rtc.getDateTime();

  // Terminal readout: HH:MM:SS with live seconds and wide tracking.
  char tBuf[12];
  snprintf(tBuf, sizeof(tBuf), "%02d:%02d:%02d", dt.time.hours, dt.time.minutes, dt.time.seconds);
  useL();
  canvas.setTextColor(COL_AMBER, COL_BG);
  drawTracked(tBuf, CX, CY - 6, 1);

  // Uppercase date: "WED 02 JUL 2026".
  static const char* WD[7]  = { "SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT" };
  static const char* MO[12] = { "JAN", "FEB", "MAR", "APR", "MAY", "JUN",
                                "JUL", "AUG", "SEP", "OCT", "NOV", "DEC" };
  int mo = (dt.date.month >= 1 && dt.date.month <= 12) ? dt.date.month : 1;
  int wd = dayOfWeek(dt.date.year, mo, dt.date.date);
  char dBuf[20];
  snprintf(dBuf, sizeof(dBuf), "%s %02d %s %04d", WD[wd], dt.date.date, MO[mo - 1], dt.date.year);
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  drawTracked(dBuf, CX, CY + 28, 2);

  // A ring of dots filling clockwise with the minutes of the hour.
  drawDotRing(dt.time.minutes / 60.0f, COL_AMBER, COL_ARC_OFF, 60, CR - 8);
  canvas.pushSprite(0, 0);
}

static void redraw() {
  needsRedraw = false;
  switch (appState) {
    case IDLE:         drawIdle();         break;
    case SESSION_LIST: drawSessionList(); break;
    case DETAIL:       drawDetail();      break;
    case PERMISSION:   drawPermission();  break;
    case CONFIRMING:   drawConfirming();  break;
    case MODE_MENU:    drawModeMenu();    break;
    case BRIGHTNESS:   drawBrightness();  break;
    case SOUND:        drawSound();       break;
    case CONNECTION:   drawConnection();  break;
    case RESET_CONFIRM: drawReset();      break;
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
  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  char head[24];
  if (otaTargetVersion[0]) snprintf(head, sizeof(head), "installing %s", otaTargetVersion);
  else                     snprintf(head, sizeof(head), "updating firmware");
  canvas.drawString(head, CX, 90);

  char pctStr[8]; snprintf(pctStr, sizeof(pctStr), "%u%%", pct);
  useL();
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(pctStr, CX, 126);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("keep the dial close", CX, 162);

  canvas.pushSprite(0, 0);
}

// "Firmware X available — install now / later", chosen with the encoder.
static void drawOtaPrompt() {
  drawBase();
  canvas.setTextDatum(middle_center);

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("firmware update", CX, 56);

  useM();
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString(otaAvailVersion, CX, 86);

  if (otaStarting) {
    useS();
    canvas.setTextColor(COL_INK, COL_BG);
    canvas.drawString("starting..", CX, 140);
    canvas.pushSprite(0, 0);
    return;
  }

  const char* opts[2] = { "install now", "later" };
  for (int i = 0; i < 2; i++) {
    bool sel = (otaPromptChoice == i);
    useS();
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

  useS();
  canvas.setTextColor(COL_DIM, COL_BG);
  canvas.drawString("firmware", CX, 60);

  char v[24]; snprintf(v, sizeof(v), "v%s", FW_VERSION);
  useM();
  canvas.setTextColor(COL_INK, COL_BG);
  canvas.drawString(v, CX, 90);

  if (otaAvailVersion[0]) {
    char u[28]; snprintf(u, sizeof(u), "update %s", otaAvailVersion);
    useS();
    canvas.setTextColor(COL_AMBER, COL_BG);
    canvas.drawString(u, CX, 130);
    useS();
    canvas.setTextColor(COL_DIM, COL_BG);
    canvas.drawString("press to install", CX, 156);
  } else {
    useS();
    canvas.setTextColor(COL_GRAY, COL_BG);
    canvas.drawString("up to date", CX, 138);
  }
  canvas.pushSprite(0, 0);
}

// ─────────────────────────────────────────────────────────────────────────────
// Input
// ─────────────────────────────────────────────────────────────────────────────
static void handleEncoder(int delta) {
  // A short, direction-pitched detent tick (M5's trick: no haptic motor, so the
  // buzzer fakes one) makes blind rotation legible. Only where the encoder moves
  // something, and not on SOUND — which previews the real volume level instead.
  switch (appState) {
    case SESSION_LIST: case PERMISSION: case MODE_MENU:
    case OTA_PROMPT:   case BRIGHTNESS:  case RESET_CONFIRM:
      playEarcon(delta > 0 ? SND_TICK_CW : SND_TICK_CCW);
      break;
    default: break;
  }

  switch (appState) {
    case SESSION_LIST: {
      // Move the selection caret (scroll follows it in drawSessionList).
      int n = activeSessions();
      if (n > 0) {
        rosterSel += (delta > 0) ? 1 : -1;
        if (rosterSel < 0)  rosterSel = 0;
        if (rosterSel >= n) rosterSel = n - 1;
      }
      needsRedraw = true;
      break;
    }
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
    case RESET_CONFIRM:
      resetChoice ^= 1;                       // toggle cancel <-> reset
      needsRedraw = true;
      break;
    case BRIGHTNESS: {
      int b = (int)brightness + (delta > 0 ? 12 : -12);
      if (b < 20)  b = 20;
      if (b > 255) b = 255;
      brightness = (uint8_t)b;
      applyBrightness();   // honor night dim while adjusting
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
      // Nothing to confirm on the clock. (Long-press opens the mode menu.)
      break;

    case SESSION_LIST:
      // Drill into the selected conversation's detail view.
      if (rosterSelSid[0]) {
        strlcpy(detailSid, rosterSelSid, sizeof(detailSid));
        playEarcon(SND_TICK);
        appState    = DETAIL;
        needsRedraw = true;
      }
      break;

    case DETAIL:                             // back to the roster
      playEarcon(SND_TICK);
      appState    = homeView();
      needsRedraw = true;
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
        case 4: appState = CONNECTION;    break;
        case 5: appState = FIRMWARE_INFO; break;
        case 6: appState = RESET_CONFIRM; resetChoice = 0; break;  // default to cancel
      }
      needsRedraw = true;
      break;

    case RESET_CONFIRM:
      if (resetChoice == 1) {               // confirmed → wipe and reboot (no return)
        playEarcon(SND_REJECT);
        doFactoryReset();
      }
      appState = MODE_MENU; menuChoice = 6; // cancel → back to the menu
      needsRedraw = true;
      break;

    case CONNECTION:                       // informational — press returns to the menu
      appState = MODE_MENU; menuChoice = 4;
      needsRedraw = true;
      break;

    case FIRMWARE_INFO:
      if (otaAvailVersion[0]) {            // an update is offered → go to install/later
        otaPromptChoice = 0; otaStarting = false;
        appState = OTA_PROMPT;
      } else {
        appState = MODE_MENU; menuChoice = 5;
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
      if (y < 34) break;                               // title zone
      int base = CY - (MENU_N - 1) * (MENU_GAP / 2);   // first entry's y
      int c = (y - base + MENU_GAP / 2) / MENU_GAP;    // nearest row
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

    case RESET_CONFIRM:
      resetChoice = (y < 165) ? 0 : 1;                 // rows at 150 / 180
      needsRedraw = true;
      handlePress();
      break;

    case FIRMWARE_INFO:
    case CONNECTION:
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

  Serial.begin(115200);            // USB CDC — boot/font diagnostics

  auto cfg = M5.config();
  M5Dial.begin(cfg, true, false);  // encoder on, RFID off

  prefs.begin("cdial", false);
  brightness = prefs.getUChar("bright", 180);
  M5Dial.Display.setBrightness(0);   // stay dark until the boot fade-in
  soundVol = prefs.getUChar("vol", 128);
  M5Dial.Speaker.setVolume(soundVol);

  canvas.setColorDepth(16);          // RGB565 — set depth before allocating
  canvas.createSprite(240, 240);

  initFonts();                       // load the embedded JetBrains Mono VLW faces
  Serial.printf("[claude-dial] boot fw=%s  %s\n", FW_VERSION, g_fontMsg);

  // Soft boot fade-in (M5's touch): show a splash, then ramp the backlight up
  // instead of snapping on.
  drawBase();
  canvas.setTextDatum(middle_center);
  useM();
  canvas.setTextColor(COL_AMBER, COL_BG);
  canvas.drawString("claude-dial", CX, CY);
  canvas.pushSprite(0, 0);
  for (int b = 0; b <= brightness; b += 8) { M5Dial.Display.setBrightness(b); delay(12); }
  applyBrightness();   // settle to the night-dimmed level if it's night

  M5Dial.Rtc.begin();
  memset(sessions, 0, sizeof(sessions));

  rxQueue = xQueueCreate(8, sizeof(RxMsg));

  NimBLEDevice::init("Claude-Dial");
  strlcpy(dialId, NimBLEDevice::getAddress().toString().c_str(), sizeof(dialId));
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

  // Boot diagnostics: reprint font-load status each second for the first 15s so a
  // serial monitor attached shortly after flashing always catches it.
  static uint32_t lastBootPrint = 0;
  if (millis() < 15000 && millis() - lastBootPrint > 1000) {
    lastBootPrint = millis();
    Serial.printf("[boot %lus] %s | ble=%d\n", millis() / 1000, g_fontMsg, (int)bleConnected);
  }

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
    hostName[0]    = 0;
    usagePct       = 0;
    if (appState != MODE_MENU && appState != BRIGHTNESS && appState != SOUND &&
        appState != CONNECTION && appState != CLOCK && appState != RESET_CONFIRM)
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
  static unsigned long lastSlow = 0, lastFast = 0, lastNight = 0;
  if (millis() - lastNight > 30000) { lastNight = millis(); applyBrightness(); }  // night auto-dim
  if (appState == IDLE && millis() - lastSlow > 10000) {
    lastSlow = millis(); needsRedraw = true;
  }
  if (appState == CLOCK && millis() - lastFast > 1000) {   // tick the seconds readout
    lastFast = millis(); needsRedraw = true;
  }
  if (appState == PERMISSION && millis() - lastFast > 1000) {
    lastFast = millis(); needsRedraw = true;
  }
  if (appState == CONFIRMING && millis() - lastFast > 100) {   // animate the undo arc
    lastFast = millis(); needsRedraw = true;
  }
  if ((appState == SESSION_LIST || appState == DETAIL) && millis() - lastFast > 150) {
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
