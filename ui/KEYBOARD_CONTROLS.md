# Terminal Keys

On touch screens the strip appears immediately above an open native keyboard
and disappears when the keyboard closes. Quick actions provides
access while the keyboard is closed. Keys opens Favorites without focusing
an editor and stays open for repeated actions. Keys closes it; native text,
composition, paste, or a terminal tap also closes it. An armed modifier can
consume the next A–Z letter typed directly into the terminal through keydown
or cancelable, non-composing insertText. The chord uses xterm's existing
encoder and control gates, and the next letter is unmodified. Other native
editing is unaffected.

All keys contains Navigation, Letters, Numbers, Symbols and Sequences; the
adjacent Fn view exposes F1–F12. Ctrl, Alt and Shift apply to the next explicit
panel or strip key, or a native terminal letter. Tap an armed modifier to cancel
it. Composer, settings and clipboard editing never consume terminal modifiers.
Favorites and its Terminal preset include Alt by default. An existing nonempty
browser Favorites layout gains Alt once without resetting its other keys, bar
or prefixes. Explicitly empty layouts and layouts at the 100-item limit remain
unchanged. Later removal of Alt is retained.
Classic aliases explain when combinations share bytes; unavailable encodings
explain their limitation. Alt+Tab uses Escape-prefixed Tab. Keyboard
focus stays on the same logical key after a one-shot modifier is consumed.
Group and sequence-prefix choices use focus-preserving disclosure buttons
inside the panel. Native selects would move focus out of xterm and summon an
OS picker, whose keyboard transition can dismiss the accessory. Choice buttons
retain draft/selection, use the same gesture-generation guards as other panel
controls, and return keyboard focus to their disclosure after selection.
Escape directed at these controls closes the chooser; terminal-directed input
keeps its existing path. Explicit keyboard dismissal still closes the panel.

The catalog is tested against xterm in normal and application cursor modes.
Printable characters use xterm’s public insertText path; named/control keys
use its keyboard evaluator followed by a neutral key release. Alt printable
chords send Escape followed by the character/control
key, without changing the physical keyboard’s Option-key policy.

The backspace helper leaves its sole invisible sentinel in place during
beforeinput. If the native edit retains that sentinel, the input capture
removes it before reconciling the field. Rewriting the field before the edit
causes WebKit to discard the next native insertText after Ctrl+C.
Composition retains its separate exclusion and cleanup.

Dashboard Keyboard settings and the panel gear open the same full-size,
closable editor. It can add keys/chords, remove and reorder favorites and strip
controls, restore defaults, and apply Terminal, Shell, Vim,
tmux or screen presets. Preset labels identify key combinations, not guesses
about the foreground application's state. The complete strip layout is
customizable and scrolls when necessary. Edits use explicit Save; closing
unsaved edits requires an explicit discard or return to editing.

Prefix bindings are user-named and independent. tmux Ctrl+B and screen Ctrl+A
are convenience defaults. Add a name such as Custom, then choose its actual
prefix key/chord. The Sequences group sends that configured prefix followed
by the chosen suffix. These are raw ordered keystrokes to the current terminal,
not a provider API or a promise that the foreground app has those bindings.
Configure your actual mappings before using them. All steps are validated
before sending; no delayed sequence or transient-input replay queue exists.

The top toolbar provides composer, selection, copy and font-fit controls.

Shared defaults persist through `/api/keyboard-preferences`, separately from
appearance settings. Revision/ETag comparison prevents silent concurrent
overwrites. Explicit device overrides use `persea-terminal.keyboard-device.v1`
and apply to all terminals in that browser/origin. Layout and prefixes inherit
independently; Use shared defaults removes their overrides. The document
service refreshes on foreground/storage/settings events and periodically.
Legacy `persea-terminal.keys.v1` choices migrate only into browser overrides;
their original bytes remain for rollback. Storage failures retain usable
in-memory state and report that persistence is unavailable.
The separate `persea-terminal.keyboard-defaults-upgrade.v1` marker records the
one-time Alt addition without changing the stored layout schema. Explicit device
saves also set it, so future reloads do not undo the user's choices.

In normal layout the panel uses its content height up to three quarters of the
band remaining below the toolbar and above the keyboard, composer and strip,
reserving at least 88px for terminal content when space permits. This grows
with the actual unobscured viewport rather than a device model or fixed 240px
ceiling. Long catalogs scroll only when they exceed that budget. Short bands
use two compact rows; when a composer shares the smallest band they use one
horizontally scrollable row. The fixed Keys control returns to the core strip
and clears modifiers. Layout switches wait for held pointers to end.
The composer retains its draft, selection and focus; Hide keyboard deliberately
blurs the current editor when more space is wanted. Targets stay at least
44 CSS pixels. Layout, panel and keyboard
changes never send a remote resize. Opening/closing preserves the local scroll
anchor; normal terminal input retains its existing follow-to-bottom behavior.

Session, transport, control-authority and background transitions cancel panel
state and pending gestures. Pointer cancellation clears pending modifiers;
drags do not send keys. Input uses the same current source/epoch/control gates
and explicit Fit/refit seals as composer input.

Validation commands:

- `npm run test:terminal-actions`
- `npm run test:terminal-keys-browser` (Chromium and WebKit; four viewport sizes,
  reduced keyboard-sized bands, native dictation fixture, persistence, target
  sizes, ordered prefix delivery, lifecycle cancellation and all supported
  catalog encodings)
- `npm run test:terminal-keys-mutants` (compiled negative controls)
- `npm run test:terminal-keys-editor-browser` (editor intent exits, logical-key
  keyboard focus, and native focus preservation in Chromium and WebKit)
- `npm run test:terminal-keys-picker-browser` (trusted acquisition and choices,
  draft/selection preservation, keyboard focus return, cancellation and genuine
  dismissal in both engines; included in the main Keys browser gate)
- Existing `npm test` and `npm run test:browser` regression gates.

Desktop browser emulation cannot prove OS software keyboard placement,
dictation UI, non-English IME behavior, VoiceOver/TalkBack, floating keyboards
or phone rotation/wake behavior. Verify those behaviors on physical iOS and
Android devices with their native keyboards and accessibility tools.
