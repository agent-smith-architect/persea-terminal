import type { UnifiedViewportInsetSource, InsertTextResult, UnifiedTerminalPageOptions } from "./unified_terminal_options";
export type { UnifiedViewportInsetSource, InsertTextResult, UnifiedTerminalPageOptions } from "./unified_terminal_options";
import type { WidthRefitResult, WidthRefitAttempt, PendingWidthRefit, UnifiedGeometryAction, UnifiedGeometryAvailabilityCode, UnifiedGeometryAvailability, UnifiedGeometryAvailabilityInput, GeometryFormView } from "./unified_terminal_geometry_types";
export type { WidthRefitResult, WidthRefitAttempt, UnifiedGeometryAction, UnifiedGeometryAvailabilityCode, UnifiedGeometryAvailability, UnifiedGeometryAvailabilityInput } from "./unified_terminal_geometry_types";
import type { CopyToClipsResult, UnifiedPopoverOwner, UnifiedExplainerTopic, ScrollAnchor, FontIntent, ComposerFontIntent, InputTraceEntry } from "./unified_terminal_state_types";
import { Terminal } from "@xterm/xterm";
import type { FinalizeCause, PortSendResult } from "./attachment_port";
import type { AttachmentTransportSink, ReconnectStatus } from "./websocket_attachment_transport";
import { MAX_FIT_CELLS, MAX_FIT_ROWS, MIN_FIT_ROWS, validVerticalFit, validateServerFrame, type BrowserFrame, type Prepare, type ServerFrame } from "./attachment_protocol";
import { Composer, normalizeComposedText, type ComposerAvailability, type ComposerInjectionResult, type ComposerTypographyState } from "./composer";
import type { ComposerStagedImage } from "./composer_attachments";
import { IOSBackspaceRouter, syntheticInsertTextEvent } from "./continuous_surface/ios_backspace_router";
import type { LogicalKey } from "./continuous_surface/types";
import { bindExplainedTapActivation, bindGenerationFencedClickActivation, bindTapActivation } from "./tap_activation";
import { isStorableSnippetBody, parseOSC52, type SnippetOutcome } from "./snippet_client";
import { ClipboardPanel } from "./clipboard_panel";
import { clipboardIcon } from "./clipboard_icons";
import "./terminal_menu.css";
import "./scrollback_control.css";
import { createScrollbackControl, readScrollbackRows, readTerminalScrollbackRows, saveTerminalScrollbackRows, terminalScrollbackOverride, type ScrollbackRows } from "./scrollback_preferences";
import { COMPOSER_FONT_SIZE_MAX, COMPOSER_FONT_SIZE_MIN, DEFAULT_COMPOSER_FONT_SIZE, type OperatorPreferenceOutcome, type OperatorPreferenceSnapshot, type OperatorPreferenceSubscription } from "./operator_preferences";
import { commitFocusClaim, pointerRuleClaims, type CommitFocusContext } from "./unified_focus_claim";
import { createPreferencesStore } from "./preferences";
import { TerminalKeysPanel } from "./terminal_keys_panel";
import { actionGlyph, keyAction, keyEncodingNote, planKey, resolveAction, type KeyChord, type KeyModifiers, type TerminalActionEntry } from "./terminal_actions";
import { unifiedComposerAvailability, withCompactDensity, withStoredDensity } from "./unified_composer_adapter";
import { syntheticCtrlReleaseEvent, syntheticKeydownEvent, unifiedKeyDescriptor, type UnifiedKeyDescriptor } from "./unified_key_bar";
import { UNIFIED_RECONNECTABLE_NOTICES, UNIFIED_TAKEOVER_REASONS, automaticClaimAllowed, boundedUnifiedReason, classifyUnifiedClose, unifiedCloseNotice } from "./unified_close_policy";
import { REFUSAL_NOTICE_MS, refusalReleasesFit, unifiedRefusalNotice } from "./unified_refusal_notice";
import { UnifiedKeyboardBaseline } from "./unified_keyboard_baseline";
import { SessionSwitcherView, type SessionSwitcherInventory } from "./session_switcher";
import type { DashboardSession } from "./dashboard";
import { UNIFIED_THEME_IDS, unifiedTheme } from "./unified_themes";
import { sessionScopeIdentity } from "./session_memory";
import { CopyFeedback, type CopyFeedbackState } from "./copy_feedback";
import { frozenRangeAtColumn, serializeFrozenRange, serializeFrozenScreen, type FrozenTerminalCellStyle, type FrozenTerminalPoint, type FrozenTerminalSegment, type FrozenTerminalSnapshot } from "./frozen_terminal_snapshot";
import { bindFrozenSelectionInput } from "./frozen_selection_input";

// Reattach burst limiter: more than this many re-attachable refusals within the
// window means the session is genuinely stuck, so it degrades to a terminal
// notice rather than looping.
const REATTACH_WINDOW_MS = 60_000;
const REATTACH_BURST_LIMIT = 3;
// A view evicted for falling behind before its first MODE never caught up with
// the history backlog. That alone does not mean it never will: a burst of
// output ends, and while the output continues it brings the session's journal
// to its next rotation, which replaces a long history with a short
// reconstruction. A third such eviction in a row, however far apart the
// attempts are, means this connection cannot deliver the history faster than
// the session adds to it, so the page stops instead of streaming the backlog
// again and again.
const MAX_CATCH_UP_FAILURES = 3;
// A cross-device reopen may auto-take control this many times before it stops
// fighting, so two devices reopening each other cannot ping-pong forever.
const MAX_AUTO_TAKEOVERS = 3;
const INPUT_SATURATED_NOTICE = "Input not sent — the connection is busy";
const INPUT_CATCHING_UP_NOTICE = "Input not sent — history is still loading";
const TERMINAL_LONG_PRESS_MS = 500;
const TERMINAL_LONG_PRESS_MOVE_PX = 12;
const ANSI_THEME_KEYS = Object.freeze([
  "black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
  "brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue", "brightMagenta", "brightCyan", "brightWhite",
] as const);

const geometryUnavailable = (code: Exclude<UnifiedGeometryAvailabilityCode, "available">, message: string): UnifiedGeometryAvailability =>
  Object.freeze({ enabled: false, code, message });

/**
 * Closed availability owner for every explicit terminal-size action. Keeping
 * the reason next to the predicate means visual, pointer and keyboard paths
 * cannot disagree about why an operation is unavailable.
 */
export function unifiedGeometryAvailability(input: UnifiedGeometryAvailabilityInput): UnifiedGeometryAvailability {
  if (input.closed) return geometryUnavailable("closed", "This terminal is closed.");
  if (!input.prepared || !input.committed) return geometryUnavailable("connecting", "Terminal size is unavailable until this attachment is ready.");
  if (input.capabilityMode === "observe") return geometryUnavailable("observe_mode", "Take control before changing terminal size.");
  if (!input.controlGranted) return geometryUnavailable("no_control", "Take control before changing terminal size.");
  if (input.replaying) return geometryUnavailable("replaying", "Wait for terminal history to finish loading.");
  if (input.fitPending || input.refitPending) return geometryUnavailable("pending_size", "A terminal size change is already in progress.");
  if (!(input.committedColumns > 0) || !(input.committedRows > 0)) return geometryUnavailable("unknown_geometry", "Terminal geometry is not known yet.");
  if (input.action !== "apply" && input.keyboardGuard) return geometryUnavailable("keyboard", "Close the software keyboard before fitting to the visible area.");
  if (input.action === "fit_width" && input.bufferType === "alternate") {
    return geometryUnavailable("alternate_buffer", "Width cannot change while a full-screen program is running; rows can");
  }
  if (input.action !== "apply") {
    if (input.measurement === undefined) return geometryUnavailable("missing_measurement", "The terminal is not measurable yet.");
    const committed = input.action === "fit_width" ? input.committedColumns : input.committedRows;
    if (input.measurement === committed) {
      return geometryUnavailable("already_fit", `${input.action === "fit_width" ? "Width" : "Height"} already fits (${committed}).`);
    }
  }
  return Object.freeze({ enabled: true, code: "available", message: "" });
}



// The COMMIT focus-claim context and its monotone resolution live in
// unified_focus_claim.ts (unit-pinned); the type is re-exported for workspaces.
export type { CommitFocusContext } from "./unified_focus_claim";

// How long the transient toast stays up.
const TOAST_MS = 3_000;
const UNIFIED_EXPLAINERS: ReadonlyArray<Readonly<{ topic: UnifiedExplainerTopic; title: string; detail: string }>> = Object.freeze([
  Object.freeze({ topic: "copy", title: "Copy", detail: "Copies the selection to this device and adds it to your shared Clipboard for 30 minutes. Copying selected Persea text with the device’s Copy command also shares it; copies made in other apps need Import from this device." }),
  Object.freeze({ topic: "select", title: "Select", detail: "Freezes a selectable copy of the terminal so you can select text with your finger (a long press on the terminal does the same). Tap blank terminal space to return to typing, or tap Select again or press Escape to leave without copying." }),
  Object.freeze({ topic: "paste", title: "Paste / Copy", detail: "Opens your shared Clipboard. Choose text, review or edit its preview, then send it to the terminal. Cancel sends nothing. Persea does not append Enter, but the terminal app decides how pasted text is handled; undo after sending is not universal. Images go into the composer as attachments. While text is selected, this control becomes Copy." }),
  Object.freeze({ topic: "fit", title: "Fit rows / Fit width", detail: "Fit rows changes terminal rows to fit the visible height. Fit width rebuilds this session at the column count that fits the visible width. Both run only when you tap them. Both fix the tmux window at the new size, so other terminals attached to this session no longer resize it. To give sizing back to those terminals, run tmux set-option -wu window-size in that window." }),
  Object.freeze({ topic: "view", title: "View and size", detail: "Opens text zoom and explicit terminal-size controls." }),
  Object.freeze({ topic: "size", title: "Terminal size", detail: "Type columns and rows, then Apply. A row-only change resizes in place; a column change rebuilds this session at that width, carrying the rows you typed. Like Fit, Apply fixes the tmux window at that size for every terminal attached to this session." }),
  Object.freeze({ topic: "sessions", title: "Sessions", detail: "Choose a named session for this terminal view, or return to the Dashboard." }),
]);


// The fixed refusal table for every snippet/clip mutation. Bounded, never an
// echo of a body, a label or a server message.
const SNIPPET_REFUSALS: Readonly<Record<SnippetOutcome, string>> = Object.freeze({
  ok: "",
  conflict: "Changed on another device — list refreshed",
  full: "The shared clipboard is full",
  too_large: "Too large to save (16 KiB limit)",
  unavailable: "Shared clipboard unavailable",
  refused: "The server refused that change",
  unreachable: "Shared clipboard is unreachable",
});

// the local copy landed and only the STORE half was out of grammar.
const CLIPS_TOO_LARGE_COPY_TEXT = "Copied to this device; too large for the shared clipboard";

// copying to THIS DEVICE never depends on the shared store. When the
// store is known unavailable the local copy still happens, no snippet mutation
// is emitted, and the status says exactly which half worked.
const CLIPS_UNAVAILABLE_COPY_TEXT = "Copied to this device; shared clipboard unavailable";
// The fit hint shows when the fitted row count and the committed row count
// disagree by more than this fraction of the committed rows.
const FIT_HINT_MISMATCH = 0.3;

// What the session tag's status dot means, in words. The dot is decoration;
// this is the state an assistive technology is given.
const UNIFIED_PHASE_WORDS: Readonly<Record<"live" | "pending" | "down", string>> = Object.freeze({
  live: "attached",
  pending: "connecting",
  down: "disconnected",
});



// The size xterm is constructed with before anything measures the viewport. It
// is a seed, not a default preference: in auto mode the first fit replaces it
// within a frame, and an explicit preference replaces it outright.
const UNIFIED_SEED_FONT_SIZE = 14;

// data-font-baseline carries the tri-state directly, so a reader never has to
// infer "auto" from a number that an operator could also have chosen.
function fontBaselineAttribute(preference: number | null): string {
  return preference === null ? "auto" : String(preference);
}




const INPUT_TRACE_LIMIT = 300;
const INPUT_TRACE_TEXT_LIMIT = 160;
const INPUT_TRACE_DECODER = new TextDecoder();
// a toolbar control whose word lives in its own span, so gates
// can measure the word against the button's content box; the button keeps
// its own token and its aria-label.
function wordedButton(button: HTMLButtonElement, word: string): HTMLSpanElement {
  const label = document.createElement("span");
  label.className = "persea-unified-toolbar__word";
  label.textContent = word;
  button.replaceChildren(label);
  return label;
}

// The trace keeps the tail of a string: the recent end of a dictation field is
// the part a repeat fault shows, and a bounded tail keeps 300 entries small.
function traceText(value: string | null | undefined): string | null {
  if (value === null || value === undefined) return null;
  return value.length > INPUT_TRACE_TEXT_LIMIT ? `…${value.slice(-INPUT_TRACE_TEXT_LIMIT)}` : value;
}

function openTerminalWithStyleNonce(terminal: Terminal, host: HTMLElement, nonce: string): void {
  if (!/^[A-Za-z0-9+/]{22}$/.test(nonce)) throw new Error("UnifiedTerminalPage requires a valid per-document style nonce");
  const createElement = document.createElement;
  document.createElement = function (tagName: string, options?: ElementCreationOptions): HTMLElement {
    const element = createElement.call(document, tagName, options);
    if (tagName.toLowerCase() === "style") element.setAttribute("nonce", nonce);
    return element;
  } as typeof document.createElement;
  try {
    terminal.open(host);
  } finally {
    document.createElement = createElement;
  }
}

function rgbColor(value: number): string {
  return `#${(value & 0xffffff).toString(16).padStart(6, "0")}`;
}

function paletteColor(index: number, theme: Readonly<Record<string, string | undefined>>): string | undefined {
  if (index >= 0 && index < ANSI_THEME_KEYS.length) return theme[ANSI_THEME_KEYS[index]!];
  if (index >= 16 && index <= 231) {
    const value = index - 16;
    const levels = [0, 95, 135, 175, 215, 255] as const;
    return rgbColor((levels[Math.floor(value / 36)]! << 16) | (levels[Math.floor(value / 6) % 6]! << 8) | levels[value % 6]!);
  }
  if (index >= 232 && index <= 255) {
    const shade = 8 + (index - 232) * 10;
    return rgbColor((shade << 16) | (shade << 8) | shade);
  }
  return undefined;
}

function frozenStyleKey(style: FrozenTerminalCellStyle): string {
  return [
    style.foreground, style.background, style.bold, style.dim, style.italic,
    style.underline, style.inverse, style.strikethrough, style.invisible,
    style.blink, style.overline,
  ].join("|");
}

// Distinguishes the toolbar popover ids should two pages ever share a document.
let toolbarSerial = 0;
let geometrySerial = 0;

// UnifiedTerminalPage deliberately has one transcript authority: xterm. The
// page keeps only protocol coordinates and forwards journal bytes directly to
// the renderer; it never constructs DOM history rows or a second text model.
export class UnifiedTerminalPage implements AttachmentTransportSink {
  private readonly terminal: Terminal;
  private readonly viewport: HTMLDivElement;
  private readonly scrollRange: HTMLDivElement;
  private readonly host: HTMLDivElement;
  private readonly resizeObserver: ResizeObserver;
  private readonly terminalDisposables: Array<{ dispose(): void }> = [];
  private readonly cleanupListeners: Array<() => void> = [];
  private readonly copyFeedback = new Map<HTMLButtonElement, CopyFeedback>();
  private readonly copyAnnouncementAttempts = new Map<HTMLElement, number>();
  private cellHeightPixels = 0;
  private projectedRow = -1;
  private projectedBuffer: "normal" | "alternate" = "normal";
  private applyingProjection = false;
  private selectionPointerActive = false;
  private selectionEdgeActive = false;
  private nativeInput: "wheel" | "touch" | undefined;
  private writesInFlight = 0;
  private fitting = false;
  private replaying = false;
  private reconcileFrame: number | undefined;
  private activeBufferType: "normal" | "alternate" = "normal";
  private normalAnchor: ScrollAnchor = { bufferType: "normal", row: 0, fraction: 0, scalarRemainder: 0, following: true };
  private generation = 0;
  private prepared?: Prepare;
  private committed = false;
  private closed = false;
  private readonly geometryReadout: HTMLButtonElement;
  private readonly viewPopover: HTMLDivElement;
  private viewPopoverOpen = false;
  private popoverOwner: UnifiedPopoverOwner = "none";
  private readonly explainerPopover: HTMLElement;
  private readonly explainerHeading: HTMLElement;
  private readonly explainerBody: HTMLElement;
  private explainerReturnFocus?: HTMLElement;
  // The session tag and its details popover. The dot's state is derived
  // from this page's existing admission/reconnect state on every render; it is
  // never latched, so it cannot disagree with the surfaces beside it.
  private readonly identityTag: HTMLButtonElement;
  private readonly identityDot: HTMLSpanElement;
  private readonly identityName: HTMLSpanElement;
  private readonly identityAlias: HTMLSpanElement;
  private readonly identityDetails: HTMLElement;
  private readonly identityFacts: HTMLDListElement;
  private readonly identitySessionList: HTMLElement;
  private readonly identitySessionStatus: HTMLElement;
  private identitySessionSwitcher?: SessionSwitcherView;
  // What the popover currently lists. Rows are rebuilt only when this changes,
  // so a repeated render never replaces the DOM under a reading operator.
  private identityDetailsKey = "";
  private readonly coarsePointerQuery = window.matchMedia?.("(pointer: coarse)");
  // The iOS backspace router's eligibility mirrors the legacy interaction
  // controller: any-pointer, not primary-pointer — an iPad with a trackpad
  // attached still has the software keyboard.
  private readonly anyCoarsePointerQuery = window.matchMedia?.("(any-pointer: coarse)");
  private readonly iosBackspace: IOSBackspaceRouter;
  // the last INPUT_TRACE_LIMIT input events on this page, kept
  // only in memory until the operator copies them from Help. Deliberately
  // includes the hidden field's tail and the bytes sent, because a dictation
  // fault is exactly a disagreement between those two.
  private readonly inputTrace: InputTraceEntry[] = [];
  // Depth of our own synthetic keydown dispatches (key bar, router). The
  // custom key handler waves these through: the router must never classify
  // our own dispatches as operator keydowns.
  private syntheticKeyDepth = 0;
  // The committed geometry is the server's, never this page's guess. It moves
  // only when a committed geometry event says so.
  private committedColumns = 0;
  private committedRows = 0;
  private fitPending = false;
  private pendingRefit?: PendingWidthRefit;
  private readonly geometryForms: GeometryFormView[] = [];
  // Diagnostics only: how many input bytes the seal dropped. Nothing is kept
  // for replay, so this is a count, never a buffer.
  private sealedInputBytes = 0;

  private get refitPending(): boolean { return this.pendingRefit !== undefined; }
  // Failure/reconnect surface. The blank-page class dies here: every
  // terminal-fatal close renders a visible notice with a way out, and
  // transient losses render a visible reconnecting state.
  private readonly connectionStatus: HTMLDivElement;
  // Honest initial state over the xterm viewport. It is presentation only:
  // the attachment protocol remains the sole owner of replay readiness, and
  // the first accepted COMMIT removes this in the same receiveDecoded task.
  private readonly loadingPanel: HTMLElement;
  // The loading surface names the session it is waiting for. An in-place
  // session switch changes that name, so the label is kept addressable.
  private readonly loadingIdentityLabel: HTMLElement;
  // Operational refusals — one request's outcome on a live attachment —
  // pass through here and dismiss themselves; they are never a state.
  private readonly refusalStrip: HTMLElement;
  private refusalTimer?: ReturnType<typeof setTimeout>;
  private readonly noticePanel: HTMLElement;
  private readonly noticeHeadline: HTMLHeadingElement;
  private readonly noticeDetail: HTMLParagraphElement;
  private readonly noticeCode: HTMLParagraphElement;
  private readonly takeControlButton: HTMLButtonElement;
  private readonly reconnectButton: HTMLButtonElement;
  private takeoverPending = false;
  private firstCommitPublished = false;
  // The control grant, mirroring the legacy page: input is refused by the
  // broker until a MODE_REQUEST → MODE(CONTROL) round-trip completes, and the
  // front door turns that refusal into a socket close. sendInput, the composer,
  // and the vertical fit all gate on this, and no INPUT — a keystroke or
  // xterm's own focus-in report — is emitted before it. Reset per admission.
  private controlGranted = false;
  // Whether this admission has received its first MODE. Until then a missing
  // grant means the history backlog is still arriving, not that control was
  // taken away.
  private modeReceived = false;
  // Consecutive evictions before a first MODE; see MAX_CATCH_UP_FAILURES.
  private catchUpFailures = 0;
  // The reattach burst limiter. A broker input_refused recovers by
  // re-attaching, but a session that keeps refusing must still reach a terminal
  // notice rather than loop. Timestamps within the window are counted; the
  // window is NOT reset by a commit, so a refusal that recurs across
  // re-attachments is still bounded.
  private readonly reattachEvents: number[] = [];
  // Bounded automatic control takeover for a cross-device reopen. Reopening is
  // explicit operator intent, so a lease held by another live attachment is
  // claimed automatically — but only a few times, so two devices cannot fight
  // forever. Reset on a successful commit.
  private autoTakeoverAttempts = 0;
  // Operator intent to control this session: the page was just opened,
  // Reconnect was pressed, or the session was switched. The next COMMIT
  // consumes it; after that only a stale lease of this page's own may be
  // claimed automatically (automaticClaimAllowed).
  private claimIntent = true;
  // When this page last lost a committed control connection to a transport
  // failure, while the front door may still hold that connection's lease.
  private controlLostAt?: number;
  // The code of the failure notice on screen, if any.
  private shownNotice?: string;
  // The last prepared source binding; survives transport generations so a
  // takeover claim can be source-based once any PREPARE has been seen.
  private knownSource?: string;
  private scrollbackRows: ScrollbackRows;
  private scrollbackScope: string | undefined;
  private scrollbackSelect?: HTMLSelectElement;
  private readonly shell: HTMLElement;
  private readonly insetViewport?: UnifiedViewportInsetSource;
  private readonly keyBar: HTMLDivElement;
  private readonly keyBankToggle: HTMLButtonElement;
  private readonly keyRow: HTMLDivElement;
  private readonly keysPanel: TerminalKeysPanel;
  private readonly barKeyDisposers: (() => void)[] = [];
  private barRenderKey = "";
  private manualKeysOpen = false;
  private keysScrollAnchor?: ScrollAnchor;
  // Every disclosure/bank transition advances this token. A touch release
  // captured by a now-hidden sheet tile or replaced bank node must not gain
  // authority in the new presentation state.
  private keyInteractionGeneration = 0;
  private readonly keysPointers = new Set<number>();
  // The compact composer. Constructed for both capability modes, like the
  // legacy page: an observe page shows it with availability explaining why
  // Insert is refused. The dock is its own grid row directly above the key
  // bar, inside the shell, so the keyboard height pin keeps it above the
  // software keyboard with no positioning arithmetic. Optional because it is
  // assigned last in the constructor, after paths that already run.
  private composer?: Composer;
  private readonly composerToggles: HTMLButtonElement[] = [];
  // The shell height currently pinned for the software keyboard, in CSS
  // pixels; 0 means the stylesheet's 100dvh is in force.
  private keyboardInsetHeight = 0;
  // The visible band, published to the stylesheet as
  // --persea-visual-viewport-height. Several composer rules are written
  // against it and fall back to 100dvh; on this page nobody published it, so
  // every one of them was sizing against the WHOLE screen — which is how the
  // expanded composer grew under the keyboard. undefined means the
  // fallback is deliberately in force (a pinch-zoomed page magnifies a fixed
  // layout; it does not lose screen space).
  private visualViewportHeight: number | undefined;
  // The keyboard state machine. One boolean drives both outputs — the shell's
  // inset pin and the key bar row — in one synchronous batch, so they can
  // never disagree. It is re-derived from live viewport readings on every
  // signal, never latched off a stale event.
  private keyboardOpen = false;
  // The resting high-water baseline and its rotation/split-view reseed rules,
  // extracted so the open-across-rotation behavior is directly testable.
  private readonly keyboardBaseline = new UnifiedKeyboardBaseline();
  private keyboardVerifyTimer: number | undefined;
  private keyboardWatchdog: number | undefined;
  // "Keyboard only when asked" (coarse pointers): the one-shot restore,
  // captured when a transport generation closes (keyboard OPEN per the
  // classifier AND a page text entry focused), consumed by the next COMMIT's
  // focus claim and cleared by anything that ends the keyboard's session —
  // an explicit Hide, a dismissal the classifier sees, failure, destroy.
  private restoreKeyboardOnCommit = false;
  // Set when this page's admission came through a control-takeover claim
  // (the automatic cross-device reopen or the manual button); the COMMIT that
  // follows displaced another controller and says so once.
  private admittedViaTakeover = false;
  // The menu opens tools; input keys have one home in Keys.
  private readonly quickActionsToggle: HTMLButtonElement;
  private readonly sheet: HTMLElement;
  private sheetOpen = false;
  private readonly sheetImageTile: HTMLButtonElement;
  private readonly sheetShowKeyboardTile: HTMLButtonElement;
  private readonly sheetHideKeyboardTile: HTMLButtonElement;
  private readonly sheetStatus: HTMLElement;
  private readonly sheetClipboardTile: HTMLButtonElement;
  private readonly clipboardPanel?: ClipboardPanel;
  private clipboardRestoreTerminalFocus = false;
  private readonly sessionSwitcher?: SessionSwitcherView;
  private readonly sheetSwitchTile?: HTMLButtonElement;
  private sessionSwitcherOpen = false;
  private sessionInventoryLoaded = false;
  private sessionInventory?: SessionSwitcherInventory;
  private sessionInventoryAbort?: AbortController;
  private sessionInventoryRequest?: Promise<void>;
  private sessionSwitchPending = false;
  private readonly selectOverlay: HTMLElement;
  private readonly selectBody: HTMLElement;
  private readonly selectStatus: HTMLElement;
  private readonly selectContextButton: HTMLButtonElement;
  private readonly selectWord: HTMLSpanElement;
  private readonly pasteWord: HTMLSpanElement;
  // The Paste slot: Paste until a selection exists, then Copy for that
  // selection (nothing can be pasted into a frozen selection), then the copy
  // outcome, then Paste again. One box, four faces, no layout change.
  private readonly pasteSlotButton: HTMLButtonElement;
  private pasteCopyFailed = false;
  private pasteCopyFailedTimer?: ReturnType<typeof setTimeout>;
  private readonly selectAnnouncement: HTMLElement;
  private selectMode = false;
  private selectEpoch = 0;
  private selectCopyAttempt = 0;
  private selectCopied = false;
  private selectContextGesture = false;
  private selectSnapshot?: FrozenTerminalSnapshot;
  private selectSelectionValue = "";
  private selectRestoreFocus?: HTMLElement;
  private selectRestoreKeyboard = false;
  private selectCopiedTimer?: ReturnType<typeof setTimeout>;
  private sessionName?: string;
  private sessionAlias?: string;
  private sessionDraftScope: string | null;
  private endpointOperation = 0;
  private takeoverController?: AbortController;
  // preferences presentation authority. Every pane in a document subscribes to the
  // same service; these arrays contain the fine-pointer and coarse-pointer
  // views of that one record, never independent stores.
  private preferenceSubscription?: OperatorPreferenceSubscription;
  // The composer face. Like the theme, it is presentation this page
  // applies immediately and a record it then writes; the applied value is a
  // custom property on the shell, so the stylesheet owns every face rule and
  // this page owns only the number.
  private composerFontPreference = DEFAULT_COMPOSER_FONT_SIZE;
  private composerFontIntentID = 0;
  private composerFontIntent?: ComposerFontIntent;
  // The font preference this page currently honours: an explicit size that
  // overrides auto-fit on every load, reattach and resize, or `null` for auto,
  // where the viewport decides.
  private fontPreference: number | null = null;
  private fontIntentID = 0;
  private fontIntent?: FontIntent;
  private fontSaveTimer?: ReturnType<typeof setTimeout>;
  // Transient, non-blocking notices ("Control taken from another window").
  private readonly toast: HTMLElement;
  private toastTimer?: ReturnType<typeof setTimeout>;
  // The one-time "Tap ↕ to fit rows" hint. Per page instance only: it is
  // evaluated once per admission until it has been shown, and hidden for good
  // by the first explicit fit or a dismissal. Nothing is persisted.
  private readonly fitHint: HTMLButtonElement;
  private fitHintState: "unseen" | "shown" | "done" = "unseen";
  private fitHintPending = false;

  constructor(private readonly options: UnifiedTerminalPageOptions) {
    this.scrollbackScope = options.scrollbackScope ?? options.composerStorageScope;
    this.scrollbackRows = readTerminalScrollbackRows(this.scrollbackScope, options.historyRows ?? readScrollbackRows());
    this.sessionName = options.sessionName;
    this.sessionAlias = options.aliasLabel;
    this.sessionDraftScope = options.composerStorageScope ?? null;
    const initialPreferences = options.preferences?.snapshot();
    const initialTheme = unifiedTheme(initialPreferences?.preferences.theme ?? "default");
    this.fontPreference = initialPreferences?.preferences.fontSize ?? null;
    this.composerFontPreference = initialPreferences?.preferences.composerFontSize ?? DEFAULT_COMPOSER_FONT_SIZE;
    const shell = document.createElement("section");
    shell.className = "persea-unified-terminal";
    shell.dataset.theme = initialTheme.id;
    shell.dataset.fontBaseline = fontBaselineAttribute(this.fontPreference);
    shell.style.setProperty("--persea-composer-font-size", `${this.composerFontPreference}px`);
    shell.dataset.composerFont = String(this.composerFontPreference);
    // The toolbar is a control row, and a control row never scrolls. Its
    // controls are split by priority: the essential ones (the geometry
    // readout with its ↕ action, the ✎ composer toggle) always render in the
    // viewport; the secondary ones (zoom, fit font, copy) render
    // inline on a wide viewport and collapse under one ⋯ toggle into a
    // positioned popover on a narrow one. The stylesheet owns the breakpoint;
    // this page only owns the toggle's state.
    const toolbar = document.createElement("div");
    toolbar.className = "persea-unified-toolbar";
    // The top bar's leading slot is the session's identity, not a build
    // caption. It is a tag: a status dot whose colour is the page's OWN
    // connection/attachment state — no second state machine — and the session
    // name (with its alias when the fragment carried one) in a code face. The
    // tag is a button because the details an operator occasionally needs
    // (server, session id, size) must not cost row width: they live in a
    // positioned popover, which is not a grid row and changes no measured box.
    const identity = document.createElement("div");
    identity.className = "persea-unified-identity";
    const identityDetails = document.createElement("div");
    identityDetails.className = "persea-unified-identity__details";
    identityDetails.id = `persea-unified-identity-${(toolbarSerial += 1)}`;
    identityDetails.hidden = true;
    const identityFacts = document.createElement("dl");
    identityFacts.className = "persea-unified-identity__facts";
    const identitySessionList = document.createElement("div");
    identitySessionList.className = "persea-unified-identity__sessions";
    identitySessionList.hidden = options.sessionSwitch === undefined;
    const identitySessionStatus = document.createElement("p");
    identitySessionStatus.className = "persea-unified-identity__session-status";
    identitySessionStatus.setAttribute("role", "status");
    const currentDetails = document.createElement("details");
    currentDetails.className = "persea-unified-identity__current-details";
    const currentSummary = document.createElement("summary"); currentSummary.textContent = "Current session details";
    currentDetails.append(currentSummary, identityFacts);
    identityDetails.append(currentDetails, identitySessionStatus, identitySessionList);
    const tag = document.createElement("button");
    tag.type = "button";
    tag.className = "persea-unified-tag";
    tag.setAttribute("aria-expanded", "false");
    tag.setAttribute("aria-controls", identityDetails.id);
    const dot = document.createElement("span");
    dot.className = "persea-unified-tag__dot";
    dot.dataset.state = "pending";
    // The dot is decoration: its meaning is in the tag's accessible name, so a
    // reader is never asked to interpret a colour.
    dot.setAttribute("aria-hidden", "true");
    const tagName = document.createElement("span");
    tagName.className = "persea-unified-tag__name";
    const tagAlias = document.createElement("span");
    tagAlias.className = "persea-unified-tag__alias";
    tagAlias.hidden = true;
    tag.append(dot, tagName, tagAlias);
    identity.append(tag, identityDetails);
    this.cleanupListeners.push(bindTapActivation(
      tag,
      () => this.setIdentityDetails(Boolean(identityDetails.hidden)),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    this.identityTag = tag;
    this.identityDot = dot;
    this.identityName = tagName;
    this.identityAlias = tagAlias;
    this.identityDetails = identityDetails;
    this.identityFacts = identityFacts;
    this.identitySessionList = identitySessionList;
    this.identitySessionStatus = identitySessionStatus;
    const controls = document.createElement("div");
    controls.className = "persea-unified-toolbar__controls";
    const viewPopover = document.createElement("div");
    viewPopover.className = "persea-unified-view-popover";
    viewPopover.id = `persea-unified-view-${(toolbarSerial += 1)}`;
    viewPopover.hidden = true;
    viewPopover.setAttribute("role", "group");
    viewPopover.setAttribute("aria-label", "View and size");
    const viewZoom = document.createElement("div");
    viewZoom.className = "persea-unified-view-popover__zoom";
    const zoomOut = document.createElement("button");
    zoomOut.type = "button";
    zoomOut.className = "persea-unified-view-popover__zoom-symbol persea-unified-view-popover__zoom-out";
    zoomOut.textContent = "Zoom out";
    zoomOut.setAttribute("aria-label", "Zoom out");
    zoomOut.title = "Zoom out";
    const fit = document.createElement("button");
    fit.type = "button";
    // Presentation only. Named apart from the geometry action on purpose: two
    // controls called "Fit" next to each other, one of which changes the real
    // tmux window and one of which does not, is a trap.
    fit.textContent = "Fit font";
    fit.title = "Fit font size to the visible area";
    const zoomIn = document.createElement("button");
    zoomIn.type = "button";
    zoomIn.className = "persea-unified-view-popover__zoom-symbol persea-unified-view-popover__zoom-in";
    zoomIn.textContent = "Zoom in";
    zoomIn.setAttribute("aria-label", "Zoom in");
    zoomIn.title = "Zoom in";
    viewZoom.append(zoomOut, fit, zoomIn);
    const geometryReadout = document.createElement("button");
    geometryReadout.type = "button";
    geometryReadout.className = "persea-unified-geometry persea-unified-view-disclosure";
    geometryReadout.setAttribute("aria-live", "polite");
    geometryReadout.setAttribute("aria-controls", viewPopover.id);
    geometryReadout.setAttribute("aria-expanded", "false");
    geometryReadout.title = "View and size";
    // Select is a plain toggle (Select ⇄ Selecting) whose second tap
    // always leaves the frozen overlay, selection or not. The copy of that
    // selection lives on the Paste slot next to it: while text is selected
    // nothing can be pasted, so that box reads Copy, then the outcome, then
    // Paste again. Both boxes are fixed-width; only their language changes.
    const selectContext = document.createElement("button");
    selectContext.type = "button";
    selectContext.className = "persea-unified-copy persea-unified-select-context";
    // the word lives in its own span so gates can measure it
    // against the button's content box; the button keeps its own token —
    // font, height, padding — and its accessible name (aria-label).
    this.selectWord = wordedButton(selectContext, "Select");
    selectContext.title = "Select terminal text";
    selectContext.setAttribute("aria-label", "Select terminal text");
    selectContext.setAttribute("aria-pressed", "false");
    selectContext.dataset.selectState = "select";
    const paste = document.createElement("button");
    paste.type = "button";
    paste.className = "persea-unified-toolbar-paste";
    this.pasteWord = wordedButton(paste, "");
    this.pasteWord.append(clipboardIcon("clipboard"));
    paste.title = "Open clipboard";
    paste.setAttribute("aria-label", "Open clipboard");
    paste.setAttribute("aria-haspopup", "dialog");
    paste.dataset.pasteState = "paste";
    // A tap on either box collapses the document selection before the action
    // runs; the gesture flag keeps the frozen selection value alive across it.
    const beginSelectContextGesture = () => { this.selectContextGesture = true; };
    const endSelectContextGesture = () => { setTimeout(() => { this.selectContextGesture = false; }, 0); };
    for (const button of [selectContext, paste]) {
      button.addEventListener("pointerdown", beginSelectContextGesture, true);
      for (const type of ["pointerup", "pointercancel", "lostpointercapture"] as const) {
        button.addEventListener(type, endSelectContextGesture, true);
      }
      this.cleanupListeners.push(
        () => button.removeEventListener("pointerdown", beginSelectContextGesture, true),
        ...(["pointerup", "pointercancel", "lostpointercapture"] as const).map((type) =>
          () => button.removeEventListener(type, endSelectContextGesture, true)),
      );
    }
    this.selectContextButton = selectContext;
    this.pasteSlotButton = paste;
    this.cleanupListeners.push(bindExplainedTapActivation(
      paste,
      (event) => this.activatePasteSlot(event),
      () => this.openExplainer("paste", paste),
      () => undefined,
      () => !this.closed && !paste.disabled,
      () => this.keyInteractionGeneration,
    ));
    // The copy OUTCOME still has to reach a screen reader, and it is the one
    // thing the button's own state cannot say. It lives in a visually hidden
    // live region: announced, never painted, and outside the row's layout, so
    // it cannot move a box.
    const copyStatus = document.createElement("span");
    copyStatus.className = "persea-unified-copy-status";
    copyStatus.setAttribute("role", "status");
    copyStatus.setAttribute("aria-live", "polite");
    this.selectAnnouncement = copyStatus;
    // The ONE quick-actions opener, on every pointer. It used to be a
    // floating puck on a coarse pointer and this button on a fine one, so a
    // phone paid terminal area for a control a laptop kept in the row — and
    // the two had to be swapped whenever the pointer medium changed. One
    // control in one place is the same surface with none of that.
    //
    // It carries the puck's keyboard-preserving activation, which it now needs:
    // on a phone this is the control an operator reaches with the keyboard up,
    // and a button that takes focus on tap would drop it. The pointerdown is
    // cancelled (focus never moves), the action fires on the tap's pointerup,
    // and a keyboard-driven activation still arrives as a detail-0 click.
    const quickActions = document.createElement("button");
    quickActions.type = "button";
    quickActions.className = "persea-unified-quick-actions";
    quickActions.textContent = "\u2630";
    quickActions.title = "Quick actions";
    quickActions.setAttribute("aria-label", "Quick actions");
    quickActions.setAttribute("aria-expanded", "false");
    this.cleanupListeners.push(bindTapActivation(
      quickActions,
      () => this.setSheet(!this.sheetOpen),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    this.quickActionsToggle = quickActions;
    // The composer's fine-pointer affordance. The key bar only exists on
    // coarse pointers, so the always-present toolbar carries the same toggle
    // for desktop; on a phone it works too, at the cost of one keyboard
    // round-trip (the composer textarea's focus brings it back).
    const composerToggle = document.createElement("button");
    composerToggle.type = "button";
    composerToggle.className = "persea-unified-composer-toggle";
    composerToggle.textContent = "✎";
    composerToggle.title = "Open composer";
    composerToggle.setAttribute("aria-label", "Open composer");
    composerToggle.setAttribute("aria-expanded", "false");
    this.cleanupListeners.push(bindTapActivation(
      composerToggle,
      (event) => this.toggleComposer(event),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    this.composerToggles.push(composerToggle);
    // The retired one-time hint remains as state only so old lifecycle paths
    // can settle it. It is deliberately not rendered in the compact row.
    const fitHint = document.createElement("button");
    fitHint.type = "button";
    fitHint.className = "persea-unified-fit-hint";
    fitHint.textContent = "Tap \u2195 to fit rows";
    fitHint.title = "Dismiss";
    fitHint.setAttribute("aria-label", "Tap \u2195 to fit rows (dismiss this hint)");
    fitHint.hidden = true;
    this.cleanupListeners.push(bindGenerationFencedClickActivation(
      fitHint,
      () => this.dismissFitHint(),
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    const viewSize = this.createGeometryForm("view");
    // Theme remains on the dashboard, and the composer face now lives in the
    // composer header. This popover keeps what is about THIS terminal's view —
    // zoom and size.
    viewPopover.append(viewZoom, viewSize);
    controls.append(selectContext, paste, geometryReadout, quickActions, composerToggle);
    const explainer = document.createElement("div");
    explainer.className = "persea-unified-explainer";
    explainer.hidden = true;
    explainer.setAttribute("role", "dialog");
    explainer.setAttribute("aria-modal", "false");
    const explainerHeading = document.createElement("h3");
    explainerHeading.className = "persea-unified-explainer__heading";
    const explainerBody = document.createElement("div");
    explainerBody.className = "persea-unified-explainer__body";
    explainer.append(explainerHeading, explainerBody);
    toolbar.append(identity, controls, viewPopover, explainer);
    this.explainerPopover = explainer;
    this.explainerHeading = explainerHeading;
    this.explainerBody = explainerBody;
    this.viewPopover = viewPopover;
    // The popover closes on a pointer landing outside the toolbar or on
    // Escape; it stays open across its own controls so zoom can be repeated.
    const onToolbarOutsidePointer = (event: PointerEvent) => {
      if (toolbar.contains(event.target as Node | null)) return;
      if (this.viewPopoverOpen) this.setViewPopover(false);
      if (!this.identityDetails.hidden) this.setIdentityDetails(false);
      if (!this.explainerPopover.hidden) this.closeExplainer();
    };
    const onToolbarKeydown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      if (!this.explainerPopover.hidden) {
        event.preventDefault();
        event.stopPropagation();
        const restore = this.explainerReturnFocus;
        this.closeExplainer();
        if (!this.selectMode && restore?.isConnected) restore.focus({ preventScroll: true });
        return;
      }
      if (this.viewPopoverOpen) {
        event.preventDefault();
        event.stopPropagation();
        const restoreDisclosureFocus = document.activeElement instanceof HTMLElement
          && this.viewPopover.contains(document.activeElement);
        this.setViewPopover(false);
        // A terminal-focused operator may open View without giving it focus.
        // Preserve that xterm/software-keyboard posture on Escape; only return
        // focus to the disclosure when Escape actually came from the popover.
        if (restoreDisclosureFocus) geometryReadout.focus({ preventScroll: true });
        return;
      }
      if (!this.identityDetails.hidden) {
        event.preventDefault();
        event.stopPropagation();
        this.setIdentityDetails(false);
        tag.focus({ preventScroll: true });
        return;
      }
    };
    document.addEventListener("pointerdown", onToolbarOutsidePointer, true);
    document.addEventListener("keydown", onToolbarKeydown, true);
    this.cleanupListeners.push(
      () => document.removeEventListener("pointerdown", onToolbarOutsidePointer, true),
      () => document.removeEventListener("keydown", onToolbarKeydown, true),
    );
    const viewport = document.createElement("div");
    viewport.className = "persea-unified-scroll";
    const scrollRange = document.createElement("div");
    scrollRange.className = "persea-unified-scroll-range";
    const host = document.createElement("div");
    host.className = "persea-unified-xterm";
    host.tabIndex = 0;
    host.setAttribute("role", "application");
    host.setAttribute("aria-label", "Unified terminal input");
    scrollRange.append(host);
    viewport.append(scrollRange);
    const loading = document.createElement("section");
    loading.className = "persea-unified-loading";
    loading.dataset.state = "replaying";
    loading.setAttribute("role", "status");
    loading.setAttribute("aria-live", "polite");
    loading.setAttribute("aria-atomic", "true");
    const loadingIdentity = document.createElement("span");
    loadingIdentity.className = "persea-unified-loading__identity";
    loadingIdentity.textContent = options.sessionName || "Unified terminal";
    const loadingDetail = document.createElement("span");
    loadingDetail.className = "persea-unified-loading__detail";
    loadingDetail.textContent = "Replaying…";
    loading.append(loadingIdentity, loadingDetail);
    this.loadingPanel = loading;
    this.loadingIdentityLabel = loadingIdentity;

    // The mobile key bar. A phone keyboard has no arrows, Esc, Tab or Ctrl, so
    // coarse-pointer devices get an accessory key row docked under the
    // terminal, shown exactly while the software keyboard is up (the keyboard
    // state machine below drives it together with the inset pin). Toggling
    // the bar changes only the viewport's height, and the ResizeObserver's
    // coarse-pointer height-only gate keeps that out of fitFont, so the
    // toggle cannot flap the font — the flicker that once kept this bar
    // always-visible. A fine pointer never sees the bar.
    const keyBar = document.createElement("div");
    keyBar.className = "persea-unified-keybar";
    keyBar.setAttribute("role", "toolbar");
    keyBar.setAttribute("aria-label", "Terminal keys");
    const keyRow = document.createElement("div");
    keyRow.className = "persea-unified-keybar-row";
    const keyButton = (label: string, title: string, activate: (event: Event) => void): HTMLButtonElement => {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "persea-unified-keybar-key";
      button.textContent = label;
      button.title = title;
      button.setAttribute("aria-label", title);
      this.cleanupListeners.push(bindTapActivation(button, activate, () => {}, () => !this.closed, () => this.keyInteractionGeneration));
      return button;
    };
    this.keyRow = keyRow;
    const keyBankToggle = keyButton("Keys", "Terminal Keys", () => {
      const focused = document.activeElement === this.keyBankToggle;
      this.keysPanel.toggle();
      if (focused && this.keyBar.hidden) this.quickActionsToggle.focus({ preventScroll: true });
    });
    keyBankToggle.classList.add("persea-unified-keybar-key--bank");
    keyBankToggle.setAttribute("aria-expanded", "false");
    this.keyBankToggle = keyBankToggle;
    keyBar.append(keyRow, keyBankToggle);
    keyBar.hidden = true;
    this.keyBar = keyBar;
    this.keysPanel = new TerminalKeysPanel({
      generation: () => this.keyInteractionGeneration,
      isLive: () => !this.closed && !document.hidden,
      dispatch: (entry, event) => { const result = this.dispatchTerminalAction(entry, event); this.showToast(result); return result; },
      beforeChange: () => { this.keysScrollAnchor ??= this.captureAnchor(); },
      changed: () => {
        this.keyInteractionGeneration++;
        this.keyBankToggle.setAttribute("aria-expanded", String(this.keysPanel.open));
        this.keyBankToggle.setAttribute("aria-pressed", String(this.keysPanel.open));
        if (!this.keysPanel.open && this.manualKeysOpen) {
          this.manualKeysOpen = false;
          this.setKeyBarVisible(this.keyboardOpen);
        }
        this.renderCtrlLatch();
        this.composer?.remeasureInset();
        this.measureKeysPanel();
        if (this.keysScrollAnchor) {
          const anchor = this.keysScrollAnchor;
          this.keysScrollAnchor = undefined;
          this.syncNativeScroll(anchor.following, anchor);
        }
        this.scheduleReconcile();
      },
      preferencesChanged: () => this.renderKeyBar(),
      keyboardOpen: () => this.keyboardOpen,
      settingsReturnFocus: () => this.quickActionsToggle,
      hideKeyboard: () => {
        if (this.pageTextEntryFocused()) (document.activeElement as HTMLElement)?.blur();
      },
    });
    this.keysPanel.element.id = `persea-terminal-keys-${toolbarSerial}`;
    keyBankToggle.setAttribute("aria-controls", this.keysPanel.element.id);
    this.renderKeyBar();
    let nativeKeysDismissTimer: number | undefined;
    const onNativeEdit = (event: Event): void => {
      const target = event.target;
      if (this.syntheticKeyDepth > 0 || this.keysPanel.element.contains(target as Node)) return;
      if (!(target instanceof HTMLTextAreaElement || target instanceof HTMLInputElement || (target instanceof HTMLElement && target.isContentEditable))) return;
      if (!this.keysPanel.open && !this.keysPanel.hasModifiers) return;
      const character = event.isTrusted && event.cancelable && !event.defaultPrevented && target === this.terminal.textarea
        ? event instanceof KeyboardEvent && event.type === "keydown" && !event.isComposing && !event.metaKey && !event.ctrlKey && !event.altKey
          ? event.key
          : event instanceof InputEvent && event.type === "beforeinput" && event.inputType === "insertText" && !event.isComposing
            ? event.data ?? "" : ""
        : "";
      const nativeChord = this.keysPanel.takeNativeCharacter(character);
      if (event.type === "keydown" && !nativeChord) return;
      // Consume only an explicit single-letter terminal chord. Other editing
      // (including composition and paste) disarms it without touching the input.
      // Defer disclosure geometry changes until the native transaction finishes.
      const generation = ++this.keyInteractionGeneration;
      this.keysPanel.disarmForNativeInput();
      this.renderCtrlLatch();
      if (nativeChord) {
        event.preventDefault(); event.stopImmediatePropagation();
        const result = this.dispatchTerminalAction(nativeChord, event);
        if (!result.includes(" sent")) this.showToast(result);
      }
      if (nativeKeysDismissTimer !== undefined) clearTimeout(nativeKeysDismissTimer);
      nativeKeysDismissTimer = window.setTimeout(() => {
        nativeKeysDismissTimer = undefined;
        if (!this.closed && this.keyInteractionGeneration === generation) this.keysPanel.cancel();
      }, 0);
    };
    const onTerminalPointer = (): void => this.keysPanel.cancel();
    const onKeysBackground = (): void => { if (document.hidden) this.resetKeyInteractionAuthorityForLifecycle(); };
    for (const name of ["keydown", "beforeinput", "input", "compositionstart", "paste"]) {
      shell.addEventListener(name, onNativeEdit, true);
      this.cleanupListeners.push(() => shell.removeEventListener(name, onNativeEdit, true));
    }
    this.cleanupListeners.push(() => { if (nativeKeysDismissTimer !== undefined) clearTimeout(nativeKeysDismissTimer); });
    host.addEventListener("pointerdown", onTerminalPointer, true);
    document.addEventListener("visibilitychange", onKeysBackground);
    this.cleanupListeners.push(() => host.removeEventListener("pointerdown", onTerminalPointer, true), () => document.removeEventListener("visibilitychange", onKeysBackground), () => this.keysPanel.dispose());


    // Transient losses render here (aria-live so a reader hears the state
    // change); CSS hides the strip while it is empty.
    const connectionStatus = document.createElement("div");
    connectionStatus.className = "persea-unified-connection";
    connectionStatus.setAttribute("role", "status");
    connectionStatus.setAttribute("aria-live", "polite");
    // Terminal-fatal closes render here: headline, sentence, the raw code for
    // diagnosability, and a way out (dashboard; take-control when claimable).
    // Operational refusals render here and leave by themselves. The strip
    // is positioned out of the grid flow on purpose: it must never change the
    // viewport's height, because the Fit measures that height.
    const refusal = document.createElement("div");
    refusal.className = "persea-unified-refusal";
    refusal.setAttribute("role", "status");
    refusal.setAttribute("aria-live", "polite");
    refusal.hidden = true;
    const notice = document.createElement("section");
    notice.className = "persea-unified-notice";
    notice.hidden = true;
    const noticeHeadline = document.createElement("h2");
    noticeHeadline.className = "persea-unified-notice__headline";
    const noticeDetail = document.createElement("p");
    noticeDetail.className = "persea-unified-notice__detail";
    const noticeCode = document.createElement("p");
    noticeCode.className = "persea-unified-notice__code";
    const noticeActions = document.createElement("div");
    noticeActions.className = "persea-unified-notice__actions";
    const takeControlButton = document.createElement("button");
    takeControlButton.type = "button";
    takeControlButton.className = "persea-unified-notice__take-control";
    takeControlButton.textContent = "Take control here";
    takeControlButton.hidden = true;
    this.cleanupListeners.push(bindGenerationFencedClickActivation(
      takeControlButton,
      (event) => this.onTakeControlClick(event),
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    const reconnectButton = document.createElement("button");
    reconnectButton.type = "button";
    reconnectButton.className = "persea-unified-notice__reconnect";
    reconnectButton.textContent = "Reconnect";
    reconnectButton.hidden = true;
    this.cleanupListeners.push(bindGenerationFencedClickActivation(
      reconnectButton,
      (event) => {
        if (!event.isTrusted || reconnectButton.hidden || reconnectButton.disabled || this.noticePanel.hidden) return;
        reconnectButton.disabled = true;
        this.hideFailureNotice();
        this.claimIntent = true;
        this.catchUpFailures = 0;
        // A trusted retry starts the existing finite recovery cycle. It never
        // reuses a capability or replays input from the disconnected socket.
        this.options.port.attachAgain?.();
      },
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    const noticeDashboard = document.createElement("a");
    noticeDashboard.className = "persea-unified-notice__dashboard";
    noticeDashboard.textContent = "Dashboard";
    noticeDashboard.href = "/";
    noticeActions.append(takeControlButton, reconnectButton, noticeDashboard);
    notice.append(noticeHeadline, noticeDetail, noticeCode, noticeActions);
    this.connectionStatus = connectionStatus;
    this.refusalStrip = refusal;
    this.noticePanel = notice;
    this.noticeHeadline = noticeHeadline;
    this.noticeDetail = noticeDetail;
    this.noticeCode = noticeCode;
    this.takeControlButton = takeControlButton;
    this.reconnectButton = reconnectButton;

    // The composer dock: an in-flow grid row directly above the key bar. Its
    // only visible content is the composer panel, so while the panel is
    // hidden the row contributes nothing and showing or hiding the composer
    // is a height-only change to the viewport — which the ResizeObserver's
    // coarse-pointer height-only gate keeps out of fitFont, exactly like the
    // key bar row.
    const composerDock = document.createElement("div");
    composerDock.className = "persea-unified-composer-dock";
    composerDock.dataset.composer = "closed";

    // The stage is the viewport's grid track. The sheet is
    // absolutely positioned inside it, so "docked above the key bar (and the
    // composer dock)" holds by composition: the stage's bottom edge IS the top
    // of whichever bottom rows are showing, and no offset is ever published
    // or measured. Neither is a grid row, so opening the sheet changes no box
    // the ResizeObserver watches and can never reach fitFont.
    const stage = document.createElement("div");
    stage.className = "persea-unified-stage";
    const quick = this.buildQuickActions(quickActions);
    const selectOverlay = document.createElement("section");
    selectOverlay.className = "persea-unified-select";
    selectOverlay.hidden = true;
    selectOverlay.setAttribute("aria-label", "Frozen terminal selection");
    const selectStatus = document.createElement("span");
    selectStatus.className = "persea-unified-select__status";
    selectStatus.textContent = "";
    selectStatus.setAttribute("role", "status");
    selectStatus.setAttribute("aria-live", "polite");
    const selectBody = document.createElement("div");
    selectBody.className = "persea-unified-select__body";
    selectOverlay.append(selectStatus, selectBody);
    stage.append(viewport, loading, quick.dock, selectOverlay);
    this.selectOverlay = selectOverlay;
    this.selectBody = selectBody;
    this.selectStatus = selectStatus;
    this.cleanupListeners.push(bindFrozenSelectionInput(selectBody,
      () => !this.closed && this.selectMode ? this.selectEpoch : undefined,
      () => this.exitSelectMode(true),
    ));
    const onFrozenSelectionChange = () => this.refreshFrozenSelection();
    document.addEventListener("selectionchange", onFrozenSelectionChange);
    const onFrozenSelectionEscape = (event: KeyboardEvent) => {
      if (!event.isTrusted || !this.selectMode || event.key !== "Escape") return;
      if (!this.explainerPopover.hidden) return;
      event.preventDefault();
      event.stopPropagation();
      this.exitSelectMode();
    };
    document.addEventListener("keydown", onFrozenSelectionEscape, true);
    this.cleanupListeners.push(
      () => document.removeEventListener("selectionchange", onFrozenSelectionChange),
      () => document.removeEventListener("keydown", onFrozenSelectionEscape, true),
    );
    this.sheet = quick.sheet;
    this.sheetImageTile = quick.imageTile;
    this.sheetShowKeyboardTile = quick.showKeyboardTile;
    this.sheetHideKeyboardTile = quick.hideKeyboardTile;
    this.sheetStatus = quick.status;
    this.sheetClipboardTile = quick.clipboardTile;
    this.sessionSwitcher = quick.sessionSwitcher;
    this.sheetSwitchTile = quick.switchTile;
    if (options.sessionSwitch) {
      const identityActions = document.createElement("div");
      identityActions.className = "persea-unified-identity__session-actions";
      const dashboard = document.createElement("button");
      dashboard.type = "button";
      dashboard.textContent = "Dashboard";
      dashboard.title = "Open the dashboard";
      dashboard.setAttribute("aria-label", "Open the dashboard");
      this.cleanupListeners.push(bindTapActivation(dashboard, (event) => {
        if (event.isTrusted) window.location.assign("/");
      }, () => undefined, () => !this.closed, () => this.keyInteractionGeneration));
      const list = document.createElement("div");
      identityActions.append(dashboard);
      this.identitySessionList.append(identityActions, list);
      const switcher = new SessionSwitcherView({
        root: list,
        currentDraftScope: () => this.options.sessionSwitch?.currentDraftScope() ?? null,
        blockedMessage: (session) => this.options.sessionSwitch?.blockedMessage(session) ?? "Unified terminal unavailable",
        select: (session) => { void this.selectSession(session, "tag"); },
        interactionGeneration: () => this.keyInteractionGeneration,
      });
      this.cleanupListeners.push(bindTapActivation(
        switcher.refresh,
        () => { void this.loadSessionInventory(true); },
        () => undefined,
        () => !this.closed,
        () => this.keyInteractionGeneration,
      ));
      this.identitySessionSwitcher = switcher;
    }
    this.fitHint = fitHint;
    // Transient notices, positioned out of the grid flow like the refusal
    // strip so they never change the viewport's height.
    const toast = document.createElement("div");
    toast.className = "persea-unified-toast";
    toast.setAttribute("role", "status");
    toast.setAttribute("aria-live", "polite");
    toast.hidden = true;
    this.toast = toast;

    shell.append(toolbar, connectionStatus, notice, stage, composerDock, this.keysPanel.element, keyBar, refusal, toast, copyStatus);
    const keysPointerDown = (event: PointerEvent): void => { this.keysPointers.add(event.pointerId); };
    const keysPointerEnd = (event: PointerEvent): void => {
      // Wait until the activation handler has completed before moving controls.
      queueMicrotask(() => { this.keysPointers.delete(event.pointerId); if (!this.closed) this.measureKeysPanel(); });
    };
    shell.addEventListener("pointerdown", keysPointerDown, true);
    window.addEventListener("pointerup", keysPointerEnd, true);
    window.addEventListener("pointercancel", keysPointerEnd, true);
    this.cleanupListeners.push(() => shell.removeEventListener("pointerdown", keysPointerDown, true), () => window.removeEventListener("pointerup", keysPointerEnd, true), () => window.removeEventListener("pointercancel", keysPointerEnd, true));
    let keysResizeFrame: number | undefined;
    const keysResize = new ResizeObserver(() => {
      if (keysResizeFrame !== undefined) return;
      // The composer and key strip share a height budget. Settle those writes
      // together outside ResizeObserver delivery, after the strip mode is known.
      keysResizeFrame = requestAnimationFrame(() => {
        keysResizeFrame = undefined;
        if (!this.closed) { this.measureKeysPanel(); this.composer?.remeasureInset(); }
      });
    });
    keysResize.observe(shell);
    keysResize.observe(composerDock);
    keysResize.observe(toolbar);
    keysResize.observe(keyBar);
    this.cleanupListeners.push(() => { keysResize.disconnect(); if (keysResizeFrame !== undefined) cancelAnimationFrame(keysResizeFrame); }, () => this.barKeyDisposers.splice(0).forEach((dispose) => dispose()));
    options.root.replaceChildren(shell);
    this.shell = shell;
    this.insetViewport = options.viewportInset ?? (typeof window === "undefined" ? undefined : window.visualViewport ?? undefined);

    this.terminal = new Terminal({
      allowProposedApi: false,
      cols: 80,
      rows: 24,
      scrollback: this.scrollbackRows,
      convertEol: false,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
      fontSize: this.fontPreference ?? UNIFIED_SEED_FONT_SIZE,
      theme: initialTheme.xterm,
    });
    openTerminalWithStyleNonce(this.terminal, host, options.styleNonce);
    // OSC 52 clipboard capture: exactly ONE handler per xterm instance, disposed
    // with that instance — no addon, no package dependency, and no global
    // parser state that could cross panes. Every sequence is consumed with
    // `true`, so a READ or a malformed payload produces no reply and no
    // browser-to-pane byte; only a valid `c`/`p` write reaches the
    // device-local coalescer.
    this.terminalDisposables.push(this.terminal.parser.registerOscHandler(52, (data) => {
      if (!this.closed) {
        const verdict = parseOSC52(data);
        if (verdict.kind === "write") this.options.snippets?.publishOSC(verdict.body);
      }
      return true;
    }));
    host.addEventListener("focus", () => { if (!this.selectMode) this.terminal.focus(); });
    this.viewport = viewport;
    this.scrollRange = scrollRange;
    this.host = host;
    this.geometryReadout = geometryReadout;
    this.activeBufferType = this.terminal.buffer.active.type;
    this.projectedBuffer = this.activeBufferType;
    this.terminal.attachCustomWheelEventHandler(() => false);
    const suppressXtermWheel = (event: WheelEvent) => event.stopPropagation();
    host.addEventListener("wheel", suppressXtermWheel, { passive: true, capture: true });
    this.cleanupListeners.push(() => host.removeEventListener("wheel", suppressXtermWheel, true));

    // iOS hold-backspace. The software keyboard's native accelerating delete
    // (characters, then whole words) only engages while the keyboard is
    // deleting real text from a real editable field, so the legacy router
    // seeds xterm's own hidden textarea with a zero-width sentinel and
    // translates the resulting deleteContentBackward / deleteWordBackward
    // input events into logical Backspace / Ctrl+W dispatches. The sentinel is
    // invisible to xterm's data path: its input listener forwards only
    // insertText data and never reads textarea.value, the intercepted
    // Backspace keydown bypasses its keyboard evaluator through the custom key
    // handler below, and the capture-phase compositionstart listener removes
    // the sentinel before xterm's own listener records composition positions.
    this.iosBackspace = new IOSBackspaceRouter({
      liveHost: host,
      isFeatureEligible: () => !this.closed
        && this.options.capabilityMode === "control"
        && this.anyCoarsePointerQuery?.matches === true
        && this.terminal.options.screenReaderMode !== true,
      sendLogicalKey: (key) => {
        const descriptor = unifiedKeyDescriptor(key);
        return descriptor !== undefined && this.dispatchSyntheticKey(descriptor);
      },
      dispatchSyntheticInsert: (data) => this.dispatchSyntheticInsertText(data),
    });
    this.terminal.attachCustomKeyEventHandler((event) => {
      if (this.syntheticKeyDepth > 0) return true;
      const disposition = this.iosBackspace.handleCustomKey(event);
      if (event.type === "keydown") {
        this.traceInput("keydown", { key: event.key, code: event.code, keyCode: event.keyCode, composing: event.isComposing, repeat: event.repeat, disposition });
      }
      return disposition !== "intercept-backspace" && disposition !== "intercept-dictation-prelude";
    });
    const onRouterBeforeInput = (event: Event) => {
      const input = event as InputEvent;
      // Before the browser applies the edit: the field still holds the prior text.
      this.traceInput("beforeinput", { inputType: input.inputType, data: traceText(input.data), composing: input.isComposing });
      this.iosBackspace.onBeforeInput(input);
    };
    // Capture phase runs before xterm's own textarea listener, so the router
    // can stop a raw dictation replacement from being forwarded verbatim.
    const onRouterInputCapture = (event: Event) => {
      const input = event as InputEvent;
      this.traceInput("input", { inputType: input.inputType, data: traceText(input.data), composing: input.isComposing });
      this.iosBackspace.onInputCapture(input);
    };
    const onRouterInput = (event: Event) => this.iosBackspace.onInput(event as InputEvent);
    const onRouterCompositionStart = (event: Event) => {
      this.traceInput("compositionstart", { data: traceText((event as CompositionEvent).data) });
      this.iosBackspace.onCompositionStart(event as CompositionEvent);
    };
    const onRouterCompositionEnd = (event: Event) => {
      this.traceInput("compositionend", { data: traceText((event as CompositionEvent).data) });
      this.iosBackspace.onCompositionEnd(event as CompositionEvent);
    };
    const onRouterFocusIn = (event: Event) => this.iosBackspace.onFocusIn(event as FocusEvent);
    const onRouterBlur = (event: Event) => this.iosBackspace.onBlur(event as FocusEvent);
    const onRouterSelectionChange = () => this.iosBackspace.onSelectionChange();
    const onRouterPolicyChange = () => this.iosBackspace.onPolicyChange();
    // xterm's paste handler clears textarea.value synchronously; reconcile
    // after it has run (same-phase listener registered after xterm's own).
    const onRouterPaste = () => this.iosBackspace.onXtermOperationComplete();
    host.addEventListener("beforeinput", onRouterBeforeInput, true);
    host.addEventListener("input", onRouterInputCapture, true);
    host.addEventListener("input", onRouterInput);
    host.addEventListener("compositionstart", onRouterCompositionStart, true);
    host.addEventListener("compositionend", onRouterCompositionEnd);
    host.addEventListener("focusin", onRouterFocusIn, true);
    host.addEventListener("blur", onRouterBlur, true);
    document.addEventListener("selectionchange", onRouterSelectionChange);
    this.anyCoarsePointerQuery?.addEventListener("change", onRouterPolicyChange);
    const routerPasteTarget = this.terminal.textarea;
    routerPasteTarget?.addEventListener("paste", onRouterPaste);
    this.cleanupListeners.push(
      () => host.removeEventListener("beforeinput", onRouterBeforeInput, true),
      () => host.removeEventListener("input", onRouterInputCapture, true),
      () => host.removeEventListener("input", onRouterInput),
      () => host.removeEventListener("compositionstart", onRouterCompositionStart, true),
      () => host.removeEventListener("compositionend", onRouterCompositionEnd),
      () => host.removeEventListener("focusin", onRouterFocusIn, true),
      () => host.removeEventListener("blur", onRouterBlur, true),
      () => document.removeEventListener("selectionchange", onRouterSelectionChange),
      () => this.anyCoarsePointerQuery?.removeEventListener("change", onRouterPolicyChange),
      () => routerPasteTarget?.removeEventListener("paste", onRouterPaste),
    );
    // Enter and Ctrl+C clear xterm's textarea synchronously; reseed after.
    this.terminalDisposables.push(this.terminal.onKey(({ key, domEvent }) => this.iosBackspace.onXtermKeyHandled(domEvent, key)));

    const onViewportScroll = () => this.applyCanonicalScroll();
    viewport.addEventListener("scroll", onViewportScroll, { passive: true });
    this.cleanupListeners.push(() => viewport.removeEventListener("scroll", onViewportScroll));

    const markNativeInput = (kind: "wheel" | "touch") => {
      this.nativeInput = kind;
      setTimeout(() => {
        if (this.nativeInput === kind) this.nativeInput = undefined;
      }, 0);
    };
    const onWheel = () => markNativeInput("wheel");
    const onTouch = () => markNativeInput("touch");
    viewport.addEventListener("wheel", onWheel, { passive: true, capture: true });
    viewport.addEventListener("touchstart", onTouch, { passive: true, capture: true });
    viewport.addEventListener("touchmove", onTouch, { passive: true, capture: true });
    viewport.addEventListener("touchend", onTouch, { passive: true, capture: true });
    this.cleanupListeners.push(
      () => viewport.removeEventListener("wheel", onWheel, true),
      () => viewport.removeEventListener("touchstart", onTouch, true),
      () => viewport.removeEventListener("touchmove", onTouch, true),
      () => viewport.removeEventListener("touchend", onTouch, true),
    );

    let terminalLongPressTimer: number | undefined;
    let terminalLongPressOrigin: Readonly<{ x: number; y: number }> | undefined;
    const cancelTerminalLongPress = () => {
      if (terminalLongPressTimer !== undefined) clearTimeout(terminalLongPressTimer);
      terminalLongPressTimer = undefined;
      terminalLongPressOrigin = undefined;
    };
    const onPointerDown = (event: PointerEvent) => {
      if (event.button !== 0) return;
      this.selectionPointerActive = true;
      this.selectionEdgeActive = false;
      if (this.coarsePointerQuery?.matches !== true || !event.isPrimary || this.selectMode) return;
      cancelTerminalLongPress();
      terminalLongPressOrigin = Object.freeze({ x: event.clientX, y: event.clientY });
      terminalLongPressTimer = window.setTimeout(() => {
        terminalLongPressTimer = undefined;
        const origin = terminalLongPressOrigin;
        terminalLongPressOrigin = undefined;
        if (!origin || this.closed || this.selectMode) return;
        const point = this.frozenCellAtClientPoint(origin.x, origin.y);
        if (point) this.enterSelectMode(point);
      }, TERMINAL_LONG_PRESS_MS);
    };
    const onPointerMove = (event: PointerEvent) => {
      if (!this.selectionPointerActive || (event.buttons & 1) === 0) return;
      if (terminalLongPressOrigin
        && Math.hypot(event.clientX - terminalLongPressOrigin.x, event.clientY - terminalLongPressOrigin.y) > TERMINAL_LONG_PRESS_MOVE_PX) {
        cancelTerminalLongPress();
      }
      const bounds = viewport.getBoundingClientRect();
      this.selectionEdgeActive = event.clientY <= bounds.top + 1 || event.clientY >= bounds.bottom - 1;
    };
    const endPointer = () => {
      cancelTerminalLongPress();
      this.selectionPointerActive = false;
      this.selectionEdgeActive = false;
    };
    host.addEventListener("pointerdown", onPointerDown);
    window.addEventListener("pointermove", onPointerMove, true);
    window.addEventListener("pointerup", endPointer, true);
    window.addEventListener("pointercancel", endPointer, true);
    this.cleanupListeners.push(
      () => host.removeEventListener("pointerdown", onPointerDown),
      () => window.removeEventListener("pointermove", onPointerMove, true),
      () => window.removeEventListener("pointerup", endPointer, true),
      () => window.removeEventListener("pointercancel", endPointer, true),
      cancelTerminalLongPress,
    );

    this.terminalDisposables.push(
      this.terminal.onRender(() => this.scheduleReconcile()),
      this.terminal.buffer.onBufferChange((buffer) => this.bufferChanged(buffer.type)),
      this.terminal.onScroll((line) => this.selectionEdgeScroll(line)),
    );
    // Only a real container resize re-fits. A scrollbar appearing or vanishing
    // changes the content box but not the border box, and re-fitting on that
    // would revert a manual zoom the moment its own scrollbar showed up.
    let observedInline = -1;
    let observedBlock = -1;
    this.resizeObserver = new ResizeObserver((entries) => {
      const box = entries[entries.length - 1]?.borderBoxSize?.[0];
      let heightOnly = false;
      if (box) {
        if (box.inlineSize === observedInline && box.blockSize === observedBlock) return;
        heightOnly = observedInline !== -1 && box.inlineSize === observedInline;
        observedInline = box.inlineSize;
        observedBlock = box.blockSize;
      }
      // On a coarse-pointer device a height-only box change is the software
      // keyboard or the browser chrome coming and going. The fit ratio takes
      // the smaller of the width and height ratios, so refitting here would
      // flap the font on every keyboard open and close; the projection
      // reconciles and the font stays put. Width remains the refit trigger.
      if (heightOnly && this.coarsePointerQuery?.matches === true) {
        this.scheduleReconcile();
        return;
      }
      this.autoFitFont();
    });
    this.resizeObserver.observe(viewport);
    // The iOS software keyboard overlays the layout viewport without resizing
    // it: 100dvh keeps the key bar and the cursor row underneath the keyboard,
    // where no scroll can reach them. The keyboard state machine listens to
    // every signal that can accompany a keyboard transition — visual viewport
    // resize and scroll, window resize (rotation), focus entering or leaving
    // the terminal (blur is how iOS dismissals that drop the resize event
    // still announce themselves), visibility changes, and the pointer medium —
    // and re-derives the state from live readings each time, so no single
    // missed event can strand a pin.
    const onKeyboardSignal = () => this.evaluateKeyboardState();
    const insetViewport = this.insetViewport;
    if (insetViewport) {
      insetViewport.addEventListener("resize", onKeyboardSignal);
      insetViewport.addEventListener("scroll", onKeyboardSignal);
      this.cleanupListeners.push(
        () => insetViewport.removeEventListener("resize", onKeyboardSignal),
        () => insetViewport.removeEventListener("scroll", onKeyboardSignal),
      );
    }
    this.coarsePointerQuery?.addEventListener("change", onKeyboardSignal);
    window.addEventListener("resize", onKeyboardSignal);
    shell.addEventListener("focusin", onKeyboardSignal);
    shell.addEventListener("focusout", onKeyboardSignal);
    document.addEventListener("visibilitychange", onKeyboardSignal);
    this.cleanupListeners.push(
      () => this.coarsePointerQuery?.removeEventListener("change", onKeyboardSignal),
      () => window.removeEventListener("resize", onKeyboardSignal),
      () => shell.removeEventListener("focusin", onKeyboardSignal),
      () => shell.removeEventListener("focusout", onKeyboardSignal),
      () => document.removeEventListener("visibilitychange", onKeyboardSignal),
      () => this.disarmKeyboardTimers(),
    );
    this.cleanupListeners.push(
      bindGenerationFencedClickActivation(zoomOut, () => this.setFontSize(this.renderedFontSize() - 1), () => !this.closed, () => this.keyInteractionGeneration),
      bindGenerationFencedClickActivation(zoomIn, () => this.setFontSize(this.renderedFontSize() + 1), () => !this.closed, () => this.keyInteractionGeneration),
      bindGenerationFencedClickActivation(fit, () => this.requestAutoFont(), () => !this.closed, () => this.keyInteractionGeneration),
    );
    // These top-row controls share one tap/long-press recognizer. The committed
    // geometry is the sole view disclosure; opening it is presentation only.
    this.cleanupListeners.push(
      bindExplainedTapActivation(
        geometryReadout,
        () => this.setViewPopover(!this.viewPopoverOpen),
        () => this.openExplainer("view", geometryReadout),
        () => undefined,
        () => !this.closed,
        () => this.keyInteractionGeneration,
      ),
      bindExplainedTapActivation(
        selectContext,
        () => this.activateContextualSelection(),
        () => this.openExplainer("select", selectContext),
        () => undefined,
        () => !this.closed && (this.selectMode || (this.committed && !this.replaying)),
        () => this.keyInteractionGeneration,
      ),
    );
    this.terminalDisposables.push(this.terminal.onSelectionChange(() => {
      // A new selection retires the previous outcome: "Selection copied" left
      // standing over a different selection is a lie.
      copyStatus.textContent = "";
      this.renderSheetState();
      // A mouse selection turns the Paste slot into Copy on a fine pointer too.
      this.renderContextualSelectionState();
    }));
    this.terminalDisposables.push(this.terminal.onData((value) => {
      this.scrollToBottom();
      this.sendInput(new TextEncoder().encode(value));
    }));
    this.terminalDisposables.push(this.terminal.onBinary((value) => {
      this.scrollToBottom();
      const bytes = Uint8Array.from(value, (character) => character.charCodeAt(0) & 0xff);
      this.sendInput(bytes);
    }));
    // The composer, reused whole from the legacy page and adapted at this
    // boundary. The legacy overlay-inset machinery (a panel floating over the
    // terminal's bottom edge) has no counterpart here: the dock is an in-flow
    // grid row, so opening the composer shrinks the viewport and the ordinary
    // reconcile/anchor path re-pins the tail. The overlay hooks therefore
    // reduce to the invariants they exist to protect rather than the
    // mechanism they drove on the legacy page.
    const preferencesStore = createPreferencesStore(window.localStorage);
    this.composer = new Composer({
      page: shell,
      dock: composerDock,
      symbolicChrome: true,
      interactionGeneration: () => this.keyInteractionGeneration,
      ...(options.composerStorageScope ? { storageScope: options.composerStorageScope } : {}),
      ...(options.stageImage ? { stageImage: options.stageImage } : {}),
      availability: () => this.composerAvailability(),
      inject: (text) => this.injectComposerText(text),
      refocusTerminal: () => this.focusTerminalPreservingKeyboard(),
      // The most height the panel may claim. Measured off the shell and the
      // rows around the viewport — never off the viewport's current height,
      // which already includes the panel's own claim and would feed back
      // through the panel's ResizeObserver. Keeps at least four terminal
      // rows visible.
      insetBudget: () => this.composerInsetBudget(),
      // In flow there is nothing to commit: the grid reflows synchronously
      // when the panel's box changes. Resolving true keeps the Composer's
      // publication bookkeeping settled (false would make it retry forever).
      commitOverlayInset: async () => true,
      // "Reveal the live edge" survives as its invariant: after the panel
      // takes or releases height, the operator who was at the prompt stays
      // at the prompt.
      revealLiveEdgeForOverlay: () => this.syncNativeScroll(true),
      // No reveal scheduler exists on this page; there is nothing to cancel.
      cancelPendingOverlayReveal: () => undefined,
      // Compact IS the unified design: reads force compact density, writes
      // restore the stored density so the shared store never flips the
      // legacy page's layout.
      preferences: () => withCompactDensity(preferencesStore.snapshot().preferences),
      updatePreferences: (preferences) => {
        preferencesStore.update(withStoredDensity(preferencesStore.snapshot().preferences, preferences));
      },
      initialComposerFontSize: this.composerFontPreference,
      updateComposerFontSize: (size) => this.requestComposerFontSize(size),
      claimTypographyPopover: () => this.claimPopover("typography"),
      releaseTypographyPopover: () => {
        if (this.popoverOwner === "typography") this.popoverOwner = "none";
      },
      onOpenChange: (open) => {
        composerDock.dataset.composer = open ? "open" : "closed";
        for (const toggle of this.composerToggles) {
          const label = open ? "Hide composer" : "Open composer";
          toggle.setAttribute("aria-expanded", open ? "true" : "false");
          toggle.setAttribute("aria-label", label);
          toggle.title = label;
        }
      },
    });
    this.updateGeometryControl();
    this.clipboardPanel = options.snippets ? new ClipboardPanel({
      text: options.snippets,
      terminal: {
        identity: () => `${this.generation}:${this.prepared?.source ?? ""}:${this.prepared?.epoch ?? ""}`,
        unavailable: () => { const state = this.composerAvailability(); return state.canInject ? "" : state.reason; },
        send: (text, event) => {
          const result = this.insertText(text, "paste", event);
          const accepted = result === "SENT" || result === "COMPOSER";
          const message = this.insertStatus(result, "paste");
          if (accepted) this.showToast(message);
          // The modal must close before terminal focus can be restored.
          // Cancel and the multiline review guard never resume terminal input.
          const afterClose = result === "SENT" && this.clipboardRestoreTerminalFocus
            ? () => { if (!this.closed) this.focusTerminalPreservingKeyboard(); }
            : undefined;
          return { accepted, message, afterClose };
        },
        canAttach: () => this.composer?.canAttachImages ?? false,
        attach: (file) => {
          const accepted = this.composer?.addClipboardImage(file) ?? false;
          if (accepted) this.showToast("Image added to the composer for review");
          return { accepted, message: "The composer cannot add this image. Remove an attachment and try again." };
        },
      },
    }) : undefined;
    this.installNativeCopySharing();
    this.preferenceSubscription = options.preferences?.subscribe((snapshot) => this.applyPreferences(snapshot));
    this.evaluateKeyboardState();
    this.scheduleReconcile();
  }

  // --- operator preferences (preferences) -----------------------------------------

  // The composer face picker. Same shape and the same intent/settlement
  // discipline as the theme picker: apply immediately so the operator sees the
  // face they chose, write the record, and let the matching publication be the
  // sole authority once the write settles.
  // Applying is one custom property on the shell plus a data attribute that
  // makes the applied value readable without parsing a style. The stylesheet
  // owns what the face actually becomes — notably the coarse-pointer floor,
  // which no number here may go under.
  private applyComposerFontSize(size: number, state?: ComposerTypographyState): void {
    this.composerFontPreference = size;
    const typographyState = state ?? Object.freeze({
      size,
      status: "ready",
      message: "",
      enabled: this.options.preferences !== undefined,
    });
    if (this.composer) {
      this.composer.applyTypographyState(typographyState);
    } else {
      this.shell.style.setProperty("--persea-composer-font-size", `${size}px`);
      this.shell.dataset.composerFont = String(size);
    }
    // The panel's height cap is measured off the textarea, which just changed
    // face; re-measure rather than wait for the next unrelated signal.
    this.composer?.remeasureInset();
  }

  private applyTheme(id: (typeof UNIFIED_THEME_IDS)[number]): void {
    const theme = unifiedTheme(id);
    this.shell.dataset.theme = id;
    document.body.dataset.perseaTheme = id;
    this.terminal.options.theme = theme.xterm;
  }

  private applyPreferences(snapshot: OperatorPreferenceSnapshot): void {
    if (this.closed) return;
    this.applyTheme(snapshot.preferences.theme);
    // A pending intent is authority even when its value is `null`: the operator
    // asked for auto and the record has not caught up yet. `??` would read that
    // as "no intent" and let the stale stored number win.
    const fontPreference = this.fontIntent !== undefined ? this.fontIntent.value : snapshot.preferences.fontSize;
    if (this.fontPreference !== fontPreference) {
      this.fontPreference = fontPreference;
      this.shell.dataset.fontBaseline = fontBaselineAttribute(fontPreference);
      // Explicit overrides the fit; auto hands the decision back to the
      // viewport, which is what a fresh fit computes.
      if (fontPreference === null) this.autoFitFont();
      else this.applyFontSize(fontPreference, this.captureAnchor());
    }
    const composerFont = this.composerFontIntent?.value ?? snapshot.preferences.composerFontSize;
    const status: ComposerTypographyState["status"] = this.composerFontIntent !== undefined || snapshot.status === "loading"
      ? "saving"
      : snapshot.status === "conflict"
        ? "conflict"
        : snapshot.status === "unavailable"
          ? "unavailable"
          : "ready";
    const message = status === "saving" ? "Saving text size…" : status === "ready" ? "" : snapshot.message;
    this.applyComposerFontSize(composerFont, Object.freeze({
      size: composerFont,
      status,
      message,
      enabled: snapshot.available,
    }));
  }

  private requestComposerFontSize(value: number): void {
    if (!Number.isInteger(value) || value < COMPOSER_FONT_SIZE_MIN || value > COMPOSER_FONT_SIZE_MAX) return;
    const preferences = this.options.preferences;
    if (preferences === undefined || !preferences.snapshot().available || this.composerFontIntent !== undefined) return;
    const preview = preferences.preview?.({ composerFontSize: value });
    const intent = Object.freeze({ id: ++this.composerFontIntentID, value, ...(preview === undefined ? {} : { preview }) });
    this.composerFontIntent = intent;
    const saving = Object.freeze({ size: value, status: "saving" as const, message: "Saving text size…", enabled: true });
    this.applyComposerFontSize(value, saving);
    void preferences.update({ composerFontSize: value }, intent.preview).then(
      (outcome) => this.settleComposerFontIntent(intent, outcome),
      () => this.discardComposerFontIntent(intent),
    );
  }

  private settleComposerFontIntent(intent: ComposerFontIntent, outcome: OperatorPreferenceOutcome): void {
    if (this.closed || this.composerFontIntent?.id !== intent.id) return;
    this.composerFontIntent = undefined;
    this.applyPreferences(this.options.preferences!.snapshot());
    if (outcome === "conflict") this.showToast("Text size not saved — changed on another device");
    else if (outcome === "unavailable") this.showToast("Text size applied for this page only — preferences unavailable");
    else if (outcome === "refused") this.showToast("Text size could not be saved");
  }

  private discardComposerFontIntent(intent: ComposerFontIntent): void {
    if (this.closed || this.composerFontIntent?.id !== intent.id) return;
    this.composerFontIntent = undefined;
    this.applyPreferences(this.options.preferences!.snapshot());
    this.showToast("Text size could not be saved");
  }

  // --- quick actions: the sheet -----------------------------------------------
  //
  // The toolbar's opener reveals a grid sheet — Keys · View ·
  // Sessions · Saved text · Image · Keyboard. Every tile reuses an existing
  // path: Keys tiles go through sendKeyBarKey (a synthetic keydown xterm
  // evaluates against its tracked DEC modes — measured to work with the
  // textarea unfocused, P0-a), ↕ is a second button on the toolbar's trusted-
  // click guard, View tiles are the ⋯ panel's items, Image opens the composer
  // with its picker, Keyboard is the one deliberate focus/blur. Opening,
  // closing and dismissing the sheet make no focus call: the element that had
  // focus keeps it, so the keyboard state is exactly what it was.
  private buildQuickActions(opener: HTMLButtonElement): Readonly<{
    dock: HTMLElement; sheet: HTMLElement; status: HTMLElement;
    clipboardTile: HTMLButtonElement; imageTile: HTMLButtonElement;
    showKeyboardTile: HTMLButtonElement; hideKeyboardTile: HTMLButtonElement;
    sessionSwitcher?: SessionSwitcherView; switchTile?: HTMLButtonElement;
  }> {
    const sheet = document.createElement("section");
    sheet.className = "persea-unified-sheet persea-unified-sheet--menu";
    sheet.id = "persea-unified-sheet-" + (++toolbarSerial);
    sheet.setAttribute("role", "group"); sheet.setAttribute("aria-label", "Terminal menu"); sheet.hidden = true;
    opener.setAttribute("aria-controls", sheet.id);
    const status = document.createElement("div"); status.className = "persea-unified-sheet__status";
    status.setAttribute("role", "status"); status.setAttribute("aria-live", "polite");
    const tile = (glyph: string, label: string, title = label): HTMLButtonElement => {
      const button = document.createElement("button"); button.type = "button"; button.className = "persea-unified-sheet__tile";
      const symbol = document.createElement("span"); symbol.className = "persea-unified-sheet__glyph"; symbol.textContent = glyph; symbol.setAttribute("aria-hidden", "true");
      const name = document.createElement("span"); name.className = "persea-unified-sheet__label"; name.textContent = label;
      const reason = document.createElement("span"); reason.className = "persea-unified-sheet__reason";
      button.append(symbol, name, reason); button.title = title; button.setAttribute("aria-label", title); return button;
    };
    const section = (heading: string, tiles: readonly HTMLElement[]): HTMLElement => {
      const block = document.createElement("section"); block.className = "persea-unified-sheet__section";
      const head = document.createElement("h3"); head.className = "persea-unified-sheet__heading"; head.textContent = heading;
      const grid = document.createElement("div"); grid.className = "persea-unified-sheet__grid"; grid.append(...tiles);
      block.append(head, grid); block.hidden = tiles.every((item) => item.hidden); return block;
    };
    const bind = (button: HTMLButtonElement, action: (event: Event) => void | boolean): void => this.bindSheetTap(button, action);
    const heading = document.createElement("div"); heading.className = "persea-unified-sheet__menu-heading";
    const title = document.createElement("h2"); title.textContent = "Terminal";
    const close = tile("×", "Close", "Close terminal menu"); bind(close, () => this.setSheet(false)); heading.append(title, close);
    const clipboardTile = tile("⎘", "Clipboard", "Open shared clipboard");
    bind(clipboardTile, () => { this.openClipboard(); return false; });
    const keys = tile("⇥", "Keys", "Open Terminal Keys");
    bind(keys, () => { this.setSheet(false); this.openKeys(); return false; });
    const composer = tile("✎", "Composer", "Open composer");
    bind(composer, (event) => { this.setSheet(false); this.composer?.openFromTrustedEvent(event); return false; });
    const imageTile = tile("⊕", "Attach image", "Attach an image in the composer");
    this.cleanupListeners.push(bindGenerationFencedClickActivation(imageTile, (event) => {
      if (!event.isTrusted || this.closed || !this.composer) return;
      if (this.composer.openImagePickerFromTrustedEvent(event)) this.setSheet(false);
    }, () => !this.closed, () => this.keyInteractionGeneration));
    const showKeyboardTile = tile("⌨", "Show keyboard", "Show keyboard");
    this.cleanupListeners.push(bindGenerationFencedClickActivation(showKeyboardTile, (event) => {
      if (!event.isTrusted || this.closed) return;
      this.focusTerminalPreservingKeyboard(); this.setSheet(false);
    }, () => !this.closed, () => this.keyInteractionGeneration));
    const hideKeyboardTile = tile("⌄", "Hide keyboard", "Hide keyboard");
    this.cleanupListeners.push(bindGenerationFencedClickActivation(hideKeyboardTile, (event) => {
      if (!event.isTrusted || this.closed) return;
      this.restoreKeyboardOnCommit = false;
      const active = document.activeElement;
      if (active instanceof HTMLElement && this.shell.contains(active)) active.blur();
      this.renderSheetState();
    }, () => !this.closed, () => this.keyInteractionGeneration));
    let sessionSwitcher: SessionSwitcherView | undefined; let switchTile: HTMLButtonElement | undefined;
    const sessionTiles: HTMLButtonElement[] = [];
    const sessionList = document.createElement("div"); sessionList.hidden = true;
    if (!this.options.workspaceCell) {
      const dashboard = tile("▦", "Dashboard", "Open the dashboard");
      bind(dashboard, (event) => { if (event.isTrusted) window.location.assign("/"); }); sessionTiles.push(dashboard);
    }
    if (this.options.sessionSwitch) {
      switchTile = tile("⇄", "Switch session", "Choose another session");
      bind(switchTile, () => { void this.openSessionSwitcher(false); }); sessionTiles.push(switchTile);
      sessionSwitcher = new SessionSwitcherView({
        root: sessionList,
        currentDraftScope: () => this.options.sessionSwitch?.currentDraftScope() ?? null,
        blockedMessage: (session) => this.options.sessionSwitch?.blockedMessage(session) ?? "Unified terminal unavailable",
        select: (session) => { void this.selectSession(session, "sheet"); },
        interactionGeneration: () => this.keyInteractionGeneration,
      });
      this.bindSheetTap(sessionSwitcher.refresh, () => { void this.openSessionSwitcher(true); });
    }
    const keyboardSettings = tile("⚙", "Keyboard settings", "Keyboard settings: shared defaults and this device");
    bind(keyboardSettings, () => { this.setSheet(false); this.keysPanel.openSettings(); return false; });
    const help = tile("ⓘ", "Help", "Explain terminal controls"); bind(help, () => { this.openExplainer(undefined, help); return false; });
    const scrollback = document.createElement("section"); scrollback.className = "persea-unified-sheet__scrollback";
    const scrollbackStatus = document.createElement("output"); scrollbackStatus.setAttribute("role", "status");
    const chooseHistory = (rows: ScrollbackRows, inherit: boolean): void => {
      if (this.closed) return;
      const increased = rows > this.scrollbackRows;
      const saved = saveTerminalScrollbackRows(this.scrollbackScope, inherit ? undefined : rows);
      this.applyScrollbackRows(rows);
      scrollbackStatus.textContent = `${rows ? `${rows.toLocaleString()} rows` : "Screen only"}.${saved ? (inherit ? " Using this device's default." : " Saved for this terminal on this device.") : " Applied here; this browser could not save the setting."}${increased ? " Reload to restore available older rows." : ""}`;
    };
    const historyControl = createScrollbackControl(this.scrollbackRows, rows => chooseHistory(rows, false), {
      useDeviceDefault: () => chooseHistory(readScrollbackRows(), true),
    });
    this.scrollbackSelect = historyControl.select;
    this.syncScrollbackControl();
    const historyHelp = document.createElement("p"); historyHelp.textContent = "Older rows must still be available in tmux or the recording.";
    const reloadHistory = tile("↻", "Reload available history", "Reload available recorded history");
    reloadHistory.hidden = !this.options.reloadRecordedHistory;
    bind(reloadHistory, () => {
      if (!this.committed || this.replaying || this.selectMode || !this.options.reloadRecordedHistory?.()) {
        scrollbackStatus.textContent = "Wait for this session to finish connecting or changing, then try again."; return false;
      }
      scrollbackStatus.textContent = "Reloading available recorded history…";
      this.setSheet(false); return false;
    });
    scrollback.append(historyControl.label, historyHelp, reloadHistory, scrollbackStatus);
    sheet.append(heading, status, section("Tools", [clipboardTile, keys, composer, imageTile]),
      section("Keyboard", [showKeyboardTile, hideKeyboardTile]), section("Sessions", sessionTiles), sessionList,
      section("Settings and help", [keyboardSettings, help]), scrollback);
    const onOutsidePointer = (event: PointerEvent): void => {
      if (!this.sheetOpen) return;
      const target = event.target as Node | null;
      if (sheet.contains(target) || opener.contains(target)) return;
      this.setSheet(false);
    };
    const onEscape = (event: KeyboardEvent): void => {
      if (!event.isTrusted || !this.sheetOpen || event.key !== "Escape") return;
      event.preventDefault(); event.stopPropagation(); this.setSheet(false);
    };
    document.addEventListener("pointerdown", onOutsidePointer, true); document.addEventListener("keydown", onEscape, true);
    this.cleanupListeners.push(() => document.removeEventListener("pointerdown", onOutsidePointer, true), () => document.removeEventListener("keydown", onEscape, true));
    const dock = document.createElement("div"); dock.className = "persea-unified-quick"; dock.append(sheet);
    return Object.freeze({ dock, sheet, status, clipboardTile, imageTile, showKeyboardTile, hideKeyboardTile,
      ...(sessionSwitcher && switchTile ? { sessionSwitcher, switchTile } : {}) });
  }

  private sessionSwitcherRoot(): HTMLElement | undefined {
    return this.sessionSwitcher?.search.parentElement?.parentElement ?? undefined;
  }

  private async openSessionSwitcher(refresh: boolean): Promise<void> {
    if (this.closed || !this.options.sessionSwitch || !this.sessionSwitcher) return;
    const root = this.sessionSwitcherRoot();
    if (!root) return;
    if (!refresh && this.sessionSwitcherOpen) {
      this.sessionSwitcherOpen = false;
      root.hidden = true;
      return;
    }
    this.sessionSwitcherOpen = true;
    root.hidden = false;
    await this.loadSessionInventory(refresh);
  }

  currentScrollbackRows(): ScrollbackRows { return this.scrollbackRows; }

  private syncScrollbackControl(): void {
    const select = this.scrollbackSelect;
    if (!select) return;
    const defaults = readScrollbackRows();
    const option = select.querySelector<HTMLOptionElement>('option[value="default"]');
    if (option) option.textContent = `Device default (${defaults ? `${defaults.toLocaleString()} rows` : "screen only"})`;
    select.value = terminalScrollbackOverride(this.scrollbackScope) === undefined && this.scrollbackRows === defaults ? "default" : String(this.scrollbackRows);
  }

  private applyScrollbackRows(rows: ScrollbackRows): void {
    const anchor = this.captureAnchor();
    this.scrollbackRows = rows;
    this.terminal.options.scrollback = rows;
    this.syncNativeScroll(anchor.following, anchor);
    this.syncScrollbackControl();
    this.options.onScrollbackChanged?.(rows);
  }

  private async loadSessionInventory(refresh: boolean): Promise<void> {
    if (this.closed || !this.options.sessionSwitch) return;
    if (this.sessionInventoryRequest) return this.sessionInventoryRequest;
    const loading = refresh ? "Refreshing sessions…" : "Loading sessions…";
    this.sheetStatus.textContent = loading;
    this.identitySessionStatus.textContent = loading;
    const controller = new AbortController();
    this.sessionInventoryAbort = controller;
    let request: Promise<void> | undefined;
    request = (async () => {
      try {
        const inventory = await this.options.sessionSwitch!.inventory(refresh, controller.signal);
        if (this.closed || controller.signal.aborted || this.sessionInventoryAbort !== controller) return;
        this.sessionInventory = inventory;
        this.sessionInventoryLoaded = true;
        this.sessionSwitcher?.setInventory(inventory);
        this.identitySessionSwitcher?.setInventory(inventory);
        this.sheetStatus.textContent = "";
        this.identitySessionStatus.textContent = "";
      } catch {
        if (!controller.signal.aborted && !this.closed) {
          this.sheetStatus.textContent = "Inventory unavailable";
          this.identitySessionStatus.textContent = "Inventory unavailable";
        }
      } finally {
        if (this.sessionInventoryAbort === controller) this.sessionInventoryAbort = undefined;
        if (this.sessionInventoryRequest === request) this.sessionInventoryRequest = undefined;
      }
    })();
    this.sessionInventoryRequest = request;
    await request;
  }

  private async selectSession(session: DashboardSession, surface: "sheet" | "tag"): Promise<void> {
    if (this.closed || !this.options.sessionSwitch) return;
    if (this.sessionSwitchPending) {
      this.sheetStatus.textContent = "Switching…";
      this.identitySessionStatus.textContent = "Switching…";
      return;
    }
    this.sessionSwitchPending = true;
    this.sheetStatus.textContent = `Switching to ${session.name}…`;
    this.identitySessionStatus.textContent = `Switching to ${session.name}…`;
    this.renderSheetState();
    try {
      const result = await this.options.sessionSwitch.select(session);
      if (this.closed) return;
      if (!result.ok) {
        this.sheetStatus.textContent = result.message;
        this.identitySessionStatus.textContent = result.message;
        return;
      }
      this.sessionSwitcher?.render();
      this.identitySessionSwitcher?.render();
      if (surface === "sheet") this.setSheet(false);
      else this.setIdentityDetails(false);
    } finally {
      this.sessionSwitchPending = false;
      this.renderSheetState();
    }
  }

  // Exactly one control opens the sheet, and which one depends on the pointer
  // Open/close changes disclosure ownership. It advances the interaction
  // generation and clears Ctrl before changing visibility, so neither a
  // hidden modifier nor a captured release from the prior sheet state can
  // cross the transition. No focus call or blur is manufactured here.
  private setSheet(open: boolean, coordinated = false): void {
    if (this.closed || this.sheetOpen === open) return;
    this.advanceKeyInteractionGeneration();
    if (open) this.keysPanel.setOpen(false);
    if (open && !coordinated) this.claimPopover("sheet");
    this.sheetOpen = open;
    this.sheet.hidden = !open;
    this.quickActionsToggle.setAttribute("aria-expanded", open ? "true" : "false");
    this.quickActionsToggle.dataset.open = open ? "true" : "false";
    if (open) {
      this.sheetStatus.textContent = "";
      this.syncScrollbackControl();
      this.renderSheetState();
    } else {
      this.sessionSwitcherOpen = false;
      const sessionSwitcher = this.sessionSwitcherRoot();
      if (sessionSwitcher) sessionSwitcher.hidden = true;
    }
    if (!open && this.popoverOwner === "sheet") this.popoverOwner = "none";
  }

  // Every tile that can be unavailable renders why, on the tile.
  private renderSheetState(): void {
    if (this.closed) return;
    // `reason` is what the tile says; `disable` is whether it stops working.
    // They are usually the same, but not always: clipboard-store-recovery needs a tile that
    // reports an outage and stays usable, because what it opens still works.
    const setAvailability = (button: HTMLButtonElement, reason: string, disable = reason !== ""): void => {
      const available = !disable;
      button.disabled = disable;
      button.setAttribute("aria-disabled", available ? "false" : "true");
      const label = button.getAttribute("aria-label") ?? "";
      button.title = reason === "" ? label : `${label} \u2014 ${reason}`;
      const reasonNode = button.querySelector<HTMLElement>(".persea-unified-sheet__reason");
      if (reasonNode) reasonNode.textContent = reason;
    };
    setAvailability(this.sheetImageTile, this.composer?.canAttachImages ? "" : "Images unavailable for this session");
    const textEntryFocused = this.pageTextEntryFocused();
    setAvailability(this.sheetShowKeyboardTile, textEntryFocused ? "Keyboard is already open" : "");
    setAvailability(this.sheetHideKeyboardTile, textEntryFocused || this.keyboardOpen ? "" : "Keyboard is closed");
    this.sheetShowKeyboardTile.hidden = textEntryFocused || this.keyboardOpen;
    this.sheetHideKeyboardTile.hidden = !this.sheetShowKeyboardTile.hidden;
    setAvailability(this.sheetClipboardTile, this.options.snippets ? "" : "Shared clipboard unavailable", !this.options.snippets);
    const switchReason = this.sessionSwitchPending ? "Switching…" : "";
    if (this.sheetSwitchTile) setAvailability(this.sheetSwitchTile, switchReason);
    // Hide a category when none of its items are visible.
    for (const block of Array.from(this.sheet.querySelectorAll<HTMLElement>(".persea-unified-sheet__section"))) {
      const grid = block.querySelector<HTMLElement>(".persea-unified-sheet__grid");
      const items = grid === null ? [] : Array.from(grid.children).filter((child): child is HTMLElement => child instanceof HTMLElement);
      block.hidden = items.every((item) => item.hidden);
    }
  }

  // The state that makes an explicit Fit unavailable, for the tile's reason.
  private fitUnavailableReason(): string {
    if (this.closed) return "the page is closed";
    if (this.options.capabilityMode !== "control") return "observing";
    if (this.prepared === undefined || !this.committed) return "connecting";
    if (this.replaying) return "replaying";
    if (!this.controlGranted) return "control is pending";
    if (this.fitPending) return "a fit is pending";
    if (!(this.committedColumns > 0 && this.committedRows > 0)) return "geometry is unknown";
    if (this.softwareKeyboardGuardActive()) return "the keyboard is open";
    return "";
  }

  // --- Shared Clipboard and terminal paste ----------------------------------
  //
  // Everything below is either presentation of the ONE document-global
  // snapshot or a terminal effect on THIS page. No path here consults a
  // global "active terminal", a focused element, or a sibling controller:
  // the row the operator tapped belongs to this page, so the bytes go to this
  // page's pane and to no other.

  /**
   * Keyboard-preserving tap activation for anything inside the scrollable
   * sheet — tiles and snippet rows alike. The pointerdown is cancelled so
   * focus never moves (an open keyboard stays open), the action fires on the
   * tap's pointerup so a scroll fires nothing, and the terminal's focus is
   * restored ONLY when a text entry of this page held it.
   */
  private bindSheetTap(button: HTMLButtonElement, activate: (event: Event) => void | boolean): void {
    let restore = false;
    this.cleanupListeners.push(bindTapActivation(
      button,
      (event) => {
        if (button.disabled) return;
        restore = this.pageTextEntryFocused();
        if (activate(event) === false) restore = false;
      },
      () => { if (restore && !this.selectMode) this.focusTerminalPreservingKeyboard(); restore = false; },
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
  }

  private bindSheetExplainedTap(
    button: HTMLButtonElement,
    topic: UnifiedExplainerTopic,
    activate: (event: Event) => void | boolean,
  ): void {
    let restore = false;
    this.cleanupListeners.push(bindExplainedTapActivation(
      button,
      (event) => {
        if (button.disabled) return false;
        restore = this.pageTextEntryFocused();
        if (activate(event) === false) { restore = false; return false; }
      },
      () => { restore = false; this.openExplainer(topic, button); },
      () => { if (restore && !this.selectMode) this.focusTerminalPreservingKeyboard(); restore = false; },
      () => !this.closed && !button.disabled,
      () => this.keyInteractionGeneration,
    ));
  }

  private openClipboard(): void {
    if (this.closed) return;
    // Only a completed terminal Send resumes existing text input on a phone.
    // Other dialog exits return to their opener without raising a keyboard.
    this.clipboardRestoreTerminalFocus = this.pageTextEntryFocused() || this.coarsePointerQuery?.matches !== true;
    this.setSheet(false); this.keysPanel.setOpen(false);
    if (!this.clipboardPanel) { this.showToast("Shared clipboard unavailable"); return; }
    this.clipboardPanel.open(this.pasteSlotButton);
  }

  private installNativeCopySharing(): void {
    const onCopy = (event: ClipboardEvent): void => {
      if (!event.isTrusted || this.closed || !this.options.snippets) return;
      const target = event.target;
      let text = "";
      if (target instanceof HTMLTextAreaElement && this.shell.contains(target)) {
        text = target.value.slice(target.selectionStart, target.selectionEnd);
        if (!text && this.host.contains(target)) text = this.terminal.getSelection();
      } else {
        const selection = document.getSelection();
        if (selection?.anchorNode && selection.focusNode && this.selectBody.contains(selection.anchorNode) && this.selectBody.contains(selection.focusNode)) text = selection.toString();
        else if (target instanceof Node && this.host.contains(target)) text = this.terminal.getSelection();
      }
      // A normal Ctrl+C without a selection remains the terminal's interrupt.
      // Only an actual browser Copy of selected text creates a shared item.
      if (text) void this.shareCopiedText(text);
    };
    document.addEventListener("copy", onCopy);
    this.cleanupListeners.push(() => document.removeEventListener("copy", onCopy));
  }

  private async shareCopiedText(text: string, local?: boolean): Promise<SnippetOutcome> {
    const body = text.replace(/\r\n?/g, "\n");
    const service = this.options.snippets;
    if (!service || service.snapshot().status === "unavailable") {
      this.showToast(local ? CLIPS_UNAVAILABLE_COPY_TEXT : "Shared clipboard unavailable");
      return "unavailable";
    }
    if (!isStorableSnippetBody(body)) {
      this.showToast(local ? CLIPS_TOO_LARGE_COPY_TEXT : "This copy cannot be added to the shared clipboard (16 KB text limit).");
      return "too_large";
    }
    let outcome: SnippetOutcome;
    try { outcome = await service.createClip(body); } catch { outcome = "unreachable"; }
    if (this.closed) return outcome;
    if (outcome === "ok") this.showToast(local === true ? "Copied · shared clipboard updated" : "Shared clipboard updated");
    else this.showToast(local ? "Copied to this device; " + SNIPPET_REFUSALS[outcome].toLocaleLowerCase() : SNIPPET_REFUSALS[outcome]);
    return outcome;
  }

  /** Local copying settles independently of the shared storage request. */
  private async copyToClips(text: string): Promise<CopyToClipsResult> {
    const body = text.replace(/\r\n?/g, "\n");
    if (!body) { this.showToast("Nothing to copy"); return { local: false, store: "not-attempted" }; }
    let copied = false;
    try { await navigator.clipboard.writeText(body); copied = true; this.options.snippets?.acknowledgeClipboard(body); } catch { /* Shared storage can still accept this explicit copy. */ }
    if (!this.options.snippets || this.options.snippets.snapshot().status === "unavailable") {
      this.showToast(copied ? CLIPS_UNAVAILABLE_COPY_TEXT : "Copy unavailable");
      return { local: copied, store: "not-attempted" };
    }
    if (!isStorableSnippetBody(body)) {
      this.showToast(copied ? CLIPS_TOO_LARGE_COPY_TEXT : "This text is too large for the shared clipboard");
      return { local: copied, store: "too-large" };
    }
    void this.shareCopiedText(body, copied);
    return { local: copied, store: "pending" };
  }

  private frozenCellAtClientPoint(clientX: number, clientY: number): Readonly<{ row: number; column: number }> | undefined {
    const screen = this.host.querySelector<HTMLElement>(".xterm-screen");
    const bounds = screen?.getBoundingClientRect();
    if (!bounds || clientX < bounds.left || clientX > bounds.right || clientY < bounds.top || clientY > bounds.bottom) return undefined;
    const rowHeight = bounds.height / this.terminal.rows;
    const columnWidth = bounds.width / this.terminal.cols;
    if (!(rowHeight > 0) || !(columnWidth > 0)) return undefined;
    const visibleRow = Math.max(0, Math.min(this.terminal.rows - 1, Math.floor((clientY - bounds.top) / rowHeight)));
    const column = Math.max(0, Math.min(this.terminal.cols - 1, Math.floor((clientX - bounds.left) / columnWidth)));
    return Object.freeze({ row: this.terminal.buffer.active.viewportY + visibleRow, column });
  }

  private captureFrozenSnapshot(): FrozenTerminalSnapshot {
    const buffer = this.terminal.buffer.active;
    const lineCount = buffer.type === "alternate" ? Math.min(buffer.length, this.terminal.rows) : buffer.length;
    const theme = (this.terminal.options.theme ?? {}) as Readonly<Record<string, string | undefined>>;
    const exemplar = buffer.getLine(0)?.getCell(0);
    const color = (cell: NonNullable<typeof exemplar>, foreground: boolean): string | undefined => {
      if (foreground ? cell.isFgRGB() : cell.isBgRGB()) return rgbColor(foreground ? cell.getFgColor() : cell.getBgColor());
      if (foreground ? cell.isFgPalette() : cell.isBgPalette()) {
        let index = foreground ? cell.getFgColor() : cell.getBgColor();
        if (foreground && cell.isBold() && index < 8 && this.terminal.options.drawBoldTextInBrightColors !== false) index += 8;
        return paletteColor(index, theme);
      }
      return undefined;
    };
    const lines = [];
    for (let index = 0; index < lineCount; index += 1) {
      const line = buffer.getLine(index);
      const segments: FrozenTerminalSegment[] = [];
      const columnOffsets: number[] = [];
      const columnRanges: Array<Readonly<{ start: number; end: number }>> = [];
      let text = "";
      let copyText = "";
      let leadColumns: number[] = [];
      let leadStart = 0;
      let leadInvisible = false;
      for (let column = 0; column < this.terminal.cols; column += 1) {
        columnOffsets[column] = text.length;
        const cell = line?.getCell(column);
        if (!cell) {
          const start = text.length;
          text += " ";
          copyText += " ";
          columnRanges[column] = Object.freeze({ start, end: text.length });
          continue;
        }
        if (cell.getWidth() === 0) {
          const combining = cell.getChars();
          if (combining && leadColumns.length > 0) {
            text += combining;
            copyText += leadInvisible ? " ".repeat(combining.length) : combining;
            const previous = segments.at(-1);
            if (previous) segments[segments.length - 1] = Object.freeze({ text: previous.text + combining, style: previous.style });
            for (const leadColumn of leadColumns) columnRanges[leadColumn] = Object.freeze({ start: leadStart, end: text.length });
          }
          const range = leadColumns.length > 0
            ? Object.freeze({ start: leadStart, end: text.length })
            : Object.freeze({ start: text.length, end: text.length });
          columnRanges[column] = range;
          continue;
        }
        const chars = cell.getChars() || " ";
        let foreground = color(cell, true);
        let background = color(cell, false);
        const inverse = cell.isInverse() !== 0;
        if (inverse) {
          const originalForeground = foreground ?? theme.foreground;
          foreground = background ?? theme.background;
          background = originalForeground;
        }
        const style: FrozenTerminalCellStyle = Object.freeze({
          ...(foreground ? { foreground } : {}),
          ...(background ? { background } : {}),
          ...(cell.isBold() ? { bold: true as const } : {}),
          ...(cell.isDim() ? { dim: true as const } : {}),
          ...(cell.isItalic() ? { italic: true as const } : {}),
          ...(cell.isUnderline() ? { underline: true as const } : {}),
          ...(inverse ? { inverse: true as const } : {}),
          ...(cell.isStrikethrough() ? { strikethrough: true as const } : {}),
          ...(cell.isInvisible() ? { invisible: true as const } : {}),
          ...(cell.isBlink() ? { blink: true as const } : {}),
          ...(cell.isOverline() ? { overline: true as const } : {}),
        });
        const previous = segments.at(-1);
        if (previous && frozenStyleKey(previous.style) === frozenStyleKey(style)) {
          segments[segments.length - 1] = Object.freeze({ text: previous.text + chars, style });
        } else {
          segments.push(Object.freeze({ text: chars, style }));
        }
        leadStart = text.length;
        leadInvisible = style.invisible === true;
        text += chars;
        copyText += style.invisible ? " ".repeat(chars.length) : chars;
        leadColumns = [];
        const width = Math.max(1, cell.getWidth());
        for (let offset = 0; offset < width && column + offset < this.terminal.cols; offset += 1) {
          leadColumns.push(column + offset);
          columnRanges[column + offset] = Object.freeze({ start: leadStart, end: text.length });
        }
      }
      columnOffsets[this.terminal.cols] = text.length;
      if (typeof Intl.Segmenter === "function") {
        for (const part of new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(text)) {
          const start = part.index;
          const end = start + part.segment.length;
          for (let column = 0; column < columnRanges.length; column += 1) {
            const range = columnRanges[column];
            if (range && range.end > start && range.start < end) columnRanges[column] = Object.freeze({ start, end });
          }
        }
      }
      lines.push(Object.freeze({
        text,
        copyText,
        isWrapped: line?.isWrapped === true,
        segments: Object.freeze(segments),
        columnOffsets: Object.freeze(columnOffsets),
        columnRanges: Object.freeze(columnRanges),
      }));
    }
    return Object.freeze({
      bufferType: buffer.type,
      columns: this.terminal.cols,
      rows: this.terminal.rows,
      baseY: buffer.baseY,
      viewportY: buffer.viewportY,
      fontFamily: this.terminal.options.fontFamily ?? "ui-monospace, monospace",
      fontSizePixels: this.renderedFontSize(),
      rowHeightPixels: this.cellHeightPixels || this.measureCellHeight() || 17,
      lines: Object.freeze(lines),
    });
  }

  private renderFrozenSnapshot(snapshot: FrozenTerminalSnapshot): void {
    // Freeze the renderer's actual presentation together with its cells. The
    // live xterm may refit while this overlay is open; selection geometry must
    // continue to describe the captured grid, not a fallback 14px transcript.
    this.selectBody.style.setProperty("--persea-terminal-font-family", snapshot.fontFamily);
    this.selectBody.style.setProperty("--persea-terminal-font-size", `${snapshot.fontSizePixels}px`);
    this.selectBody.style.setProperty("--persea-terminal-row-height", `${snapshot.rowHeightPixels}px`);
    const fragment = document.createDocumentFragment();
    snapshot.lines.forEach((line, index) => {
      const row = document.createElement("span");
      row.className = "persea-unified-select__row";
      row.dataset.row = String(index);
      if (line.segments && line.segments.length > 0) {
        for (const segment of line.segments) {
          const cell = document.createElement("span");
          cell.className = "persea-unified-select__cell";
          cell.dataset.style = frozenStyleKey(segment.style);
          cell.textContent = segment.text;
          if (segment.style.foreground) cell.style.color = segment.style.foreground;
          if (segment.style.background) cell.style.backgroundColor = segment.style.background;
          if (segment.style.bold) cell.style.fontWeight = "700";
          if (segment.style.dim) cell.style.opacity = "0.6";
          if (segment.style.italic) cell.style.fontStyle = "italic";
          if (segment.style.invisible) {
            cell.dataset.invisible = "true";
            cell.style.visibility = "hidden";
            cell.setAttribute("aria-hidden", "true");
          }
          if (segment.style.blink) {
            cell.dataset.blink = "true";
            cell.classList.add("persea-unified-select__cell--blink");
          }
          if (segment.style.overline) cell.dataset.overline = "true";
          const decorations = [
            segment.style.underline ? "underline" : "",
            segment.style.strikethrough ? "line-through" : "",
            segment.style.overline ? "overline" : "",
          ].filter(Boolean);
          if (decorations.length > 0) cell.style.textDecorationLine = decorations.join(" ");
          row.append(cell);
        }
      } else {
        row.textContent = line.text;
      }
      fragment.append(row);
    });
    this.selectBody.replaceChildren(fragment);
  }

  private selectFrozenCell(snapshot: FrozenTerminalSnapshot, cell: Readonly<{ row: number; column: number }>): void {
    const line = snapshot.lines[cell.row];
    const row = this.selectBody.querySelector<HTMLElement>(`.persea-unified-select__row[data-row="${cell.row}"]`);
    if (!line || !row) return;
    const exact = frozenRangeAtColumn(line, cell.column);
    const start = exact?.start ?? line.columnOffsets?.[cell.column] ?? Math.min(line.text.length, cell.column);
    const end = exact?.end ?? line.columnOffsets?.[cell.column + 1] ?? Math.min(line.text.length, start + 1);
    if (start >= end || start >= line.text.length) return;
    const domPoint = (offset: number): Readonly<{ node: Text; offset: number }> | undefined => {
      const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
      let remaining = offset;
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        const text = node as Text;
        if (remaining <= text.data.length) return Object.freeze({ node: text, offset: remaining });
        remaining -= text.data.length;
      }
      return undefined;
    };
    const first = domPoint(start);
    const last = domPoint(end);
    if (!first || !last) return;
    const range = document.createRange();
    range.setStart(first.node, first.offset);
    range.setEnd(last.node, last.offset);
    const selection = window.getSelection();
    if (!selection) return;
    this.selectContextGesture = true;
    selection.removeAllRanges();
    selection.addRange(range);
    this.selectSelectionValue = serializeFrozenRange(snapshot, { row: cell.row, offset: start }, { row: cell.row, offset: end });
    this.renderContextualSelectionState();
    setTimeout(() => { this.selectContextGesture = false; }, 0);
  }

  private enterSelectMode(cell?: Readonly<{ row: number; column: number }>): void {
    if (this.closed || this.selectMode || !this.committed || this.replaying) return;
    const active = document.activeElement instanceof HTMLElement && this.shell.contains(document.activeElement)
      ? document.activeElement
      : undefined;
    this.selectEpoch += 1;
    this.selectCopied = false;
    if (this.selectCopiedTimer !== undefined) clearTimeout(this.selectCopiedTimer);
    this.selectCopiedTimer = undefined;
    this.selectRestoreFocus = active;
    this.selectRestoreKeyboard = this.keyboardOpen && active !== undefined;
    this.selectSnapshot = this.captureFrozenSnapshot();
    this.selectSelectionValue = "";
    this.renderFrozenSnapshot(this.selectSnapshot);
    if (active) active.blur();
    this.setSheet(false);
    this.selectMode = true;
    this.viewport.inert = true;
    this.viewport.setAttribute("aria-hidden", "true");
    this.selectOverlay.hidden = false;
    this.selectStatus.textContent = "";
    this.selectBody.scrollTop = this.selectSnapshot.viewportY * this.selectSnapshot.rowHeightPixels;
    if (cell) this.selectFrozenCell(this.selectSnapshot, cell);
    this.renderContextualSelectionState();
    this.renderSheetState();
  }

  private exitSelectMode(resumeInput = false): void {
    if (!this.selectMode) return;
    const epoch = this.selectEpoch;
    const helper = this.host.querySelector(".xterm-helper-textarea");
    const restore = resumeInput ? this.terminal.textarea
      : this.selectRestoreKeyboard || this.selectRestoreFocus === helper ? this.selectRestoreFocus : undefined;
    this.selectMode = false;
    this.selectSnapshot = undefined;
    this.selectSelectionValue = "";
    this.selectRestoreFocus = undefined;
    this.selectRestoreKeyboard = false;
    this.selectOverlay.hidden = true;
    this.selectBody.replaceChildren();
    this.viewport.inert = false;
    this.viewport.removeAttribute("aria-hidden");
    if (restore?.isConnected && epoch === this.selectEpoch) {
      restore.focus({ preventScroll: true });
      // Selection/copy changes document selection and may make WebKit clear or
      // displace xterm's helper value. Reconcile in the same synchronous
      // focus-return task so a Backspace already held by the operator still
      // finds the sentinel on its next native repeat.
      this.iosBackspace.onXtermOperationComplete();
    }
    this.renderContextualSelectionState();
    this.renderSheetState();
  }

  private frozenPoint(node: Node, offset: number): FrozenTerminalPoint | undefined {
    const element = node instanceof Element ? node : node.parentElement;
    const row = element?.closest<HTMLElement>(".persea-unified-select__row");
    if (!row || !this.selectBody.contains(row)) return undefined;
    const rowIndex = Number(row.dataset.row);
    if (!Number.isSafeInteger(rowIndex)) return undefined;
    const prefix = document.createRange();
    prefix.selectNodeContents(row);
    try {
      prefix.setEnd(node, offset);
    } catch {
      return undefined;
    }
    const textOffset = prefix.toString().length;
    return Object.freeze({ row: rowIndex, offset: textOffset });
  }

  private refreshFrozenSelection(): void {
    if (!this.selectMode || !this.selectSnapshot) return;
    const selection = window.getSelection();
    let value = "";
    if (selection && selection.rangeCount === 1 && !selection.isCollapsed) {
      const range = selection.getRangeAt(0);
      const start = this.frozenPoint(range.startContainer, range.startOffset);
      const end = this.frozenPoint(range.endContainer, range.endOffset);
      if (start !== undefined && end !== undefined) value = serializeFrozenRange(this.selectSnapshot, start, end);
    }
    if (value === "" && this.selectContextGesture && this.selectSelectionValue !== "") return;
    const changed = value !== this.selectSelectionValue;
    this.selectSelectionValue = value;
    if (changed) {
      this.selectCopyAttempt += 1;
      this.selectStatus.textContent = "";
    }
    this.renderContextualSelectionState();
  }

  // Select is a toggle: the second tap always leaves the frozen overlay, with
  // or without a selection. Copying is the Paste slot's job while selected.
  private activateContextualSelection(): void {
    if (this.selectMode) this.exitSelectMode();
    else this.enterSelectMode();
  }

  // The text a Copy on the Paste slot would take: the frozen selection while
  // selecting, otherwise xterm's own mouse selection.
  private contextualSelectionText(): string {
    if (this.selectMode) return this.selectSelectionValue;
    return this.terminal.hasSelection() ? this.terminal.getSelection() : "";
  }

  private renderContextualSelectionState(): void {
    const button = this.selectContextButton;
    button.setAttribute("aria-pressed", this.selectMode ? "true" : "false");
    // Readiness (committed, not replaying) is evaluated at activation time by
    // the gesture binding and enterSelectMode, never stored here: this renderer
    // also runs on xterm selection changes during replay, and a stored
    // `disabled` would outlive the state that set it.
    button.disabled = this.closed;
    if (this.selectMode) {
      this.selectWord.textContent = "Select";
      button.title = "Leave selection (nothing is copied)";
      button.setAttribute("aria-label", "Leave selection without copying");
      button.dataset.selectState = "selecting";
    } else {
      this.selectWord.textContent = "Select";
      button.title = "Select terminal text";
      button.setAttribute("aria-label", "Select terminal text");
      button.dataset.selectState = "select";
    }
    this.renderPasteSlot();
  }

  private renderPasteSlot(): void {
    const button = this.pasteSlotButton;
    button.disabled = this.closed;
    button.removeAttribute("aria-haspopup");
    if (this.selectCopied) {
      this.pasteWord.textContent = "Copied";
      button.title = "Selection copied";
      button.setAttribute("aria-label", "Selection copied");
      button.dataset.pasteState = "copied";
      return;
    }
    if (this.pasteCopyFailed) {
      this.pasteWord.textContent = "Failed";
      button.title = "Copy failed · the selection is still there";
      button.setAttribute("aria-label", "Copy failed, the selection is still there");
      button.dataset.pasteState = "failed";
      return;
    }
    if (this.contextualSelectionText() !== "") {
      this.pasteWord.textContent = "Copy";
      button.title = "Copy the selected text";
      button.setAttribute("aria-label", "Copy the selected text");
      button.dataset.pasteState = "copy";
      return;
    }
    if (!this.pasteWord.querySelector("svg")) this.pasteWord.replaceChildren(clipboardIcon("clipboard"));
    button.title = "Open clipboard";
    button.setAttribute("aria-label", "Open clipboard");
    button.setAttribute("aria-haspopup", "dialog");
    button.dataset.pasteState = "paste";
  }

  // Copy while a selection exists; otherwise open the shared Clipboard.
  // Device reads happen only in its explicit Paste from device gesture.
  private activatePasteSlot(event: Event): void {
    if (this.selectCopied || this.pasteCopyFailed) return;
    const text = this.contextualSelectionText();
    if (text === "") {
      this.openClipboard();
      return;
    }
    if (this.selectMode) this.copyContextualSelection();
    else this.copyLiveSelection(text);
  }

  private showPasteSlotCopied(): void {
    if (this.selectCopiedTimer !== undefined) clearTimeout(this.selectCopiedTimer);
    this.selectCopied = true;
    this.renderContextualSelectionState();
    this.announceCopy(this.selectAnnouncement, "Selection copied");
    this.selectCopiedTimer = setTimeout(() => {
      this.selectCopiedTimer = undefined;
      this.selectCopied = false;
      this.renderContextualSelectionState();
    }, 1_200);
  }

  private showPasteSlotCopyFailed(): void {
    if (this.pasteCopyFailedTimer !== undefined) clearTimeout(this.pasteCopyFailedTimer);
    this.pasteCopyFailed = true;
    this.renderContextualSelectionState();
    this.announceCopy(this.selectAnnouncement, "Selection copy unavailable");
    this.pasteCopyFailedTimer = setTimeout(() => {
      this.pasteCopyFailedTimer = undefined;
      this.pasteCopyFailed = false;
      this.renderContextualSelectionState();
    }, 2_500);
  }

  // A mouse selection on a fine pointer: one local write, xterm's selection
  // stays (a later selection change retires the outcome as it always did).
  private copyLiveSelection(text: string): void {
    const attempt = (this.selectCopyAttempt += 1);
    void this.copyToClips(text).then((result) => {
      if (this.closed || attempt !== this.selectCopyAttempt) return;
      if (result.local) {
        // the copied range is consumed, so the slot can return to
        // Paste after the dwell; a failed copy keeps the range for a retry.
        this.terminal.clearSelection();
        this.showPasteSlotCopied();
      } else {
        this.showPasteSlotCopyFailed();
      }
    });
  }

  private copyContextualSelection(): void {
    const text = this.selectSelectionValue;
    if (!this.selectMode || text === "") return;
    const epoch = this.selectEpoch;
    const attempt = (this.selectCopyAttempt += 1);
    void this.copyToClips(text).then((result) => {
      if (!this.selectMode || epoch !== this.selectEpoch || attempt !== this.selectCopyAttempt) return;
      if (!result.local) {
        this.selectStatus.textContent = "Copy failed · selection remains frozen";
        this.showPasteSlotCopyFailed();
        return;
      }
      this.exitSelectMode();
      this.showPasteSlotCopied();
    });
  }

  private insertStatus(result: InsertTextResult, via: "snippet" | "clip" | "paste"): string {
    switch (result) {
      case "SENT":
        // the receipt says what happened. Help explains that insertion never
        // appends Return; individual receipts need not repeat it.
        return via === "paste" ? "Pasted" : via === "clip" ? "Clip inserted" : "Snippet inserted";
      case "COMPOSER":
        return "Multi-line insert needs bracketed paste — opened in the composer instead";
      case "REFUSED_NO_CONTROL":
        return this.composerAvailability().reason;
      case "REFUSED_EMPTY":
        return "Nothing to insert";
      case "REFUSED_DESTROYED":
        return "This page is closed.";
    }
  }

  /**
   * The ONE insert mechanism for snippets, clips and pastes. It never
   * appends Return: the text is normalized (hostile controls and trailing
   * newlines removed) and delivered through xterm's own paste path, so the
   * last byte on the wire is the body's last visible character. Multiline or
   * tabbed text with bracketed paste OFF sends nothing at all and lands in
   * this pane's composer draft, where the composer's own guard applies. On a
   * coarse pointer the delivery does not focus the helper textarea, so an
   * insert never raises a keyboard the operator did not ask for.
   */
  insertText(text: string, via: "snippet" | "clip" | "paste", event?: Event): InsertTextResult {
    if (this.closed) return "REFUSED_DESTROYED";
    void via;
    const normalized = normalizeComposedText(text);
    if (normalized === "") return "REFUSED_EMPTY";
    const availability = this.composerAvailability();
    if (/[\n\t]/.test(normalized) && !availability.bracketedPasteMode) {
      this.handOffToComposer(normalized, event);
      return "COMPOSER";
    }
    if (!availability.canInject) return "REFUSED_NO_CONTROL";
    const coarse = this.coarsePointerQuery?.matches === true;
    return this.deliverText(normalized, !coarse) === "SENT" ? "SENT" : "REFUSED_NO_CONTROL";
  }

  /**
   * The multiline guard's landing: the text goes to this pane's composer
   * draft (never a sibling's).
   *
   * clipboard-focus-ownership / CLIPBOARD-1: it does NOT claim focus, and it DOES make the composer
   * visible. Opening from the tap's trusted event focused the textarea, which
   * raised the keyboard on a coarse pointer — the thing "one tap, no
   * keyboard" forbids. Merely writing the draft was the opposite error: the
   * status said "opened in the composer instead" while the composer stayed
   * hidden, so the text went somewhere the operator could not see. The draft
   * is populated first, then the panel is shown through the composer's own
   * focus-free open transition. Whatever had focus keeps it, in both
   * keyboard states.
   */
  private handOffToComposer(text: string, event?: Event): void {
    void event;
    const composer = this.composer;
    if (!composer) return;
    const existing = composer.textarea.value;
    composer.textarea.value = existing === "" ? text : `${existing}\n${text}`;
    composer.textarea.dispatchEvent(new Event("input", { bubbles: true }));
    composer.showWithoutFocus();
  }

  private showToast(text: string): void {
    if (this.closed) return;
    if (this.toastTimer !== undefined) clearTimeout(this.toastTimer);
    this.toast.textContent = text;
    this.toast.hidden = false;
    this.toastTimer = setTimeout(() => {
      this.toastTimer = undefined;
      if (this.closed) return;
      this.toast.hidden = true;
      this.toast.textContent = "";
    }, TOAST_MS);
  }

  // --- COMMIT focus claim -------------------------------------------------------
  //
  // The one automatic focus on this page. Fine pointers: always, exactly as
  // before. Coarse pointers: only to restore a keyboard the operator had open
  // when the previous generation closed. The hook, when present, has the last
  // word (workspace per-pane routing).
  private claimFocusOnCommit(): boolean {
    if (this.selectMode) return false;
    const coarsePointer = this.coarsePointerQuery?.matches === true;
    const restoreKeyboardOnCommit = this.restoreKeyboardOnCommit;
    const context: CommitFocusContext = Object.freeze({
      coarsePointer, restoreKeyboardOnCommit, pointerRuleClaims: pointerRuleClaims(coarsePointer, restoreKeyboardOnCommit),
    });
    // Monotone: the hook can veto the page's claim, never promote one.
    return commitFocusClaim(context, this.options.claimFocusOnCommit);
  }

  // --- keyboard state machine ------------------------------------------------
  //
  // The presence judgement — resting high-water baseline, rotation/split-view
  // reseeding, and the keyboard baseline rule that a width-change sample taken while OPEN
  // and focused never becomes the baseline — lives in UnifiedKeyboardBaseline
  // (unified_keyboard_baseline.ts), where it is unit-tested directly. Every
  // evaluation re-derives the state from live getters, so a wrong belief
  // lasts only until the next signal; while OPEN a 1s watchdog manufactures
  // that signal, because a dismissal iOS forgets to announce (or announces
  // while the page is backgrounded) would otherwise strand the pin. Misreads
  // bias toward CLOSED, whose outputs — no pin, no bar — are always safe.
  // The visible band, as a CSS length the stylesheet can use. Published near
  // scale 1 only, and withdrawn otherwise, exactly as the legacy page does:
  // under pinch-zoom the operator is magnifying a fixed layout, so the page
  // must not shrink to the magnified band.
  private publishVisualViewportHeight(): void {
    const viewport = this.insetViewport;
    if (viewport === undefined) return;
    if (viewport.scale < 0.99 || viewport.scale > 1.01) {
      if (this.visualViewportHeight === undefined) return;
      this.visualViewportHeight = undefined;
      this.shell.style.removeProperty("--persea-visual-viewport-height");
      this.composer?.remeasureInset();
      return;
    }
    const height = Math.round(viewport.height);
    if (!(height > 0) || height === this.visualViewportHeight) return;
    this.visualViewportHeight = height;
    this.shell.style.setProperty("--persea-visual-viewport-height", `${height}px`);
    this.composer?.remeasureInset();
  }

  private evaluateKeyboardState(): void {
    if (this.closed) return;
    const coarse = this.coarsePointerQuery?.matches === true;
    const viewport = this.insetViewport;
    // Every keyboard signal is also a visual-viewport signal, and the band can
    // move without the pin or the bar changing (an iOS accessory row appearing,
    // a partial keyboard dismissal). Publishing here rather than inside
    // applyKeyboardOutputs — which returns early when neither output moves —
    // is what makes "recompute on visualViewport resize and scroll" true.
    this.publishVisualViewportHeight();
    if (!viewport) {
      // With no viewport signal, focused text is the best available hint.
      // The explicit Keys action remains available in the toolbar's menu.
      this.keyboardOpen = coarse && this.pageTextEntryFocused();
      this.setKeyBarVisible(this.keyboardOpen || this.manualKeysOpen);
      this.refreshGeometryAffordances();
      return;
    }
    const open = this.keyboardBaseline.evaluate({
      scaledHeight: viewport.height * viewport.scale,
      layoutWidth: window.innerWidth,
      coarsePointer: coarse,
      textEntryFocused: this.pageTextEntryFocused(),
    });
    const changed = open !== this.keyboardOpen;
    this.keyboardOpen = open;
    if (changed && !open) { this.manualKeysOpen = false; this.keysPanel.cancel(); }
    if (changed) this.keysPanel.keyboardChanged();
    // A dismissal the classifier sees (blur, the keyboard going) ends the
    // keyboard's session: a pending restore would reopen what the operator
    // just closed.
    if (!open) {
      this.restoreKeyboardOnCommit = false;
    }
    this.applyKeyboardOutputs();
    this.refreshGeometryAffordances();
    if (this.sheetOpen) this.renderSheetState();
    // Failsafe: one short-delay re-read after each transition catches a value
    // sampled mid-transition; the watchdog covers dismissals that fire no
    // event at all. Both re-enter this method and stop rescheduling as soon
    // as a re-read confirms the standing state.
    if (changed) this.scheduleKeyboardVerify();
    this.armKeyboardWatchdog(open);
  }

  // every keyboard / visual-viewport signal (resize, scroll,
  // scale, focus, visibility) changes what the geometry block may offer — the
  // typed geometry availability and the width measurement itself — so the
  // visible affordance is recomputed
  // on the same signal, not only on a keyboard-state transition. Only an open
  // surface can show stale state; a closed popover is refreshed as it opens.
  private refreshGeometryAffordances(): void {
    if (this.viewPopover.hidden && !this.sheetOpen) return;
    this.updateGeometryControl();
  }

  // The single writer of both keyboard outputs, one synchronous batch so the
  // bar and the pin cannot disagree within a frame. Releasing the pin is
  // always safe and is never gated; APPLYING it is gated on scale ≈ 1,
  // because a pinch-zoomed shell pinned to the visual height would fight the
  // operator's own zoom. The anchor is captured before the box changes:
  // following-the-tail is decided against the geometry the operator was
  // actually reading.
  private applyKeyboardOutputs(): void {
    const viewport = this.insetViewport;
    if (!viewport) return;
    const open = this.keyboardOpen;
    const coarse = this.coarsePointerQuery?.matches === true;
    const scaleNeutral = viewport.scale >= 0.99 && viewport.scale <= 1.01;
    // iOS pans the layout viewport to chase the focused textarea. The pinned
    // shell already keeps it visible, so the pan only hides the toolbar.
    if (open && scaleNeutral && window.scrollY !== 0) window.scrollTo(0, 0);
    // Safari can leave a residual pan behind after the keyboard goes,
    // stranding the page shifted with nothing left to scroll. The classic
    // trigger — focus auto-zoom on a sub-16px input — is cured at the
    // stylesheet (the helper textarea's 16px font); this restore is belt and
    // braces for a pan that outlived the keyboard. A real zoom (scale off 1)
    // is the operator's own and is never fought.
    if (!open && coarse && scaleNeutral
      && (viewport.offsetLeft !== 0 || viewport.offsetTop !== 0 || window.scrollX !== 0 || window.scrollY !== 0)) {
      window.scrollTo(0, 0);
    }
    const target = open && scaleNeutral ? Math.round(viewport.height) : 0;
    const barVisible = open || (this.manualKeysOpen && this.keysPanel.open);
    const barChanges = this.keyBar.hidden !== !barVisible;
    if (!barChanges && target === this.keyboardInsetHeight) return;
    // The anchor is captured before EITHER box change — the bar row and the
    // pin land in the same batch, and following-the-tail must be decided
    // against the geometry the operator was actually reading.
    const anchor = this.captureAnchor();
    this.setKeyBarVisible(barVisible);
    this.keyboardInsetHeight = target;
    if (target === 0) this.shell.style.removeProperty("height");
    else this.shell.style.height = `${target}px`;
    this.syncNativeScroll(anchor.following, anchor);
    // The pin and the bar row both feed the composer's height budget; an open
    // panel re-measures against the new shell geometry.
    this.composer?.remeasureInset();
  }

  // Whether a text-entry element of THIS page holds focus: the xterm helper
  // textarea or the composer's input. The keyboard baseline rule keys on it — while it holds
  // and the keyboard reads open, a rotation/split-view sample is
  // keyboard-reduced and must not become the resting baseline.
  private pageTextEntryFocused(): boolean {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || !this.shell.contains(active)) return false;
    return active instanceof HTMLTextAreaElement || active instanceof HTMLInputElement || active.isContentEditable;
  }

  private setKeyBarVisible(visible: boolean): void {
    if (this.keyBar.hidden === !visible) return;
    this.keyInteractionGeneration++;
    this.keyBar.hidden = !visible;
    this.keysPanel.cancelModifiers();
    if (this.sheetOpen) this.renderSheetState();
  }

  private scheduleKeyboardVerify(): void {
    if (this.keyboardVerifyTimer !== undefined) clearTimeout(this.keyboardVerifyTimer);
    this.keyboardVerifyTimer = window.setTimeout(() => {
      this.keyboardVerifyTimer = undefined;
      this.evaluateKeyboardState();
    }, 250);
  }

  private armKeyboardWatchdog(open: boolean): void {
    if (open === (this.keyboardWatchdog !== undefined)) return;
    if (open) {
      this.keyboardWatchdog = window.setInterval(() => this.evaluateKeyboardState(), 1000);
    } else {
      clearInterval(this.keyboardWatchdog);
      this.keyboardWatchdog = undefined;
    }
  }

  private disarmKeyboardTimers(): void {
    if (this.keyboardVerifyTimer !== undefined) clearTimeout(this.keyboardVerifyTimer);
    this.keyboardVerifyTimer = undefined;
    if (this.keyboardWatchdog !== undefined) clearInterval(this.keyboardWatchdog);
    this.keyboardWatchdog = undefined;
  }

  openTransport(generation: number): void {
    if (this.closed) return;
    // A newly opened transport is a fresh input authority even when the
    // visible row was already Standard. Clear held gestures and Ctrl without
    // manufacturing focus before accepting the new generation.
    this.resetKeyInteractionAuthorityForLifecycle();
    this.generation = generation;
    this.prepared = undefined;
    this.committed = false;
    this.controlGranted = false;
    // The ordinary vertical Fit is socket-local and never resent. An explicit
    // width refit is different: transportClosed classified whether this is its
    // typed generation_refit handoff or a terminal/unrelated close, so its
    // exact-operation seal deliberately survives only the former.
    this.fitPending = false;
    this.updateGeometryControl();
  }

  receiveDecoded(generation: number, value: unknown): "ENQUEUED" | "STALE" | "CLOSED" {
    if (this.closed) return "CLOSED";
    if (generation !== this.generation) return "STALE";
    let frame: ServerFrame;
    try { frame = validateServerFrame(value); } catch { this.options.port.finalize({ generation, cause: "MALFORMED_FRAME" }); return "CLOSED"; }
    if (frame.type === "PREPARE") {
      this.acceptPrepare(frame);
      return "ENQUEUED";
    }
    if (!this.prepared || frame.source !== this.prepared.source || frame.epoch !== this.prepared.epoch) {
      this.options.port.finalize({ generation, cause: "ACTIVE_TUPLE_MISMATCH" });
      return "CLOSED";
    }
    if (frame.type === "COMMIT") {
      if (frame.cut !== this.prepared.cut) {
        this.options.port.finalize({ generation, cause: "ACTIVE_TUPLE_MISMATCH" });
        return "CLOSED";
      }
      this.committed = true;
      const pendingRefit = this.pendingRefit;
      const expectedRefit = pendingRefit?.expected;
      if (pendingRefit && expectedRefit
		&& pendingRefit.successorSource === frame.source
        && expectedRefit.generation === generation
        && expectedRefit.source === frame.source
        && expectedRefit.epoch === frame.epoch
        && expectedRefit.cut === frame.cut) {
		this.settlePendingRefit(pendingRefit.operation);
      }
      // The replay write callback has already sent READY, so this COMMIT is
      // the first moment the painted bytes are authoritative. Remove the
      // honest loading surface synchronously here — no timeout/rAF gap can
      // expose both a committed transcript and a stale loading claim.
      this.loadingPanel.hidden = true;
      // A successful commit means the session is healthy again: reset the
      // auto-takeover budget so a later, unrelated displacement gets fresh
      // attempts. The operator intent that opened this connection is spent.
      this.autoTakeoverAttempts = 0;
      this.claimIntent = false;
      if (!this.firstCommitPublished) {
        this.firstCommitPublished = true;
        this.options.onFirstCommit?.();
      }
      this.options.onCommit?.(generation);
      this.hideFailureNotice();
      this.connectionStatus.textContent = "";
      this.updateGeometryControl();
      this.options.port.connectionCommitted?.(generation);
      // Ask for the control grant BEFORE focusing the terminal. focus() can
      // make xterm emit a focus-in report (ESC[I) once the replayed journal has
      // armed DECSET 1004, and that byte must never reach the broker ahead of
      // the grant — the broker would refuse it and the front door would close
      // the attachment. sendInput additionally refuses until the grant, so even
      // a synchronous focus report is dropped rather than sent early. An
      // observe page asks for OBSERVE, which changes nothing: its answer, like
      // the grant, arrives after the whole history backlog, so every
      // admission learns when its view has caught up.
      this.send({ type: "MODE_REQUEST", version: 1, source: frame.source, epoch: frame.epoch, mode: this.options.capabilityMode === "control" ? "CONTROL" : "OBSERVE" });
      if (this.claimFocusOnCommit()) this.terminal.focus();
      this.restoreKeyboardOnCommit = false;
      if (this.admittedViaTakeover) {
        this.admittedViaTakeover = false;
        this.showToast("Control taken from another window");
      }
      // The fit hint is judged once the Fit is actually available (the
      // grant arrives after this frame); see updateGeometryControl.
      this.fitHintPending = true;
      return "ENQUEUED";
    }
    if (frame.type === "MODE") {
      // The control grant. Input is sealed until CONTROL is granted and sealed
      // again if it is ever revoked to OBSERVE.
      if (this.controlGranted !== (frame.mode === "CONTROL")) this.resetKeyInteractionAuthorityForLifecycle();
      this.controlGranted = frame.mode === "CONTROL";
      if (!this.modeReceived) {
        // The first MODE follows the whole history backlog: the view caught up.
        this.catchUpFailures = 0;
        this.options.port.connectionCaughtUp?.(generation);
      }
      this.modeReceived = true;
      this.updateGeometryControl();
      return "ENQUEUED";
    }
    if (frame.type === "LIVE") {
      if (!this.committed || frame.cut !== this.prepared.cut) {
        this.options.port.finalize({ generation, cause: "OUT_OF_STATE" });
        return "CLOSED";
      }
      const anchor = this.captureAnchor();
      this.writesInFlight++;
      this.terminal.write(frame.data, this.guardedWriteCallback(generation, "LIVE_WRITE_FAILED", () => {
        this.writesInFlight--;
        if (this.closed) return;
        if (anchor.bufferType === this.terminal.buffer.active.type) this.syncNativeScroll(anchor.following, anchor);
        else this.scheduleReconcile();
      }));
      return "ENQUEUED";
    }
    if (frame.type === "END") this.transportClosed(generation, frame.reason);
    return "ENQUEUED";
  }

  transportClosed(generation: number, reason: string): void {
    if (generation !== this.generation || this.closed) return;
    // Captured before anything else resets: on a coarse pointer the next
    // COMMIT restores the keyboard only if it was OPEN (the viewport
    // classifier's verdict — focus alone is not keyboard state) AND a text
    // entry of this page held focus as this generation ended.
    this.restoreKeyboardOnCommit = this.coarsePointerQuery?.matches === true && this.keyboardOpen && this.pageTextEntryFocused();
    // Internal cuts (including generation_refit) do not close presentation
    // overlays, but they are still transport authority boundaries.
    this.resetKeyInteractionAuthorityForLifecycle();
    const closeClass = classifyUnifiedClose(reason);
    if (closeClass !== "internal") {
      this.admittedViaTakeover = false;
      this.closePresentationOverlays(true);
    }
    if (closeClass === "transient" && this.committed && this.options.capabilityMode === "control") this.controlLostAt = Date.now();
    this.committed = false;
    this.controlGranted = false;
    this.fitPending = false;
	if (reason === "generation_refit") {
	  const pending = this.pendingRefit;
	  if (pending && pending.predecessorGeneration === generation && this.prepared?.source === pending.predecessorSource) {
		pending.awaitingSuccessor = true;
	  } else {
		this.settlePendingRefit();
	  }
	} else {
	  this.settlePendingRefit();
	}
    this.updateGeometryControl();
    switch (closeClass) {
      case "internal":
        // The page inflicted this close on itself; nothing to render.
        return;
      case "transient":
        // The transport keeps its bounded auto-retry; render it visibly.
        // Exhaustion arrives through reconnectStatus and is terminal there.
        this.connectionStatus.textContent = this.reconnectingText();
        return;
      case "reattach": {
        // A recoverable broker refusal (input_refused): the transport already
        // scheduled a re-attach on the same identity — render it, do not stop
        // it. A burst within the window gives up, and so does a view that
        // keeps falling behind before it catches up (MAX_CATCH_UP_FAILURES).
        if (reason === "subscriber_lagged" && !this.modeReceived && ++this.catchUpFailures >= MAX_CATCH_UP_FAILURES) {
          this.options.port.detach?.(reason);
          this.showFailureNotice(reason);
          return;
        }
        const now = Date.now();
        this.reattachEvents.push(now);
        while (this.reattachEvents.length > 0 && now - this.reattachEvents[0] > REATTACH_WINDOW_MS) this.reattachEvents.shift();
        if (this.reattachEvents.length > REATTACH_BURST_LIMIT) {
          this.options.port.detach?.(reason);
          this.showFailureNotice(reason);
          return;
        }
        this.connectionStatus.textContent = this.reconnectingText();
        return;
      }
      case "terminal":
        // Opening, Reconnect and a session switch are explicit operator
        // intent, so a lease held by another live attachment is claimed
        // automatically — bounded, so two devices cannot fight forever. An
        // automatic recovery claims only this visible page's own stale lease
        // (automaticClaimAllowed). control_displaced/takeover_superseded keep
        // the manual button: the operator moved control there deliberately.
        if (reason === "lease_held"
          && this.options.capabilityMode === "control"
          && this.options.takeControl !== undefined
          && this.options.port.takeControl !== undefined
          && this.autoTakeoverAttempts < MAX_AUTO_TAKEOVERS
          && automaticClaimAllowed({ operatorIntent: this.claimIntent, visible: document.visibilityState === "visible", controlLostAt: this.controlLostAt, now: Date.now() })) {
          this.autoTakeoverAttempts += 1;
          this.claimControl(true);
          return;
        }
        // A typed refusal of this attachment: retrying replays the refusal,
        // so stop the reconnect loop and show the reason with a way out.
        this.options.port.detach?.(reason);
        this.showFailureNotice(reason);
        return;
    }
  }

  // The reconnecting strip names the session being restored when the fragment
  // carried a display name, so a reopen reads as "restoring THIS" rather than a
  // bare loss.
  private reconnectingText(): string {
    const name = this.sessionName;
    return name ? `Reconnecting to ${name}…` : "Connection lost — reconnecting…";
  }

  // The flow acknowledgement point for a delivered frame. xterm runs write
  // callbacks in order, so an empty write completes only after every write
  // the frame caused, replay and output alike, has been parsed. A frame for a
  // superseded generation is consumed at once: its socket is gone and the
  // transport can no longer send for it.
  afterConsumed(generation: number, done: () => void): void {
    if (this.closed || generation !== this.generation) {
      done();
      return;
    }
    this.terminal.write("", this.guardedWriteCallback(generation, "LIVE_WRITE_FAILED", done));
  }

  // xterm runs write callbacks inside its write loop, and a callback that
  // throws stops that loop for good: every later write, and with it every
  // flow acknowledgement, would wait behind a frozen view while liveness
  // keeps the connection open. A failed callback ends the attachment with a
  // notice instead, and the loop carries on.
  private guardedWriteCallback(generation: number, cause: FinalizeCause, callback: () => void): () => void {
    return () => {
      try {
        callback();
      } catch {
        try { this.options.port.finalize({ generation, cause }); } catch { /* the loop must still continue */ }
      }
    };
  }

  // An operational refusal relayed in-band by the front door. The transport
  // is still open and the attachment is still live: the only local state a
  // refusal touches is the Fit seal it answers.
  operationalRefusal(generation: number, code: string): void {
    if (this.closed || generation !== this.generation) return;
    if (refusalReleasesFit(code) && this.fitPending) {
      this.fitPending = false;
      this.updateGeometryControl();
    }
    this.showRefusalNotice(unifiedRefusalNotice(code));
  }

  private showRefusalNotice(text: string): void {
    if (this.closed) return;
    if (this.refusalTimer !== undefined) clearTimeout(this.refusalTimer);
    this.refusalStrip.textContent = text;
    this.refusalStrip.hidden = false;
    this.refusalTimer = setTimeout(() => {
      this.refusalTimer = undefined;
      if (this.closed) return;
      this.refusalStrip.hidden = true;
      this.refusalStrip.textContent = "";
    }, REFUSAL_NOTICE_MS);
  }

  reconnectStatus(status: ReconnectStatus): void {
    if (this.closed) return;
    // The strip this method writes is one of the dot's three inputs, so the
    // tag is re-derived after every branch below.
    try {
      this.applyReconnectStatus(status);
    } finally {
      if (!this.closed) this.renderSessionTag();
    }
  }

  private applyReconnectStatus(status: ReconnectStatus): void {
    switch (status.state) {
      case "CONNECTED":
        this.connectionStatus.textContent = "";
        return;
      case "WAITING":
      case "ATTEMPTING": {
        // A probe under the OFFLINE notice leaves the page as the operator
        // arranged it: overlays closed when the connection was lost, and
        // anything opened since (the switcher, details) stays open.
        if (this.shownNotice !== "reconnect_offline") this.closePresentationOverlays();
        const name = this.sessionName;
        const base = name ? `Reconnecting to ${name}…` : "Reconnecting…";
        this.connectionStatus.textContent = `${base} (attempt ${status.attempt})`;
        return;
      }
      case "OFFLINE":
        // Automatic attempts continue; the notice says so and offers an
        // immediate one. A probe that failed again only clears its strip.
        if (this.shownNotice === "reconnect_offline") {
          this.connectionStatus.textContent = "";
          return;
        }
        this.showFailureNotice("reconnect_offline");
        return;
      case "EXHAUSTED":
        this.showFailureNotice(status.reason ?? "reconnect_exhausted");
        return;
      case "DETACHED":
        // Either this page asked for the detach (the notice is already up) or
        // the app is unmounting; nothing to render.
        return;
    }
  }

  private showFailureNotice(reason: string): void {
    const code = boundedUnifiedReason(reason);
    this.reconnectButton.hidden = this.options.port.attachAgain === undefined || !UNIFIED_RECONNECTABLE_NOTICES.has(code);
    this.reconnectButton.disabled = false;
    this.closePresentationOverlays();
    this.selectRestoreKeyboard = false;
    this.exitSelectMode();
    this.restoreKeyboardOnCommit = false;
    // A typed terminal failure replaces initial loading; a pane must never
    // expose a blank shell or claim it is still replaying after refusal.
    this.loadingPanel.hidden = true;
    const notice = unifiedCloseNotice(code);
    this.noticeHeadline.textContent = notice.headline;
    this.noticeDetail.textContent = notice.detail;
    this.noticeCode.textContent = `code: ${code}`;
    const claimable = UNIFIED_TAKEOVER_REASONS.has(code)
      && this.options.capabilityMode === "control"
      && this.options.takeControl !== undefined
      && this.options.port.takeControl !== undefined;
    this.takeControlButton.hidden = !claimable;
    this.takeControlButton.disabled = false;
    this.takeControlButton.textContent = "Take control here";
    this.noticePanel.hidden = false;
    this.shownNotice = code;
    this.connectionStatus.textContent = "";
    this.renderSessionTag();
  }

  private hideFailureNotice(): void {
    this.noticePanel.hidden = true;
    this.shownNotice = undefined;
    this.renderSessionTag();
  }

  // Mirrors the legacy page's claim flow: trusted click only, one claim in
  // flight, source-based when a PREPARE has named one, and a refusal renders
  // its closed code on the button rather than dead-ending.
  private onTakeControlClick(event: MouseEvent): void {
    if (!event.isTrusted) return;
    this.claimControl(false);
  }

  // A host-driven manual claim (a workspace cell's own "Take control"
  // affordance): the same path as the notice button — one claim in
  // flight, source-based once a PREPARE has named one.
  requestControlTakeover(): void {
    this.claimControl(false);
  }

  // The controller calls this inside its one synchronous identity commit,
  // before it opens the target endpoint. It seals A input immediately,
  // cancels A-only takeover work, flushes/rescopes the draft and image
  // authority, and clears the one renderer without replacing it.
  replaceSessionPresentation(value: Readonly<{
    sessionName: string;
    aliasLabel?: string;
    composerStorageScope: string;
    historyRows?: ScrollbackRows;
    stageImage?: (file: File, signal: AbortSignal) => Promise<ComposerStagedImage>;
  }>): void {
    if (this.closed) return;
    this.claimIntent = true;
    this.closePresentationOverlays();
    this.exitSelectMode();
    this.endpointOperation += 1;
    this.takeoverController?.abort();
    this.takeoverController = undefined;
    this.takeoverPending = false;
    this.sessionName = value.sessionName;
    this.sessionAlias = value.aliasLabel;
    this.sessionDraftScope = value.composerStorageScope;
    this.scrollbackScope = value.composerStorageScope;
    this.composer?.rescope(value.composerStorageScope, value.stageImage);
    this.prepared = undefined;
    this.committed = false;
    this.controlGranted = false;
    this.fitPending = false;
    // A session identity commit is the terminal owner for a pending refit on
    // the previous incarnation. The replacement's later COMMIT must never
    // settle or inherit that operation's input-drop accounting.
	this.settlePendingRefit();
    this.replaying = false;
    this.knownSource = undefined;
    this.terminal.reset();
    this.applyScrollbackRows(value.historyRows ?? readTerminalScrollbackRows(this.scrollbackScope));
    // The transcript has just been cleared and the new session has not
    // committed anything yet, so the viewport is blank. Restore the same
    // honest loading surface a first attach shows — named for the session
    // now being awaited — and take down the previous session's terminal
    // notice, which described a session this pane no longer holds. The
    // accepted COMMIT hides the surface again on the ordinary path, and a
    // typed failure replaces it, exactly as on first attach.
    this.loadingIdentityLabel.textContent = value.sessionName || "Unified terminal";
    this.hideFailureNotice();
    this.loadingPanel.hidden = false;
    this.updateGeometryControl();
    this.sessionSwitcher?.render();
  }

  sessionSwitchState(): Readonly<{
    transcriptOwner: string | null;
    draftScope: string | null;
    admission: Readonly<{ prepared: boolean; committed: boolean; controlGranted: boolean }>;
  }> {
    return Object.freeze({
      transcriptOwner: this.sessionName ?? null,
      draftScope: this.sessionDraftScope,
      admission: Object.freeze({ prepared: this.prepared !== undefined, committed: this.committed, controlGranted: this.controlGranted }),
    });
  }

  failSessionSwitch(reason: string): void {
    this.options.port.detach?.(reason);
    this.showFailureNotice(reason);
  }

  // The control-takeover claim, shared by the manual button and the automatic
  // cross-device reopen. Manual claims surface progress and refusals on the
  // button; automatic claims surface them on the reconnecting strip and fall
  // back to the manual affordance if the claim itself fails, so an auto path
  // can never strand the page.
  private claimControl(auto: boolean): void {
    if (this.closed || this.takeoverPending || !this.options.takeControl || !this.options.port.takeControl) return;
    this.takeoverPending = true;
    if (auto) {
      this.hideFailureNotice();
      this.connectionStatus.textContent = this.reconnectingText();
    } else {
      this.takeControlButton.disabled = true;
      this.takeControlButton.textContent = "Taking control…";
    }
    const controller = new AbortController();
    const operation = this.endpointOperation;
    this.takeoverController = controller;
    void this.options.takeControl(this.knownSource, controller.signal).then((endpoint) => {
      if (controller.signal.aborted || operation !== this.endpointOperation) return;
      this.takeoverController = undefined;
      this.takeoverPending = false;
      if (this.closed) return;
      this.hideFailureNotice();
      // The COMMIT this claim leads to displaced whoever held the lease.
      this.admittedViaTakeover = true;
      this.options.port.takeControl?.(endpoint);
    }).catch((error: unknown) => {
      if (controller.signal.aborted || operation !== this.endpointOperation) return;
      this.takeoverController = undefined;
      this.takeoverPending = false;
      if (this.closed) return;
      const code = error instanceof Error && /^[a-z][a-z0-9_]{0,63}$/.test(error.message) ? error.message : "takeover_unavailable";
      if (auto) {
        // The automatic claim could not be made; offer the manual affordance
        // rather than loop or dead-end silently.
        this.connectionStatus.textContent = "";
        this.showFailureNotice("lease_held");
      } else {
        this.takeControlButton.textContent = `Takeover unavailable · ${code}`;
        this.takeControlButton.disabled = false;
      }
    });
  }

  destroy(): void {
    if (this.closed) return;
	this.settlePendingRefit();
    this.selectRestoreKeyboard = false;
    this.exitSelectMode();
    // Restore every reusable presentation node before the controller becomes
    // closed; disclosure cleanup must run before the page becomes unavailable.
    this.closePresentationOverlays();
    this.closed = true;
    this.endpointOperation += 1;
    this.takeoverController?.abort();
    this.takeoverController = undefined;
    this.sessionInventoryAbort?.abort();
    this.sessionInventoryAbort = undefined;
    this.restoreKeyboardOnCommit = false;
    this.clipboardPanel?.dispose();
    this.preferenceSubscription?.dispose();
    this.preferenceSubscription = undefined;
    if (this.fontSaveTimer !== undefined) clearTimeout(this.fontSaveTimer);
    this.fontSaveTimer = undefined;
    this.fontIntent = undefined;
    this.composerFontIntent = undefined;
    for (const feedback of this.copyFeedback.values()) feedback.dispose();
    this.copyFeedback.clear();
    this.copyAnnouncementAttempts.clear();
    // Before terminal teardown: destroy() flushes any pending draft to
    // storage and detaches the composer's own observers and listeners.
    this.composer?.destroy();
    if (this.refusalTimer !== undefined) clearTimeout(this.refusalTimer);
    if (this.toastTimer !== undefined) clearTimeout(this.toastTimer);
    if (this.reconcileFrame !== undefined) cancelAnimationFrame(this.reconcileFrame);
    this.resizeObserver.disconnect();
    for (const dispose of this.terminalDisposables.splice(0)) dispose.dispose();
    for (const cleanup of this.cleanupListeners.splice(0)) cleanup();
    // Before terminal.dispose(): removing the sole sentinel needs the textarea.
    this.iosBackspace.destroy();
    this.terminal.dispose();
    this.options.port.destroy?.();
  }

  private registerCopyFeedback(
    button: HTMLButtonElement,
    glyph: string,
    readyLabel: string,
    trailingLabel = "",
  ): void {
    const existingGlyph = button.querySelector<HTMLElement>(".persea-unified-sheet__glyph");
    const glyphNode = existingGlyph ?? document.createElement("span");
    glyphNode.classList.add("persea-copy-feedback__glyph");
    glyphNode.textContent = glyph;
    glyphNode.setAttribute("aria-hidden", "true");
    if (existingGlyph === null && trailingLabel === "") {
      button.replaceChildren(glyphNode);
    } else if (existingGlyph === null) {
      const label = document.createElement("span");
      label.className = "persea-copy-feedback__label";
      label.textContent = trailingLabel;
      button.replaceChildren(glyphNode, label);
    }
    const feedback = new CopyFeedback((state: CopyFeedbackState) => {
      button.dataset.copyState = state;
      glyphNode.textContent = state === "done" ? "✓" : state === "failed" ? "✗" : glyph;
      const label = state === "done" ? "Copied" : state === "failed" ? "Copy failed" : readyLabel;
      button.title = label;
      button.setAttribute("aria-label", label);
    });
    this.copyFeedback.set(button, feedback);
  }

  private setCopyReady(button: HTMLButtonElement, ready: boolean): void {
    this.copyFeedback.get(button)?.setReady(ready);
  }

  private selectionChanged(button: HTMLButtonElement, ready: boolean): void {
    this.copyFeedback.get(button)?.selectionChanged(ready);
  }

  private flashCopy(button: HTMLButtonElement, copied: boolean): void {
    this.copyFeedback.get(button)?.flash(copied);
  }

  private beginCopy(button: HTMLButtonElement): number {
    return this.copyFeedback.get(button)?.beginAttempt() ?? 0;
  }

  private settleCopy(button: HTMLButtonElement, attempt: number, copied: boolean): void {
    this.copyFeedback.get(button)?.settleAttempt(attempt, copied);
  }

  private announceCopy(region: HTMLElement, message: string): void {
    const attempt = (this.copyAnnouncementAttempts.get(region) ?? 0) + 1;
    this.copyAnnouncementAttempts.set(region, attempt);
    region.textContent = "";
    queueMicrotask(() => {
      if (!this.closed && attempt === this.copyAnnouncementAttempts.get(region)) region.textContent = message;
    });
  }

  private acceptPrepare(frame: Prepare): void {
    if (frame.history.length !== 0 || frame.kind === "HISTORY") {
      this.options.port.finalize({ generation: this.generation, cause: "ADMISSION_INVARIANT" });
      return;
    }
    if (frame.kind === "RESIZE") {
      // A committed geometry event, carried in the same ordered stream as
      // output. It is not an admission: the transcript, the cursor, the
      // selection and the reader's scroll position all have to survive it.
      const active = this.prepared;
      if (!active || !this.committed || frame.source !== active.source || frame.epoch !== active.epoch || frame.cut !== active.cut) {
        this.options.port.finalize({ generation: this.generation, cause: "ACTIVE_TUPLE_MISMATCH" });
        return;
      }
      this.applyCommittedGeometry(frame);
      return;
    }
	const pendingRefit = this.pendingRefit;
	if (pendingRefit && this.endpointOperation === pendingRefit.predecessorEndpointOperation
	  && this.generation !== pendingRefit.predecessorGeneration) {
	  if (!pendingRefit.awaitingSuccessor || pendingRefit.successorSource === "" || frame.source !== pendingRefit.successorSource) {
		this.options.port.finalize({ generation: this.generation, cause: "ACTIVE_TUPLE_MISMATCH" });
		return;
	  }
	  const expected = Object.freeze({ generation: this.generation, source: frame.source, epoch: frame.epoch, cut: frame.cut });
	  if (pendingRefit.expected === undefined) pendingRefit.expected = expected;
	}
    this.prepared = frame;
    this.committed = false;
    this.controlGranted = false;
    this.modeReceived = false;
    this.fitPending = false;
    this.knownSource = frame.source;
    // A fresh admission supersedes any earlier failure surface.
    this.hideFailureNotice();
    this.connectionStatus.textContent = "";
    this.options.rememberSource(frame.source);
    this.replaying = true;
    this.projectedRow = -1;
    this.normalAnchor = { bufferType: "normal", row: 0, fraction: 0, scalarRemainder: 0, following: true };
    // Reset exactly once per admission, then size to the generation's birth
    // geometry. Replay re-derives the current geometry from the committed
    // events that follow, in the order the live session produced them, so a
    // reload wraps its lines the same way the live screen did.
    this.terminal.reset();
    this.applyTerminalGeometry(frame.columns, frame.rows);
    this.terminal.write(frame.replay, this.guardedWriteCallback(this.generation, "REPLAY_FAILED", () => {
      this.replaying = false;
      if (this.prepared !== frame || this.closed) return;
      this.syncNativeScroll(true);
      // The resize observer can run against the seed grid before admission.
      // Fit again with the admitted geometry after xterm has painted it.
      requestAnimationFrame(() => this.autoFitFont());
      this.updateGeometryControl();
      this.send({ type: "READY", version: 1, source: frame.source, epoch: frame.epoch, cut: frame.cut });
    }));
  }

  // applyCommittedGeometry is the mid-session path, and it is deliberately not
  // the admission path: it must never call reset(). Both the normal and the
  // alternate buffer participate in a public xterm resize.
  private applyCommittedGeometry(frame: Prepare): void {
    const anchor = this.captureAnchor();
    this.applyTerminalGeometry(frame.columns, frame.rows);
    this.fitPending = false;
    this.syncNativeScroll(anchor.following, anchor);
    this.updateGeometryControl();
  }

  private applyTerminalGeometry(columns: number, rows: number): void {
    this.committedColumns = columns;
    this.committedRows = rows;
    if (this.terminal.cols !== columns || this.terminal.rows !== rows) this.terminal.resize(columns, rows);
  }

  // --- explicit vertical fit ------------------------------------------------

  // --- the session tag --------------------------------------------------
  //
  // One derivation, read live on every render from the state the rest of the
  // page already keeps: the failure notice, the reconnect strip, and the
  // admission triple (prepared / committed / replaying). Nothing is latched
  // and no second state machine exists, so the dot cannot contradict the
  // strip or the notice it sits above.
  private connectionPhase(): "live" | "pending" | "down" {
    if (this.closed) return "down";
    if (!this.noticePanel.hidden) return "down";
    if (this.connectionStatus.textContent !== "") return "pending";
    if (this.prepared !== undefined && this.committed && !this.replaying) return "live";
    return "pending";
  }

  private renderSessionTag(): void {
    const phase = this.connectionPhase();
    const name = this.sessionName === undefined || this.sessionName === "" ? "terminal" : this.sessionName;
    const alias = this.sessionAlias ?? "";
    const status = UNIFIED_PHASE_WORDS[phase];
    this.identityDot.dataset.state = phase;
    this.identityName.textContent = name;
    this.identityAlias.textContent = alias;
    this.identityAlias.hidden = alias === "";
    // The dot is decoration; the state is in the accessible name, so nothing
    // here asks a reader to interpret a colour.
    const spoken = `Session ${name}${alias === "" ? "" : ` (${alias})`} \u2014 ${status}. Show session details`;
    this.identityTag.setAttribute("aria-label", spoken);
    this.identityTag.title = spoken;
    if (this.identityDetails.hidden) return;
    this.renderIdentityDetails(phase);
  }

  // The details an operator occasionally needs, none of which is worth row
  // width. Realm, server and session id are decoded from the draft scope this
  // page already holds; the size is the committed geometry, the same number
  // the readout renders. Anything the page does not hold is not listed at all;
  // an empty row would be a claim.
  private renderIdentityDetails(phase: "live" | "pending" | "down"): void {
    const scope = this.sessionDraftScope ? sessionScopeIdentity(this.sessionDraftScope) : undefined;
    const rows: Array<readonly [string, string]> = [["Status", UNIFIED_PHASE_WORDS[phase]]];
    if (scope) rows.push(["Server", `${scope.realm} \u00b7 ${scope.server}`]);
    if (this.sessionName) rows.push(["Session", this.sessionName]);
    if (this.sessionAlias) rows.push(["Alias", this.sessionAlias]);
    if (scope) rows.push(["Session id", scope.sessionId]);
    if (this.committedColumns > 0 && this.committedRows > 0) {
      rows.push(["Size", `${this.committedColumns}\u00d7${this.committedRows}`]);
    }
    const key = rows.map(([term, value]) => `${term} ${value}`).join("");
    if (this.identityDetailsKey === key) return;
    this.identityDetailsKey = key;
    this.identityFacts.replaceChildren(...rows.map(([term, value]) => {
      const row = document.createElement("div");
      row.className = "persea-unified-identity__row";
      const term_ = document.createElement("dt");
      term_.textContent = term;
      const detail = document.createElement("dd");
      detail.textContent = value;
      row.append(term_, detail);
      return row;
    }));
  }

  // Presentation only, exactly like the overflow popover: it writes its own
  // hidden flag and the tag's expanded state. The popover is positioned, so
  // opening it changes no box the ResizeObserver watches.
  private setIdentityDetails(open: boolean, coordinated = false): void {
    if (this.closed || this.identityDetails.hidden === !open) return;
    if (open && !coordinated) this.claimPopover("tag");
    if (open) {
      this.renderIdentityDetails(this.connectionPhase());
      if (this.options.sessionSwitch) void this.loadSessionInventory(false);
    }
    this.identityDetails.hidden = !open;
    this.identityTag.setAttribute("aria-expanded", open ? "true" : "false");
    if (!open && this.popoverOwner === "tag") this.popoverOwner = "none";
  }

  private updateGeometryControl(): void {
    if (this.closed) return;
    const readout = this.committedColumns > 0 && this.committedRows > 0
      ? `${this.committedColumns}\u00d7${this.committedRows}`
      : "";
    this.geometryReadout.textContent = readout;
    this.geometryReadout.setAttribute("aria-label", readout === "" ? "View and size" : `View and size, committed ${readout}`);
    const fitRowsMeasurement = this.measureFitRows();
    const fitWidthMeasurement = this.measureFitColumns();
    const fitRowsAvailability = this.geometryAvailability("fit_rows", false, fitRowsMeasurement);
    const fitWidthAvailability = this.geometryAvailability("fit_width", false, fitWidthMeasurement);
    for (const form of this.geometryForms) {
      // A focus transfer can briefly leave the form unfocused. While Apply is
      // pending, the committed size is still old and must not replace its draft.
      if (!this.fitPending && !form.root.matches(":focus-within")) {
        form.columns.value = this.committedColumns > 0 ? String(this.committedColumns) : "";
        form.rows.value = this.committedRows > 0 ? String(this.committedRows) : "";
      }
      const known = this.committedColumns > 0 && this.committedRows > 0;
      form.columns.disabled = !known;
      form.rows.disabled = !known;
      const applyAvailability = this.geometryApplyAvailability(form);
      // Explainable actions remain native-focusable. aria-disabled paints the
      // state; the guarded activation below owns both pointer and keyboard
      // suppression and publishes the same bounded reason inside View.
      form.apply.disabled = false;
      form.apply.textContent = this.refitPending ? "Refitting…" : this.fitPending ? "Applying…" : "Apply";
      form.fitRows.disabled = false;
      form.fitWidth.disabled = false;
      this.renderGeometryAvailability(form.fitRows, fitRowsAvailability, "Fit terminal rows to the visible height");
      this.renderGeometryAvailability(form.fitWidth, fitWidthAvailability,
        fitWidthMeasurement === undefined
          ? "Fit terminal width to the visible area"
          : `Rebuild this session at ${fitWidthMeasurement} columns to fit the visible width`);
      this.renderGeometryAvailability(form.apply, applyAvailability, "Apply the typed columns and rows");
      const currentAction = form.reason.dataset.action as UnifiedGeometryAction | undefined;
      if (currentAction !== undefined && !form.reason.hidden) {
        const current = currentAction === "fit_rows" ? fitRowsAvailability
          : currentAction === "fit_width" ? fitWidthAvailability : applyAvailability;
        if (current.enabled) this.clearGeometryReason(form);
        else form.reason.value = current.message;
      }
    }
    // Every availability-changing transition passes through here (commit,
    // transport loss, seal open/close, committed geometry), so this is the
    // composer's one refresh choke point.
    this.composer?.refreshAvailability();
    this.renderSheetState();
    this.renderSessionTag();
    // The one-time hint: judged at the first moment after an admission that
    // an explicit Fit is available, and only until it has been shown once.
    if (this.fitHintPending && fitRowsAvailability.enabled) {
      this.fitHintPending = false;
      if (this.fitHintState === "unseen" && this.fitRowsDisagree()) {
        this.fitHintState = "shown";
        this.fitHint.hidden = false;
      }
    }
  }

  private geometryAvailability(action: UnifiedGeometryAction, explicit: boolean, measurement?: number): UnifiedGeometryAvailability {
    return unifiedGeometryAvailability({
      action,
      closed: this.closed,
      prepared: this.prepared !== undefined,
      committed: this.committed,
      controlGranted: this.controlGranted,
      replaying: this.replaying,
      capabilityMode: this.options.capabilityMode,
      fitPending: this.fitPending,
      refitPending: this.refitPending,
      committedColumns: this.committedColumns,
      committedRows: this.committedRows,
      keyboardGuard: !explicit && this.softwareKeyboardGuardActive(),
      bufferType: this.activeBufferType,
      ...(measurement === undefined ? {} : { measurement }),
    });
  }

  private geometryApplyAvailability(form: GeometryFormView): UnifiedGeometryAvailability {
    const columns = /^(?:0|[1-9][0-9]*)$/.test(form.columns.value) ? Number(form.columns.value) : undefined;
    if (columns !== undefined && columns !== this.committedColumns) {
      return this.geometryAvailability("fit_width", true, columns);
    }
    return this.geometryAvailability("apply", true);
  }

  private renderGeometryAvailability(button: HTMLButtonElement, availability: UnifiedGeometryAvailability, enabledTitle: string): void {
    button.setAttribute("aria-disabled", availability.enabled ? "false" : "true");
    button.dataset.availability = availability.code;
    button.title = availability.enabled ? enabledTitle : availability.message;
  }

  private showGeometryReason(form: GeometryFormView, action: UnifiedGeometryAction, availability: UnifiedGeometryAvailability): void {
    form.reason.dataset.action = action;
    form.reason.value = availability.message;
    form.reason.hidden = false;
  }

  private clearGeometryReason(form: GeometryFormView): void {
    delete form.reason.dataset.action;
    form.reason.value = "";
    form.reason.hidden = true;
  }

  private activateGeometry(
    form: GeometryFormView,
    action: UnifiedGeometryAction,
    availability: UnifiedGeometryAvailability,
    activate: () => void,
  ): boolean {
    if (!availability.enabled) {
      this.showGeometryReason(form, action, availability);
      return false;
    }
    this.clearGeometryReason(form);
    activate();
    return true;
  }

  // Whether the rows that would fit and the committed rows disagree by more
  // than the hint threshold. A measurement only — it never resizes anything.
  private fitRowsDisagree(): boolean {
    if (!(this.committedRows > 0)) return false;
    const rows = this.measureFitRows();
    if (rows === undefined) return false;
    return Math.abs(rows - this.committedRows) > FIT_HINT_MISMATCH * this.committedRows;
  }

  private dismissFitHint(): void {
    this.fitHintState = "done";
    this.fitHintPending = false;
    this.fitHint.hidden = true;
  }

  // --- View and size popover --------------------------------------------------

  private setViewPopover(open: boolean, coordinated = false): void {
    if (this.closed || this.viewPopoverOpen === open) return;
    if (open && !coordinated) this.claimPopover("view");
    if (!open && document.activeElement instanceof HTMLElement && this.viewPopover.contains(document.activeElement)) {
      document.activeElement.blur();
    }
    this.viewPopoverOpen = open;
    this.viewPopover.hidden = !open;
    // The Fit width measurement depends on the current viewport, so refresh
    // the block's availability as the popover opens.
    if (open) this.updateGeometryControl();
    else for (const form of this.geometryForms) this.clearGeometryReason(form);
    this.geometryReadout.setAttribute("aria-expanded", open ? "true" : "false");
    if (!open && this.popoverOwner === "view") this.popoverOwner = "none";
  }

  private openExplainer(topic: UnifiedExplainerTopic | undefined, anchor: HTMLElement): void {
    if (this.closed) return;
    this.claimPopover("explainer");
    this.explainerReturnFocus = anchor;
    const rows = topic === undefined ? UNIFIED_EXPLAINERS : UNIFIED_EXPLAINERS.filter((entry) => entry.topic === topic);
    this.explainerHeading.textContent = topic === undefined ? "Terminal help" : rows[0]?.title ?? "Terminal help";
    if (topic === undefined) {
      const list = document.createElement("dl");
      list.className = "persea-unified-explainer__list";
      for (const entry of rows) {
        const title = document.createElement("dt");
        title.textContent = entry.title;
        const detail = document.createElement("dd");
        detail.textContent = entry.detail;
        list.append(title, detail);
      }
      const tools = document.createElement("p");
      tools.className = "persea-unified-explainer__tools";
      const traceButton = document.createElement("button");
      traceButton.type = "button";
      traceButton.className = "persea-unified-explainer__tool";
      traceButton.textContent = "Copy input trace";
      traceButton.setAttribute("aria-label", "Copy the recent input trace for a bug report");
      this.cleanupListeners.push(bindGenerationFencedClickActivation(
        traceButton,
        () => void this.copyInputTrace(),
        () => !this.closed && !this.explainerPopover.hidden,
        () => this.keyInteractionGeneration,
      ));
      const traceNote = document.createElement("span");
      traceNote.className = "persea-unified-explainer__tool-note";
      traceNote.textContent = "For a typing or dictation bug report: the last few hundred key, input, and send events on this page, including the text they carried.";
      tools.append(traceButton, traceNote);
      this.explainerBody.replaceChildren(list, tools);
    } else {
      const detail = document.createElement("p");
      detail.textContent = rows[0]?.detail ?? "";
      this.explainerBody.replaceChildren(detail);
    }
    this.explainerPopover.hidden = false;
  }

  private traceInput(kind: string, fields: Record<string, unknown>): void {
    const textarea = this.terminal.textarea;
    const router = this.iosBackspace.instrumentation();
    this.inputTrace.push(Object.freeze({
      t: Math.round(performance.now()),
      kind,
      ...fields,
      field: traceText(textarea?.value),
      fieldLength: textarea?.value.length ?? null,
      selection: textarea === undefined ? null : [textarea.selectionStart, textarea.selectionEnd],
      router: {
        active: router.active,
        composing: router.composing,
        tracked: router.trackedEmissionCount,
        pendingInsert: router.pendingInsertKind,
        prelude: router.dictationPreludePending,
        replacementDisabled: router.replacementDisabledForFocusSession,
        rewrites: router.replacementRewrites,
      },
    }));
    if (this.inputTrace.length > INPUT_TRACE_LIMIT) this.inputTrace.splice(0, this.inputTrace.length - INPUT_TRACE_LIMIT);
  }

  // The trace goes to the local clipboard only — never to the clips store,
  // where it would sit beside the operator's own snippets.
  private async copyInputTrace(): Promise<void> {
    const report = {
      version: 1,
      capturedAt: new Date().toISOString(),
      userAgent: navigator.userAgent,
      coarsePointer: this.coarsePointerQuery?.matches ?? null,
      geometry: { columns: this.terminal.cols, rows: this.terminal.rows },
      entries: this.inputTrace,
    };
    try {
      await navigator.clipboard.writeText(JSON.stringify(report, null, 1));
      this.showToast(`Input trace copied (${this.inputTrace.length} events)`);
    } catch {
      this.showToast("Could not copy the input trace");
    }
  }

  private closeExplainer(coordinated = false): void {
    if (this.explainerPopover.hidden) return;
    this.explainerPopover.hidden = true;
    this.explainerReturnFocus = undefined;
    if (!coordinated && this.popoverOwner === "explainer") this.popoverOwner = "none";
  }

  // A transport transition owns the whole terminal surface. No pane-local
  // disclosure may remain above a reconnect, replacement loading panel, or
  // typed terminal failure. This is deliberately focus-neutral: lifecycle
  // teardown never manufactures a focus target on a surface that is ending.
  private closePresentationOverlays(keyAuthorityAlreadyCut = false): void {
    if (!keyAuthorityAlreadyCut) this.resetKeyInteractionAuthorityForLifecycle();
    if (this.viewPopoverOpen) this.setViewPopover(false, true);
    if (!this.identityDetails.hidden) this.setIdentityDetails(false, true);
    if (this.sheetOpen) this.setSheet(false, true);
    if (!this.explainerPopover.hidden) this.closeExplainer(true);
    this.composer?.closeTypographyPopover(true);
    this.popoverOwner = "none";
  }

  private claimPopover(owner: UnifiedPopoverOwner): void {
    if (owner !== "view" && this.viewPopoverOpen) this.setViewPopover(false, true);
    if (owner !== "tag" && !this.identityDetails.hidden) this.setIdentityDetails(false, true);
    if (owner !== "sheet" && this.sheetOpen) this.setSheet(false, true);
    if (owner !== "explainer" && !this.explainerPopover.hidden) this.closeExplainer(true);
    if (owner !== "typography") this.composer?.closeTypographyPopover(true);
    this.popoverOwner = owner;
  }

  // the software-keyboard guard protects MEASURED fits (Fit rows,
  // Fit width read the visible band, which the keyboard has just shrunk). A
  // typed size is the operator's explicit number and is not measured, so an
  // explicit request ignores the guard — on a phone the keyboard is still up
  // when the operator taps Apply right after typing, and refusing there made
  // Apply look dead.
  // The scale-aware software-keyboard guard. A shrunken visual viewport while
  // the keyboard is up is not the space the terminal has; measuring then would
  // shrink the real session to the size of a keyboard. Pinch-zoom shrinks the
  // visual viewport too and is likewise not a measurement opportunity. Real iOS
  // keyboard behaviour stays hardware-pending; no desktop engine reproduces it.
  private softwareKeyboardGuardActive(): boolean {
    const viewport = this.insetViewport;
    if (!viewport) return false;
    if (viewport.scale < 0.99 || viewport.scale > 1.01) return true;
    return viewport.height + 1 < window.innerHeight;
  }

  // Measures how many rows fit in the terminal route's actually available
  // content height at the current font size. Columns are not measured: the
  // browser is never authoritative for width.
  private measureFitRows(): number | undefined {
    const cellHeight = this.measureCellHeight();
    if (cellHeight === undefined) return undefined;
    const style = getComputedStyle(this.host);
    const verticalPadding = Number.parseFloat(style.paddingTop) + Number.parseFloat(style.paddingBottom);
    const available = this.viewport.clientHeight - (Number.isFinite(verticalPadding) ? verticalPadding : 0);
    if (!Number.isFinite(available) || available <= 0) return undefined;
    const rows = Math.floor(available / cellHeight);
    return Number.isFinite(rows) && rows > 0 ? rows : undefined;
  }

  // the Terminal size block reads top-down the way the operator
  // thinks about it — first the two fit-to-view actions, then the explicit
  // form `Columns [ ] Rows [ ] [Apply]`. Apply is ONE request: a row-only
  // change is the live rows request; a column change is the generation refit,
  // which now carries the typed rows too. Nothing here can emit a width
  // through the attachment frame.
  private createGeometryForm(surface: "view"): HTMLElement {
    const root = document.createElement("div");
    root.className = "persea-unified-size";
    root.dataset.surface = surface;
    const actions = document.createElement("div");
    actions.className = "persea-unified-size__actions";
    const fitRows = document.createElement("button");
    fitRows.type = "button";
    fitRows.className = "persea-unified-size__secondary persea-unified-size__fit";
    fitRows.textContent = "↕ Fit rows";
    fitRows.title = "Fit terminal rows to the visible height";
    fitRows.setAttribute("aria-label", "Fit rows");
    const fitWidth = document.createElement("button");
    fitWidth.type = "button";
    fitWidth.className = "persea-unified-size__secondary persea-unified-size__fit-width";
    fitWidth.textContent = "↔ Fit width";
    fitWidth.title = "Rebuild this terminal at the column count that fits the visible width";
    fitWidth.setAttribute("aria-label", "Fit width");
    actions.append(fitRows, fitWidth);
    const form = document.createElement("div");
    form.className = "persea-unified-size__form";
    // a compact row. The visible caption is short; the
    // accessible name stays the full word.
    const field = (caption: string, name: string): { label: HTMLLabelElement; input: HTMLInputElement } => {
      const label = document.createElement("label");
      const text = document.createElement("span");
      text.className = "persea-unified-size__caption";
      text.textContent = caption;
      const input = document.createElement("input");
      input.type = "text";
      input.inputMode = "numeric";
      input.setAttribute("aria-label", name);
      label.append(text, input);
      return { label, input };
    };
    const columnsField = field("Cols", "Columns");
    const rowsField = field("Rows", "Rows");
    const columns = columnsField.input;
    const rows = rowsField.input;
    const apply = document.createElement("button");
    apply.type = "button";
    apply.className = "persea-unified-size__primary";
    apply.textContent = "Apply";
    apply.title = "Apply the typed columns and rows";
    const reason = document.createElement("output");
    reason.id = `persea-unified-geometry-reason-${(geometrySerial += 1)}`;
    reason.className = "persea-unified-size__reason";
    reason.setAttribute("role", "status");
    reason.setAttribute("aria-live", "polite");
    reason.hidden = true;
    for (const button of [fitRows, fitWidth, apply]) button.setAttribute("aria-describedby", reason.id);
    const view: GeometryFormView = Object.freeze({ root, columns, rows, apply, fitRows, fitWidth, reason });
    this.cleanupListeners.push(bindExplainedTapActivation(
      fitRows,
      () => this.activateGeometry(view, "fit_rows", this.geometryAvailability("fit_rows", false, this.measureFitRows()), () => this.requestVerticalFit()),
      () => this.openExplainer("fit", fitRows),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    this.cleanupListeners.push(bindExplainedTapActivation(
      fitWidth,
      () => this.activateGeometry(view, "fit_width", this.geometryAvailability("fit_width", false, this.measureFitColumns()), () => this.requestHorizontalFit()),
      () => this.openExplainer("fit", fitWidth),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    this.cleanupListeners.push(bindExplainedTapActivation(
      apply,
      () => this.activateGeometry(view, "apply", this.geometryApplyAvailability(view), () => this.applyTypedGeometry(columns.value, rows.value)),
      () => this.openExplainer("size", apply),
      () => undefined,
      () => !this.closed,
      () => this.keyInteractionGeneration,
    ));
    const onInput = (): void => this.updateGeometryControl();
    columns.addEventListener("input", onInput);
    rows.addEventListener("input", onInput);
    this.cleanupListeners.push(
      () => columns.removeEventListener("input", onInput),
      () => rows.removeEventListener("input", onInput),
    );
    form.append(columnsField.label, rowsField.label, apply);
    root.append(actions, form, reason);
    this.geometryForms.push(view);
    return root;
  }

  // The one Apply. Validation is strict and reject-not-clamp, like the broker.
  private applyTypedGeometry(columnsText: string, rowsText: string): void {
    if (!/^(?:0|[1-9][0-9]*)$/.test(columnsText) || !/^(?:0|[1-9][0-9]*)$/.test(rowsText)) {
      this.showRefusalNotice("Terminal size must use whole decimal numbers");
      return;
    }
    const columns = Number(columnsText);
    const rows = Number(rowsText);
    if (!Number.isSafeInteger(columns) || columns < 20 || columns > 300) {
      this.showRefusalNotice("Terminal columns must be between 20 and 300");
      return;
    }
    if (!validVerticalFit(columns, rows)) {
      this.showRefusalNotice(rows < MIN_FIT_ROWS || rows > MAX_FIT_ROWS
        ? `Terminal rows must be between ${MIN_FIT_ROWS} and ${MAX_FIT_ROWS}`
        : `${columns}×${rows} exceeds ${MAX_FIT_CELLS} cells`);
      return;
    }
    if (columns === this.committedColumns && rows === this.committedRows) {
      this.showToast(`Already ${columns}×${rows}`);
      return;
    }
    if (columns === this.committedColumns) {
      this.requestRowsOnly(rows, true);
      return;
    }
    const widthAvailability = this.geometryAvailability("fit_width", true, columns);
    if (!widthAvailability.enabled) {
      this.showRefusalNotice(widthAvailability.message);
      return;
    }
    this.requestWidthRefit(columns, rows === this.committedRows ? undefined : rows, true);
  }

  // Columns that fit the visible width at the current font: the rendered
  // grid's own cell width against the viewport's content box, the same two
  // measurements the automatic font fit trusts. Never automatic: only the
  // explicit ↔ tap reaches this.
  private measureFitColumns(): number | undefined {
    const bounds = this.renderedScreen()?.getBoundingClientRect();
    if (!bounds || bounds.width <= 0 || this.terminal.cols <= 0) return undefined;
    const cellWidth = bounds.width / this.terminal.cols;
    const style = getComputedStyle(this.host);
    const horizontalPadding = Number.parseFloat(style.paddingLeft) + Number.parseFloat(style.paddingRight);
    const available = this.viewport.clientWidth - (Number.isFinite(horizontalPadding) ? horizontalPadding : 0);
    if (!Number.isFinite(available) || available <= 0 || !Number.isFinite(cellWidth) || cellWidth <= 0) return undefined;
    const columns = Math.floor(available / cellWidth);
    return Number.isFinite(columns) && columns > 0 ? columns : undefined;
  }

  private requestHorizontalFit(): void {
    const columns = this.measureFitColumns();
    if (!this.geometryAvailability("fit_width", false, columns).enabled || columns === undefined) return;
    if (columns < 20 || columns > 300) {
      this.showRefusalNotice(`Fit not possible: ${columns} columns is outside 20–300`);
      return;
    }
    if (!validVerticalFit(columns, this.committedRows)) {
      this.showRefusalNotice(`Fit not possible: ${columns}×${this.committedRows} exceeds ${MAX_FIT_CELLS} cells`);
      return;
    }
    if (columns === this.committedColumns) {
      this.showToast(`Width already fits (${columns} columns)`);
      return;
    }
    this.requestWidthRefit(columns);
  }

  private requestWidthRefit(columns: number, rows?: number, explicit = false): void {
	if (!this.geometryAvailability("fit_width", explicit, columns).enabled) return;
	if (!Number.isSafeInteger(columns) || columns < 20 || columns > 300) {
	  this.showRefusalNotice("Terminal columns must be between 20 and 300");
	  return;
	}
	if (columns === this.committedColumns) return;
	const request = this.options.refitWidth;
	if (!request) return;
	const attempt = request(columns, rows);
	this.pendingRefit = {
	  operation: attempt.operation,
	  predecessorSource: attempt.predecessorSource,
	  predecessorIncarnation: attempt.predecessorIncarnation,
	  predecessorGeneration: this.generation,
	  predecessorEndpointOperation: this.endpointOperation,
	  successorSource: "",
	  awaitingSuccessor: false,
	  droppedBytes: 0,
	};
	this.closePresentationOverlays();
	this.updateGeometryControl();
	void attempt.result.then((result) => {
	  const pending = this.pendingRefit;
	  if (this.closed || pending?.operation !== attempt.operation
		|| pending.predecessorSource !== attempt.predecessorSource
		|| pending.predecessorIncarnation !== attempt.predecessorIncarnation) return;
	  if (result.ok) {
		pending.successorSource = result.successorSource;
		return;
	  }
	  if (result.disposition === "refused") {
		this.settlePendingRefit(attempt.operation);
	  } else if (result.disposition === "terminal") {
	    // The exact operation is terminal even if the socket close is a later
	    // task. Remove input authority now; no byte may slip into the uncertain
	    // geometry interval.
		this.settlePendingRefit(attempt.operation);
	    this.committed = false;
	    this.controlGranted = false;
	  }
	  // "uncertain" deliberately retains the seal until transport lifecycle
	  // produces an exact successor or terminal transition.
	  this.updateGeometryControl();
	  this.showRefusalNotice(result.message);
	});
  }

	// Every terminal owner of an explicit refit converges here. The operation
	// token makes settlement idempotent; the pane-local record owns exactly its
	// dropped bytes, which are reported once and are never queued or replayed.
	private settlePendingRefit(operation?: string): PendingWidthRefit | undefined {
	  const pending = this.pendingRefit;
	  if (!pending || (operation !== undefined && pending.operation !== operation)) return undefined;
	  this.pendingRefit = undefined;
	  if (pending.droppedBytes > 0) this.showToast(`${pending.droppedBytes} input bytes were not sent during width refit`);
	  return pending;
	}

  private requestVerticalFit(): void {
    // The first explicit fit retires the hint whatever its outcome.
    this.dismissFitHint();
    const rows = this.measureFitRows();
    if (!this.geometryAvailability("fit_rows", false, rows).enabled || rows === undefined) return;
    this.requestRowsOnly(rows);
  }

  // The sole attachment-frame construction site for explicit geometry. Width
  // remains the committed witness; scrollback owns generation refit.
  private requestRowsOnly(rows: number, explicit = false): void {
    const active = this.prepared;
    const availability = explicit
      ? this.geometryAvailability("apply", true)
      : this.geometryAvailability("fit_rows", false, rows);
    if (!active || !availability.enabled) {
      // A typed request is never dropped silently.
      if (explicit) this.showRefusalNotice(availability.message);
      return;
    }
    // Out-of-policy measurements are refused here, never clamped and never
    // sent: the broker's policy is reject-not-clamp, and the browser is not
    // the place to quietly invent a different request. The operator is told
    // why nothing happened.
    if (!validVerticalFit(this.committedColumns, rows)) {
      this.showRefusalNotice(rows < MIN_FIT_ROWS || rows > MAX_FIT_ROWS
        ? `Fit not possible: ${rows} rows is outside ${MIN_FIT_ROWS}–${MAX_FIT_ROWS}`
        : `Fit not possible: ${this.committedColumns}×${rows} exceeds ${MAX_FIT_CELLS} cells`);
      return;
    }
    // Already the committed height: complete as a no-op rather than spending a
    // deliberate tmux mutation on nothing.
    if (rows === this.committedRows) return;
    this.fitPending = true;
    this.updateGeometryControl();
    const result = this.send({
      type: "RESIZE_REQUEST", version: 1, source: active.source, epoch: active.epoch,
      columns: this.committedColumns, rows,
    });
    if (result !== "ACCEPTED") {
      this.fitPending = false;
      this.updateGeometryControl();
    }
  }

  private renderedScreen(): HTMLElement | undefined {
    return this.terminal.element?.querySelector<HTMLElement>(".xterm-screen") ?? undefined;
  }

  private measureCellHeight(): number | undefined {
    const height = this.renderedScreen()?.getBoundingClientRect().height ?? 0;
    const measured = height / this.terminal.rows;
    return Number.isFinite(measured) && measured > 0 ? measured : undefined;
  }

  // The rendered grid's own border-box footprint. Scroll geometry is derived
  // from this, never from the viewport: the viewport is what the reader has,
  // the grid is what there is to read.
  private measureGrid(): { cellHeight: number; width: number; height: number } | undefined {
    const rect = this.renderedScreen()?.getBoundingClientRect();
    if (!rect) return undefined;
    const cellHeight = rect.height / this.terminal.rows;
    if (!Number.isFinite(cellHeight) || cellHeight <= 0 || !(rect.width > 0)) return undefined;
    const style = getComputedStyle(this.host);
    const horizontal = Number.parseFloat(style.paddingLeft) + Number.parseFloat(style.paddingRight);
    const vertical = Number.parseFloat(style.paddingTop) + Number.parseFloat(style.paddingBottom);
    return {
      cellHeight,
      width: Math.ceil(rect.width + (Number.isFinite(horizontal) ? horizontal : 0)),
      height: Math.ceil(rect.height + (Number.isFinite(vertical) ? vertical : 0)),
    };
  }

  private isFollowingTail(): boolean {
    const maximum = Math.max(0, this.viewport.scrollHeight - this.viewport.clientHeight);
    return this.viewport.scrollTop >= maximum - 1;
  }

  private captureAnchor(
    bufferType = this.terminal.buffer.active.type,
    baseY = this.terminal.buffer.active.baseY,
  ): ScrollAnchor {
    const measured = this.cellHeightPixels || this.measureCellHeight() || 0;
    if (!(measured > 0)) {
      return { bufferType, row: 0, fraction: 0, scalarRemainder: 0, following: this.isFollowingTail() };
    }
    const top = Math.max(0, this.viewport.scrollTop);
    const row = Math.max(0, Math.min(baseY, Math.floor((top + 0.001) / measured)));
    const scalarRemainder = Math.max(0, top - row * measured);
    return {
      bufferType,
      row,
      fraction: Math.max(0, Math.min(0.999_999, scalarRemainder / measured)),
      scalarRemainder,
      following: this.isFollowingTail(),
    };
  }

  private syncNativeScroll(followTail: boolean, requestedAnchor?: ScrollAnchor): void {
    if (this.closed) return;
    const grid = this.measureGrid();
    if (grid === undefined) {
      this.scheduleReconcile();
      return;
    }
    const measured = grid.cellHeight;
    const previousHeight = this.cellHeightPixels || measured;
    const anchor = requestedAnchor ?? this.captureAnchor();
    this.cellHeightPixels = measured;
    this.host.style.height = `${grid.height}px`;
    this.host.style.width = `${grid.width}px`;
    const buffer = this.terminal.buffer.active;
    // Sizing the last screen by grid height rather than viewport height is what
    // keeps the final rows reachable: a grid taller than the viewport needs that
    // surplus in the range, or the bottom of the screen has nowhere to scroll to.
    const rangeHeight = Math.ceil(buffer.baseY * measured + Math.max(grid.height, this.viewport.clientHeight));
    this.scrollRange.style.height = `${rangeHeight}px`;
    this.scrollRange.style.width = `${grid.width}px`;
    // Read the range back from layout rather than trusting the height just
    // written. clientHeight is reported as a whole number, so under page zoom
    // it understates a fractional content box and the computed maximum lands
    // past what the browser will honour.
    const maximumTop = Math.max(0, this.viewport.scrollHeight - this.viewport.clientHeight);

    let targetTop = 0;
    if (buffer.type === "normal") {
      if (followTail) {
        targetTop = maximumTop;
      } else if (anchor.bufferType === buffer.type) {
        const remainder = Math.abs(previousHeight - measured) < 0.01
          ? anchor.scalarRemainder
          : anchor.fraction * measured;
        targetTop = anchor.row * measured + remainder;
      } else {
        targetTop = this.viewport.scrollTop;
      }
    } else if (buffer.type === this.projectedBuffer) {
      targetTop = Math.min(this.viewport.scrollTop, maximumTop);
    }
    this.viewport.scrollTop = Math.max(0, Math.min(maximumTop, targetTop));
    this.applyCanonicalScroll();
  }

  private applyCanonicalScroll(): void {
    const cellHeight = this.cellHeightPixels || this.measureCellHeight();
    if (cellHeight === undefined) {
      this.scheduleReconcile();
      return;
    }
    this.cellHeightPixels = cellHeight;
    const buffer = this.terminal.buffer.active;
    // At the bottom of the range the projected row is known outright: it is
    // baseY. Deriving it from scrollTop instead costs a whole row every time the
    // browser settles the scroll a fraction of a pixel short of the computed
    // maximum, which page zoom makes routine, and the row it costs is the one
    // carrying the cursor. History above the tail keeps the cell arithmetic,
    // where landing a fraction of a row out is invisible.
    const maximumTop = Math.max(0, this.viewport.scrollHeight - this.viewport.clientHeight);
    const target = this.viewport.scrollTop >= maximumTop - 1
      ? buffer.baseY
      : Math.max(0, Math.min(buffer.baseY, Math.floor((this.viewport.scrollTop + 0.001) / cellHeight)));
    this.host.style.top = `${target * cellHeight}px`;
    // Font measurement can make xterm reconcile its own scrollbar after our
    // last projection. Its live viewport must agree too; the cached row alone
    // cannot prove that the requested history is still what it is painting.
    if (target === this.projectedRow && buffer.type === this.projectedBuffer && buffer.viewportY === target) return;
    this.projectedRow = target;
    this.projectedBuffer = buffer.type;
    this.applyingProjection = true;
    try {
      this.terminal.scrollToLine(target);
    } finally {
      this.applyingProjection = false;
    }
  }

  private scheduleReconcile(): void {
    if (this.closed || this.reconcileFrame !== undefined || this.writesInFlight !== 0 || this.replaying || this.fitting) return;
    this.reconcileFrame = requestAnimationFrame(() => {
      this.reconcileFrame = undefined;
      if (this.closed || this.writesInFlight !== 0 || this.replaying || this.fitting) return;
      const anchor = this.captureAnchor();
      this.syncNativeScroll(anchor.following, anchor);
    });
  }

  private bufferChanged(type: "normal" | "alternate"): void {
    if (type === this.activeBufferType) return;
    if (this.activeBufferType === "normal") {
      this.normalAnchor = this.captureAnchor("normal", this.terminal.buffer.normal.baseY);
    }
    this.activeBufferType = type;
    this.projectedRow = -1;
    if (type === "alternate") this.syncNativeScroll(true);
    else this.syncNativeScroll(this.normalAnchor.following, this.normalAnchor);
    // Width refits are refused on the alternate screen.
    this.updateGeometryControl();
  }

  private selectionEdgeScroll(line: number): void {
    if (
      this.closed
      || !this.selectionPointerActive
      || !this.selectionEdgeActive
      || this.applyingProjection
      || this.nativeInput !== undefined
      || this.writesInFlight !== 0
      || this.replaying
      || this.fitting
    ) return;
    const cellHeight = this.cellHeightPixels || this.measureCellHeight();
    if (cellHeight === undefined) return;
    const row = Math.max(0, Math.min(this.terminal.buffer.active.baseY, Math.floor(line)));
    this.viewport.scrollTop = row * cellHeight;
    this.applyCanonicalScroll();
  }

  private scrollToBottom(): void {
    this.syncNativeScroll(true);
  }

  // The size xterm is painting right now, which is what a zoom step must move
  // by one. In auto mode this is the fitted size, not the stored preference:
  // stepping the stored number is exactly the defect auto sizing must prevent (a phone
  // fitted to 9px jumped to 15px on the first "Zoom in").
  private renderedFontSize(): number {
    return this.terminal.options.fontSize ?? UNIFIED_SEED_FONT_SIZE;
  }

  // The resize path. An explicit preference is not a starting point the
  // viewport may overrule, so a box change only re-anchors; auto refits.
  private autoFitFont(): void {
    if (this.closed || !this.prepared) return;
    if (this.fitting) {
      requestAnimationFrame(() => this.autoFitFont());
      return;
    }
    if (this.fontPreference !== null) {
      this.applyFontSize(this.fontPreference, this.captureAnchor());
      return;
    }
    this.fitFont();
  }

  // The "Fit font" control: back to auto, and stored as auto. Storing the
  // fitted *number* instead would make the preference viewport-derived — a
  // phone would pin 9px onto the same operator's desktop — and would leave no
  // way back from an explicit zoom.
  private requestAutoFont(): void {
    this.fontPreference = null;
    this.shell.dataset.fontBaseline = fontBaselineAttribute(null);
    this.autoFitFont();
    this.storeFontPreference(null);
  }

  private setFontSize(value: number): void {
    const next = Math.max(9, Math.min(24, Math.round(value)));
    this.fontPreference = next;
    this.shell.dataset.fontBaseline = fontBaselineAttribute(next);
    this.applyFontSize(next, this.captureAnchor());
    this.storeFontPreference(next);
  }

  // One debounced write per font decision, carrying the exact value it intends
  // (preferences R1 point 1). The identity fence and the settle/discard pair are
  // unchanged: only the value's type widened.
  private storeFontPreference(value: number | null): void {
    if (this.fontSaveTimer !== undefined) clearTimeout(this.fontSaveTimer);
    const preferences = this.options.preferences;
    if (preferences === undefined) {
      this.fontSaveTimer = undefined;
      this.fontIntent = undefined;
      return;
    }
    const intent = Object.freeze({ id: ++this.fontIntentID, value });
    this.fontIntent = intent;
    const timer = setTimeout(() => {
      if (this.fontSaveTimer === timer) this.fontSaveTimer = undefined;
      if (this.closed || this.fontIntent?.id !== intent.id) return;
      void preferences.update({ fontSize: intent.value }).then(
        (outcome) => this.settleFontIntent(intent, outcome),
        () => this.discardFontIntent(intent),
      );
    }, 500);
    this.fontSaveTimer = timer;
  }

  private settleFontIntent(intent: FontIntent, outcome: OperatorPreferenceOutcome): void {
    if (this.closed || this.fontIntent?.id !== intent.id) return;
    this.fontIntent = undefined;
    // Saved and unavailable writes publish the exact attempted value. A CAS
    // conflict publishes server authority instead. Reconcile only after this
    // matching intent is no longer allowed to mask that outcome.
    this.applyPreferences(this.options.preferences!.snapshot());
    // a Zoom that did not persist says so where the operator zoomed
    // (the bounded toast); the dashboard card reports the store, never this
    // page's write. Only the matching intent's settlement speaks — an older
    // operation that lost to a newer Zoom is silent, its value never shown.
    if (outcome === "unavailable") this.showToast("Zoom applied for this page only — preferences unavailable");
    else if (outcome === "conflict") this.showToast("Zoom not saved — changed on another device");
    else if (outcome === "refused") this.showToast("Zoom could not be saved");
  }

  // The preference service publishes its record synchronously and only then
  // resolves the operation, so an update() rejection means a subscriber threw
  // out of a publication this page has already been given. The write itself is
  // settled either way: this intent must stop masking the service's snapshot,
  // exactly as fulfilment would, or it stays the page's live authority for the
  // rest of the document's life and hides every later authoritative
  // publication. The identity fence is the same one the fulfilled path uses,
  // so a newer intent is never discarded by an older operation's failure, and
  // handling the rejection here is what keeps the void call from leaking an
  // unhandled rejection.
  private discardFontIntent(intent: FontIntent): void {
    if (this.closed || this.fontIntent?.id !== intent.id) return;
    this.fontIntent = undefined;
    this.applyPreferences(this.options.preferences!.snapshot());
    // the settled record is on screen, but the write's outcome is
    // unknown to this page — say so rather than nothing.
    this.showToast("Zoom could not be saved");
  }

  private applyFontSize(value: number, anchor: ScrollAnchor): void {
    if (this.closed || Math.abs((this.terminal.options.fontSize ?? UNIFIED_SEED_FONT_SIZE) - value) < 0.01) {
      this.syncNativeScroll(anchor.following, anchor);
      return;
    }
    this.fitting = true;
    const admission = this.prepared;
    const replaying = this.replaying;
    this.terminal.options.fontSize = value;
    requestAnimationFrame(() => {
      if (this.closed) return;
      this.fitting = false;
      // A replay may finish between the font change and this layout callback.
      // Its fresh transcript owns the viewport; an older anchor must not move it.
      if (!replaying && !this.replaying && this.prepared === admission) this.syncNativeScroll(anchor.following, anchor);
      else this.scheduleReconcile();
    });
  }

  private fitFont(): void {
    if (this.closed || this.fitting || !this.prepared) return;
    const screen = this.renderedScreen();
    const bounds = screen?.getBoundingClientRect();
    if (!bounds || bounds.width <= 0 || bounds.height <= 0 || this.viewport.clientWidth <= 0 || this.viewport.clientHeight <= 0) {
      this.scheduleReconcile();
      return;
    }
    const style = getComputedStyle(this.host);
    const horizontalPadding = Number.parseFloat(style.paddingLeft) + Number.parseFloat(style.paddingRight);
    const verticalPadding = Number.parseFloat(style.paddingTop) + Number.parseFloat(style.paddingBottom);
    const availableWidth = Math.max(1, this.viewport.clientWidth - horizontalPadding);
    const availableHeight = Math.max(1, this.viewport.clientHeight - verticalPadding);
    const anchor = this.captureAnchor();
    // xterm rounds cell dimensions to device pixels, so scaling the current
    // grid by a ratio is neither exact nor repeatable. Its DOM renderer updates
    // dimensions synchronously on font changes. Search those actual bounds in
    // hundredths of a pixel, within the same task so only the final size paints.
    let lower = 900;
    let upper = 2400;
    let best = lower;
    while (lower <= upper) {
      const candidate = Math.floor((lower + upper) / 2);
      this.terminal.options.fontSize = candidate / 100;
      const measured = screen!.getBoundingClientRect();
      if (measured.width <= availableWidth && measured.height <= availableHeight) {
        best = candidate;
        lower = candidate + 1;
      } else {
        upper = candidate - 1;
      }
    }
    this.terminal.options.fontSize = best / 100;
    // A replay's fresh transcript owns the viewport; an older anchor must not move it.
    if (this.replaying) this.scheduleReconcile();
    else this.syncNativeScroll(anchor.following, anchor);
  }

  // sendInput is sealed for exactly one pending explicit Fit.
  //
  // A resize is a cut, and the attachment's protocol reducer does not accept
  // input during one. The seal is deliberately a *drop*, not a queue: a
  // keystroke held across a cut and replayed afterwards would arrive in the
  // wrong screen at the wrong geometry, and one held across a refusal, a
  // transport loss, or a reconnect would arrive in a session the operator has
  // stopped looking at. Dropping is the only behaviour that cannot surprise.
  //
  // The seal opens again when the committed geometry event has been applied,
  // which is the first moment the page knows the resize the click asked for is
  // durable truth.
  private sendInput(data: Uint8Array): void {
    if (data.byteLength === 0) return;
    this.traceInput("send", { bytes: data.byteLength, text: traceText(INPUT_TRACE_DECODER.decode(data)) });
	// The refit operation owns attempted input across the transport gap too.
	// Charge before connectivity/grant guards: after generation_refit those
	// guards are intentionally false, but the seal remains authoritative until
	// its exact successor or terminal owner settles it.
	if (this.pendingRefit) {
      this.sealedInputBytes += data.byteLength;
	  this.pendingRefit.droppedBytes += data.byteLength;
      return;
    }
	if (this.selectMode || !this.prepared || !this.committed || this.options.capabilityMode !== "control") return;
	if (!this.controlGranted) {
	  // Control follows the history backlog, which a slow link can take a
	  // while to deliver. A keystroke typed before it is dropped, and the
	  // operator is told why.
	  if (!this.modeReceived) this.showRefusalNotice(INPUT_CATCHING_UP_NOTICE);
	  return;
	}
	if (this.fitPending || this.refitPending) {
	  this.sealedInputBytes += data.byteLength;
	  return;
	}
    // Input is never queued for later, so a send the transport could not
    // accept is lost: say so rather than let the keystroke vanish.
    if (this.send({ type: "INPUT", version: 1, source: this.prepared.source, epoch: this.prepared.epoch, data }) === "SATURATED") {
      this.showRefusalNotice(INPUT_SATURATED_NOTICE);
    }
  }

  private send(frame: BrowserFrame): PortSendResult {
    return this.options.port.trySend(this.generation, frame);
  }

  // --- mobile key bar --------------------------------------------------------

  private resetKeyInteractionAuthorityForLifecycle(): void {
    this.keyInteractionGeneration++;
    this.keysPanel?.cancel();
    this.renderCtrlLatch();
  }

  private advanceKeyInteractionGeneration(): void {
    this.keyInteractionGeneration++;
    this.keysPanel?.cancelModifiers();
    this.renderCtrlLatch();
  }

  private renderCtrlLatch(): void {
    this.keyRow?.querySelectorAll<HTMLButtonElement>("[data-bar-modifier]").forEach((button) => {
      const active = String(this.keysPanel.isModifierLatched(button.dataset.barModifier as keyof KeyModifiers));
      button.setAttribute("aria-pressed", active); button.dataset.latched = active;
    });
  }

  private measureKeysPanel(): void {
    if (!this.keysPanel || this.keysPointers.size) return;
    const compact = this.keysPanel.open && this.keysUseStrip();
    const panel = this.keysPanel.element;
    // Keyboard/composer changes can schedule this writer for a later frame.
    // Preserve the reading position across this separate layout change;
    // capturing afterward can mistake a newly hidden tail for reading history.
    const anchor = this.keysPanel.open ? this.captureAnchor() : undefined;
    const previousMaximum = panel.style.maxHeight;
    const compactRows = !this.composerIsOpen() && this.shell.getBoundingClientRect().bottom - this.viewport.getBoundingClientRect().top >= 150 ? 2 : 1;
    const rowsChanged = panel.dataset.compactRows !== String(compactRows);
    const modeChanged = compact !== (panel.dataset.compact === "true");
    panel.dataset.compactRows = String(compactRows);
    if (modeChanged) {
      panel.dataset.compact = String(compact);
      this.keyRow.hidden = compact;
      if (compact) this.keyBar.insertBefore(panel, this.keyBankToggle);
      else this.shell.insertBefore(panel, this.keyBar);
      this.composer?.remeasureInset();
    }
    if (!this.keysPanel.open) return;
    if (compact) panel.style.maxHeight = compactRows === 2 ? "92px" : "44px";
    else {
      const shell = this.shell.getBoundingClientRect();
      const stage = this.viewport.getBoundingClientRect();
      const dock = this.shell.querySelector<HTMLElement>(".persea-unified-composer-dock")?.getBoundingClientRect().height ?? 0;
      const bar = this.keyBar.hidden ? 0 : this.keyBar.getBoundingClientRect().height;
      // Independent of panel height, so ResizeObserver cannot create feedback.
      // Reserve readable terminal content. In very short bands the entire Keys
      // surface moves into the existing strip, including its navigation.
      const available = Math.max(0, shell.bottom - stage.top - dock - bar);
      // Let taller viewports fit complete key rows, reserving a quarter of this
      // band, or 88px when larger, for terminal context where space permits.
      // A maximum, rather than a fixed height, leaves smaller catalogs compact.
      const maximum = Math.max(44, Math.min(available * .75, available - 88));
      panel.style.maxHeight = `${maximum}px`;
    }
    if (anchor && (modeChanged || rowsChanged || panel.style.maxHeight !== previousMaximum)) {
      this.syncNativeScroll(anchor.following, anchor);
    }
  }

  private keysUseStrip(): boolean {
    if (this.keyBar.hidden) return false;
    // Compact mode removes decorative top padding. Use the normal strip's
    // size for the threshold so removing padding cannot reverse the decision.
    const removedPadding = this.keysPanel.open && this.keysPanel.element.dataset.compact === "true" ? .3 * parseFloat(getComputedStyle(document.documentElement).fontSize) : 0;
    const extraPanelHeight = this.keysPanel.open && this.keysPanel.element.dataset.compact === "true" && this.keysPanel.element.dataset.compactRows === "2" ? 48 : 0;
    const band = this.shell.getBoundingClientRect().bottom - this.viewport.getBoundingClientRect().top - this.keyBar.getBoundingClientRect().height - removedPadding + extraPanelHeight;
    return band < (this.composerIsOpen() ? 320 : 220);
  }

  private openKeys(group?: string, ctrl = false): void {
    this.manualKeysOpen = !this.keyboardOpen;
    this.setKeyBarVisible(true);
    if (group) this.keysPanel.showAll(group, ctrl);
    else this.keysPanel.setOpen(true);
  }

  private renderKeyBar(): void {
    if (!this.keysPanel || this.closed) return;
    const preferences = this.keysPanel.preferences;
    const ids = preferences.layout.bar;
    const renderKey = ids.join("|");
    if (this.barRenderKey === renderKey && this.keyRow.children.length === ids.length) return;
    const focused = this.keyRow.contains(document.activeElement) ? (document.activeElement as HTMLElement).dataset.barKey : undefined;
    this.barRenderKey = renderKey;
    this.barKeyDisposers.splice(0).forEach((dispose) => dispose());
    this.keyRow.replaceChildren();
    for (const id of ids) {
      const button = document.createElement("button");
      button.type = "button"; button.className = "persea-unified-keybar-key"; button.dataset.barKey = id;
      let activate: (event: Event) => void;
      let repeat: Parameters<typeof bindTapActivation>[5];
      if (id.startsWith("modifier:")) {
        const modifier = id.slice(9) as keyof KeyModifiers;
        button.textContent = modifier === "shift" ? "⇧" : modifier === "ctrl" ? "Ctrl" : "Alt";
        button.title = `Use ${modifier === "shift" ? "Shift" : button.textContent} with the next terminal key or typed letter`;
        button.classList.add("persea-unified-keybar-key--latch"); button.dataset.barModifier = modifier;
        activate = () => this.keysPanel.toggleModifier(modifier);
      } else {
        const entry = resolveAction(id); if (!entry) continue;
        button.textContent = actionGlyph(entry); button.title = entry.label;
        button.dataset.keyGlyph = String(button.textContent.length === 1);
        activate = (event) => this.keysPanel.activateBar(entry, event);
        if (entry.action.kind === "key" && /^arrow-(left|right|up|down)$/.test(entry.action.chord.key)) {
          repeat = {
            begin: (event) => {
              const chord = this.keysPanel.activateBar(entry, event);
              return () => { this.dispatchTerminalAction(chord, event); };
            },
            isLive: () => !this.keyBar.hidden && !this.keyRow.hidden && !this.selectMode && this.composerAvailability().canInject,
          };
        }
      }
      button.setAttribute("aria-label", button.title);
      this.barKeyDisposers.push(bindTapActivation(button, activate, () => {}, () => !this.closed && !document.hidden, () => this.keyInteractionGeneration, repeat));
      this.keyRow.append(button);
    }
    this.renderCtrlLatch();
    if (focused) (Array.from(this.keyRow.querySelectorAll<HTMLButtonElement>("[data-bar-key]")).find((button) => button.dataset.barKey === focused) ?? this.keyBankToggle).focus({ preventScroll: true });
  }

  private dispatchTerminalAction(entry: TerminalActionEntry, event: Event): string {
    if (this.closed || document.hidden) return "Terminal unavailable.";
    const action = entry.action;
    if (action.kind === "local") {
      this.keysPanel.cancelModifiers();
      switch (action.action) {
        case "composer": this.keysPanel.setOpen(false); this.toggleComposer(event); return "Composer";
        case "select": this.keysPanel.setOpen(false); this.activateContextualSelection(); return "Select text";
        case "copy": {
          const text = this.contextualSelectionText();
          if (!text) return "Select terminal text first.";
          if (this.selectMode) this.copyContextualSelection(); else this.copyLiveSelection(text);
          return "Copy requested";
        }
        case "fit-font": this.requestAutoFont(); return "Font fitted locally";
      }
    }
    const availability = this.composerAvailability();
    if (this.selectMode || !availability.canInject) return this.selectMode ? "Return from selection to send keys." : availability.reason;
    // Resolve all steps before any emission. No timers or queues: one gesture
    // owns this exact source, epoch and control generation for the entire send.
    if (action.kind === "text") return this.deliverText(action.text, false) === "SENT" ? "Text inserted" : "Text unavailable";
    let chords: readonly KeyChord[];
    if (action.kind === "sequence") {
      const prefix = resolveAction(this.keysPanel.preferences.prefixes[action.prefix]);
      if (prefix?.action.kind !== "key") return "Configure a supported prefix first.";
      chords = [prefix.action.chord, ...action.keys];
    } else chords = [action.chord];
    const plans = chords.map(planKey);
    const refusal = plans.find((plan) => typeof plan === "string");
    if (typeof refusal === "string") return refusal;
    const authority = this.keyInteractionGeneration;
    for (const plan of plans) {
      if (typeof plan === "string") return plan;
      if (authority !== this.keyInteractionGeneration || !this.composerAvailability().canInject) return "Key action canceled.";
      if (plan.prefixEscape && !this.dispatchSyntheticKey(unifiedKeyDescriptor("escape")!)) return "Key was not encoded.";
      let emitted = false;
      if (plan.character !== undefined) {
        this.syntheticKeyDepth++;
        try { emitted = this.dispatchSyntheticInsertText(plan.character); }
        finally { this.syntheticKeyDepth--; }
      } else if (plan.descriptor) emitted = this.dispatchSyntheticKey(plan.descriptor);
      if (!emitted) return "Key was not encoded.";
    }
    const note = action.kind === "key" ? keyEncodingNote(action.chord) : "";
    return `${entry.label} sent${note ? ". " + note : ""}`;
  }

  // Key bar presses are dispatched as synthetic keydowns at xterm's own hidden
  // textarea, where its keydown listener lives, so its keyboard evaluator
  // encodes the bytes against the DEC private modes it tracked from the
  // replayed journal — application cursor keys make arrows ESC O A rather than
  // ESC [ A. The page never encodes escape sequences itself. The emitted bytes
  // arrive through the ordinary onData path, so mode gating and the Fit seal
  // apply unchanged.
  private sendKeyBarKey(key: LogicalKey): void {
    const entry = typeof key === "string" ? keyAction(key) : keyAction(key.ctrl, { ctrl: true, alt: false, shift: false });
    this.keysPanel.cancelModifiers();
    const result = this.dispatchTerminalAction(entry, new Event("terminal-key"));
    if (!result.endsWith(" sent")) this.showToast(result);
  }

  // The one synthetic dispatch path, shared by the key bar and the iOS
  // backspace router. The depth guard makes the custom key handler wave our
  // own dispatches through, and the onData subscription reports whether xterm
  // actually emitted bytes — the router treats that as the delete intent.
  private dispatchSyntheticKey(descriptor: UnifiedKeyDescriptor): boolean {
    const textarea = this.terminal.textarea;
    if (this.closed || !textarea) return false;
    let emitted = false;
    const subscription = this.terminal.onData((data) => {
      if (data.length > 0) emitted = true;
    });
    this.syntheticKeyDepth += 1;
    try {
      try {
        textarea.dispatchEvent(syntheticKeydownEvent(descriptor));
      } finally {
        // xterm tracks whether keydown preceded composed insertText. A neutral
        // Control release clears that public event-path state without focusing.
        textarea.dispatchEvent(syntheticCtrlReleaseEvent());
      }
    } finally {
      this.syntheticKeyDepth -= 1;
      subscription.dispose();
    }
    return emitted;
  }

  /**
   * Route a rewritten dictation revision through xterm's real insertText
   * path, confirming through onData that xterm emitted it. Same probe shape
   * as dispatchSyntheticKey above.
   */
  private dispatchSyntheticInsertText(data: string): boolean {
    const textarea = this.terminal.textarea;
    if (this.closed || !textarea) return false;
    let emitted = false;
    const subscription = this.terminal.onData((chunk) => {
      if (chunk === data) emitted = true;
    });
    try {
      textarea.dispatchEvent(syntheticInsertTextEvent(data));
    } finally {
      subscription.dispose();
    }
    return emitted;
  }

  private focusTerminalPreservingKeyboard(): void {
    if (this.selectMode) return;
    // preventScroll matters: the default focus behaviour scrolls the element
    // into view, which on a shrunken visual viewport moves the grid under the
    // operator.
    this.terminal.textarea?.focus({ preventScroll: true });
  }

  // --- composer ---------------------------------------------------------------

  // canInject mirrors sendInput's own gate exactly (prepared, committed,
  // control capability, and NOT the Fit input seal), so an Insert the
  // composer permits can never be dropped by the seal. bracketedPasteMode is
  // xterm's tracked DEC 2004 state: this page's terminal replayed the full
  // journal through the same parser, so the mode is already correct and
  // tracks live writes.
  private composerAvailability(): ComposerAvailability {
    return unifiedComposerAvailability({
      closed: this.closed,
      capabilityMode: this.options.capabilityMode,
      prepared: this.prepared !== undefined,
      committed: this.committed,
      controlGranted: this.controlGranted,
      // Width refit and vertical Fit share one input floor: neither the
      // composer nor direct Paste may bypass the exact-operation seal.
      fitPending: this.fitPending || this.refitPending,
      bracketedPasteMode: this.terminal.modes.bracketedPasteMode,
    });
  }

  // Same delivery path as the legacy page's live paste: xterm's own paste()
  // converts newlines to carriage returns and applies bracketed-paste framing
  // per its tracked mode, then emits through onData into sendInput — so mode
  // gating and the Fit seal apply unchanged (and can no longer bite: the
  // availability gate above was just consulted in the same tick).
  private injectComposerText(text: string): ComposerInjectionResult {
    return this.deliverText(text, true);
  }

  // The single delivery point for every path that puts operator text in the
  // pane (the composer's Insert and clipboard's snippets/clips/paste). `focusFirst`
  // is the composer's long-standing behaviour and keeps the ONE terminal
  // focus() call site this method has always had; the snippet paths pass
  // false on coarse pointers, where focusing would raise a keyboard nobody
  // asked for.
  private deliverText(text: string, focusFirst: boolean): ComposerInjectionResult {
    if (this.closed || this.selectMode) return "REFUSED_DESTROYED";
    if (!this.composerAvailability().canInject) return "REFUSED_NO_CONTROL";
    if (focusFirst) this.terminal.focus();
    this.terminal.paste(text);
    this.iosBackspace.onXtermOperationComplete();
    return "SENT";
  }

  // The most height the composer panel may claim, published into its
  // max-height. Every term is independent of the panel's own current height —
  // shell height, the chrome above the viewport, the key bar row, four
  // terminal rows — so the panel's ResizeObserver cannot feed the budget back
  // into itself.
  // The most height the composer panel may claim. It is the band between the
  // top of the terminal viewport (everything above it — the control row, the
  // reconnect strip — is already excluded by construction) and the lowest edge
  // the operator can actually see, less the key bar and four terminal rows.
  //
  // The lowest visible edge is the shell's bottom AND the visual viewport's
  // bottom, whichever is higher. The shell's own keyboard pin usually makes
  // those the same, but not always: the pin is gated on the classifier having
  // decided the keyboard is open and on the page not being zoomed, and in the
  // gap between a keyboard appearing and that decision the shell is still
  // 100dvh. Bounding by both boxes is what stops the panel from being sized
  // against screen the keyboard is covering.
  private composerInsetBudget(): number {
    const shellRect = this.shell.getBoundingClientRect();
    if (!(shellRect.height > 0)) return 0;
    const stageTop = this.viewport.getBoundingClientRect().top;
    const keyBarHeight = this.keyBar.hidden ? 0 : this.keyBar.getBoundingClientRect().height;
    const cellHeight = this.measureCellHeight() ?? 17;
    const viewport = this.insetViewport;
    // getBoundingClientRect and visualViewport.offsetTop are both relative to
    // the layout viewport's origin, so they compare directly.
    const visibleBottom = viewport === undefined || viewport.scale < 0.99 || viewport.scale > 1.01
      ? shellRect.bottom
      : Math.min(shellRect.bottom, viewport.offsetTop + viewport.height);
    const keysReserve = this.keysPanel.open && !this.keysUseStrip() ? 88 : 0;
    const composer = this.shell.querySelector<HTMLElement>(".attachment-page__composer");
    const style = composer ? getComputedStyle(composer) : undefined;
    // max-height bounds the content box; padding and borders still occupy
    // dock space and must not consume the terminal rows reserved above.
    const decoration = style && style.boxSizing !== "border-box"
      ? [style.paddingTop, style.paddingBottom, style.borderTopWidth, style.borderBottomWidth].reduce((sum, value) => sum + (parseFloat(value) || 0), 0) : 0;
    return Math.max(48, Math.floor(visibleBottom - stageTop - keyBarHeight - Math.max(44, 4 * cellHeight) - keysReserve - decoration));
  }

  private toggleComposer(event: Event): void {
    if (this.closed || !this.composer) return;
    if (this.composerIsOpen()) this.composer.closeFromTrustedEvent(event);
    else this.composer.openFromTrustedEvent(event);
  }

  private composerIsOpen(): boolean {
    return this.composerToggles[0]?.getAttribute("aria-expanded") === "true";
  }

  // Keyboard-preserving focus hand-off for the key-bar toggle: to the
  // composer textarea when the toggle opened it, back to the terminal when it
  // closed it. Either way an editable element keeps focus synchronously
  // inside the gesture, so the software keyboard never drops.
  private restoreComposerFocus(): void {
    if (this.closed || this.selectMode) return;
    if (this.composerIsOpen() && this.composer) this.composer.textarea.focus({ preventScroll: true });
    else this.focusTerminalPreservingKeyboard();
  }
}
