# Changelog

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
