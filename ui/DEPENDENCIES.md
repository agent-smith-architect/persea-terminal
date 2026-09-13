# Dependency maintenance

Use the latest stable Node, npm and Go releases. `.nvmrc` tracks Node Current;
CI selects Go `stable` and installs `npm@latest`. `npm ci` consumes the committed
lockfile. The npm lifecycle policy explicitly allows esbuild's installation
step; the project's `postinstall` applies the xterm patch and fails if it no
longer applies. Do not bypass lifecycle scripts in a release build.

`go.mod` declares the minimum supported Go version, not a compiler pin.
Install the current compiler before building; offline builds must provision
their tools and dependencies in advance.

UI development dependencies use `latest`; run `npm update` to refresh the
lockfile, then run the normal tests, including browser checks. `npm ci` deliberately
reproduces that recorded dependency set. Dependabot proposes weekly lockfile,
Go module and GitHub Action updates. Actions track their stable major release,
receiving minor and patch updates automatically. Go modules require concrete
versions; update them with `go get -u ./...` and run the race checks.

The xterm dependency remains exact because its patch is version-specific.
Review terminal-library and major dependency changes before merging updates.

`npm audit` and a current Go vulnerability scanner are useful release checks;
zero findings means no matching advisory was found, not that the code is secure.
Retain third-party license notices when generating assets.

## xterm integration

`@xterm/xterm` 6.0.0 uses a version-specific `patch-package` patch:
[`patches/@xterm+xterm+6.0.0.patch`](patches/@xterm+xterm+6.0.0.patch).
It changes normal-screen scrollback handling for CSI Scroll Up, including
scrolling regions whose top is below the first row. Alternate-screen scrolling
and Delete Lines must not add normal history. The patch updates the generated
CJS and ESM distributions.

The related upstream report is [xterm issue 6010](https://github.com/xtermjs/xterm.js/issues/6010)
and [PR 6011](https://github.com/xtermjs/xterm.js/pull/6011). The proposed upstream
behavior covers regions starting at the first row; our acceptance cases also
cover a lower region. Check the actual merged behavior before removing the patch.

When upgrading xterm:

1. Read the upstream changes and install the candidate version in a branch.
2. Run `npm run test:region-scrollback-browser` against the unpatched candidate
   to establish which required behaviors it supplies. Do not weaken expectations
   merely to make an upgrade pass.
3. If still necessary, port the minimal source change to that exact upstream
   version, build its distributions, regenerate the patch with `patch-package`,
   and review the CJS/ESM changes. Verify a fresh `npm ci` applies it.
4. Run the complete UI suite. In particular check scrollback, resize geometry,
   selection, and CSP in a real browser.

Application code in `openTerminalWithStyleNonce` temporarily intercepts style
creation during `terminal.open` to attach the page's CSP nonce, and restores the
original behavior in `finally`. Check this integration when upgrading xterm.
