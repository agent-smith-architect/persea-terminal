# Publication checklist

Publish a reviewed source snapshot into a new repository when development history
contains private identities, paths, or operator evidence. Keep the original
repository private. Deleting a file, moving it to an archive, or adding it to
`.gitignore` does not remove it from Git history. The retired terminal is absent
from the current tree but remains in the private development history.

Before publishing:

1. Freeze a clean, tested commit. Export it with `git archive HEAD` into a new
   directory, inspect that exact tree, and initialize separate public history.
   Choose the public author identity deliberately; do not copy local Git config,
   remotes, reflogs, tags, or private branches into the candidate.
2. Scan source, all candidate Git objects and commit metadata, generated bundles,
   binaries, release archives, filenames, screenshots, and embedded source maps
   for credentials, personal information, internal paths, and real operator
   content. Use both a secret scanner and project-specific private search terms
   supplied from outside the public tree. Review matches without publishing the
   matched secret values. Rotate any credential that was exposed; rewriting
   history does not revoke it.
3. Run the documented fresh-clone setup and CI checks as a normal user in a clean
   Linux environment. Verify local startup and shutdown, production build,
   dependency license notices, xterm regressions, and deployment validation.
   Linux process/socket permissions, real browser behavior, and race checks
   cannot be replaced by type checking alone.
4. Review GitHub surfaces separately: issue/PR bodies and comments, Actions logs
   and artifacts, releases and attachments, wiki, descriptions, branch/tag names,
   and security settings. A source-only export does not migrate these surfaces.
   Do not change an existing private repository's visibility merely because its
   current branch is clean.
5. Enable and verify GitHub private vulnerability reporting before making the
   reporting promise in `SECURITY.md` public. Set branch protection/required CI
   checks, review Dependabot proposals, and provide a release tag and accurate
   README. Confirm the intended public repository, author identity, and exact
   reviewed commit before the final publication action.

For the default branch, require the `Go`, `UI`, and `Deploy tooling` checks from
the `CI` workflow, require the branch to be current before merging, and block
force pushes and deletion. Verify the rules with a pull request on the new
destination; a green run on a private staging branch does not configure those
rules. Check feature availability for the destination's visibility and plan.
Enable private vulnerability reporting and confirm that its reporting entry
point is available before publishing the security policy. Review available
secret scanning and push protection settings there as well.

Include `LICENSE` and `THIRD_PARTY_NOTICES.md` with binary release archives, and
retain the UI build's `THIRD_PARTY_NOTICES.txt` alongside its bundled assets.

For terminal reliability, test the actual network boundaries: temporary loss,
blackholed connections, delayed output, reconnect exhaustion, and explicit
recovery. Assert retained readable output, no replayed input, no unrequested
resize, and correct Control ownership. Use a sustained-output run to measure
resident memory and input responsiveness on a constrained browser. Add buffering
or concurrency machinery only if these measurements demonstrate a problem.
Desktop browser emulation does not prove iOS keyboard behavior or low-end
hardware performance; label those gaps and test representative devices.

Keep the release evidence private when it contains local paths or operator
details. A successful automated scan is evidence of its stated coverage, not a
guarantee that no private information or vulnerability exists.
