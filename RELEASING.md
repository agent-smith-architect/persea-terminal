# Releasing

The first public release is **0.1.0**, with Git tag **v0.1.0**. Tags identify
product releases; the private UI build package has no separate public version.

During 0.x development, use a patch release for compatible fixes and a minor
release for features or breaking changes. Describe breaking changes and migration
steps in the changelog. Use 1.0.0 when the public configuration and deployment
interfaces have a settled compatibility commitment; thereafter, breaking changes
require a major release.

1. Add user-facing changes under `Unreleased` in [CHANGELOG.md](CHANGELOG.md).
2. Move the released entries under their version and release date, leaving a
   fresh `Unreleased` section. Commit the changelog.
3. Wait for CI on that commit. Create an annotated `vX.Y.Z` tag at the tested
   commit and push it. Published tags are immutable; corrections get a new version.
4. Create the GitHub release from that tag, using its changelog section as the
   release notes. Use pre-release status for preview versions such as
   `0.2.0-rc.1`; ordinary 0.x releases can be marked as the latest release.

## Release checks

Before you tag a release, also check these points:

- Include `LICENSE` and `THIRD_PARTY_NOTICES.md` with any binary release
  archive, and keep the UI build's `THIRD_PARTY_NOTICES.txt` with its bundled
  assets.
- Test the actual network boundaries of the terminal: temporary loss,
  blackholed connections, delayed output, reconnect exhaustion, and explicit
  recovery. Assert retained readable output, no replayed input, no unrequested
  resize, and correct Control ownership.
- Use a sustained-output run to measure resident memory and input
  responsiveness on a constrained browser. Add buffering or concurrency
  machinery only if these measurements show a problem.
- Desktop browser emulation does not prove iOS keyboard behavior or low-end
  hardware performance. State those gaps in the release notes, or test on
  representative devices.
- Scan release archives, generated bundles, and screenshots for credentials,
  personal information, internal paths, and real terminal content.

Keep dependency maintenance separate from product versions. Before release,
refresh dependencies, test the recorded lockfile, and retain any required xterm
patch. See [dependency maintenance](ui/DEPENDENCIES.md).
