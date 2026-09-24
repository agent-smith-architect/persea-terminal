# Changelog

Notable user-facing changes are recorded here. Releases use [Semantic Versioning](https://semver.org/).

## Unreleased

### Fixed

- Fit the terminal font to the admitted session geometry on first open while preserving explicit font preferences.
- Let workspace session chips use available pane width instead of truncating names inside a fixed-width toolbar.

## 0.1.0 — 2026-09-13

First public release.

### Added

- Browser access to existing and new tmux sessions across configured Unix users.
- Private HTTPS access through Tailscale, with a separate broker for each Unix user.
- Mobile-friendly selection, copying, keyboard controls, and terminal preferences.
- Output history and session reopening, with bounded journal storage.
- Deployment checks, immutable releases, and rollback tooling.

See [release instructions](RELEASING.md) for tagging and publishing.
