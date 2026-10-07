# Changelog

Notable user-facing changes are recorded here. Releases use [Semantic Versioning](https://semver.org/).

## 0.1.21 — 2026-10-07

### Changed

- Terminal history now crosses the network compressed. Opening a session, switching to another session and reloading the page send the whole history; terminal output usually becomes three to four times smaller, so these wait less on a slow connection. Small messages, such as typing reports and connection checks, are not compressed. The browser decompresses natively, so the page itself does not change.

### Testing notes

- Server tests read the frames on the wire: large output is compressed, small messages and connection checks are not, a browser that does not offer compression gets uncompressed messages, and a compressed browser message that would expand past the size limit ends the connection as "message too big" before it is read in full. The slow-connection test, now also in WebKit, measures the bytes on the link while the whole history loads. The ingress test checks that its proxy carries the compression offer through. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.20 — 2026-10-07

### Changed

- The terminal now knows what became of the text you type and insert. The server reports, for each piece of input, whether it was written to the terminal in full, refused, or written only in part. When the connection ends before that report arrives, the page tells you once that your last input may not have arrived. Nothing is ever sent again automatically.
- The composer shows "Inserting" until the terminal has taken the text, then "Inserted". Only one Insert waits at a time; Insert, Restore and Clear wait with it. If nothing of it reached the terminal, the text goes back into the empty draft. If it may have arrived, completely or in part, the text stays available with Restore and the composer asks you to check the terminal before you insert it again.
- An Insert that the connection did not take, or that is too large to send at once, keeps your draft and says why. Before, the draft was cleared and the composer reported the text as inserted.

### Fixed

- A Fit requested while an earlier size change was still waiting to be drawn was treated as done when that earlier change applied, so typing was allowed before the requested size arrived. A Fit now waits until earlier size changes are drawn.
- When the history was replayed after a connection was lost in the middle of a non-ASCII character, the first character of the replay could be joined to that broken character. The replay now starts clean.

### Upgrade notes

- A terminal page that was open during the upgrade cannot reconnect correctly, because the server now sends input reports it does not know. Reload it once.

### Testing notes

- Server tests check that each input gets exactly one outcome by the bytes the terminal took (also when control is revoked during the write, when the write fails, and for input held during a size change), that outcomes are sent in order and grouped, that a full backlog (also while output to the front door is stalled) and a failed report end the connection, and that the front door rejects reports that go back or ahead. A test with a real tmux server checks the reports for refused and written input. In Chromium and WebKit, tests check the composer for each outcome and for a connection that ends first, one Insert at a time, Clear and Ctrl+Enter while an Insert waits, a session switch while an Insert waits, a focus report sent just before an Insert, an Insert the connection refuses or that is too large, and the order checks on reports. Other new browser tests cover the two fixes. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.19 — 2026-10-07

### Fixed

- The session list that opens when you select the session name above the terminal now uses the height of the page. Before, it stopped at a fixed height, so on a tall window it showed only a few sessions above an empty area. It stops a small margin above the bottom of the visible page, and above an open on-screen keyboard.

### Testing notes

- In Chromium and WebKit, at phone, landscape and desktop sizes, a test with more sessions than fit checks that the list ends near the bottom of the page and stays inside it. The test was shown to fail without the change.

## 0.1.18 — 2026-10-06

### Changed

- After a short connection loss, the terminal now receives only the output it missed instead of its whole history. On a slow link this makes a reconnect take about as long as the missed output needs, not as long as the history. The terminal keeps its screen, scroll position and selection. When the page cannot continue exactly where it stopped, the server sends the whole history, as before. This happens after a server restart or a width change, when you switch sessions, and when you reload the history. A reconnect that must first take control back from the page's own lost connection also resumes, which is the usual case when a phone changes network.

### Fixed

- A size change that arrived while the terminal was still drawing earlier output could apply before that output, so the output wrapped at the wrong width. A size change and a reconnect's reset now wait until the earlier output is drawn.
- After a connection was lost in the middle of a control sequence, the first characters of the history replayed on the next connection could be lost. The replay now starts clean.

### Upgrade notes

- A terminal page that was open during the upgrade cannot reconnect, because the server now sends stream positions it does not know. Reload it once.

### Testing notes

- Server tests resume from the position after every frame of a stream that has small records, a record larger than one frame, and size changes. Each resume shows exactly what a page that never lost the connection shows. Other server tests check that a position from another stream or outside the stream gets the whole history, and that everything missed is sent before COMMIT. In Chromium and WebKit, a test checks the order of output and size changes, when the page offers a position, that it refuses a resumed admission it did not offer or that does not continue its screen, that connections which overlap never make it receive output twice, and that a history replay after a connection lost inside a control sequence starts clean. A test with a real tmux server over a slow link drops the connection while the shell prints, once seen by both ends and once by the browser only, where the page must take its control back first. After the reconnect, the screen must equal tmux's screen, every attempt must offer the page's position, and less than one sixteenth of the history may cross the link. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.17 — 2026-10-06

### Fixed

- A favorite now follows its session when tmux restarts, as an alias does. The server keeps the session name of each favorite. When the session is gone, the favorite moves to the running session with the same name on the same server, if that server's session list was read completely and no other favorite already holds the session. When two saved favorites have the same name, the newer one moves, and the older one is removed once its server's session list shows it is gone. The dashboard shows the moved favorite at its next refresh.
- When you remove a favorite at the same moment the server moves it to a restarted session, the dashboard now says that favorites changed and asks you to refresh. Before, it reported success and the restarted session kept its star.
- The size limits on tmux command output now apply. Before, a reply of any size was kept in full in server memory.

### Upgrade notes

- An earlier release cannot read the favorites saved by this release. Before a rollback, see "Favorites and rollback" in `deploy/README.md`.

### Testing notes

- A test with a real tmux server checks that a favorite follows a restarted session. A test runs a command the way tmux commands are run and checks that output larger than the limit keeps exactly the bytes the limit allows. Server tests check the order of inventory passes, servers that cannot be reached, same-name favorites, and storage faults. In Chromium and WebKit, a test checks that the dashboard reads favorites again when a refresh reports a newer revision during a read, and that a removal that meets a moved favorite is reported. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.16 — 2026-10-06

### Added

- **Output** in the dashboard's session filters, beside All, Favorites and Recent. It shows every running session with the most recent output first, so you can see which sessions printed something lately. It sorts the session list the dashboard already reads, so it adds no session list requests and no polling. A row that comes into view loads its preview, as when scrolling. A session whose output time is unknown comes last.

### Fixed

- A session's output time now counts output in all of its windows. Before, only the window shown in tmux counted.
- When a dashboard refresh moved a row whose alias editor was open, the editor stayed on screen but no longer blocked the page, and Escape did not close it. The row now keeps its place in the page while the other rows move around it.

### Upgrade notes

- A page that was open before the update shows the earlier filters until it is reloaded.

### Testing notes

- A test with a real tmux server checks that output in a window other than the current one updates the session's output time. In Chromium and WebKit, at phone, landscape and desktop sizes, a test checks the Output order (ties by name, unknown and future times last), that a refresh sorts it again, that switching to it does not read the session list again, that an alias editor stays open and modal while its row moves up or down, and that the filter row fits the screen. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.15 — 2026-10-05

### Fixed

- On a connection that stops passing data without closing, such as Wi-Fi with no internet behind it, every page request now ends within a time limit, including the reply body. Before, the dashboard could keep Refresh disabled for minutes, a terminal could wait for its appearance settings, and a workspace pane could wait behind a session list read that never ended.
- The first connection of a terminal now stops after 10 seconds when it cannot open, and the terminal then recovers as after any other lost connection. Before, it could stay at "Connecting" until the browser gave up.

### Changed

- After a failed refresh, the dashboard tries again by itself with increasing delays (up to 30 seconds), and at once when the device comes back online. The status line says it will try again. A session that was being created when the server stopped answering is shown as possibly created, and the list is read again.
- A workspace pane whose session list cannot be read offers Retry. When a pane creates a session and gets no reply, it says the session may have been created; Retry reads the session list and does not create it again.
- When the connection is lost, the terminal tries to reconnect every 5 seconds for the first two minutes, then every 15 seconds.
- The terminal tag stays at "Connecting" until the terminal has received its history. When this takes more than a second, the status line says "Loading history…".
- The session list scrolls to the current session when it opens, from the terminal tag or from quick actions.
- Dashboard previews load one at a time.
- The browser keeps the app's code and styles between visits and reloads. A page load after the first one downloads only the small page document. The server names each file by its content, so a new release is always loaded fresh.
- A terminal or workspace starts its session list, settings and workspace reads at the same time. Reconnecting no longer reads the session list only to renew the request token.

### Upgrade notes

- A page that was open before the update keeps the earlier behavior until it is reloaded.

### Testing notes

- A test server holds replies open after the first byte. In Chromium and WebKit, the tests check that the dashboard, the appearance settings and the shared session list read end at their time limits and recover by themselves when the server answers again, also when the device comes back online. Other new tests cover the cached file headers and the request token (Go), the first-connection limit and the reconnect timing, the "Loading history…" state, and the session list scroll. Each new test was shown to fail without the change it checks. Not tested on a real iPhone.

## 0.1.14 — 2026-10-03

### Fixed

- Session aliases work again. An alias now names a session by its tmux server and session name. It follows the session when tmux renames it, and it comes back after a restart when a session with the same name starts again on the same server. Before, every alias was lost at each restart, and the text of a lost alias could not be used again.
- The page title and the terminal tag show the current alias, also when it was changed on another device. A save is never undone by a session list that was read before it, also in workspaces.

### Added

- **Terminal position** in Settings → Appearance. It sets where a terminal sits when it is smaller than its window: **Top center** (the new default), **Top left** (the earlier placement) or **Center**. On an axis where the terminal is larger than its window, it starts at the edge and scrolls, as before. A change applies at once to every open terminal and workspace pane.
- The dashboard shows the sessions most recently opened on this device above the session list. They use the same rows as the list, with Preview, Favorite and the other actions. Settings sets how many are shown: Off, 3 (the default), 5 or 8.
- The alias of the current session can be edited from the terminal tag.

### Changed

- The session list that opens from the terminal tag starts with a Dashboard button. Its rows show the alias first, then the tmux name, and mark the current session. On a short screen, such as a phone in landscape, the whole panel scrolls so the list keeps room for its rows.
- The dashboard shows the alias first, then the tmux name. The toolbar is smaller: Refresh is an icon button, and the default scrollback setting is now in Settings → Sessions.
- Aliases can no longer be set in the configuration file. A configuration that still contains `aliases` does not load.

### Upgrade notes

- At the first start, the existing alias store is replaced by an empty one, because the earlier format did not record session names. Set your aliases again.
- A page that was open before the update cannot save appearance settings. Reload it.
- An earlier release cannot read appearance preferences saved by this release. Before a rollback, see "Appearance preferences and rollback" in `deploy/README.md`.

### Testing notes

- Aliases were tested against real tmux servers and the broker: a restart with a new tmux server, renames, a session replaced by one with the same name, incomplete session lists, and the store limits. In Chromium and WebKit, at phone, landscape and desktop sizes, the tests cover alias editing, saves that overlap with session list refreshes (also in workspaces), Recent sessions, and each terminal position with scrolling, selection and Select. Each new test was shown to fail without the change it checks. Touch scrolling and the on-screen keyboard were not tested on a real iPhone.

## 0.1.13 — 2026-10-02

### Fixed

- A terminal no longer stops updating until the service restarts when the recorder of its output loses its connection to tmux while it starts a new recording file.
- The session list no longer misses its reply deadline when many hidden attachment sessions wait for cleanup. Each reply checks at most four of them, in turn.

### Testing notes

- The recorder fix was tested with a forced order of events and with real tmux output bursts: none of 48 bursts on 4 CPUs and on 1 CPU left a recorder stuck (before the fix, 14 did). The session-list fix was tested with 600 hidden sessions over the real broker protocol: each reply took well under 0.1 s. The first cleanup of a newly started tmux server still checks every hidden session, and one tmux call that hangs can still delay a reply, as before.

## 0.1.12 — 2026-10-02

### Fixed

- The service no longer restarts repeatedly at boot while waiting for a user's tmux server to start.
- An open terminal page keeps trying when its tmux server or broker is temporarily unavailable or its session list is incomplete, instead of reporting that the session has ended.
- Stray hidden sessions restored after a reboot are cleaned up as soon as the broker sees them. Attachments that belong to a running or suspended broker are kept.
- Hidden attachment sessions are no longer part of the session list, so they cannot push regular sessions out of a long list.
- A very long session list is shortened and marked as incomplete instead of failing to load.

### Changed

- "New session" is available only when that account's tmux server is running. Start tmux on the host first; Persea Terminal never starts a tmux server itself.
- Session names that start with `persea-attach-` followed by 32 hexadecimal characters are reserved for hidden attachment sessions. "New session" refuses them.

### Testing notes

- The boot condition was reproduced: the service ran with a read-only temporary directory and no tmux server. 0.1.11 exits at start. 0.1.12 starts, reports that no tmux server is running, and works normally when tmux starts. Cleanup of restored hidden sessions, protection of attachments whose broker is running, paused or hidden from other processes, and very long session lists were tested against real tmux servers. Page recovery during outages and with incomplete session lists was tested in Chromium and WebKit. A full host reboot was not part of the tests.

## 0.1.11 — 2026-09-27

### Fixed

- Installing 0.1.10 could stop at random, because one of the server tests that the installer runs failed in about 3 % of runs. The server started the 30-second clock for output that a page has not taken in only after the output had reached the page. It now starts the clock before it sends the output. In 1,200 repeated runs the test did not fail.

### Testing notes

- Only the order of two steps in the server changed. The test that failed passed in 1,200 repeated runs under load, and the full server test suite passed.

## 0.1.10 — 2026-09-27

### Changed

- Terminal output uses fewer bytes, helping it arrive sooner on slow connections. Pages open during this upgrade must be reloaded once to reconnect.

### Fixed

- Right after a page caught up, a command could still be refused as typed while behind, because the server's record of what the page had shown could lag by up to a tenth of a second. The server now updates that record before it judges each key.

### Testing notes

- Measured on recorded output, the new format sends about 23–24 % fewer bytes for the same terminal output, and the server spends much less time checking it. Tested on a local stack in Chromium and WebKit at desktop and phone sizes, and in desktop Chromium through a link limited to 32 KiB/s with 150 ms of delay in each direction. Real mobile networks and iPhone keyboards were not tested.

## 0.1.9 — 2026-09-27

### Changed

- Each open terminal page now costs the server about 64 KiB of connection buffers instead of 32 MiB. With four pages open, the front door's memory high-water mark fell from about 80 MiB to about 22 MiB in testing.
- When no terminal is open, the session host now checks its recording work once per second instead of 20 times per second, so an idle server stays idle.
- The front door and the Tailscale sidecar now run with memory limits, so a fault in either cannot use up the host's memory. Normal use stays far below these limits.
- When a page falls far behind the session, typing is paused instead of going to a screen that may be a minute or more old. Keys typed while the page is more than about 10 seconds behind are not sent, and the page shows "Catching up — what you typed was not sent". The notice stays on screen with a Resume typing button, and until you press it nothing you type, paste or insert is sent. Press it once the page has caught up, then type the whole command again; nothing typed before the press is sent later. If the page is still behind, the next key you type is not sent and the notice comes back. The page keeps control of the session and keeps showing output while it catches up.

### Fixed

- A page that kept answering the server's connection checks but stopped taking in terminal output kept its connection, and control of the session, indefinitely. The server now closes such a connection when output has waited 30 seconds without progress. A page that works again reconnects by itself.

### Testing notes

- Tested on a local stack in desktop Chromium through a link limited to 32 KiB/s, with 150 ms of delay in each direction. 11 seconds after about 1 MiB of output, a page that was still taking it in refused a typed command: no key reached the shell and nothing more was sent until Resume typing was pressed, and the command typed after that ran. On this link the notice appeared about 12 seconds after typing began, because it waits behind output already on its way to the page; keys typed meanwhile were not sent either. Real mobile networks and iPhone keyboards were not tested.

## 0.1.8 — 2026-09-27

### Changed

- A view that falls behind is no longer disconnected. Before, when 4 MiB of output waited for a view, the server ended it and the page reloaded the whole history; on a slow connection with busy output, it could keep doing so until the server shortened the history. Now the server stops buffering output for that view, and the view reads what it missed from the server's saved history on the same connection, then continues live. How far a view can fall behind is still limited: when the server shortens the history, the page reconnects to the short version as before.
- When output is faster than the connection, the page now keeps control and keeps catching up, instead of reconnecting and reloading the history about every two minutes. What it shows can fall behind the session until the server next shortens the history.
- "This page kept falling behind" now appears only when the server cannot keep sending a view its output, for example when it runs out of memory for readers. A slow connection alone no longer causes it.

### Fixed

- After another window took control, a "Take control here" that failed to connect could get a new round of quick retries based on the old connection. The old connection is now judged when it ends.

### Testing notes

- Tested on a local stack in desktop Chromium through a link limited to 32 KiB/s, with 150 ms of delay in each direction and 3.5 MiB of history, for 10 minutes each. With about 20 KB/s of new output, the page got control after about 2.5 minutes, as before, and no view was ended for falling behind. With about 41 KB/s of new output, faster than the link, no view was ended for falling behind, and the page had control on every connection after the first; it reconnected only when the server shortened the history. Real mobile networks were not tested.

## 0.1.7 — 2026-09-26

### Fixed

- 0.1.6 could not be installed. The installer runs the test suite in a deeper temporary directory, and there a new test's Unix socket path was longer than the system allows. 0.1.7 contains the same changes as 0.1.6 and installs; use it instead of 0.1.6. CI now runs the Go tests with a temporary directory as long as the installer's.

## 0.1.6 — 2026-09-26

### Changed

- A connection now counts as stable only after its view has received all history and then stayed up for 20 seconds. Before, the 20 seconds started when the view opened, so on a slow connection a view that never finished loading started a new round of quick retries after each loss.
- When a view falls behind three times in a row before it has received all history, the terminal stops with "This page kept falling behind" and a Reconnect button, instead of loading the same history again and again. The count starts again when the server shortens the history, when you switch sessions and when you press Reconnect. This is a limit on retries: a view that would have caught up much later also stops, and Reconnect resumes it.

### Fixed

- On a slow connection, the page now learns why the server ended a view, for example because the view fell behind or because the server shortened the history. Before, the server waited only 1 second for that reason to get through, so the page treated it as a lost connection: it waited between retries, and it could not tell that a view kept falling behind.
- An error in the page's own handling of written output, such as keeping the scroll position, no longer freezes the view. The terminal now stops with "The terminal stopped" and a Reconnect button. Before, no later output or input reached the screen, and the page did not say why.

### Testing notes

- Tested in desktop Chromium through a local link limited to 32 KiB/s, with 150 ms of delay in each direction, 3.5 MiB of history and about 20 KB/s of new output. The page received the server's reason each time a view ended: once because it fell behind, and once because the server shortened the history, as it does when history grows large. The page then got control after about 2.5 minutes. Real mobile networks were not tested.

## 0.1.5 — 2026-09-26

### Changed

- Opening or reconnecting to a session with a long history now works on slow connections. The terminal opens after a small part of the history arrives, and the rest follows at the speed of the connection. The server sends output only as fast as the page can show it, so liveness checks wait behind at most about 150 KiB of output. As before, you can type only after the page has received all history: on a 32 KiB/s connection, 1 MiB of history takes about 45 seconds. A key typed before then now shows "Input not sent — history is still loading".
- The terminal connection uses a new protocol version. A terminal page that was open during the upgrade cannot reconnect; reload it.

### Fixed

- On a slow connection, a session with a long history no longer fails to open and retries over and over. Before, up to 256 KiB of history had to arrive before the session could open, and liveness checks waited behind all output already sent.

### Testing notes

- Tested in desktop Chromium through a local link limited to 32 KiB/s and to 256 KiB/s, with 150 ms of delay in each direction and more than 1 MiB of history. Real mobile networks were not tested.

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
