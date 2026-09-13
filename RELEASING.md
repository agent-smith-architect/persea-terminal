# Releasing

The first public release is **0.1.0**, with Git tag **v0.1.0**. Tags identify
product releases; the private UI build package has no separate public version.

During 0.x development, use a patch release for compatible fixes and a minor
release for features or breaking changes. Describe breaking changes and migration
steps in the changelog. Use 1.0.0 when the public configuration and deployment
interfaces have a settled compatibility commitment; thereafter, breaking changes
require a major release.

1. Add user-facing changes under `Unreleased` in [CHANGELOG.md](CHANGELOG.md).
2. For a release, replace that heading with the version and release date, remove
   the planning note, and add a fresh `Unreleased` section. Commit the changelog.
3. Wait for CI on that commit. Create an annotated `vX.Y.Z` tag at the tested
   commit and push it. Published tags are immutable; corrections get a new version.
4. Create the GitHub release from that tag, using its changelog section as the
   release notes. Mark 0.x releases as pre-releases while the interfaces evolve.

Keep dependency maintenance separate from product versions. Before release,
refresh dependencies, test the recorded lockfile, and retain any required xterm
patch. See [dependency maintenance](ui/DEPENDENCIES.md).
