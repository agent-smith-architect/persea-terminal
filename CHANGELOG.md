# Changelog

Notable user-facing changes are recorded here. Releases use [Semantic Versioning](https://semver.org/).

## Unreleased

### Fixed

- Composer font changes preserve the draft's scroll position after a scroll gesture is interrupted by switching tabs or leaving the window.
- Existing sessions running full-screen programs can be opened without exiting the program, including after a resize. Journal rotation and explicit width refit reconstruct the visible alternate screen and fit the hidden normal screen to the current geometry. [Saved-screen restoration after a resize](docs/terminal-reconstruction.md) approximates cells tmux hides from capture.

## 0.1.1 — 2026-09-24

### Added

- Old releases are pruned after a successful install or rollback. Five are kept by default; use `--keep-releases <n>` or `PERSEA_KEEP_RELEASES` to change the count, or `--no-prune` to keep every release. The current and previous releases are never removed.
- Each GitHub release includes a source archive, a checksum file and a build provenance attestation.
- Issue templates and a code of conduct.

### Changed

- The installer keeps its deployment lock until every step it started has finished, including cancelled steps. Build steps run without access to the lock.
- The installer refuses to upgrade an install that predates the first public release.

### Removed

- Migration and rollback tools for layouts that predate the first public release.

### Fixed

- On first open, the automatic font size now matches what Fit font gives. Before, a desktop could stay at 24px and overflow, and a phone could open at 9px. An explicit font choice still wins.
- Workspace session chips show the session name instead of one letter.
- A width refit could end the previous attachment with a fault instead of refitting it.
- Changing the composer font no longer moves the draft's scroll position, and no longer interrupts a scroll the user has started.
- The composer no longer keeps a height measured while the temporary focus font was applied, and auto-grow no longer skips a content measurement when an inset update is already pending.
- When recording fails before a rotation starts, the rotation now reports the storage error instead of a generic failure.

### Testing notes

- Browser behavior was tested in desktop Chromium and WebKit, including phone-sized viewports. iOS keyboard behavior and low-end hardware were not tested on real devices.

## 0.1.0 — 2026-09-13

First public release.

### Added

- Browser access to existing and new tmux sessions across configured Unix users.
- Private HTTPS access through Tailscale, with a separate broker for each Unix user.
- Mobile-friendly selection, copying, keyboard controls, and terminal preferences.
- Output history and session reopening, with bounded journal storage.
- Deployment checks, immutable releases, and rollback tooling.

See [release instructions](RELEASING.md) for tagging and publishing.
