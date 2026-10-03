# Dashboard and session discovery

The dashboard opens on Sessions. Workspaces and Settings have separate views.
The header has one New session action and the shared Clipboard. New session
expands a single form above the list: choose the user/server explicitly when
more than one is available, supply a required tmux name, and optionally supply
a display alias. Creation uses the selected user's existing creation API. Its
returned session ID locates the new session before a separate alias save; a
failed alias save offers a retry that never repeats creation. A retry refreshes
the inventory and refuses to bind a different incarnation or duplicate a save
whose response was lost.

Every available session has the same terminal icon and Open control. An open
session follows a normal same-tab link. An adoptable session performs its one
adoption only after a trusted click, then opens in the same tab.

Favorites and a preview are always available beside Open. A session's alias is
its main title, with the tmux name in smaller text beside it. Without an alias,
the tmux name is the title. A pencil beside the title opens the alias editor in a focused dialog for both adding and
editing an alias. Saving or clearing it closes the dialog and returns focus to
the pencil. Its draft, baseline revision and current incarnation remain governed
by the same refresh and conflict rules. The information button reveals session
facts directly with a preview beside them; on phones the preview comes first.
There is no second information disclosure. A single-server user
heading shows the user's display name; multiple servers retain their labels so
the choice stays clear.

Recent appears above the filter with the hint “Opened on this device”. It shows
the three most recently opened sessions that are still running, newest first.
Each uses the same row as the main list, including Preview, favorite, information,
alias editing and Open. A session can appear in both places; each row keeps its
own focus, open preview and alias draft during inventory refreshes. Recent uses
the inventory and preview snapshots the list already loads.

Settings has a Sessions card with “Recent sessions on the Sessions tab”: Off, 3,
5 or 8. The choice takes effect immediately and is saved on this device. Off hides
Recent. Default scrollback is in the same card. Refresh is the ↻ button beside the
filter; the live count follows the All, Favorites and Recent filters.

Recent first looks for the exact session previously opened. After a restart, it
can show a single running session with the same name in the same realm and server.
It never chooses between multiple matches. Ended, ambiguous and blocked sessions
keep their short explanations. A configured default is offered as a normal row
under “Default session” only when there is no recent live session. Opening the
dashboard with `?resume=1`, including from the installed app, focuses the first
Recent row's Open action (or the default's Open action); it never activates it.

The dashboard uses one scoped control and spacing system: 44px minimum targets,
clear primary actions, a bounded desktop width, and reflow for phones and enlarged
text. Appearance, Keyboard and aliases of sessions that are not running are collapsible Settings
panels with inset controls. Dashboard chrome follows the operator's terminal
palette; these layout rules do not restyle the terminal surface.

Session rows share their metadata wording with the terminal's session menu:

- Size is columns × rows of the active tmux window.
- Attached counts tmux clients; it is not an agent or task status.
- **Output … ago** uses the optional `output_activity` inventory field, taken
  from tmux's `window_activity`. This is active-window output activity; tmux also
  initializes it when a window is created. Unknown, zero or implausibly future
  values are reported as unknown. The older `activity` field still carries
  session interaction time and remains available in Details.

There is no eight-hour limit. The old **quiet** label used `session_activity`,
which can remain unchanged while a process continues producing output. In tmux
3.4, [window creation](https://github.com/tmux/tmux/blob/3.4/window.c) and
[pane output parsing](https://github.com/tmux/tmux/blob/3.4/input.c) update window
activity; [session interaction](https://github.com/tmux/tmux/blob/3.4/server-client.c)
updates the separate session timer. The broker test drives background output in
a private tmux server to prove that the two values can diverge.

The history explanation in Session information means that earlier history was
bootstrapped from a capture of the screen and the selected available scrollback
when browser access began. Default scrollback in Settings controls the
import and browser retention limits. An open terminal offers the same control
and an explicit reload of available recorded history. See [Scrollback](scrollback.md)
for bounds, storage, replay, and what an increase can recover. The typed journal
provenance remains unchanged.

## Refresh and identity

Session cards and their controls are keyed by exact `draftScope` incarnation,
never by a reused name or a reusable tmux session ID alone. Connected DOM islands
remain in place on refresh. Their metadata, links and callbacks read the latest
inventory while alias forms retain their baseline revision and unsaved values.
An alias change elsewhere must still pass the server's revision check.

Workspace forms retain drafts across background, foreground and manual refresh.
List request generations prevent an older response from undoing a later local
mutation. Workspace deletion requires a confirmation naming the workspace, then
uses the existing revision precondition. Deletion removes the saved layout; it
does not close any terminal session. Phone workspace limitations are unchanged.

Filtering updates the matching count and offers a clear action for no results.
A failed refresh retains the last good inventory with an explicit retry
instruction. There is no ambiguous "Updated just now" timestamp. The visible
document checks inventory every 60 seconds; a quick window return does not
repeat a recent read. Manual refresh stays available. Reads are deduplicated and
do not replace connected row controls. Hidden documents do not poll. Destruction
retires outstanding reads and removes listeners.

## Output snapshots

Wide rows load one snapshot when their small thumbnail enters the viewport.
Passive first captures are staggered below the server's two-per-second budget.
Narrow rows show an eye button and load on demand. Opening information or the
preview dialog also loads it if necessary. The row, information panel and dialog
share that capture. Inventory refresh, returning to the window, reopening
information and enlarging the thumbnail reuse it. Only
Refresh preview requests another capture. The old image stays visible while a
refresh is pending or fails. The server may reuse a capture for three seconds.
The modal shows capture time and dimensions, supports
Escape and normal focus restoration, and closes when its session disappears.

The broker's existing read-only `capture-pane` call includes SGR attributes.
Plain `rows` remain compatible with older clients; additive `ansi_rows` preserve
bounded style information. Other escapes are removed, inherited colors survive
row truncation, and the combined capture has the same 8 KiB budget. The browser
creates text spans with known styles and terminal palette colors, including
16-color, indexed and RGB colors. It never interprets captured text as HTML or
terminal commands. Preview reads do not attach, resize or send input.

## Shared favorites and device history

All, Favorites and Recent are presentation filters. Favorites sort first, then
natural sorting handles names such as `session1`, `session2`, `session10`. Up to 128 favorites
are exact incarnation identities persisted per authenticated operator through
`/api/dashboard-preferences`, shared across that operator's devices. ETag/If-Match
revision checks and intent rebasing preserve unrelated concurrent changes. The
client reconciles a lost reply before retrying and imports old device pins only
after the shared save is confirmed. A same-name replacement never inherits a
favorite. Aliases name a session by its realm, server and tmux name. After a
restart, an alias returns when a session with the same name starts on that
server. If tmux renames a running session, its alias follows the rename. Each
running session has at most one alias, and running sessions cannot share alias
text. Reusing text from a detached alias removes that older entry.

Settings lists aliases of sessions that are not running, with their session
names and a Remove button. This section is hidden when empty. Removing an
entry prevents that alias from returning. At the store limit, a new alias
replaces the oldest detached entry; running aliases are retained.

The terminal tag and page title use the current alias from the session list.
Opening the tag refreshes that list. The alias leads the tag in bold; the tmux
name follows on wide screens and is hidden on phones. The compact Alias row shows
the current name or None. Edit opens a field with Save, Clear when a name exists,
and Cancel. Save or Cancel returns to the compact row. Revision conflicts preserve
a draft and offer Reload saved alias. Name
conflicts explain that another running session uses the alias. The same wording
appears in the dashboard and in the optional alias step after session creation.

Alias store version 2 records session names and the last incarnation on which
each alias was shown. Older stores are replaced with an empty version 2 store
on load; old records cannot be converted because they do not contain names.
Alias text updates and removal require the saved revision; only creation needs
a live session handle. Writes remain atomic and durable. The store keeps at most
1024 records and 1 MiB; detached entries are evicted oldest first. If the store
cannot be saved, the session list stays available and the front door logs the
failure once. Creating an alias while a server's inventory is incomplete is
refused as unavailable.

The dedicated durable `dashboard-preferences.json` file is a sibling of the
configured appearance preferences store. It preserves that store's closed schema
and rollback compatibility: older releases ignore the new file. Store failures
report unavailable state and do not silently downgrade to device-only favorites.

Recent remains device history, recorded only after an accepted terminal COMMIT.
It keeps at most 32 records from the last 30 days. Each new history entry includes
the session name so it can be found after a restart; earlier entries without a name
can still match their exact identity. The section count is saved under
`persea-terminal.recent-sessions.v1`.
Neither history nor favorites contain capability handles or terminal content;
they grant no authority.

The scrollback choice is also device-local, under `persea-terminal.scrollback.v1`.
It is shared between this origin's tabs and applied to later opens. An active
terminal changes its own retention only after the operator changes its selector;
another tab cannot unexpectedly shrink a reader's active buffer.

The shared menu retains natural ordering within its existing realm/server
groups. Current-session facts are disclosed separately; search and the Dashboard
action remain outside the scrolling list. Existing admission, trusted activation,
Control and geometry rules still govern every selection.

## Verification

`npm run build && npm run test:dashboard-redesign-browser` runs the focused
Chromium and WebKit gates. They also run inside the full `npm run test:browser`
regression gate. The focused checks cover a real 60-second refresh, exact DOM
focus, drafts and selection, fresh links, deletion confirmation and revision,
same-name reincarnation, empty filters, themes, enlarged text and responsive
layout. `test:dashboard-cohesion-browser` adds shared favorites across independent
browser contexts, concurrent saves, passive Recent rows, consistent actions, creation
and alias recovery, colored cached previews, modal errors and session removal,
settings spacing and responsive layouts. Session-menu tests use a private fixture and assert zero additional
terminal input, adoption or resize during menu interaction.

Dashboard viewports include 320, 390, 430, 768, 844, 1440 and 1920 CSS pixels.
Menu viewports include 360, 390, 430, 844 and 1440. The existing toolbar's very
narrow identity-chip fallback remains covered by the full toolbar and quick-sheet
tests. Browser engines do not prove the iOS software keyboard or dictation;
those remain physical-device checks.
