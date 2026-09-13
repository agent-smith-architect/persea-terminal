# Working on Persea Terminal

Read these instructions before changing the repository.

## What this is

One Go binary serves a browser interface for configured tmux servers. Existing
sessions can be adopted; new sessions require an enabled session-creation policy.
The broker refuses unconfigured servers and does not discover them automatically.

- `internal/frontdoor` — HTTP/WebSocket front door. Listens on AF_UNIX only.
- `internal/broker` — one per realm, runs as that realm's unix user, speaks to tmux.
- `internal/terminal`, `internal/attachmentwire`, `internal/proto` — the attachment protocol.
- `ui/src` — TypeScript. `ui/dist` is generated; never edit or commit it.
- Clipboard behavior and input ownership: [ui/clipboard.md](ui/clipboard.md);
  retention, storage, and API contract: [internal/frontdoor/clipboard.md](internal/frontdoor/clipboard.md).
- `deploy/` — production packaging. See [deploy/README.md](deploy/README.md).

## Current terminal

Unified is the only supported terminal. Serving brokers require enabled
`unified_terminal_dev` configuration; attachment requests require the Unified
engine token. Retired UI deep links and empty-engine attachments are refused.

The retired terminal implementation and tests are available only in Git history.
Do not restore them or add compatibility paths during current-terminal work.
Shared Epoch/control, wire protocol, stylesheet, and iOS router remain active
despite historical names.

## Build and test

```sh
go build ./... && go vet ./... && go test ./...     # needs tmux installed
(cd ui && npm ci && npx --no-install playwright install --with-deps chromium webkit)
(cd ui && npm test && npm run test:browser)
python3 deploy/tests/host-config-test.py
```

The Go suites start a **private** tmux server on their own socket. They never
touch your default server or your sessions.

Environment caveats for the Go suite: socket-binding tests need short temp
paths (AF_UNIX caps socket paths near 107 bytes — run with a minimal `TMPDIR`
such as `/tmp`), and permission tests assume `umask 0022`. Current reopen
coverage runs with `npm run test:unified-reopen-browser`. Two harness facts
worth knowing before writing browser tests: Playwright's fake clock
(`clock.install`) removes the navigation-timing entries some product logic
reads, and WebKit refuses `Secure`/`__Host-` cookies over plaintext
`http://localhost`, so WebKit runs need a TLS origin.

`npm run test:browser` drives real Chromium over the DevTools protocol. It is the
only place several behaviours can be proven — touch focus, layout containment
against the visual viewport, CSP — so do not treat the unit suite as sufficient
for anything the browser actually does.

## Invariants a change must not break

Preserve these invariants and the tests that enforce them.

1. **The front door has no TCP listener.** It binds an AF_UNIX socket, verifies the
   peer uid with `SO_PEERCRED`, and only then trusts any header. Every other check
   — forwarded host, forwarded proto, operator identity, Origin, CSRF — is only
   meaningful because of that one. Header-based identity on a TCP loopback socket
   would be decoration.
2. **Unknown `Tailscale-*` headers are a denial.** That is what makes an accidental
   Funnel exposure fail closed rather than serve the internet.
3. **Attachment never mutates the operator's session.** A shadow session
   (`persea-attach-<nonce>`) links the window; the source session, its options, and
   any other attached clients are untouched. Only an explicit, acknowledged Control
   request may resize, and display fitting never does.

   The **explicit vertical Fit** is the exception. The configured target, and only that
   target, may accept a post-birth **row-fit** request from its current Control
   attachment after one trusted click on the `[↕]` control, subject to these checks:
   - **rows only.** The browser sends its committed columns; the broker requires
     them to equal the exact pane witness and rejects anything else. The browser is
     never authoritative for width.
   - **zero automatic.** No `ResizeObserver`, `window.resize`, `visualViewport`
     event, orientation change, browser zoom, font Fit, keyboard show/hide, CSS
     reconciliation, admission or reconnect may produce a resize request. Font Fit
     is presentation-only and is labelled "Fit font" so the two cannot be confused.
   - **reject, never clamp.** Rows outside 8..120, geometry above 36,000 cells, a
     second request while one is pending, a stale source/epoch/control authority, a
     multi-pane window, or any witness mismatch is refused without mutation.
   - **the guard does not move.** `resize-window` and `pty.Setsize` stay in
     `internal/broker/attachment.go`. The unified engine changes only *where* the
     already-guarded command is issued — on the observer's own control connection —
     so the guard, the exact post-command witness recheck, and the attachment PTY
     resize apply unchanged.
   - **the boundary is the last block.** A guarded `if-shell` occupies two
     `%begin`/`%end` blocks on a control connection; tmux returns to its event loop
     between them and pane output can be classified there. Sequence N is the
     completion of the **last** block. A refused guard reports `%end` with an empty
     response, so the witness recheck is the only rejection detector.
   - **geometry is durable typed truth.** The unified journal is `PUJ2`: a
     mandatory birth geometry in the generation header, then ordered committed
     `OUTPUT` and `GEOMETRY` records keyed by sequence, not byte offset. `PUJ1`
     fails unified closed and is never migrated. Pane bytes cannot construct a
     geometry event.
4. **Session destruction belongs to the attachment transaction alone.** A session
   this product did not create, it must never delete. Creation is permitted in
   exactly two audited files with exact callsite counts — see
   `TestNoDestructiveOrImplicitResizeProductCallsites`. That guard is a **token
   scan**: writing a forbidden tmux verb in a *comment* will trip it, and writing
   `new-session` in a comment inflates the callsite count. Reword; do not exempt.
5. **The browser never supplies execution input.** Session creation sends a name
   and a server label. The command, working directory, and geometry come from
   broker configuration only. The request shape is the guarantee — there is no
   validation to forget.
6. **Config makes unsafe deployments unrepresentable.** `validateIngressForEUID`
   refuses to start on anything that is not either the hermetic-loopback shape or
   the exact production shape. Prefer extending that over adding a runtime check.
7. **An operator's config may narrow policy, never widen it.** Session names pass a
   baseline grammar first (which excludes `.` and `:`, tmux target separators) and
   the operator's pattern second. A permissive manifest must not be able to open an
   injection.

## Conventions

- **Assert behaviour, not source text.** Use source-text assertions only for
  structural invariants. Exercise behavior through the relevant runtime or UI.
- **Prove a new test can fail.** Break the fix, watch the test go red, restore it.
  A test never observed failing asserts nothing.
- Errors carry a code from a closed set. Free text from a broker is never echoed to
  the browser; the UI maps codes to its own wording.
- Comments explain *why*. The code already says what.
- `ui/dist/` and `ui/node_modules/` are generated and gitignored.

## Interface verification

- **UI behavior is proven in a real browser, never inferred from source.** The
  Chromium browser suite is the frozen regression gate. A WebKit lane
  (Playwright) exists because WebKit is Safari's engine family: use it for
  Safari-specific rendering, layout, and event behavior, and prefer it when a
  finding must hold on iPhones.
- **Know what an engine cannot prove.** The iOS software keyboard, its
  accessory bar, focus zoom, and dictation are operating-system behaviors no
  desktop browser reproduces. Findings that depend on them are labeled
  hardware-pending and closed only on a real device — never by emulation, and
  never by a test that cannot fail.
- **The console must stay clean.** Every browser-driven test or audit captures
  console messages and page errors on every page it touches; any message is a
  finding to triage, not noise. Tooling caveat: some automation injects its own
  styles or scripts (e.g. during screenshots) that can trigger CSP reports —
  tag instrumentation phases so the product is never blamed for the harness.
- **Sweep across viewports, not just one.** Mobile checks run at small,
  standard, and large phone viewports plus landscape; touch-target minimums
  and truncation are per-viewport properties.

## Scope boundary

One `operator_login` reaches every realm; per-realm operator allowlists are not
implemented. There is no dashboard action for closing sessions; use `exit` in
the terminal. Commands run through terminal input, with no separate remote-exec API.
