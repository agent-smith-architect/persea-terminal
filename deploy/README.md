# Persea Terminal production packaging

This directory is the production deployment boundary. It renders one supervised
AF_UNIX broker per configured realm, one trusted front door, and an optional
isolated Tailscale Service sidecar. It never creates, discovers, renames,
resizes, or destroys tmux servers, sessions, windows, or panes. It has no
Funnel, tailnet-policy, DNS, SSH, or global Tailscale reset operation.

## Host manifest and realms

Operators configure exactly one file:

```text
/etc/persea-terminal/host.json
```

It must be an ordinary root-owned `0600` file. Copy
[`host.example.json`](host.example.json), replace every synthetic account and
Tailscale value, and retain `version: 1`. The strict parser rejects duplicate or
unknown keys, unsafe names or Unicode, normalized display-name collisions,
ambiguous tmux selectors (globally for absolute paths and per UID for named
servers), path/unit collisions, and account/UID/group drift
before installation writes.

The manifest contains:

- `front`: the Unix account and shared group used by the browser-facing process;
- `ingress`: the exact Tailscale Service hostname, operator login, and connection bound;
- `tailscale`: Service, sidecar hostname/tag, tailnet suffix, and protected main-node identity;
- `realms`: stable realm IDs, display labels, Unix account bindings, and explicit tmux selectors.

Each realm produces `broker-<id>.json`,
`persea-terminal-broker-<id>.service`, and
`/run/persea-terminal-<id>/broker.sock`. `socket_name` means tmux `-L`;
`socket_path` means tmux `-S`. Exactly one is required per server. There is no
socket-directory scan and no fallback to a default tmux server.

Each serving realm requires the Unified terminal's
closed manifest block `unified_terminal_dev`. Its required fields are
`enabled: true`, `server`, `session`, and `observer_session`; the selected
server and target must already be allowed by that realm's `session_create`
policy, and the observer must be a different safe session name. The renderer
sets the runtime path to
`/run/persea-terminal-<realm>/unified-journal` and adds a service-private
`noswap` 96 MiB/256-inode tmpfs, broker cgroup bounds, and a hard zero core
limit to that broker. An absent or disabled Unified configuration is refused;
broker startup sets and verifies `PR_SET_DUMPABLE=0` before it
opens the journal or starts broker work; an unsupported platform or failed
guard prevents the configured broker from starting.

The Unified terminal is the dashboard's action for eligible sessions, using the
adoption transaction. The optional integer `adoption_slots` (1..4096)
sets the broker's existing complete-pane admission limit; omission retains its
default of nine: eight ordinary sources and one reserved transactional
successor. Retiring and recovered journals keep their slots until cleanup
settles. This changes only the count limit: the hard byte, inode,
tmpfs, and memory budgets still apply and may refuse admission earlier.

The journal does not provide isolation between processes sharing a Unix account.
Do not add `runtime_dir`, storage sizing, or memory limits
to the host manifest, and never hand-edit generated broker configs or
units.

Composer image staging is enabled by default for every configured realm.
Set `image_staging: false` on a realm to disable it explicitly. The front's
`image_upload_max_bytes` defaults to 10 MiB when any realm enables staging;
an explicit limit must be between 1 MiB and 10 MiB. If every realm disables
staging, omit the front limit as well. Normalization preserves explicit opt-outs.
The renderer derives each staging
directory as `<staging-root>/<realm-id>` — per-realm paths are never authored
in the manifest — emits it into the broker config and opens exactly that
directory through the broker unit's `ReadWritePaths`. The staging root is
host-level configuration: `front.staging_root` (optional) overrides the
default `/var/lib/persea-terminal-staging`; it must be a clean absolute path
and may not live under `/var/lib/persea-terminal`, `/tmp`, or
`/opt/persea-terminal`. The default is
deliberately a sibling of the front's state directory, never inside it: the
front's alias store refuses to start unless `/var/lib/persea-terminal` has
mode exactly `0700` (its siblings, the operator preferences store
`preferences.json` and the snippet/clip store `snippets.json`, are rendered
into that same directory and answer 503 rather than start elsewhere), so the
front unit gains nothing image-related and its
`StateDirectoryMode` stays `0700` unconditionally (the front only relays
upload bytes to the realm broker; brokers own the staged files). The installer
pre-creates the root (`root:root 0711`, traversal only) and each realm
directory (realm-owned `0700`) before units start; a missing directory fails
broker start rather than silently disabling staging, and the broker's startup
probe re-asserts ownership and mode.

To add, remove, reorder, or edit realms, change the host manifest and rerun the
guarded installer. An unchanged broker keeps its PID. A changed/new broker is
the only broker restarted/started; the front is restarted after brokers.
Removal stops and removes only that broker service and never touches its
configured tmux socket or sessions. A manifest edit without installation is
reported as drift and has no implicit runtime effect.

For an existing pre-manifest installation, run the one-time read-only derivation
before installing this release:

```sh
sudo deploy/migrate-legacy-host.sh
```

It refuses overwrite or ambiguous state, derives the current identities and
realms from the verified immutable release, installed units, and both Tailscale
endpoints, and proves byte-identical Serve readback before atomically creating
`/etc/persea-terminal/host.json`.

## Candidate, install, verify, rollback

Generate a side-effect-free review candidate from any regular fixture:

```sh
deploy/generate-candidate.sh \
  --host-config "$PWD/deploy/host.example.json" \
  --output /absolute/review-directory
```

### Optional diagnostics

Optional diagnostic captures use a pre-existing subdirectory of
`/var/lib/persea-terminal-diagnostics`, separate from the front's private
`/var/lib/persea-terminal` state. Diagnostic files deliberately permit analyst
group reads (0640), so provision a traversable parent and a setgid capture
directory for the intended analyst group. Keep the clipboard/state directory
0700. Diagnostics are disabled unless `diagnostic_trace_dir` is configured;
they are not a reason to relax private state permissions.

### Build toolchain

Use the latest stable Go, Node and npm releases; `go.mod` and `ui/package.json`
declare minimum requirements. Build checks also need tmux, less, clang,
Python 3, and Chromium's system libraries. From a prepared checkout, install the
OS browser libraries with `cd ui && npm ci && npx --no-install playwright install-deps chromium`
(this step may request root). The installer downloads the locked Playwright
Chromium build into its disposable build workspace and runs browser regressions
there as the build user. Node, npm and Chromium are build/test dependencies,
not runtime dependencies of the installed Go service.

The installer refuses any `node`, `npm`, or `go` whose path has a group- or
other-writable ancestor directory, and it checks the resolved target as well as
the symlink. A user who can write to a build tool can substitute a compiler and
get their code into a root-installed release, so the toolchain must be writable
only by root. Package managers that install into a shared, group-writable prefix
(Homebrew under `/home/linuxbrew`, a Conda environment) are rejected by design.

Supply a root-owned toolchain and point the installer at it:

```sh
sudo mkdir -p /opt/persea-toolchain && sudo chown root:root /opt/persea-toolchain
sudo chmod 0755 /opt/persea-toolchain
# extract official go and node release archives here, root-owned
sudo env \
  PERSEA_GO_BIN=/opt/persea-toolchain/go/bin/go \
  PERSEA_NODE_BIN=/opt/persea-toolchain/node/bin/node \
  PERSEA_NPM_BIN=/opt/persea-toolchain/node/bin/npm \
  deploy/install.sh --activate-local
```

`npm` finds `node` through the build PATH the installer constructs, so only these
three variables are needed.

Install from the root of a clean committed standalone checkout. On a fresh or fully
inactive/disabled deployment, omitting `--activate-local` installs the immutable
release and units without enabling or starting them. If any managed unit is
active or enabled, omission is rejected before build/import; upgrades therefore
use `--activate-local` so changed and removed brokers are handled transactionally:

```sh
sudo deploy/install.sh
sudo deploy/install.sh --activate-local
sudo deploy/verify.sh --require-active
```

Active verification checks the configured broker's exact cgroup/core values and
enters its mount namespace to read back the private tmpfs type, owner/mode,
`noswap` option, byte ceiling, and inode ceiling. A release is not accepted from
host-namespace `/run` metadata alone because `TemporaryFileSystem=` is private
to the service mount namespace.

Unified recording broker units also set `GOMEMLIMIT=528MiB`. This is a soft Go
collector policy, not a hard heap or service cap. It was qualified with the
eight-source recording profile, simultaneous retained readers/decoder/capture
owners and dense recovery overlap; the 96 MiB tmpfs and native process/kernel
memory are accounted separately. The cgroup limits remain 850 MiB High and 1 GiB
Max. See the [recording memory worksheet](../internal/broker/recording_memory.md)
for the capacities, measurement method and end-to-end verification requirements.

The installer resolves external Node/npm and Go toolchains from its `PATH`.
For nonstandard locations, pass absolute trusted paths as `PERSEA_NODE_BIN`,
`PERSEA_NPM_BIN`, and `PERSEA_GO_BIN` through the root environment. Candidates
inside the checkout or below group/other-writable directory boundaries are
rejected. These toolchains are trusted build inputs. The installer runs UI
checks/build, Go tests, the clang-backed
race suite, and the fresh build as the configured front UID in a scrubbed,
short, private workspace. Root revokes the build UID's access before importing
individually verified artifacts into a separate root-owned stage.

Releases live under `/opt/persea-terminal/releases`. The
`/opt/persea-terminal/current` path is intentionally an atomic symlink to one
immutable release; systemd units can use one stable path while upgrades and
rollback switch the complete binary/UI/config set atomically. `previous` records
the recoverable prior target. Generated `config/host.json`,
`config/resolved-host.json`, and `config/managed-units` bind every release to the
complete manifest digest. Every payload is covered by `MANIFEST.sha256`.

Rollback restores the target host snapshot, exact unit inventory/bytes,
symlinks, and requested lifecycle. Uninstall removes only the current manifest's
application units and package pointers. Both preserve releases, tmux state, the
front's durable stores (aliases, preferences, snippets), and activation evidence:

```sh
sudo deploy/rollback.sh --activate-local
sudo deploy/rollback.sh --to-release releases/<release-id> --activate-local
sudo deploy/uninstall.sh
```

### Shared clipboard and rollback

Clipboard text uses the existing snippets file, migrated to version 2. Existing
permanent text remains permanent. New items use a shared default retention,
initially 30 minutes, stored in `clipboard-preferences.json` beside that file.
Repeated exact content renews one item and preserves any longer retention.

Images use a separate owner-only `clipboard-images/` directory beside that file,
with no new host-manifest field. The existing `image_upload_max_bytes` enables
the image API and bounds each image; total storage is limited to 20 images and
64 MiB. Images use the shared retention choices. Existing PCI1 metadata is
normalized on read; renewal or merge writes PCI2. The front's existing state
directory grants cover this directory; no broker or staging permissions widen.
See [the storage contract](../internal/frontdoor/clipboard.md) for exact routes,
formats, and reclamation behavior.

During healthy maintenance, the running front removes expired text and image
data within one maintenance interval, even without a browser. I/O failures
retry, and a faulted store requires restart. Disabling uploads preserves this
cleanup. Before upgrading across these format changes, retain an owner-only
backup of the snippets file, image directory, and clipboard preference file if
present. A package rollback preserves durable stores; it does not downgrade
their schema. For an older binary, restore the matching pre-upgrade clipboard
backup with the front stopped and preserve original ownership and permissions.
Keep the binary, UI, and configuration together when rolling back. Restoring
the backup discards clipboard changes made after it was taken.

### Shared keyboard preferences and rollback

The front stores shared keyboard defaults in `keyboard-v1.json`, alongside
`preferences.json`, with the same owner-only durable-file rules. Generated
front configuration includes `keyboard_preferences_store_path`; older/dev
configurations may omit it. An unavailable keyboard store leaves terminal
service running: its GET reports `available:false`, and mutations return 503.
The store does not change the appearance preference schema. Roll back using
the target release's configuration and binary together; older releases ignore
the separate keyboard file, and rolling forward recovers it. Do not run an
older binary against the newer strict front configuration.

`GET/PUT /api/keyboard-preferences` uses the operator identity, Origin/CSRF,
no-store, ETag and If-Match contracts of `/api/preferences`. The complete value
is `{version:1, layout:{bar:string[], favorites:string[]}, prefixes:object}`;
GET, successful PUT and 412 include `revision`, `stored`, and `available`.
The record is replaced atomically on a matching revision. Lists are ordered,
unique, and bounded to 100 entries each; prefixes are bounded to 20. There is
no relation requiring the bar to be a subset of favorites. The request limit
is 32 KiB and the store limit is 1 MiB.

Keys are canonical `key:<encodeURIComponent(key)>:<mask>` IDs, with masks
0 through 7 and keys from navigation, F1–F12, or one ASCII printable character.
Sequences are `sequence:<encodeURIComponent(prefix name)>:<key ID>`.
Both the bar and favorites additionally permit `modifier:ctrl`, `modifier:alt`, and
`modifier:shift`; prefix values must be key IDs. Validation records structural
intent, independently of whether the browser can encode a chord. Missing
sequence prefixes do not invalidate saved settings. Prefix names follow
`[A-Za-z][A-Za-z0-9 _-]{0,31}`, excluding `constructor`, `prototype`, and
`__proto__`; case-fold duplicate names are refused like other duplicate JSON
keys. Legacy convenience IDs are not accepted by this new endpoint: clients
normalize `tmux:n`, `tmux:p`, `tmux:o`, `screen:n`, and `screen:p` into canonical
sequences before writing. Device overrides belong to browser storage and are
not uploaded automatically by the server.

### Rolling back past the font tri-state

Rollback preserves the front's durable stores. Automatic font sizing stores
`"font_size": null` in `preferences.json`; releases without tri-state font support
reject the entire file because they require an integer from 9 to 24.
`GET /api/preferences` then returns defaults with `"available": false`, and PUT
requests return `503`. The stored preferences remain intact. Restore the newer
release or convert null font sizes before rolling back.

Before activating a release older than the font tri-state:

```sh
sudo systemctl stop persea-terminal-front.service
sudo deploy/preferences-store-downgrade.sh            # --dry-run to preview
sudo deploy/rollback.sh --to-release releases/<release-id> --activate-local
```

The script rewrites every `"font_size": null` to `14` — the default that release
shipped — keeping each record's theme, default session and revision, writing a
timestamped `.bak` beside the store, and refusing to run while the front unit is
active (the front would overwrite the repair on its next write). It is
idempotent: a second run reports that every record is already explicit and
changes nothing. Rolling **forward** again needs no step at all; the repaired
operators simply hold an explicit `14` instead of auto, and can set themselves
back to auto with the `Fit font` control.

## Isolated Tailscale Service identity

“Separate tagged host” means a separate Tailscale node identity, not necessarily
a second physical machine. The production host runs a second root-owned
`tailscaled` in userspace-networking mode:

```text
remote tailnet client -> manifest tailscale.service:80 or :443
  -> persea-terminal-tailscaled.service
  -> unix:/run/persea-terminal/front.sock
  -> Persea Terminal
```

The Tailscale Serve adapter omits `X-Forwarded-Proto` for plaintext
port 80 and emits `https` for TLS. Persea Terminal accepts the absent header
only as a GET/HEAD redirect signal after the trusted-peer and exact-host checks.
Application requests require exactly one `https` value; explicit `http` and
malformed values are denied. The hermetic loopback adapter instead requires
exactly one `http` value.

The protected main-node identity retains its existing routes, Tailscale SSH, and
`/run/tailscale/tailscaled.sock`. Every sidecar command uses
`/run/persea-terminal-tailscale/tailscaled.sock` explicitly; preservation
readbacks use the main socket explicitly. The sidecar's state and runtime roots
are private root-owned directories. Tailscale's LocalAPI socket mode is not the
confidentiality boundary; the non-traversable runtime parent is verified as
that boundary.

### Tailscale compatibility policy

The deploy scripts do not pin Tailscale to an exact version or commit. The
host package auto-updates, and the sidecar unit executes the system
`/usr/sbin/tailscaled`, so an exact pin can only block deploys after an
upgrade; it can never protect the running sidecar. Two mechanisms replace it:

1. **Version floor** (`PERSEA_TS_MIN_VERSION` in `deploy/lib.sh`): the
   Tailscale release the ingress slice was validated against. The CLI and the
   daemon each report their version; MAJOR.MINOR.PATCH is compared numerically
   and anything below the floor is refused with the floor named. Newer patch,
   minor, and major releases pass this check. Pre-release suffixes
   (`1.103.5-dev...`) are reduced to their core version; an unparseable version
   is refused.
2. **Capability probe** (`PERSEA_TS_REQUIRED_CAPABILITIES` in
   `deploy/lib.sh`): every `tailscaled` flag the sidecar unit's `ExecStart`
   uses, and every `tailscale` subcommand and option the deploy scripts
   invoke, is checked against the installed binary's offline `--help` output.
   A missing entry is refused with the exact help path and token named, for
   example `tailscale serve --help lacks required flag --service`. The probe
   never contacts a daemon: every `--help` form exits before opening a socket,
   and the CLI help is still requested through the explicit main-socket
   wrapper so no default-socket invocation exists.

The two checks are the only Tailscale gates. A commit hash is not a
compatibility signal and is not checked. Adding a new `tailscale` option or
`tailscaled` flag to any deploy script means adding its entry to the
capability table; the hermetic suite fakes a binary whose help lacks one
entry and expects the refusal to name it.

The running sidecar is covered separately: verification compares the sidecar
MainPID's executable inode with the on-disk daemon, so a package upgrade that
replaced the binary under a running sidecar is refused until the unit is
restarted.

Activation and deactivation record the versions they validated, without
gating on them, as `tailscale-version.json` in their evidence directory under
`/var/lib/persea-terminal-deployments/<stamp>/`: the floor, the CLI and
daemon `version` and `long version`, and the capability table that was
probed. An incident can therefore show which Tailscale a release was
activated against.

Install, verify, and interactively enroll the sidecar after the application
release is active:

```sh
sudo deploy/install-tailscale-sidecar.sh
sudo deploy/verify.sh --require-sidecar
sudo deploy/login-tailscale-sidecar.sh
```

The login requests exactly the manifest's sidecar hostname and sole tag. It
accepts no auth-key argument and never uses `--reset`. A tailnet administrator
must own the tag, authorize it to advertise the manifest Service, approve the
Service (or configure a narrow auto-approver), and grant intended users TCP 80
and 443.
For the fully synthetic example manifest, an equivalent policy fragment is:

```json
{
  "tagOwners": {"tag:terminal-service": ["autogroup:admin"]},
  "autoApprovers": {"services": {"svc:terminal": ["tag:terminal-service"]}},
  "grants": [{"src": ["autogroup:member"], "dst": ["svc:terminal"], "ip": ["tcp:80", "tcp:443"]}]
}
```

Policy edits and Service approval remain administrator actions outside these
scripts. Remote activation is a separate guarded mutation:

```sh
sudo deploy/activate-service.sh
sudo deploy/deactivate-service.sh
sudo deploy/uninstall-tailscale-sidecar.sh
sudo deploy/uninstall-tailscale-sidecar.sh --purge-state
```

Activation requires the exact tag, non-protected sidecar identity, Service FQDN,
active runtime/socket readback, and positive/peer-mismatch/positive AF_UNIX
probes. It configures only the manifest Service, then boundedly requires
advertisement, approval, DNS, exact HTTPS plus HTTP-redirect Serve config, and
Funnel-free readback.
Failure drains and clears only that Service while proving main-node and
non-target Service preservation. `--purge-state` is an explicit irreversible
request accepted only after the Service is clear and the daemon is stopped.

## Verification

Run the configuration, deployment lifecycle, and offline systemd checks:

```sh
python3 deploy/tests/host-config-test.py

deploy/tests/hermetic-deploy-test.sh
deploy/tests/systemd-unit-test.sh
```

Packaging verification alone proves packaging, not reachability. A remotely
reachable claim additionally requires sidecar login, tailnet authorization,
exact live Serve readback, and browser acceptance from another tailnet node.
