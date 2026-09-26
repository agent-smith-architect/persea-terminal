# Changelog

Notable user-facing changes are recorded here. Releases use [Semantic Versioning](https://semver.org/).

## Unreleased

### Changed

- While a view loads, waiting updates now count against the 4 MiB limit on waiting output by their size alone. Before, the view was also stopped after 1,024 updates, however small they were.

### Fixed

- A view that falls behind while it loads is now stopped at once, so the browser reconnects sooner. Before, the broker could first report the view as live and then stop it, or notice the problem only after all history was sent.

## 0.1.3 — 2026-09-25

### Changed

- The help for Fit rows, Fit width and Terminal size now says that these actions fix the tmux window at the new size for every attached terminal, and how to give sizing back.

### Fixed

- A terminal view no longer reconnects over and over while a full-screen program repaints its screen many times per second. A view can now hold up to 1,024 small updates while the browser loads the history, instead of 64; the 4 MiB limit on waiting output is unchanged.
- When the broker drops a view that has fallen behind, its log now names the limit that was reached.

## 0.1.2 — 2026-09-25

### Fixed

- Sessions running a full-screen program, such as an editor, a pager or a monitor, now open without waiting for the program to exit, including after the terminal was resized. Journal rotation and width refit work for them too. When the program exits, the normal screen is restored; after a resize, a few cells that tmux hides can differ. See [terminal reconstruction](docs/terminal-reconstruction.md).
- Composer font changes keep the draft's scroll position after a scroll gesture is interrupted by switching tabs or leaving the window.
- When the recorder refuses to open a session because its source quota is full, every resource reserved for that open is now released.

### Testing notes

- Browser behavior was tested in desktop Chromium and WebKit, including phone-sized viewports. iOS keyboard behavior and low-end hardware were not tested on real devices.

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
