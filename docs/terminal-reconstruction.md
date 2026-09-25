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
Oldest history is trimmed first. If necessary while alternate mode is active,
hidden normal rows are replaced with blanks, from top to bottom, preserving
their positions and the saved cursor. The visible alternate display is never
trimmed. An oversized visible state or malformed active capture still fails
closed, as for normal-screen reconstruction. History depth stays bounded by
the requested history limit plus saved rows moved into history during fitting.
Opening and rotation neither resize nor send input; width refit still requires
the existing explicit guarded request.

This is capture equivalence, not recovery of every emulator detail. tmux 3.4
does not expose wrap metadata, G0/G1 designation, arbitrary saved DECSC state,
cursor style or the live SGR state at the capture boundary. Those pre-existing
limitations remain. The saved normal cursor exposed for the alternate screen
is reconstructed, but hidden saved attributes cannot be recovered.

Resizing while the alternate screen is active has an additional tmux
limit: the saved grid keeps its old dimensions until alternate-screen exit.
tmux then reflows it, but its public capture formats expose neither those
dimensions nor wrap metadata. The saved capture also clips each row at the
current width, so a shrink can hide cells that tmux later restores. A regression
constructs two saved grids with identical captures after a shrink and different
normal displays after exit, proving that those captures cannot reconstruct both
outcomes. See the [tmux 3.4 capture implementation](https://github.com/tmux/tmux/blob/3.4/cmd-capture-pane.c)
and [screen restoration](https://github.com/tmux/tmux/blob/3.4/screen.c).
Any saved height and cursor are accepted. Fitting follows the measured tmux 3.4
height rules: shrink removes bottom rows below the saved cursor first, then
moves remaining top overflow into history; growth pulls available history into
view and adds blank bottom rows if needed. The saved cursor follows those row
shifts and is clamped inside the current geometry. tmux does not expose how much
history remains eligible for growth after clearing, so reconstruction uses the
captured history. Captured rows are treated as hard rows at the current width;
missing cells stay blank. The original width, wrap flags and allocation of
blank cells are hidden, so tmux's width reflow and restored cursor can differ.

After a resize while a full-screen app runs, the normal screen shown after the
app exits can differ from tmux's in the cells tmux hides. This is a documented
approximation of the hidden normal screen, never a reason to block opening,
rotation or refit. The visible alternate display and current cursor remain
capture-equivalent. Real-server tests cover height shrink and growth (including
history overflow and blank padding), width shrink and growth, and a saved cursor
beyond both new bounds. The clipped-cell and cursor cases assert specific
approximations; cases with exposed hard-row state compare the restored normal
screen and cursor directly with tmux.

New brokers do not emit `blocked_alt_screen` or
`rotation_deferred_alt_screen`. Front-door and UI parsers retain both values
for older brokers during mixed-version deployments.

The real-server regression replays committed journals in the UI's installed,
patched xterm, without a DOM. Run `npm ci` in `ui` before the Go suite. The
dashboard browser group also opens and reopens a real alternate-screen session
at four phone viewports, with console capture and no-input/no-resize checks.
