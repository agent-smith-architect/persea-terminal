# Reconstructing an existing terminal

Opening an existing session, journal rotation and explicit width refit use the
same capture-equivalent bootstrap. A full-screen program on the alternate
screen does not block any of these operations.

On tmux 3.4, measured with a private real server:

- `capture-pane -p -N -S - -E -` returns normal-screen history followed by the
  current visible display, including when that display is the alternate screen.
- `capture-pane -a -p -N` returns the saved normal visible display without
  history. With `-q`, an absent saved screen produces an empty response line.
- `alternate_saved_x` and `alternate_saved_y` identify the saved normal cursor;
  `cursor_x` and `cursor_y` identify the current cursor.
- Entry with `CSI ? 1047 h` does not save a cursor. Both saved coordinates are
  `UINT_MAX`; the matching exit retains the active cursor. This is accepted.

Both captures, the pending parser prefix and the mode probe are in one
eight-command control-mode line. Pre/post probes include the alternate state
and saved cursor. A mismatch discards the capture and retries. The existing
observer identity, subscription, pane and window checks still apply.

Reconstruction paints normal history and the saved normal display, positions
the saved cursor, enters the alternate screen with `CSI ? 1049 h`, and paints
each alternate row at its absolute position. It then restores the current
scroll region, modes, tabs and cursor, and appends the pending parser prefix
immediately before live bytes. Exiting the alternate screen restores the
normal display and retained history. Normal-screen captures use the original
single-screen reconstruction.

The bootstrap byte cap includes both displays and their switching sequences.
Oldest history is trimmed first; neither visible display is trimmed. An
oversized visible state or malformed capture fails closed. History depth stays
bounded by the requested history limit. Opening and rotation neither resize nor
send input; width refit still requires the existing explicit guarded request.

This is capture equivalence, not recovery of every emulator detail. tmux 3.4
does not expose wrap metadata, G0/G1 designation, arbitrary saved DECSC state,
cursor style or the live SGR state at the capture boundary. Those pre-existing
limitations remain. The saved normal cursor exposed for the alternate screen
is reconstructed, but hidden saved attributes cannot be recovered.

Width changes while the alternate screen is active have an additional tmux
limit: the saved grid keeps its old dimensions until alternate-screen exit.
tmux then reflows it, but its public capture formats expose neither those
dimensions nor wrap metadata. The saved capture also clips each row at the
current width, so a shrink can hide cells that tmux later restores. A regression
constructs two saved grids with identical captures after a shrink and different
normal displays after exit, proving that those captures cannot reconstruct both
outcomes. See the [tmux 3.4 capture implementation](https://github.com/tmux/tmux/blob/3.4/cmd-capture-pane.c)
and [screen restoration](https://github.com/tmux/tmux/blob/3.4/screen.c).
A saved cursor beyond the new width is accepted
and positioned within the new grid; its eventual restored position can differ
when tmux reflows the saved display. The replay tests cover growth and shrinkage
with hard rows and a saved cursor within their text. They do not prove arbitrary
saved-grid reflow after a width change.

New brokers do not emit `blocked_alt_screen` or
`rotation_deferred_alt_screen`. Front-door and UI parsers retain both values
for older brokers during mixed-version deployments.

The real-server regression replays committed journals in the UI's installed,
patched xterm, without a DOM. Run `npm ci` in `ui` before the Go suite. The
dashboard browser group also opens and reopens a real alternate-screen session
at four phone viewports, with console capture and no-input/no-resize checks.
