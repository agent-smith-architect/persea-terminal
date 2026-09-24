# Contributing

Keep changes bounded to one owner boundary and preserve fail-closed behavior.
Use synthetic realm names, accounts, hosts, identities, and terminal content in
source, tests, examples, and reports.

Take part according to the [code of conduct](CODE_OF_CONDUCT.md).

## LLM use

Mention the LLM model(s) and reasoning effort used in your PR.

## Development and checks

Use Linux (or a Linux VM/WSL2), tmux, less, ripgrep, Python 3, a C compiler for the Go race
detector, and the latest stable Go, Node and npm releases. `go.mod` and
`ui/package.json` declare minimum requirements; `ui/.nvmrc` tracks Node Current.
Before submitting a change, run from the repository directory:

```sh
umask 0022
export TMPDIR=/tmp
go vet ./...
go build ./...
CGO_ENABLED=1 go test -race ./...
(cd ui && npm ci && npx --no-install playwright install --with-deps chromium webkit)
(cd ui && npm run test:ci)
bash scripts/local-runtime-test.sh
python3 deploy/tests/host-config-test.py
bash deploy/tests/hermetic-deploy-test.sh
```

Browser tests use the locally declared Playwright package and its installed
browsers. `CHROME_BIN` may select an absolute Chromium-family executable for
additional compatibility checks. Browser installation with system dependencies
may need root; run the application and normal suites as an unprivileged user.
The browser groups are `test:browser:dashboard`, `test:browser:terminal`,
`test:browser:controls`, and `test:browser:workspace`; run one with `npm run`
for focused work. CI runs them independently. `npm run test:ci` runs everything
locally, and the reachability check prevents CI from omitting a group.
The hermetic deployment suite creates a temporary test environment. Run it as
a normal user with noninteractive sudo available for its scoped privilege-boundary
checks; it must not be pointed at production.

Optional historical captured-trace cases require `PERSEA_RETENTION_EVIDENCE_ROOT`
and skip when captures are unavailable. Synthetic retention, ordering, crash,
and batching tests run without private data. Never commit captured operator
terminal content to make those optional cases portable.

Behavior changes need a positive acceptance case and a falsifier that proves
the relevant bypass or mutation fails. Security-critical tmux operations must
pin the exact server process, session, window, pane, process start time, and
geometry inside the same tmux transaction.

Do not commit generated `ui/dist`, local runtime state, real usernames, tailnet
names, credentials, cookies, capability handles, or production logs.

See [dependency maintenance](ui/DEPENDENCIES.md)
before changing xterm, compilers, package versions, or the lockfile.
Record user-facing changes in [CHANGELOG.md](CHANGELOG.md); see
[release instructions](RELEASING.md) for versioning and publication.
