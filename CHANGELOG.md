# Changelog

Notable user-facing changes are recorded here. Releases use [Semantic Versioning](https://semver.org/).

## Unreleased

## 0.1.4 — 2026-09-26

### Changed

- While a view loads, waiting updates now count against the 4 MiB limit on waiting output by their size alone. Before, the view was also stopped after 1,024 updates, however small they were.
- After a lost connection, the terminal retries quickly for about 30 seconds as before, then keeps trying on its own: every 15 seconds while the page is visible, and at once when the network comes back, the page becomes visible again or the window gets focus. Meanwhile it shows "Connection lost" with a Reconnect button; a workspace pane shows the same state with Retry. Before, it stopped after the quick phase and waited for Reconnect.
- A connection that drops again soon after it reconnects now shares the quick retries of the loss before it, instead of starting a new round. Only a connection that stayed up for 20 seconds starts a new round, so a view that keeps failing settles into the slower retries.
- When a terminal stops because a view kept falling behind, input kept being refused, or the connection broke protocol, the notice now has a Reconnect button.
- Typed input that cannot be sent because the connection is congested now shows a short "Input not sent" notice. As before, input is never queued or replayed.
- A page that reconnects on its own now takes control automatically only while it is visible and within 30 seconds of losing its connection, when control is most likely still held by that lost connection. Otherwise, if another window or device controls the session, the page stops with "Already controlled elsewhere" and a Take control here button. Opening a session, Reconnect and switching sessions take control as before.

### Fixed

- A view that falls behind while it loads is now stopped at once, so the browser reconnects sooner. Before, the broker could first report the view as live and then stop it, or notice the problem only after all history was sent.
- After this page took control from another window, the next dropped connection no longer leaves it saying "Reconnecting…" forever without trying.
- When the server ends a page that stopped answering its liveness checks, for example a phone that was in the background, the page reconnects instead of showing "This page fell behind".
- A reconnect attempt that times out now really closes its connection. Before, the browser rejected the close, so the old attempt kept its control lease and the next attempt had to take control from it.
- A terminal page or workspace restored with the browser's back or forward button reconnects instead of staying detached, unless it had stopped with a notice, for example because another device took control.
- When a workspace pane stops reattaching, for example because its view kept falling behind, it now says why and offers Retry. Before, it kept showing "Reattaching" with nothing running.
- When the page itself stops a view because of data it cannot use, it now shows a notice with Reconnect instead of freezing silently.

### Testing notes

- Recovery was tested in desktop Chromium against simulated connection loss, stalled connections, server liveness closes, control takeovers, back/forward navigation and hidden pages. Real mobile networks, iOS and low-end hardware were not tested.

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
