# Scrollback controls

The dashboard's Default scrollback selector chooses how many rows the browser retains
above the live screen. Its choices are screen only, 500, 1,000, 2,000, 5,000,
7,500 and 10,000 rows. The unified terminal defaults to 1,000 rows. Legacy attachment routes retain their existing
default and history protocol.

## Importing an existing tmux session

Before the first unified attachment, the adoption request carries an optional
integer `history_rows`. Both HTTP and broker protocol boundaries reject values
outside 0..10,000; omission defaults to 10,000. The broker independently checks
the bound before performing any effects. Zero captures no history. When a
full-screen program is active, its visible alternate display is retained and the
hidden normal display is fitted to the current geometry. After a resize, the
normal screen shown on exit can differ in cells tmux hides from capture.

The chosen depth reaches the existing atomic capture composite. Its witness
checks, output boundary, capture byte cap, cursor and terminal-mode restoration,
and reconstructed journal provenance are unchanged. An oversized bootstrap still
trims the oldest history rows first, then blanks hidden normal rows if needed;
the visible alternate display is never trimmed. The
import can only read history tmux still has. See
[terminal reconstruction](../docs/terminal-reconstruction.md) for the two-screen
capture contract and its limits.
It does not set tmux's history limit, resize its window, or send terminal input.
Dashboard, session-switch and workspace adoption paths always request the full
bounded 10,000-row import. The device preference only controls browser retention:
the first device to open a session must not reduce the shared history available
to other devices. The optional API field remains an explicit server-import limit
for non-UI callers, independent of the browser default.

Adoption remains idempotent: an already active journal is not replaced or
recaptured when it is opened again. Output excluded by an earlier import cannot
be recovered by changing browser retention. Journal rotation and the bootstrap
byte limit can also limit how much older output remains available.

## Changing an open terminal

Quick actions contains the same Scrollback selector. It updates the existing
xterm buffer immediately while preserving the current viewport anchor where
possible. Reducing the value discards older browser rows; increasing it makes
room for more output without introducing a second renderer or text model.
The single-terminal URL follows the live selection, so a document reload keeps
the selected depth. A saved terminal override takes priority over a link's depth;
otherwise an explicit link depth applies to that opening, with the device default
used when no depth is supplied. An untouched device defaults to 1,000 rows.
Font-layout callbacks also check their admission and replay state before
restoring a saved viewport anchor. A callback started before a fresh transcript
arrives cannot scroll that transcript back to the old position.

Reload available history reconnects the current pane through its existing
source-bound remint and admission path. The unchanged journal is replayed into
the selected buffer size, recovering older recorded rows that a smaller browser
buffer had dropped. The controller refuses this action during a session switch,
geometry refit, reconnect, or disposal. The page also requires a committed,
non-replaying terminal outside selection mode. Normal input gating, leases and
generation checks continue to apply. Reload never changes tmux's geometry or
requests the legacy `HISTORY_REQUEST` protocol.

## Preference scope

The dashboard sets this browser's default in `localStorage` under
`persea-terminal.scrollback.v1`. Changing Quick actions > Scrollback saves an
override for this terminal on this device under
`persea-terminal.scrollback.sessions.v1`; it does not change the device default.
The override uses the session incarnation key, including realm and server, so a
renamed session retains its choice while a replacement session starts fresh.
The bounded store keeps the latest 128 terminal choices and contains no session
capability or terminal content. Existing explicitly saved device defaults remain.

Choose Device default in the terminal selector to remove its override and apply
the current device default. Dashboard opens, Resume, switching and workspace
panes resolve the same setting. Defaults apply when a terminal is opened; changes
in another tab never shrink a terminal already open. Dashboard links follow
storage changes. If browser storage is unavailable, the current page still
applies its selection and explains that it was not saved.

## Verification

`npm run test:scrollback-controls-browser` checks launch depth, live shrink and
growth, source-bound replay, document reload, phone/landscape/desktop layouts,
console cleanliness, and the
absence of terminal input, resizing and legacy history requests in Chromium and
WebKit. The dashboard cohesion gate checks persistence, all Open links and the
adoption payload. Broker tests generate numbered history in a private tmux server
and check actual imported rows plus unchanged geometry and tmux options. HTTP and
wire tests cover zero, larger values and invalid input.
