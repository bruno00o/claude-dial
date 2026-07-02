# Changelog

## [1.0.1](https://github.com/bruno00o/claude-dial/compare/v1.0.0...v1.0.1) (2026-07-02)


### Bug Fixes

* install hooks as fail-silent curl commands instead of type:http ([25ef149](https://github.com/bruno00o/claude-dial/commit/25ef14952c10e3725fa3148935d4bbb233ed6156))

## [1.0.0](https://github.com/bruno00o/claude-dial/compare/v0.17.0...v1.0.0) (2026-07-02)


### Bug Fixes

* **firmware:** keep screen text inside the round display and lighten the type ([04ebcad](https://github.com/bruno00o/claude-dial/commit/04ebcad9fa201776dceec48ab5d72ae22b11d6d3))
* **firmware:** type colour palette as uint32_t so LovyanGFX renders RGB888 ([a9074e2](https://github.com/bruno00o/claude-dial/commit/a9074e23ecd1d3cf989a73886a6f9c9fd4a5b1ca))
* **ui:** feed colours as RGB888, not raw RGB565 — amber was rendering green ([bba1af8](https://github.com/bruno00o/claude-dial/commit/bba1af8319bf168ed4141021ae09adf105ee03fa))


### Miscellaneous Chores

* release 1.0.0 ([7774481](https://github.com/bruno00o/claude-dial/commit/777448143db3b00208793160b8ec057b0f0ae39b))

## [0.17.0](https://github.com/bruno00o/claude-dial/compare/v0.16.0...v0.17.0) (2026-07-02)


### Features

* **ui:** port the A+C screen direction — two-temperature terminal ([f3b0410](https://github.com/bruno00o/claude-dial/commit/f3b0410aeb0a18dca4a8b0526b011380c2bc5f42))

## [0.16.0](https://github.com/bruno00o/claude-dial/compare/v0.15.0...v0.16.0) (2026-07-02)


### Features

* **ui:** factory reset from the Settings menu ([a1365f5](https://github.com/bruno00o/claude-dial/commit/a1365f5d5286a513a45efb4fb5baa198ae4cdc5e))

## [0.15.0](https://github.com/bruno00o/claude-dial/compare/v0.14.0...v0.15.0) (2026-07-02)


### Features

* **ui:** first-run setup hint on the Dial ([2eb5b79](https://github.com/bruno00o/claude-dial/commit/2eb5b7918ab615ae7350ae85e3158205ecd6f763))

## [0.14.0](https://github.com/bruno00o/claude-dial/compare/v0.13.0...v0.14.0) (2026-07-02)


### Features

* **ui:** a ring of dots around the rim — the recurring "dial" motif ([42905ec](https://github.com/bruno00o/claude-dial/commit/42905ec6b133c89021713205e778605210336ddd))
* **ui:** borrow M5 factory-UI polish — detent ticks, edge fade, wipe, boot fade ([42531c8](https://github.com/bruno00o/claude-dial/commit/42531c8f3f3d847cae0ae0ca1854068c511b06d1))
* **ui:** Settings → connection screen ([9b7b623](https://github.com/bruno00o/claude-dial/commit/9b7b623f0ba7bd92e35d586363d8bfeb41601ea0))
* **usage:** 5h usage gauge on the rim from local transcripts ([584c082](https://github.com/bruno00o/claude-dial/commit/584c0829b6cd317b7e5f1a1cb9081aba60616066))

## [0.13.0](https://github.com/bruno00o/claude-dial/compare/v0.12.0...v0.13.0) (2026-07-02)


### Features

* **ui:** show which machine the Dial is connected to ([ac494a5](https://github.com/bruno00o/claude-dial/commit/ac494a5212a3584057c75374c49e15e845c752b4))

## [0.12.0](https://github.com/bruno00o/claude-dial/compare/v0.11.0...v0.12.0) (2026-07-02)


### Features

* **sound:** distinct earcons per event + a volume/mute setting ([cc826bc](https://github.com/bruno00o/claude-dial/commit/cc826bc5b8122fe54f0042957205d9deaa2f3053))

## [0.11.0](https://github.com/bruno00o/claude-dial/compare/v0.10.0...v0.11.0) (2026-07-02)


### Features

* **ui:** read long commands in full + flag risky ones on the permission screen ([680984c](https://github.com/bruno00o/claude-dial/commit/680984c2649c34199dd95971d7fad5d7017806c7))

## [0.10.0](https://github.com/bruno00o/claude-dial/compare/v0.9.0...v0.10.0) (2026-07-02)


### Features

* **ota:** never flash firmware newer than the bridge ([e06a811](https://github.com/bruno00o/claude-dial/commit/e06a811b39f834b2bd01f30dc485c0690821fd27))
* **ui:** undo a permission decision within a grace window ([8c4a301](https://github.com/bruno00o/claude-dial/commit/8c4a3010cbe4f3ff76bd4d569cd459d8ebd16b0c))

## [0.9.0](https://github.com/bruno00o/claude-dial/compare/v0.8.0...v0.9.0) (2026-07-02)


### Features

* **ui:** tap a choice directly on the Dial's touchscreen ([1d5f358](https://github.com/bruno00o/claude-dial/commit/1d5f358f71102b79ddf9820dad01675586021708))

## [0.8.0](https://github.com/bruno00o/claude-dial/compare/v0.7.0...v0.8.0) (2026-07-02)


### Features

* **roster:** sort sessions by priority — needs-you first, idle last ([b614715](https://github.com/bruno00o/claude-dial/commit/b6147151c4f0223a9239dbafeba81626af23ddff))

## [0.7.0](https://github.com/bruno00o/claude-dial/compare/v0.6.0...v0.7.0) (2026-07-02)


### Features

* **ota:** show the target version while flashing + a firmware screen ([2bcdf31](https://github.com/bruno00o/claude-dial/commit/2bcdf31335d314cfe5bdaabd2ea06f13b4e6ea8a))

## [0.6.0](https://github.com/bruno00o/claude-dial/compare/v0.5.0...v0.6.0) (2026-07-02)


### Features

* **ota:** firmware BLE OTA service + dual-OTA partitions (phase 2a) ([5d1f9f6](https://github.com/bruno00o/claude-dial/commit/5d1f9f6955e6888f8eb9764424096f225080f0a7))
* **ota:** host-side BLE OTA client + `firmware update` (phase 2a) ([5eb6607](https://github.com/bruno00o/claude-dial/commit/5eb66076b16421f481121f9cc08bf1d266857644))
* **ota:** tactile "update available" prompt on the Dial (phase 2b) ([423a021](https://github.com/bruno00o/claude-dial/commit/423a021a5fe166e138f635447d87ffb018234ed7))

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
