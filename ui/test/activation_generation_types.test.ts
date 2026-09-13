import type { ComposerOptions } from "../src/composer";
import type { SessionSwitcherViewOptions } from "../src/session_switcher";
import { bindKeyboardPreservingActivation } from "../src/keyboard_preserving_button";
import { bindExplainedTapActivation, bindGenerationFencedClickActivation, bindTapActivation } from "../src/tap_activation";

// These are compile probes, not runtime fixtures. If a persistent owner or a
// low-level binding ever makes its interaction-generation provider optional,
// the corresponding @ts-expect-error becomes unused and typecheck fails.
declare const composerWithoutGeneration: Omit<ComposerOptions, "interactionGeneration">;
// @ts-expect-error persistent Composer owners must provide interaction authority
const rejectedComposerOwner: ComposerOptions = composerWithoutGeneration;

declare const switcherWithoutGeneration: Omit<SessionSwitcherViewOptions, "interactionGeneration">;
// @ts-expect-error persistent SessionSwitcher owners must provide interaction authority
const rejectedSwitcherOwner: SessionSwitcherViewOptions = switcherWithoutGeneration;

declare const button: HTMLButtonElement;
declare const activate: (event: Event) => void;
declare const explain: () => void;
declare const restoreFocus: () => void;
declare const isLive: () => boolean;

// @ts-expect-error tap activation cannot silently fall back to generation zero
bindTapActivation(button, activate, restoreFocus, isLive);
// @ts-expect-error explained tap activation cannot silently fall back to generation zero
bindExplainedTapActivation(button, activate, explain, restoreFocus, isLive);
// @ts-expect-error keyboard-preserving activation cannot silently fall back to generation zero
bindKeyboardPreservingActivation(button, activate, restoreFocus, isLive);
// @ts-expect-error native-focus activation cannot silently fall back to generation zero
bindGenerationFencedClickActivation(button, activate, isLive);

void rejectedComposerOwner;
void rejectedSwitcherOwner;
