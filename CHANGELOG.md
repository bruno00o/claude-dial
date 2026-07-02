# Changelog

## [0.5.0](https://github.com/bruno00o/claude-dial/compare/v0.4.0...v0.5.0) (2026-07-02)


### Features

* **ota:** detect Dial firmware version and flag available updates ([e4c0398](https://github.com/bruno00o/claude-dial/commit/e4c03984477d2c722341a2b4c6cf9cde7349ab23))

## [0.4.0](https://github.com/bruno00o/claude-dial/compare/v0.3.0...v0.4.0) (2026-07-02)


### Features

* **daemon:** persist per-session "always allow" grants ([c8fe34d](https://github.com/bruno00o/claude-dial/commit/c8fe34d99961f956834abe56b73975bca0c0f06a))
* **rules:** match Bash always-allow grants by command prefix ([b4e5cbe](https://github.com/bruno00o/claude-dial/commit/b4e5cbede5fb3f9de538cdee71f76c2b4f7d8d40))
* **service:** keep the daemon alive with a launchd agent ([25c8d76](https://github.com/bruno00o/claude-dial/commit/25c8d76ce47c40818a2701de001d8628d600f798))


### Bug Fixes

* **ble:** use write-with-response to avoid macOS write-buffer wedge ([3c57ff2](https://github.com/bruno00o/claude-dial/commit/3c57ff2cbe7ff5297332db93b1da70479dba6642))
* **firmware:** align permission timeout with daemon (120s -&gt; 90s) ([ef78fb9](https://github.com/bruno00o/claude-dial/commit/ef78fb91905933dfb46c8eab3b2944febed5e0a9))
* **state:** raise blocked-idle timeout to 60s for the away case ([75c0c47](https://github.com/bruno00o/claude-dial/commit/75c0c4749952924404332e7e89f02921a0fbfc16))
* **state:** show "needs you" for tools that request input, not "working" ([49418d9](https://github.com/bruno00o/claude-dial/commit/49418d9d45e68ec67fedc93289ab179bfb758e2b))

## [0.3.0](https://github.com/bruno00o/claude-dial/compare/v0.2.1...v0.3.0) (2026-07-02)


### Features

* amber terminal UI for the dial and simulator ([8e40f0a](https://github.com/bruno00o/claude-dial/commit/8e40f0afbb2778c39384e63397fd40a4bb4d0f51))
* **bridge:** stable project name from the repo root ([16f59d4](https://github.com/bruno00o/claude-dial/commit/16f59d471b0ab0979def0da0fa3d8ca5476ada9a))
* **bridge:** unique dial labels for same-project sessions ([070778c](https://github.com/bruno00o/claude-dial/commit/070778cd7698835c6b90d53ac0dc20c927e7107e))
* white pulsing needs-you state, distinct from working ([88b415e](https://github.com/bruno00o/claude-dial/commit/88b415ef2333685dcc44d1d2f85a88aef65c78ef))


### Bug Fixes

* **ble:** back off forced reconnects instead of flapping a dead link ([66badc0](https://github.com/bruno00o/claude-dial/commit/66badc008fd58696a7b2fba503b33c2da64a8081))
* **ble:** only write to the dial when the displayed state changes ([b2cb7d8](https://github.com/bruno00o/claude-dial/commit/b2cb7d89483dc17dd0d2f68d6dff90c55145db16))
* **ble:** recover a wedged link instead of freezing the dial ([cf81f0d](https://github.com/bruno00o/claude-dial/commit/cf81f0ddd34677509721f79914e73b560f2956d2))
* **bridge:** clear a resolved permission back to working promptly ([4a17891](https://github.com/bruno00o/claude-dial/commit/4a1789144ac6822abe429577dd8f030b02bb6891))
* **bridge:** keep a pending permission visible on the dial ([a01094d](https://github.com/bruno00o/claude-dial/commit/a01094d1f61bd72f686e545087e47a83ed92ce1a))
* **bridge:** non-blocking, delivery-confirmed BLE writes ([cddba4e](https://github.com/bruno00o/claude-dial/commit/cddba4e6a376c991db4d0c1b7702fd1373247ea9))

## [0.2.1](https://github.com/bruno00o/claude-dial/compare/v0.2.0...v0.2.1) (2026-07-01)


### Bug Fixes

* pin NimBLE-Arduino to 1.x so the firmware builds ([1433b4d](https://github.com/bruno00o/claude-dial/commit/1433b4dbc919e2c1b8ff75070cf307c924407ef2))

## [0.2.0](https://github.com/bruno00o/claude-dial/compare/v0.1.0...v0.2.0) (2026-07-01)


### Features

* drive the physical M5Stack Dial over BLE ([9e44b36](https://github.com/bruno00o/claude-dial/commit/9e44b369d1e138b75f3b156f131435119607c67a))

## 0.1.0 (2026-07-01)


### Features

* bridge daemon, web simulator, and M5 Dial firmware draft ([bf60fed](https://github.com/bruno00o/claude-dial/commit/bf60fed72e615eac239feb683916fae4a4371c92))


### Miscellaneous Chores

* restart versioning in the 0.x range ([32e4ba0](https://github.com/bruno00o/claude-dial/commit/32e4ba000e95f859c00ca7ed9c39bb77f2491db5))
