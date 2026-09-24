import {
  ALLOWED_IMAGE_TYPES,
  ATTACHMENT_LIMIT_REASON,
  IMAGE_ACCEPT_ATTRIBUTE,
  ImageStagingError,
  MAX_COMPOSER_ATTACHMENTS,
  PASTED_IMAGE_REFUSAL_COPY,
  attachmentDisplayLabel,
  attachmentFailureStatus,
  attachmentSizeLabel,
  attachmentUploadBlockReason,
  imageStagingFailureCopy,
  stagedArtifactSegments,
  stagedCount,
  triageClipboardItems,
  uploadingCount,
  type ComposerAttachment,
  type ComposerStagedImage,
} from "./composer_attachments";
import { bindKeyboardPreservingActivation } from "./keyboard_preserving_button";
import { COMPOSER_FONT_SIZE_MAX, COMPOSER_FONT_SIZE_MIN, DEFAULT_COMPOSER_FONT_SIZE } from "./operator_preferences";
import type { ComposerDensity, ComposerMode, ComposerPanelSize, PreferencesV1 } from "./preferences";
import { bindGenerationFencedClickActivation } from "./tap_activation";

export type ComposerInjectionResult =
  | "SENT"
  | "REFUSED_UNTRUSTED"
  | "REFUSED_NO_CONTROL"
  | "REFUSED_DESTROYED";

export type ComposerAvailability = Readonly<{
  canInject: boolean;
  reason: string;
  bracketedPasteMode: boolean;
}>;

export type ComposerSegment =
  | Readonly<{ kind: "text"; value: string }>
  | Readonly<{ kind: "artifact"; id: string; label: string; resolve(): string }>;

export type ComposerInstrumentation = Readonly<{
  open: boolean;
  draftLength: number;
  lines: number;
  sends: number;
  guardTrips: number;
  restores: number;
  storageWrites: number;
  storageFailures: number;
  mode: "prose" | "code";
  size: "compact" | "expanded";
  // Image attachment counts. Counts and state flags only — never a label,
  // a path, or byte content: an operator's screenshot is their content in
  // exactly the way their draft is.
  attachments: number;
  attachmentUploads: number;
  attachmentFailures: number;
  attachmentBytesTotal: number;
}>;

export type ComposerFocusScrollEvent = Readonly<{
  kind: "composer-focus-trigger" | "composer-inset-publication";
}>;

export type ComposerTypographyState = Readonly<{
  size: number;
  status: "ready" | "saving" | "conflict" | "unavailable" | "refused";
  message: string;
  enabled: boolean;
}>;

export type ComposerOptions = Readonly<{
  page: HTMLElement;
  dock: HTMLElement;
  storageScope?: string;
  // Presentation only: glyph labels for Insert/Clear/size, for hosts whose
  // chrome language is symbolic (the unified page). Accessible names are
  // unchanged; the legacy page keeps its worded labels by default.
  symbolicChrome?: boolean;
  interactionGeneration(): number;
  availability(): ComposerAvailability;
  inject(text: string, event: Event): ComposerInjectionResult;
  refocusTerminal(): void;
  insetBudget(): number;
  commitOverlayInset(inset: number, stillCurrent: () => boolean): Promise<boolean>;
  revealLiveEdgeForOverlay(): void;
  cancelPendingOverlayReveal(): void;
  preferences(): PreferencesV1;
  updatePreferences(preferences: PreferencesV1): void;
  initialComposerFontSize?: number;
  updateComposerFontSize?(size: number): void;
  claimTypographyPopover?(): void;
  releaseTypographyPopover?(): void;
  onOpenChange?(open: boolean): void;
  // Image staging, present exactly when the server advertised the capability
  // for this page's realm. Absent, no attach button, no file input, and no
  // paste interception exist: the composer is byte-identical to today. The
  // promise resolves with the staged file's server-chosen absolute path and
  // rejects with an ImageStagingError carrying a closed refusal kind.
  stageImage?(file: File, signal: AbortSignal): Promise<ComposerStagedImage>;
}>;

type StoredDraft = Readonly<{
  text: string;
  mode: "prose" | "code";
  size: "compact" | "expanded";
  savedAt: number;
}>;

const DRAFT_STORAGE_PREFIX = "persea-terminal.composer.draft.v1.";
const MAX_STORED_BYTES = 128 * 1024;
const MAX_STORED_AGE_MS = 24 * 60 * 60 * 1000;
const SAVE_DEBOUNCE_MS = 400;
const SMART_PUNCTUATION = /[\u2014\u2013\u201c\u201d\u2018\u2019]/;
const SMART_PUNCTUATION_GLOBAL = /[\u2014\u2013\u201c\u201d\u2018\u2019]/g;

let composerSerial = 0;
let attachmentSerial = 0;

/**
 * Serializes a draft into one pasteable string. Normalization precedes
 * concatenation on purpose: a draft ending in newlines loses them HERE, so a
 * newline can never sit mid-payload where xterm's \n-to-\r rewrite would
 * submit the prompt and leave the paths to be typed into whatever runs next.
 * Artifact paths join text-first with exactly one space before each path
 * (Claude Code substitutes [Image #N] positionally and splits on a space
 * followed by /), and the result is a fixed point of normalizeComposedText.
 */
export function serializeComposerSegments(segments: readonly ComposerSegment[]): string {
  let text = "";
  const artifacts: string[] = [];
  for (const segment of segments) {
    if (segment.kind === "text") text += segment.value;
    else artifacts.push(segment.resolve());
  }
  const normalized = normalizeComposedText(text);
  if (artifacts.length === 0) return normalized;
  const lead = normalized.replace(/[ \t]+$/, "");
  return lead === "" ? artifacts.join(" ") : `${lead} ${artifacts.join(" ")}`;
}

/**
 * Produces pasteable text without ever manufacturing an Enter. Ordering is
 * intentional: carriage returns are made visible to the trailing-newline rule,
 * hostile controls (including ESC) are removed, and only then are terminal
 * newlines discarded.
 */
export function normalizeComposedText(value: string): string {
  return value
    .replace(/\r\n?|\n/g, "\n")
    // eslint-disable-next-line no-control-regex
    .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/g, "")
    .replace(/\n+$/g, "");
}

export class Composer {
  readonly panel: HTMLDivElement;
  readonly tab: HTMLButtonElement;
  readonly textarea: HTMLTextAreaElement;

  private readonly headerLabel: HTMLSpanElement;
  private readonly modeButton: HTMLButtonElement;
  private readonly sizeButton: HTMLButtonElement;
  private readonly typographyRoot?: HTMLDivElement;
  private readonly typographyTrigger?: HTMLButtonElement;
  private readonly typographyPopover?: HTMLDivElement;
  private readonly typographyMinus?: HTMLButtonElement;
  private readonly typographyCurrent?: HTMLOutputElement;
  private readonly typographyPlus?: HTMLButtonElement;
  private readonly typographyReset?: HTMLButtonElement;
  private readonly typographyStatus?: HTMLOutputElement;
  private readonly closeButton: HTMLButtonElement;
  private readonly grip: HTMLDivElement;
  private readonly status: HTMLOutputElement;
  private readonly fixButton: HTMLButtonElement;
  private readonly joinButton: HTMLButtonElement;
  private readonly overrideButton: HTMLButtonElement;
  private readonly restoreButton: HTMLButtonElement;
  private readonly clearButton: HTMLButtonElement;
  private readonly sendButton: HTMLButtonElement;
  private storageKey: string | undefined;
  private stageImage: ComposerOptions["stageImage"];
  private readonly symbolic: boolean;
  private readonly resizeObserver: ResizeObserver | undefined;
  // The draft's text segment. The full pasteable serialization is DERIVED
  // (composedText) from this plus the staged attachments, so a failed or
  // uploading chip structurally cannot contribute to a paste.
  private textValue = "";
  private readonly attachments: ComposerAttachment[] = [];
  // Image-staging affordances stay mounted so a session switch can rebind the
  // capability without replacing the composer. They remain hidden while the
  // current identity has no staging authority.
  private readonly chipStrip: HTMLUListElement | undefined;
  private readonly attachButton: HTMLButtonElement | undefined;
  private readonly fileInput: HTMLInputElement | undefined;

  private destroyed = false;
  private openState = false;
  private contentState: "empty" | "draft" | "guard" | "blocked" | "sent" = "empty";
  private density: ComposerDensity = "standard";
  private mode: ComposerMode = "code";
  private size: ComposerPanelSize = "compact";
  private lastRestore = "";
  private lastSentLength = 0;
  private storageDegraded = false;
  private saveTimer: number | undefined;
  private measureFrame: number | undefined;
  private focusRevealFrame: number | undefined;
  private typographyRestoreFrame: number | undefined;
  private typographyPositionFrame: number | undefined;
  private typographyListenersCleanup: (() => void) | undefined;
  private typographyInteractionGeneration = 0;
  private readonly textareaPointers = new Set<number>();
  private readonly textareaTouches = new Set<number>();
  private appliedInset = 0;
  private pendingInset: number | undefined;
  private insetGeneration = 0;
  private resizePercent = 0;
  private drag: Readonly<{ pointerId: number; startY: number; startPercent: number }> | undefined;
  private sends = 0;
  private guardTrips = 0;
  private restores = 0;
  private storageWrites = 0;
  private storageFailures = 0;
  private lastSentImages = 0;
  private lastRestoreDroppedImages = false;
  private restoredWithoutImages = false;
  private attachmentUploads = 0;
  private attachmentFailures = 0;
  private attachmentBytesTotal = 0;
  private insetPublicationCount = 0;
  private readonly focusScrollListeners = new Set<(event: ComposerFocusScrollEvent) => void>();
  private readonly activationCleanups: Array<() => void> = [];
  private readonly renderedActivationCleanups: Array<() => void> = [];
  private typographyOpen = false;
  private typographyState: ComposerTypographyState;

  constructor(private readonly options: ComposerOptions) {
    if (!(options.page instanceof HTMLElement) || !(options.dock instanceof HTMLElement)) {
      throw new Error("Composer requires page-owned DOM");
    }
    const serial = (composerSerial += 1);
    const panelId = `persea-composer-${serial}`;
    const labelId = `${panelId}-label`;
    const statusId = `${panelId}-status`;
    this.symbolic = options.symbolicChrome === true;
    this.typographyState = Object.freeze({
      size: options.initialComposerFontSize ?? DEFAULT_COMPOSER_FONT_SIZE,
      status: "ready",
      message: "",
      enabled: options.updateComposerFontSize !== undefined,
    });
    this.stageImage = options.stageImage;
    this.storageKey = options.storageScope === undefined
      ? undefined
      : `${DRAFT_STORAGE_PREFIX}${options.storageScope}`;
    this.storageDegraded = this.storageKey === undefined;
    const preferences = options.preferences();
    this.density = preferences.composer.density;
    this.mode = preferences.composer.mode;
    this.size = preferences.composer.panelSize;

    this.tab = document.createElement("button");
    this.tab.type = "button";
    this.tab.className = "attachment-page__compose-tab";
    this.tab.textContent = "\u270e";
    this.tab.setAttribute("aria-expanded", "false");
    this.tab.setAttribute("aria-controls", panelId);
    this.tab.title = "Open composer";
    bindKeyboardPreservingActivation(
      this.tab,
      this.onTabActivate,
      () => this.restoreTextareaFocus(),
      () => !this.destroyed,
      options.interactionGeneration,
    );

    this.panel = document.createElement("div");
    this.panel.id = panelId;
    this.panel.className = "attachment-page__composer";
    if (this.symbolic) this.panel.classList.add("attachment-page__composer--symbolic");
    this.panel.hidden = true;
    this.panel.dataset.open = "false";
    this.panel.dataset.content = "empty";
    this.panel.setAttribute("role", "group");
    this.panel.setAttribute("aria-labelledby", labelId);

    const header = document.createElement("div");
    header.className = "attachment-page__composer-header";
    this.headerLabel = document.createElement("span");
    this.headerLabel.id = labelId;
    this.headerLabel.className = "attachment-page__composer-label";
    this.headerLabel.textContent = "Composer";
    // ONE toggle showing the CURRENT mode — `>_` is Code input (autocorrect,
    // autocapitalization and spellcheck off, the construction default); `Aa`
    // is Prose writing mode (language assistance on). The accessible name
    // names the current mode while aria-pressed retains the binary state.
    const modeControl = document.createElement("div");
    modeControl.className = "attachment-page__composer-mode";
    this.modeButton = this.button(">_", "Code input", this.onToggleMode, () => this.restoreTextareaFocus());
    this.modeButton.classList.add("attachment-page__composer-mode-toggle");
    modeControl.append(this.modeButton);
    const headerActions = document.createElement("div");
    headerActions.className = "attachment-page__composer-header-actions";
    this.sizeButton = this.button("\u2922 Taller", "Expand composer", this.onToggleSize, () => this.restoreTextareaFocus());
    this.sizeButton.classList.add("attachment-page__composer-size");
    this.closeButton = this.button("\u2304", "Hide composer — draft stays here", this.onClose, options.refocusTerminal);
    this.closeButton.classList.add("attachment-page__composer-close");
    if (options.updateComposerFontSize !== undefined) {
      this.typographyRoot = document.createElement("div");
      this.typographyRoot.className = "attachment-page__composer-typography";
      this.typographyTrigger = this.button(
        String(this.typographyState.size),
        `Composer options, text size ${this.typographyState.size} pixels`,
        this.onToggleTypography,
        () => undefined,
      );
      this.typographyTrigger.classList.add("attachment-page__composer-typography-trigger");
      this.typographyTrigger.setAttribute("aria-haspopup", "dialog");
      this.typographyTrigger.setAttribute("aria-expanded", "false");
      const typographyId = `${panelId}-typography`;
      const typographyLabelId = `${typographyId}-label`;
      this.typographyTrigger.setAttribute("aria-controls", typographyId);

      this.typographyPopover = document.createElement("div");
      this.typographyPopover.id = typographyId;
      this.typographyPopover.className = "attachment-page__composer-typography-popover";
      this.typographyPopover.hidden = true;
      this.typographyPopover.setAttribute("role", "dialog");
      this.typographyPopover.setAttribute("aria-modal", "false");
      this.typographyPopover.setAttribute("aria-labelledby", typographyLabelId);
      const typographyLabel = document.createElement("span");
      typographyLabel.id = typographyLabelId;
      typographyLabel.className = "attachment-page__composer-typography-label";
      typographyLabel.textContent = "Text size";
      const typographyStepper = document.createElement("div");
      typographyStepper.className = "attachment-page__composer-typography-stepper";
      this.typographyMinus = this.button("−", "Decrease composer text size", this.onTypographyMinus, () => undefined);
      this.typographyMinus.classList.add("attachment-page__composer-typography-step");
      this.typographyCurrent = document.createElement("output");
      this.typographyCurrent.className = "attachment-page__composer-typography-current";
      this.typographyCurrent.setAttribute("aria-live", "polite");
      this.typographyPlus = this.button("+", "Increase composer text size", this.onTypographyPlus, () => undefined);
      this.typographyPlus.classList.add("attachment-page__composer-typography-step");
      typographyStepper.append(this.typographyMinus, this.typographyCurrent, this.typographyPlus);
      const typographyActions = document.createElement("div");
      typographyActions.className = "attachment-page__composer-typography-actions";
      this.typographyReset = this.button("Reset", `Reset composer text size to ${DEFAULT_COMPOSER_FONT_SIZE} pixels`, this.onTypographyReset, () => undefined);
      this.typographyReset.classList.add("attachment-page__composer-typography-reset");
      typographyActions.append(this.typographyReset, this.sizeButton);
      this.typographyStatus = document.createElement("output");
      this.typographyStatus.className = "attachment-page__composer-typography-status";
      this.typographyStatus.setAttribute("role", "status");
      this.typographyStatus.setAttribute("aria-live", "polite");
      this.typographyPopover.append(typographyLabel, typographyStepper, typographyActions, this.typographyStatus);
      this.typographyRoot.append(this.typographyTrigger);
      // The dialog is a fixed, page-owned layer rather than a child of the
      // height-constrained composer. It therefore cannot expand or escape the
      // composer box, and positioning can be clamped to the visual viewport.
      options.page.append(this.typographyPopover);
      headerActions.append(this.typographyRoot, this.closeButton);
    } else {
      headerActions.append(this.sizeButton, this.closeButton);
    }
    header.append(this.headerLabel, modeControl, headerActions);

    this.grip = document.createElement("div");
    this.grip.className = "attachment-page__composer-grip";
    this.grip.setAttribute("role", "separator");
    this.grip.setAttribute("aria-orientation", "horizontal");
    this.grip.setAttribute("aria-label", "Resize composer");
    this.grip.setAttribute("aria-valuemin", "10");
    this.grip.setAttribute("aria-valuemax", "70");
    this.grip.setAttribute("aria-valuenow", "10");
    this.grip.tabIndex = 0;
    this.grip.addEventListener("pointerdown", this.onGripPointerDown);
    this.grip.addEventListener("pointermove", this.onGripPointerMove);
    this.grip.addEventListener("pointerup", this.onGripPointerEnd);
    this.grip.addEventListener("pointercancel", this.onGripPointerEnd);
    this.grip.addEventListener("keydown", this.onGripKeydown);

    this.textarea = document.createElement("textarea");
    this.textarea.className = "attachment-page__composer-textarea";
    // Compact-first: start as a true one-liner (the browser default is two
    // rows) and let auto-grow add lines as they are written. Scoped to the
    // symbolic presentation so the legacy page's footprint is unchanged.
    if (this.symbolic) this.textarea.rows = 1;
    this.textarea.setAttribute("aria-labelledby", labelId);
    this.textarea.setAttribute("aria-describedby", statusId);
    this.textarea.setAttribute("enterkeyhint", "enter");
    // The placeholder renders in the content box and inflates the empty
    // textarea's scrollHeight when it wraps, so the compact presentation
    // keeps it to one line (the empty-state status line carries the longer
    // guidance) — that is what actually makes the closed-to-open footprint a
    // one-liner.
    this.textarea.placeholder = this.symbolic
      ? "Enter adds a line — nothing runs."
      : "Write or dictate, then Insert. Enter makes a new line — it does not run anything. Drafts stay in this tab until inserted or cleared.";
    this.textarea.addEventListener("input", this.onInput);
    this.textarea.addEventListener("keydown", this.onTextareaKeydown);
    this.textarea.addEventListener("focus", this.onTextareaFocus);
    this.textarea.addEventListener("pointerdown", this.onTextareaPointerDown);
    this.textarea.addEventListener("touchstart", this.onTextareaTouchStart, { passive: true });
    this.textarea.addEventListener("scroll", this.onTextareaScroll);
    this.textarea.addEventListener("lostpointercapture", this.onTextareaPointerEnd);
    document.addEventListener("pointerup", this.onTextareaPointerEnd, true);
    document.addEventListener("pointercancel", this.onTextareaPointerEnd, true);
    document.addEventListener("touchend", this.onTextareaTouchEnd, true);
    document.addEventListener("touchcancel", this.onTextareaTouchEnd, true);
    this.textarea.addEventListener("wheel", this.onTextareaScrollIntent, { passive: true });
    this.textarea.addEventListener("touchmove", this.onTextareaScrollIntent, { passive: true });

    const guardActions = document.createElement("div");
    guardActions.className = "attachment-page__composer-guard-actions";
    this.joinButton = this.button("Join lines", "Join draft lines with spaces", this.onJoin, () => this.restoreTextareaFocus());
    this.overrideButton = this.button("Insert anyway", "Insert the multiline draft anyway", this.onOverride, options.refocusTerminal);
    guardActions.append(this.joinButton, this.overrideButton);

    const footer = document.createElement("div");
    footer.className = "attachment-page__composer-footer";
    // the status is a conditional line — it renders only while
    // there is something to act on (guard, blocked, receipt, upload, restore,
    // punctuation, storage) and is hidden otherwise. No counter (§16.3): a
    // terminal composer has no length limit, images are visible as chips,
    // and the closed ✎ tab already carries the held count.
    this.status = document.createElement("output");
    this.status.id = statusId;
    this.status.className = "attachment-page__composer-status";
    this.status.setAttribute("role", "status");
    this.status.setAttribute("aria-live", "polite");
    this.fixButton = this.button("Fix", "Replace smart punctuation in the draft", this.onFix, () => this.restoreTextareaFocus());
    this.fixButton.classList.add("attachment-page__composer-fix");
    this.restoreButton = this.button("Restore", "Restore the last cleared or inserted draft", this.onRestore, () => this.restoreTextareaFocus());
    this.restoreButton.classList.add("attachment-page__composer-restore");
    this.clearButton = this.button(this.symbolic ? "\u232b" : "Clear", "Clear composer draft", this.onClear, () => this.restoreTextareaFocus());
    this.clearButton.classList.add("attachment-page__composer-clear");
    this.sendButton = this.button(this.symbolic ? "\u27a4" : "Insert \u25b8", "Insert into terminal without running it", this.onSend, options.refocusTerminal);
    this.sendButton.classList.add("attachment-page__composer-send");
    {
      this.chipStrip = document.createElement("ul");
      this.chipStrip.className = "attachment-page__composer-chips";
      this.chipStrip.setAttribute("role", "list");
      this.chipStrip.hidden = true;
      this.fileInput = document.createElement("input");
      this.fileInput.type = "file";
      this.fileInput.hidden = true;
      this.fileInput.multiple = true;
      // Explicit enumeration, never image/*: the list excluding HEIC is what
      // induces iOS Safari to transcode camera-roll HEIC to JPEG in the
      // picker (hardware-verifiable HW-2).
      this.fileInput.setAttribute("accept", IMAGE_ACCEPT_ATTRIBUTE);
      this.fileInput.addEventListener("change", this.onFileInputChange);
      // A plain click listener on purpose: the native picker takes focus and
      // collapses the software keyboard regardless, so a keyboard-preserving
      // activation would only fight it. Focus returns to the textarea on
      // change.
      this.attachButton = document.createElement("button");
      this.attachButton.type = "button";
      this.attachButton.className = "attachment-page__button attachment-page__composer-attach";
      this.attachButton.textContent = this.symbolic ? "\u2295" : "\u2295 Image";
      this.attachButton.setAttribute("aria-label", "Attach images");
      this.attachButton.title = "Attach images";
      this.attachButton.hidden = this.stageImage === undefined;
      this.activationCleanups.push(bindGenerationFencedClickActivation(
        this.attachButton,
        (event) => this.onAttachClick(event),
        () => !this.destroyed,
        this.options.interactionGeneration,
      ));
      this.textarea.addEventListener("paste", this.onPaste);
    }
    // The action buttons are grouped so a narrow layout can give the status
    // its own full-width line above them. The group is `display: contents`
    // by default, so the footer's flex layout (and the legacy page) is
    // byte-identical.
    const actions = document.createElement("div");
    actions.className = "attachment-page__composer-actions";
    actions.append(
      this.fixButton,
      this.restoreButton,
      ...(this.attachButton ? [this.attachButton] : []),
      this.clearButton,
      this.sendButton,
    );
    footer.append(this.status, actions);
    this.panel.append(
      this.grip,
      header,
      ...(this.chipStrip ? [this.chipStrip] : []),
      this.textarea,
      guardActions,
      footer,
      ...(this.fileInput ? [this.fileInput] : []),
    );
    options.dock.prepend(this.tab, this.panel);

    this.restoreStoredDraft();
    this.syncResizeTarget(options.preferences());
    this.applyMode();
    this.applySize();
    this.renderTypography();
    this.render();

    document.addEventListener("visibilitychange", this.onVisibilityChange);
    window.addEventListener("pagehide", this.onPageHide);
    this.resizeObserver = typeof ResizeObserver === "function"
      ? new ResizeObserver(() => {
        this.scheduleInsetPublication();
        this.scheduleTypographyPosition();
      })
      : undefined;
    this.resizeObserver?.observe(this.panel);
    if (this.typographyTrigger) this.resizeObserver?.observe(this.typographyTrigger);
  }

  /**
   * Show the panel WITHOUT claiming focus (clipboard-focus-ownership).
   *
   * `openFromTrustedEvent` focuses the textarea, which on a coarse pointer
   * raises the keyboard. A caller that has populated the draft on the
   * operator's behalf — the unified page's multiline hand-off — must be able
   * to make that draft visible without taking focus, or the affordance it
   * offers contradicts the keyboard law it is trying to honour. This is the
   * open transition; focusing is the caller's separate decision.
   *
   * It needs no trusted event because it moves no focus and reads no
   * privileged API: it only makes an already-populated panel visible.
   */
  showWithoutFocus(): boolean {
    if (this.destroyed) return false;
    if (this.contentState === "sent") this.panel.dataset.sentReopened = "true";
    this.setOpen(true);
    return true;
  }

  openFromTrustedEvent(event: Event): boolean {
    if (!event.isTrusted || this.destroyed) return false;
    if (!this.showWithoutFocus()) return false;
    this.armFocusZoomGuard();
    this.textarea.focus({ preventScroll: true });
    return true;
  }

  // Whether this composer has an image affordance at all (a stageImage was
  // supplied). The server re-authorizes every upload regardless.
  get canAttachImages(): boolean {
    return this.stageImage !== undefined;
  }

  // Flush the old session's local draft before changing scope, drop every
  // staged image (its server object expires independently), then restore only
  // the new session's own draft. The stage callback is replaced in the same
  // synchronous task, so an image can never cross session authority.
  rescope(storageScope: string | undefined, stageImage?: ComposerOptions["stageImage"]): void {
    if (this.destroyed) return;
    this.flushStoredDraft();
    this.clearAttachments();
    this.storageKey = storageScope === undefined ? undefined : `${DRAFT_STORAGE_PREFIX}${storageScope}`;
    this.stageImage = stageImage;
    if (this.attachButton) this.attachButton.hidden = stageImage === undefined;
    this.setText("");
    this.lastRestore = "";
    this.lastRestoreDroppedImages = false;
    this.restoredWithoutImages = false;
    this.storageDegraded = this.storageKey === undefined;
    this.restoreStoredDraft();
    this.contentState = this.hasInsertableContent() ? "draft" : "empty";
    this.render();
  }

  // Opens the composer AND its native image picker from one trusted gesture
  // (the quick-actions ⊕ tile). The picker needs the gesture; opening first
  // is what makes the staged image land in a visible composer.
  openImagePickerFromTrustedEvent(event: Event): boolean {
    if (!this.openFromTrustedEvent(event)) return false;
    if (!this.stageImage || !this.fileInput || this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) return false;
    this.fileInput.click();
    return true;
  }

  /** Adds an explicitly chosen shared image through the same staging and
   * cancellation path as a file-picker attachment. It never sends terminal input. */
  addClipboardImage(file: File): boolean {
    if (this.destroyed || !this.stageImage || this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) return false;
    if (!ALLOWED_IMAGE_TYPES.includes(file.type)) { this.addTypeRefusedFile(file); this.showWithoutFocus(); return true; }
    this.stageFile(file); this.showWithoutFocus(); return true;
  }

  closeFromTrustedEvent(event: Event): boolean {
    if (!event.isTrusted || this.destroyed) return false;
    event.preventDefault();
    this.setOpen(false);
    this.options.refocusTerminal();
    return true;
  }

  refreshAvailability(): void {
    if (this.destroyed) return;
    if (this.hasInsertableContent() && !this.options.availability().canInject && this.contentState !== "guard") {
      this.contentState = "blocked";
    } else if (this.contentState === "blocked") {
      this.contentState = this.hasInsertableContent() ? "draft" : "empty";
    }
    this.render();
  }

  /** Apply one document-global composer face without moving focus or selection. */
  applyTypographyState(state: ComposerTypographyState): void {
    if (this.destroyed) return;
    const sizeChanged = this.typographyState.size !== state.size;
    this.typographyState = state;
    this.renderTypography();
    if (!sizeChanged) return;
    const selectionStart = this.textarea.selectionStart;
    const selectionEnd = this.textarea.selectionEnd;
    const selectionDirection = this.textarea.selectionDirection;
    const scrollTop = this.textarea.scrollTop;
    const scrollLeft = this.textarea.scrollLeft;
    const value = this.textarea.value;
    const interactionGeneration = this.typographyInteractionGeneration;
    const gestureActive = this.textareaGestureActive();
    this.options.page.style.setProperty("--persea-composer-font-size", `${state.size}px`);
    this.options.page.dataset.composerFont = String(state.size);
    const restore = () => {
      if (this.destroyed) return;
      this.textarea.setSelectionRange(selectionStart, selectionEnd, selectionDirection);
      this.textarea.scrollTop = scrollTop;
      this.textarea.scrollLeft = scrollLeft;
    };
    restore();
    this.remeasureInset();
    this.scheduleTypographyPosition();
    // Auto-grow runs on the next layout frame and can make the browser reveal
    // the selection again. Restore the operator's scroll after that reflow too.
    if (this.typographyRestoreFrame !== undefined) window.cancelAnimationFrame(this.typographyRestoreFrame);
    this.typographyRestoreFrame = window.requestAnimationFrame(() => {
      this.typographyRestoreFrame = undefined;
      // The frame belongs only to the exact draft posture captured above.
      // Input, caret movement, pointer interaction, or manual scrolling after
      // capture wins over this layout repair; stale state must never rewind
      // the operator's newer interaction.
      // Reflow can change the scroll offset without an interaction. Comparing
      // that offset here would reject precisely the layout repair we owe.
      if (gestureActive || this.textareaGestureActive()
        || this.typographyInteractionGeneration !== interactionGeneration
        || this.textarea.value !== value
        || this.textarea.selectionStart !== selectionStart
        || this.textarea.selectionEnd !== selectionEnd
        || this.textarea.selectionDirection !== selectionDirection) return;
      restore();
    });
  }

  /** Intentional product navigation wins over a pending typography repair.
   * Layout preservation writes stay internal and do not use this method. */
  scrollDraftTo(top: number, left: number): void {
    if (this.destroyed) return;
    this.noteTypographyInteraction();
    this.textarea.scrollTop = top;
    this.textarea.scrollLeft = left;
  }

  instrumentation(): ComposerInstrumentation {
    const text = this.text();
    return Object.freeze({
      open: this.openState,
      draftLength: text.length,
      lines: text.length === 0 ? 0 : text.split("\n").length,
      sends: this.sends,
      guardTrips: this.guardTrips,
      restores: this.restores,
      storageWrites: this.storageWrites,
      storageFailures: this.storageFailures,
      mode: this.mode,
      size: this.size,
      attachments: this.attachments.length,
      attachmentUploads: this.attachmentUploads,
      attachmentFailures: this.attachmentFailures,
      attachmentBytesTotal: this.attachmentBytesTotal,
    });
  }

  appliedInsetPx(): number {
    return this.appliedInset;
  }

  focusScrollSnapshot(): Readonly<Record<string, unknown>> {
    const rect = (element: Element | null): Readonly<Record<string, number>> | null => {
      if (!element) return null;
      const value = element.getBoundingClientRect();
      return Object.freeze({
        top: value.top, right: value.right, bottom: value.bottom, left: value.left,
        width: value.width, height: value.height,
      });
    };
    const cssInset = Number.parseFloat(getComputedStyle(this.options.page).getPropertyValue("--persea-composer-inset"));
    return Object.freeze({
      composer: Object.freeze({
        open: this.openState,
        size: this.size,
        panelRect: rect(this.panel),
        textarea: Object.freeze({
          rect: rect(this.textarea),
          scrollTop: this.textarea.scrollTop,
          scrollHeight: this.textarea.scrollHeight,
          clientHeight: this.textarea.clientHeight,
          valueLength: this.textarea.value.length,
          selectionStart: this.textarea.selectionStart,
          selectionEnd: this.textarea.selectionEnd,
        }),
      }),
      inset: Object.freeze({
        measuredComposerHeight: this.openState ? Math.max(0, Math.round(this.panel.getBoundingClientRect().height)) : 0,
        publishedInsetPx: this.appliedInset,
        insetBudgetPx: Math.max(0, this.options.insetBudget()),
        cssPublishedInsetPx: Number.isFinite(cssInset) ? Math.max(0, cssInset) : 0,
        publicationCount: this.insetPublicationCount,
        publicationFramePending: this.measureFrame !== undefined,
      }),
    });
  }

  onFocusScrollEvent(listener: (event: ComposerFocusScrollEvent) => void): () => void {
    this.focusScrollListeners.add(listener);
    return () => { this.focusScrollListeners.delete(listener); };
  }

  private emitFocusScroll(event: ComposerFocusScrollEvent): void {
    for (const listener of this.focusScrollListeners) {
      try { listener(event); } catch { /* diagnostic observation cannot alter Composer behavior */ }
    }
  }

  destroy(): void {
    if (this.destroyed) return;
    this.setTypographyOpen(false);
    this.flushStoredDraft();
    this.removeInset(false);
    this.destroyed = true;
    this.clearAttachments();
    this.insetGeneration += 1;
    if (this.saveTimer !== undefined) window.clearTimeout(this.saveTimer);
    if (this.measureFrame !== undefined) window.cancelAnimationFrame(this.measureFrame);
    if (this.focusRevealFrame !== undefined) window.cancelAnimationFrame(this.focusRevealFrame);
    if (this.typographyRestoreFrame !== undefined) window.cancelAnimationFrame(this.typographyRestoreFrame);
    if (this.typographyPositionFrame !== undefined) window.cancelAnimationFrame(this.typographyPositionFrame);
    this.resizeObserver?.disconnect();
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    window.removeEventListener("pagehide", this.onPageHide);
    this.textarea.removeEventListener("input", this.onInput);
    this.textarea.removeEventListener("keydown", this.onTextareaKeydown);
    this.textarea.removeEventListener("focus", this.onTextareaFocus);
    this.textarea.removeEventListener("pointerdown", this.onTextareaPointerDown);
    this.textarea.removeEventListener("touchstart", this.onTextareaTouchStart);
    this.textarea.removeEventListener("scroll", this.onTextareaScroll);
    this.textarea.removeEventListener("lostpointercapture", this.onTextareaPointerEnd);
    document.removeEventListener("pointerup", this.onTextareaPointerEnd, true);
    document.removeEventListener("pointercancel", this.onTextareaPointerEnd, true);
    document.removeEventListener("touchend", this.onTextareaTouchEnd, true);
    document.removeEventListener("touchcancel", this.onTextareaTouchEnd, true);
    this.textareaPointers.clear();
    this.textareaTouches.clear();
    this.textarea.removeEventListener("wheel", this.onTextareaScrollIntent);
    this.textarea.removeEventListener("touchmove", this.onTextareaScrollIntent);
    this.textarea.removeEventListener("paste", this.onPaste);
    this.fileInput?.removeEventListener("change", this.onFileInputChange);
    for (const cleanup of this.renderedActivationCleanups.splice(0)) cleanup();
    for (const cleanup of this.activationCleanups.splice(0)) cleanup();
    this.grip.removeEventListener("pointerdown", this.onGripPointerDown);
    this.grip.removeEventListener("pointermove", this.onGripPointerMove);
    this.grip.removeEventListener("pointerup", this.onGripPointerEnd);
    this.grip.removeEventListener("pointercancel", this.onGripPointerEnd);
    this.grip.removeEventListener("keydown", this.onGripKeydown);
    this.panel.remove();
    this.tab.remove();
    this.typographyPopover?.remove();
  }

  remeasureInset(): void {
    if (this.destroyed || !this.openState) return;
    this.scheduleAutoGrow();
  }

  applyPreferences(preferences: PreferencesV1): void {
    if (this.destroyed) return;
    const densityChanged = this.density !== preferences.composer.density;
    const modeChanged = this.mode !== preferences.composer.mode;
    const sizeChanged = this.size !== preferences.composer.panelSize;
    this.density = preferences.composer.density;
    this.mode = preferences.composer.mode;
    this.size = preferences.composer.panelSize;
    this.syncResizeTarget(preferences);
    if (modeChanged) this.applyMode();
    if (sizeChanged) this.applySize();
    if (densityChanged || sizeChanged) this.scheduleAutoGrow();
    this.render();
  }

  private button(
    text: string,
    label: string,
    listener: EventListener,
    restoreFocus: () => void,
  ): HTMLButtonElement {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "attachment-page__button";
    button.textContent = text;
    button.setAttribute("aria-label", label);
    button.title = label;
    bindKeyboardPreservingActivation(
      button,
      (event) => listener(event),
      restoreFocus,
      () => !this.destroyed,
      this.options.interactionGeneration,
    );
    return button;
  }

  private text(): string {
    return this.textValue;
  }

  private setText(value: string): void {
    this.textValue = value;
    this.textarea.value = value;
  }

  // Whether the draft has anything to insert: text, or at least one STAGED
  // image (a draft of zero text and one staged image is a legitimate prompt
  // opener). failed and uploading chips contribute nothing here.
  private hasInsertableContent(): boolean {
    return this.textValue.length > 0 || stagedCount(this.attachments) > 0;
  }

  // The full pasteable serialization: normalized text first, then each
  // staged path after exactly one space.
  private composedText(): string {
    return serializeComposerSegments([
      Object.freeze({ kind: "text" as const, value: this.textValue }),
      ...stagedArtifactSegments(this.attachments),
    ]);
  }

  private setOpen(open: boolean): void {
    if (this.openState === open) return;
    this.openState = open;
    this.panel.hidden = !open;
    this.panel.dataset.open = open ? "true" : "false";
    this.tab.hidden = open;
    this.tab.setAttribute("aria-expanded", open ? "true" : "false");
    if (!open) {
      delete this.panel.dataset.sentReopened;
      this.setTypographyOpen(false);
    }
    if (!open) this.flushStoredDraft();
    this.options.onOpenChange?.(open);
    this.render();
    if (open) this.scheduleInsetPublication();
    else this.removeInset();
  }

  private send(event: Event, override: boolean, refocusTerminal = true): void {
    if (!event.isTrusted || this.destroyed) return;
    event.preventDefault();
    const availability = this.options.availability();
    if (!availability.canInject) {
      this.contentState = "blocked";
      this.render(availability.reason);
      return;
    }
    const uploadBlock = attachmentUploadBlockReason(this.attachments);
    if (uploadBlock !== undefined) {
      this.render(uploadBlock);
      return;
    }
    // composedText() already normalizes the text segment BEFORE appending
    // staged paths, and its result is a fixed point of normalizeComposedText,
    // so nothing here needs a second pass.
    const normalized = this.composedText();
    if (normalized.length === 0) {
      this.contentState = "empty";
      this.render();
      return;
    }
    if (!override && /[\n\t]/.test(normalized) && !availability.bracketedPasteMode) {
      this.contentState = "guard";
      this.guardTrips += 1;
      this.render();
      return;
    }
    const result = this.options.inject(normalized, event);
    if (result !== "SENT") {
      this.contentState = "blocked";
      this.render(result === "REFUSED_DESTROYED" ? "This page is closed." : "Terminal typing is not available.");
      return;
    }
    // The restore slot holds the text only: a staged path could outlive its
    // TTL, and restoring a chip whose file may be gone would lie.
    this.lastRestore = normalizeComposedText(this.text());
    this.lastRestoreDroppedImages = false;
    this.restoredWithoutImages = false;
    this.lastSentLength = normalized.length;
    this.lastSentImages = stagedCount(this.attachments);
    this.setText("");
    this.clearAttachments();
    this.deleteStoredDraft();
    this.contentState = "sent";
    this.sends += 1;
    this.size = "compact";
    this.applySize();
    this.render();
    if (refocusTerminal) this.options.refocusTerminal();
  }

  private render(forcedStatus?: string): void {
    const text = this.text();
    const hasContent = this.hasInsertableContent();
    const availability = this.options.availability();
    if (!hasContent && this.contentState !== "sent") this.contentState = "empty";
    else if (hasContent && this.contentState !== "guard" && !availability.canInject) this.contentState = "blocked";
    else if (hasContent && (this.contentState === "empty" || this.contentState === "sent" || (this.contentState === "blocked" && availability.canInject))) this.contentState = "draft";
    if (this.contentState !== "sent") delete this.panel.dataset.sentReopened;
    this.panel.dataset.content = this.contentState;
    this.panel.dataset.mode = this.mode;
    this.panel.dataset.size = this.size;
    const receipt = this.contentState === "sent" && this.panel.dataset.sentReopened !== "true";
    if (this.typographyRoot) this.typographyRoot.hidden = receipt;
    if (receipt) this.setTypographyOpen(false);
    const storageHint = this.storageDegraded ? "Draft held in this tab only" : "";
    const punctuationHint = SMART_PUNCTUATION.test(text) ? "Smart punctuation found" : "";
    const uploadHint = attachmentUploadBlockReason(this.attachments) ?? "";
    const failureHint = attachmentFailureStatus(this.attachments);
    const restoreHint = this.restoredWithoutImages ? "Restored text only \u2014 images were not restored" : "";
    let stateStatus = "";
    if (forcedStatus) stateStatus = forcedStatus;
    else if (this.contentState === "guard") stateStatus = "The program in the terminal does not accept pasted line breaks safely \u2014 each line would run on arrival.";
    else if (this.contentState === "blocked") stateStatus = availability.reason;
    else if (this.contentState === "sent") stateStatus = this.lastSentImages > 0
      ? `Inserted ${this.lastSentLength.toLocaleString()} ch \u00b7 ${this.lastSentImages.toLocaleString()} image${this.lastSentImages === 1 ? "" : "s"}`
      : `Inserted ${this.lastSentLength.toLocaleString()} ch`;
    // composer input \u00a716.1/\u00a716.2: no "not run" qualifier (the receipt says what
    // happened; the no-Return law is explained in Help) and no persistent
    // instruction line for the empty and draft states.
    this.status.value = [stateStatus, uploadHint, failureHint, restoreHint, punctuationHint, storageHint].filter(Boolean).join(" \u00b7 ");
    this.status.hidden = this.status.value === "";
    this.fixButton.hidden = punctuationHint === "";
    this.joinButton.hidden = this.contentState !== "guard";
    this.overrideButton.hidden = this.contentState !== "guard";
    this.restoreButton.hidden = !(text.length === 0 && this.lastRestore.length > 0);
    this.clearButton.disabled = text.length === 0 && this.attachments.length === 0;
    this.sendButton.disabled = !hasContent || !availability.canInject || this.contentState === "guard" || uploadHint !== "";
    this.sendButton.setAttribute("aria-disabled", this.sendButton.disabled ? "true" : "false");
    if (this.sendButton.disabled) this.sendButton.setAttribute("aria-describedby", this.status.id);
    else this.sendButton.removeAttribute("aria-describedby");
    this.tab.textContent = text.length === 0 ? "\u270e" : `\u270e ${text.length.toLocaleString()}`;
    this.tab.setAttribute("aria-label", text.length === 0 ? "Open composer" : `Open composer, ${text.length.toLocaleString()} characters held`);
    this.tab.title = this.tab.getAttribute("aria-label") ?? "Open composer";
    this.renderAttachments();
    this.scheduleAutoGrow();
  }

  private renderTypography(): void {
    const trigger = this.typographyTrigger;
    const minus = this.typographyMinus;
    const current = this.typographyCurrent;
    const plus = this.typographyPlus;
    const reset = this.typographyReset;
    const status = this.typographyStatus;
    if (!trigger || !minus || !current || !plus || !reset || !status) return;
    const state = this.typographyState;
    const saving = state.status === "saving";
    trigger.textContent = String(state.size);
    trigger.setAttribute("aria-label", `Composer options, text size ${state.size} pixels`);
    trigger.title = trigger.getAttribute("aria-label") ?? "Composer options";
    current.value = `${state.size}px`;
    const setUnavailable = (button: HTMLButtonElement, unavailable: boolean): void => {
      // Native disabled drops keyboard focus in Chromium. aria-disabled keeps
      // the honest focus destination mounted while requestTypography guards
      // activation during save, outage, and bounds.
      button.disabled = false;
      button.setAttribute("aria-disabled", unavailable ? "true" : "false");
    };
    setUnavailable(minus, !state.enabled || saving || state.size <= COMPOSER_FONT_SIZE_MIN);
    setUnavailable(plus, !state.enabled || saving || state.size >= COMPOSER_FONT_SIZE_MAX);
    setUnavailable(reset, !state.enabled || saving || state.size === DEFAULT_COMPOSER_FONT_SIZE);
    this.typographyPopover?.setAttribute("aria-busy", saving ? "true" : "false");
    if (this.typographyPopover) this.typographyPopover.dataset.status = state.status;
    status.value = state.message;
    status.hidden = state.message === "";
    if (this.typographyOpen) {
      this.positionTypographyPopover();
      this.scheduleTypographyPosition();
    }
  }

  closeTypographyPopover(coordinated = false): void {
    this.setTypographyOpen(false, coordinated);
  }

  private setTypographyOpen(open: boolean, coordinated = false): void {
    if (!this.typographyTrigger || !this.typographyPopover) return;
    if (this.typographyOpen === open) {
      if (open) {
        this.positionTypographyPopover();
        this.scheduleTypographyPosition();
      }
      return;
    }
    if (open && !coordinated) this.options.claimTypographyPopover?.();
    this.typographyOpen = open;
    this.typographyPopover.hidden = !open;
    this.typographyTrigger.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) {
      // Only a visible popover owns global dismissal/position listeners.
      // Capture the target so cleanup removes them from the exact viewport.
      const viewport = window.visualViewport;
      document.addEventListener("pointerdown", this.onDocumentPointerDown);
      document.addEventListener("keydown", this.onDocumentKeydown, true);
      window.addEventListener("resize", this.onTypographyViewportChange);
      viewport?.addEventListener("resize", this.onTypographyViewportChange);
      viewport?.addEventListener("scroll", this.onTypographyViewportChange);
      this.typographyListenersCleanup = () => {
        document.removeEventListener("pointerdown", this.onDocumentPointerDown);
        document.removeEventListener("keydown", this.onDocumentKeydown, true);
        window.removeEventListener("resize", this.onTypographyViewportChange);
        viewport?.removeEventListener("resize", this.onTypographyViewportChange);
        viewport?.removeEventListener("scroll", this.onTypographyViewportChange);
      };
      this.positionTypographyPopover();
      this.scheduleTypographyPosition();
    }
    else {
      this.typographyListenersCleanup?.();
      this.typographyListenersCleanup = undefined;
      if (this.typographyPositionFrame !== undefined) window.cancelAnimationFrame(this.typographyPositionFrame);
      this.typographyPositionFrame = undefined;
      this.typographyPopover.style.removeProperty("left");
      this.typographyPopover.style.removeProperty("top");
      this.typographyPopover.style.removeProperty("max-width");
      this.typographyPopover.style.removeProperty("max-height");
      delete this.typographyPopover.dataset.placement;
      if (!coordinated) this.options.releaseTypographyPopover?.();
    }
  }

  private positionTypographyPopover(): void {
    const trigger = this.typographyTrigger;
    const popover = this.typographyPopover;
    if (!trigger || !popover || popover.hidden || this.destroyed) return;
    const viewport = window.visualViewport;
    const viewportLeft = viewport?.offsetLeft ?? 0;
    const viewportTop = viewport?.offsetTop ?? 0;
    const viewportWidth = viewport?.width ?? window.innerWidth;
    const viewportHeight = viewport?.height ?? window.innerHeight;
    const margin = 8;
    const gap = 6;
    const viewportRight = viewportLeft + viewportWidth;
    const viewportBottom = viewportTop + viewportHeight;
    const availableWidth = Math.max(1, viewportWidth - margin * 2);
    const availableHeight = Math.max(1, viewportHeight - margin * 2);
    popover.style.maxWidth = `${availableWidth}px`;
    popover.style.maxHeight = `${availableHeight}px`;
    const anchor = trigger.getBoundingClientRect();
    const measured = popover.getBoundingClientRect();
    const left = Math.min(
      Math.max(anchor.left, viewportLeft + margin),
      Math.max(viewportLeft + margin, viewportRight - margin - measured.width),
    );
    const roomAbove = anchor.top - gap - (viewportTop + margin);
    const roomBelow = viewportBottom - margin - (anchor.bottom + gap);
    const above = roomAbove >= measured.height || roomAbove > roomBelow;
    const desiredTop = above ? anchor.top - gap - measured.height : anchor.bottom + gap;
    const top = Math.min(
      Math.max(desiredTop, viewportTop + margin),
      Math.max(viewportTop + margin, viewportBottom - margin - measured.height),
    );
    popover.style.left = `${Math.round(left)}px`;
    popover.style.top = `${Math.round(top)}px`;
    popover.dataset.placement = above ? "top" : "bottom";
  }

  private scheduleTypographyPosition(): void {
    if (this.destroyed || !this.typographyOpen || this.typographyPositionFrame !== undefined) return;
    this.typographyPositionFrame = window.requestAnimationFrame(() => {
      this.typographyPositionFrame = undefined;
      this.positionTypographyPopover();
    });
  }

  private requestTypography(size: number): void {
    if (this.destroyed || !this.options.updateComposerFontSize || !this.typographyState.enabled || this.typographyState.status === "saving") return;
    if (!Number.isInteger(size) || size < COMPOSER_FONT_SIZE_MIN || size > COMPOSER_FONT_SIZE_MAX) return;
    if (size === this.typographyState.size) return;
    this.options.updateComposerFontSize(size);
  }

  // Chip strip re-render, bounded by the 8-attachment cap. Height changes
  // republish the inset through the panel's existing ResizeObserver \u2014 no new
  // publication mechanism exists here.
  private renderAttachments(): void {
    if (!this.chipStrip) return;
    // Chip controls are replaced on every render. Retire their event-state
    // fences before dropping the nodes so repeated upload progress renders
    // cannot retain stale control provenance or listener closures.
    for (const cleanup of this.renderedActivationCleanups.splice(0)) cleanup();
    if (this.attachButton) {
      const full = this.attachments.length >= MAX_COMPOSER_ATTACHMENTS;
      this.attachButton.disabled = full;
      this.attachButton.title = full ? ATTACHMENT_LIMIT_REASON : "Attach images";
      this.attachButton.setAttribute("aria-label", full ? ATTACHMENT_LIMIT_REASON : "Attach images");
    }
    if (this.attachments.length === 0) {
      delete this.panel.dataset.attachments;
      this.chipStrip.hidden = true;
      this.chipStrip.replaceChildren();
      return;
    }
    this.panel.dataset.attachments = "present";
    this.chipStrip.hidden = false;
    this.chipStrip.replaceChildren(...this.attachments.map((attachment, index) => this.renderChip(attachment, index)));
  }

  private renderChip(attachment: ComposerAttachment, index: number): HTMLLIElement {
    const chip = document.createElement("li");
    chip.className = "attachment-page__composer-chip";
    chip.dataset.status = attachment.status;
    if (attachment.status === "uploading") chip.setAttribute("aria-busy", "true");
    const label = document.createElement("span");
    label.className = "attachment-page__composer-chip-label";
    label.textContent = attachment.label;
    label.title = attachment.label;
    const size = document.createElement("span");
    size.className = "attachment-page__composer-chip-size";
    size.textContent = attachmentSizeLabel(attachment.bytes);
    chip.append(label, size);
    if (attachment.status === "uploading") {
      const busy = document.createElement("span");
      busy.className = "attachment-page__composer-chip-busy";
      busy.textContent = "\u27f3";
      chip.append(busy);
    }
    if (attachment.status === "failed") {
      const note = document.createElement("span");
      note.className = "attachment-page__composer-chip-note";
      note.textContent = attachment.error ?? imageStagingFailureCopy("network");
      note.title = note.textContent;
      chip.append(note);
      if (attachment.file) {
        const retry = this.chipButton("Retry", `Retry image ${index + 1} of ${this.attachments.length}`);
        this.renderedActivationCleanups.push(bindGenerationFencedClickActivation(retry, () => this.retryAttachment(attachment), () => !this.destroyed, this.options.interactionGeneration));
        chip.append(retry);
      }
    }
    const remove = this.chipButton("\u00d7", `Remove image ${index + 1} of ${this.attachments.length}`);
    this.renderedActivationCleanups.push(bindGenerationFencedClickActivation(remove, () => this.removeAttachment(attachment), () => !this.destroyed, this.options.interactionGeneration));
    chip.append(remove);
    return chip;
  }

  private chipButton(text: string, label: string): HTMLButtonElement {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "attachment-page__composer-chip-button";
    button.textContent = text;
    button.setAttribute("aria-label", label);
    button.title = label;
    return button;
  }

  // Removal only forgets the chip locally: a staged server-side file is left
  // to expire on its TTL. A delete endpoint would be a second write primitive
  // and a second authorization surface for a file that expires anyway.
  private removeAttachment(attachment: ComposerAttachment): void {
    if (this.destroyed) return;
    const index = this.attachments.indexOf(attachment);
    if (index === -1) return;
    attachment.abort?.abort();
    this.attachments.splice(index, 1);
    this.render();
    this.restoreTextareaFocus();
  }

  private retryAttachment(attachment: ComposerAttachment): void {
    if (this.destroyed || attachment.status !== "failed" || !attachment.file || !this.attachments.includes(attachment)) return;
    this.startUpload(attachment, attachment.file);
    this.render();
  }

  private stageFile(file: File): void {
    if (this.destroyed || !this.stageImage) return;
    if (this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) {
      this.render(ATTACHMENT_LIMIT_REASON);
      return;
    }
    const attachment: ComposerAttachment = {
      localId: `attachment-${(attachmentSerial += 1)}`,
      file,
      label: attachmentDisplayLabel(file.name),
      bytes: file.size,
      status: "uploading",
    };
    this.attachments.push(attachment);
    this.attachmentBytesTotal += file.size;
    this.startUpload(attachment, file);
    this.render();
  }

  // An immediately-failed chip for a pasted image outside the allow-list:
  // the operator always sees where their image went. The File, when the
  // clipboard yielded one, is kept so Retry stays possible after they read
  // the reason (the server re-decides and answers with the same closed copy).
  private addRefusedAttachment(file: File | undefined): void {
    if (this.destroyed) return;
    if (this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) {
      this.render(ATTACHMENT_LIMIT_REASON);
      return;
    }
    this.attachments.push({
      localId: `attachment-${(attachmentSerial += 1)}`,
      file,
      label: attachmentDisplayLabel(file?.name),
      bytes: file?.size ?? 0,
      status: "failed",
      error: PASTED_IMAGE_REFUSAL_COPY,
    });
    this.attachmentFailures += 1;
    this.render();
  }

  private startUpload(attachment: ComposerAttachment, file: File): void {
    const stageImage = this.stageImage;
    if (!stageImage) return;
    const controller = new AbortController();
    attachment.status = "uploading";
    attachment.error = undefined;
    attachment.path = undefined;
    attachment.serverId = undefined;
    attachment.expiresAt = undefined;
    attachment.abort = controller;
    this.attachmentUploads += 1;
    void stageImage(file, controller.signal).then((staged) => {
      if (this.destroyed || controller.signal.aborted || !this.attachments.includes(attachment)) return;
      attachment.status = "staged";
      attachment.path = staged.path;
      attachment.serverId = staged.id;
      attachment.expiresAt = staged.expiresAt;
      attachment.abort = undefined;
      this.render();
    }).catch((error: unknown) => {
      if (this.destroyed || controller.signal.aborted || !this.attachments.includes(attachment)) return;
      attachment.status = "failed";
      attachment.error = imageStagingFailureCopy(error instanceof ImageStagingError ? error.kind : "network");
      attachment.abort = undefined;
      this.attachmentFailures += 1;
      this.render();
    });
  }

  private clearAttachments(): void {
    for (const attachment of this.attachments) attachment.abort?.abort();
    this.attachments.length = 0;
  }

  private applyMode(): void {
    const prose = this.mode === "prose";
    this.modeButton.textContent = prose ? "Aa" : ">_";
    this.modeButton.setAttribute("aria-pressed", prose ? "true" : "false");
    this.modeButton.setAttribute("aria-label", prose ? "Prose writing mode" : "Code input");
    this.modeButton.title = prose
      ? "Prose writing mode: autocorrect, capitalization and spellcheck on. Tap for Code input"
      : "Code input: autocorrect, capitalization and spellcheck off. Tap for Prose writing mode";
    this.textarea.autocapitalize = prose ? "sentences" : "off";
    this.textarea.setAttribute("autocorrect", prose ? "on" : "off");
    this.textarea.spellcheck = prose;
    if (!prose) this.textarea.autocomplete = "off";
    else this.textarea.removeAttribute("autocomplete");
  }

  private applySize(): void {
    this.panel.dataset.size = this.size;
    this.sizeButton.setAttribute("aria-pressed", this.size === "expanded" ? "true" : "false");
    this.sizeButton.setAttribute("aria-label", this.size === "expanded" ? "Compact composer" : "Expand composer");
    this.sizeButton.title = this.sizeButton.getAttribute("aria-label") ?? "";
    this.sizeButton.textContent = this.size === "expanded"
      ? (this.symbolic ? "\u2921" : "\u2922 Shorter")
      : (this.symbolic ? "\u2922" : "\u2922 Taller");
  }

  private scheduleAutoGrow(): void {
    if (this.destroyed || !this.openState || this.measureFrame !== undefined) return;
    this.measureFrame = window.requestAnimationFrame(() => {
      this.measureFrame = undefined;
      if (this.destroyed || !this.openState) return;
      const insetBudget = Math.max(0, Math.floor(this.options.insetBudget()));
      this.panel.style.setProperty("--persea-composer-inset-budget", `${insetBudget}px`);
      // Measuring at auto height can move the browser's internal scroll anchor.
      // Preserve the current reading position, including an interaction that
      // occurred after a typography change scheduled this measurement.
      const scrollTop = this.textarea.scrollTop;
      const scrollLeft = this.textarea.scrollLeft;
      this.textarea.style.height = "auto";
      // The percentage the operator dragged the grip to, and the height the
      // panel is actually allowed. The second is not optional: a percentage of
      // the page is a percentage of a box that may extend under the software
      // keyboard, and "Taller" then grows the panel past the screen and takes
      // the chrome row — ⊕ ⌫ ➤ — with it (C2). The budget is measured off the
      // visible band by the page that supplies it.
      //
      // The budget bounds the WHOLE panel, so the textarea may have only what
      // is left after the panel's own chrome. That chrome is measured here,
      // with the textarea at its auto height, rather than assumed: it differs
      // between the two presentations and grows with an attachment strip or a
      // guard row.
      const chrome = Math.max(0, Math.round(
        this.panel.getBoundingClientRect().height - this.textarea.getBoundingClientRect().height,
      ));
      const budgetCap = Math.max(24, insetBudget - chrome);
      const percentCap = this.resizePercent > 0
        ? Math.max(24, Math.round(this.options.page.clientHeight * this.resizePercent / 100))
        : Number.POSITIVE_INFINITY;
      const cap = Math.min(percentCap, budgetCap);
      const height = this.resizePercent > 0 ? cap : Math.min(this.textarea.scrollHeight, cap);
      this.textarea.style.height = `${Math.max(24, height)}px`;
      this.textarea.scrollTop = scrollTop;
      this.textarea.scrollLeft = scrollLeft;
      // Auto-grow moves the fixed portal's anchor without a window resize.
      // Position it in the following layout frame, after the new panel and
      // trigger geometry is observable.
      this.scheduleTypographyPosition();
      void this.publishInset();
    });
  }

  private scheduleInsetPublication(): void {
    if (this.destroyed || this.measureFrame !== undefined) return;
    this.measureFrame = window.requestAnimationFrame(() => {
      this.measureFrame = undefined;
      void this.publishInset();
    });
  }

  private async publishInset(reveal = true): Promise<void> {
    const measured = this.openState ? Math.max(0, Math.round(this.panel.getBoundingClientRect().height)) : 0;
    const next = Math.min(measured, Math.max(0, this.options.insetBudget()));
    if (next === this.appliedInset || next === this.pendingInset) return;
    const generation = ++this.insetGeneration;
    this.pendingInset = next;
    const stillCurrent = (): boolean => {
      const measured = this.openState ? Math.max(0, Math.round(this.panel.getBoundingClientRect().height)) : 0;
      const reclamped = Math.min(measured, Math.max(0, this.options.insetBudget()));
      return generation === this.insetGeneration && this.pendingInset === next && reclamped === next;
    };
    const committed = await this.options.commitOverlayInset(next, stillCurrent);
    if (this.destroyed || generation !== this.insetGeneration || this.pendingInset !== next) return;
    this.pendingInset = undefined;
    if (!committed) {
      this.scheduleInsetPublication();
      return;
    }
    this.appliedInset = next;
    if (reveal) this.options.revealLiveEdgeForOverlay();
    else this.options.cancelPendingOverlayReveal();
    if (this.focusScrollListeners.size !== 0) {
      this.insetPublicationCount += 1;
      this.emitFocusScroll(Object.freeze({ kind: "composer-inset-publication" }));
    }
  }

  private removeInset(reveal = true): void {
    this.panel.style.removeProperty("--persea-composer-inset-budget");
    if (this.appliedInset === 0 && this.pendingInset === undefined) {
      if (!reveal) this.options.cancelPendingOverlayReveal();
      return;
    }
    void this.publishInset(reveal);
  }

  private scheduleStoredDraft(): void {
    if (this.saveTimer !== undefined) window.clearTimeout(this.saveTimer);
    this.saveTimer = window.setTimeout(() => {
      this.saveTimer = undefined;
      this.flushStoredDraft();
    }, SAVE_DEBOUNCE_MS);
  }

  private flushStoredDraft(): void {
    if (this.saveTimer !== undefined) {
      window.clearTimeout(this.saveTimer);
      this.saveTimer = undefined;
    }
    const text = this.text();
    if (text.length === 0) {
      this.deleteStoredDraft();
      return;
    }
    if (!this.storageKey) {
      this.storageDegraded = true;
      this.render();
      return;
    }
    const value = JSON.stringify({ text, mode: this.mode, size: this.size, savedAt: Date.now() } satisfies StoredDraft);
    if (new TextEncoder().encode(value).byteLength > MAX_STORED_BYTES) {
      this.storageDegraded = true;
      this.storageFailures += 1;
      this.render();
      return;
    }
    try {
      window.sessionStorage.setItem(this.storageKey, value);
      this.storageWrites += 1;
    } catch {
      this.storageDegraded = true;
      this.storageFailures += 1;
      this.render();
    }
  }

  private restoreStoredDraft(): void {
    if (!this.storageKey) return;
    try {
      const raw = window.sessionStorage.getItem(this.storageKey);
      if (raw === null) return;
      const parsed = JSON.parse(raw) as Partial<StoredDraft>;
      if (typeof parsed.text !== "string"
        || (parsed.mode !== "prose" && parsed.mode !== "code")
        || (parsed.size !== "compact" && parsed.size !== "expanded")
        || typeof parsed.savedAt !== "number"
        || !Number.isFinite(parsed.savedAt)) {
        throw new Error("stored composer draft is malformed");
      }
      if (Date.now() - parsed.savedAt > MAX_STORED_AGE_MS || parsed.savedAt > Date.now() + 60_000) {
        window.sessionStorage.removeItem(this.storageKey);
        return;
      }
      if (new TextEncoder().encode(raw).byteLength > MAX_STORED_BYTES) {
        window.sessionStorage.removeItem(this.storageKey);
        throw new Error("stored composer draft is oversized");
      }
      this.setText(parsed.text);
      this.mode = parsed.mode;
      this.size = parsed.size;
    } catch {
      try { window.sessionStorage.removeItem(this.storageKey); } catch { /* already degraded */ }
      this.storageDegraded = true;
      this.storageFailures += 1;
    }
  }

  private deleteStoredDraft(): void {
    if (!this.storageKey) return;
    try {
      window.sessionStorage.removeItem(this.storageKey);
    } catch {
      this.storageDegraded = true;
      this.storageFailures += 1;
    }
  }

  private defaultResizePercent(): number {
    if (this.density === "compact") return this.size === "compact" ? 20 : 35;
    return this.size === "compact" ? 25 : 45;
  }

  private maximumResizePercent(): number {
    return this.density === "compact" ? 55 : 70;
  }

  private syncResizeTarget(preferences: PreferencesV1): void {
    this.resizePercent = preferences.composer.heightPercent[this.density][this.size] ?? 0;
    this.grip.setAttribute("aria-valuemin", "10");
    this.grip.setAttribute("aria-valuemax", String(this.maximumResizePercent()));
    this.grip.setAttribute("aria-valuenow", String(this.resizePercent || this.defaultResizePercent()));
  }

  private persistComposer(heightPercent?: number): void {
    const current = this.options.preferences();
    const active = current.composer.heightPercent[this.density];
    const nextActive = heightPercent === undefined
      ? active
      : Object.freeze({ ...active, [this.size]: heightPercent });
    this.options.updatePreferences(Object.freeze({
      version: 1,
      composer: Object.freeze({
        density: this.density,
        mode: this.mode,
        panelSize: this.size,
        heightPercent: Object.freeze({
          standard: this.density === "standard" ? nextActive : current.composer.heightPercent.standard,
          compact: this.density === "compact" ? nextActive : current.composer.heightPercent.compact,
        }),
      }),
      keyBar: Object.freeze({ collapsed: current.keyBar.collapsed }),
    }));
  }

  private setResizePercent(value: number): void {
    this.resizePercent = Math.max(10, Math.min(this.maximumResizePercent(), Math.round(value)));
    this.grip.setAttribute("aria-valuemin", "10");
    this.grip.setAttribute("aria-valuemax", String(this.maximumResizePercent()));
    this.grip.setAttribute("aria-valuenow", String(this.resizePercent));
    this.persistComposer(this.resizePercent);
    this.scheduleAutoGrow();
  }

  private readonly onTabActivate = (event: Event): void => {
    if (!event.isTrusted || this.destroyed) return;
    if (this.contentState === "sent") this.panel.dataset.sentReopened = "true";
    this.setOpen(true);
  };
  private readonly onClose: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    event.preventDefault();
    this.setOpen(false);
    if (event.type === "click") this.options.refocusTerminal();
  };
  private readonly onSend: EventListener = (event) => { this.send(event, false, event.type === "click"); };
  private readonly onOverride: EventListener = (event) => { this.send(event, true, event.type === "click"); };
  private readonly onInput: EventListener = () => {
    if (this.destroyed) return;
    this.noteTypographyInteraction();
    this.setText(this.textarea.value);
    this.contentState = this.hasInsertableContent() ? "draft" : "empty";
    this.scheduleStoredDraft();
    this.render();
  };
  private readonly onTextareaKeydown: EventListener = (value) => {
    const event = value as KeyboardEvent;
    if (!event.isTrusted || this.destroyed) return;
    this.noteTypographyInteraction();
    if (event.key === "Escape") {
      event.preventDefault();
      this.closeFromTrustedEvent(event);
    } else if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      this.send(event, false);
    }
  };
  private readonly onJoin: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    this.setText(this.text().replace(/\n+/g, " ").replace(/\t/g, " "));
    this.contentState = "draft";
    this.scheduleStoredDraft();
    this.render();
  };
  private readonly onFix: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    const replacements: Readonly<Record<string, string>> = Object.freeze({
      "\u2014": "--", "\u2013": "-", "\u201c": "\"", "\u201d": "\"", "\u2018": "'", "\u2019": "'",
    });
    this.setText(this.text().replace(SMART_PUNCTUATION_GLOBAL, (value) => replacements[value] ?? value));
    this.contentState = "draft";
    this.scheduleStoredDraft();
    this.render();
  };
  // Clear clears the whole draft including chips (aborting in-flight
  // uploads); the restore slot keeps the TEXT only, because a staged path
  // could outlive its TTL and a restored chip would then lie.
  private readonly onClear: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed || (this.text().length === 0 && this.attachments.length === 0)) return;
    this.lastRestore = this.text();
    this.lastRestoreDroppedImages = this.attachments.length > 0;
    this.restoredWithoutImages = false;
    this.clearAttachments();
    this.setText("");
    this.deleteStoredDraft();
    this.contentState = "empty";
    this.render();
  };
  private readonly onRestore: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed || this.text().length > 0 || this.lastRestore.length === 0) return;
    this.setText(this.lastRestore);
    this.restoredWithoutImages = this.lastRestoreDroppedImages;
    this.contentState = "draft";
    this.restores += 1;
    this.scheduleStoredDraft();
    this.render();
  };
  private readonly onAttachClick: EventListener = () => {
    if (this.destroyed || !this.stageImage || !this.fileInput || this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) return;
    this.fileInput.click();
  };
  private readonly onFileInputChange: EventListener = () => {
    if (this.destroyed || !this.fileInput) return;
    const files = Array.from(this.fileInput.files ?? []);
    this.fileInput.value = "";
    for (const file of files) {
      // The accept list guides the picker but guarantees nothing; a file
      // outside the allow-list fails fast with the same closed copy the
      // server would answer with, sparing the useless upload.
      if (ALLOWED_IMAGE_TYPES.includes(file.type)) this.stageFile(file);
      else this.addTypeRefusedFile(file);
    }
    // Focus returns to the textarea after the picker; on iOS this raises the
    // keyboard only on the operator's next tap, which is the stated cost of
    // the native picker (paste is the fast path).
    this.restoreTextareaFocus();
  };
  private addTypeRefusedFile(file: File): void {
    if (this.attachments.length >= MAX_COMPOSER_ATTACHMENTS) {
      this.render(ATTACHMENT_LIMIT_REASON);
      return;
    }
    this.attachments.push({
      localId: `attachment-${(attachmentSerial += 1)}`,
      file,
      label: attachmentDisplayLabel(file.name),
      bytes: file.size,
      status: "failed",
      error: imageStagingFailureCopy("unsupported_type"),
    });
    this.attachmentFailures += 1;
    this.render();
  }
  // Paste interception, two-stage (design §3.3): the event is captured only
  // when at least one image FILE item is present, so a plain text paste — the
  // phone's only clipboard route into the terminal — is never intercepted. A
  // mixed clipboard keeps both halves; an allow-listed image stages; any
  // other image kind becomes a visible failed chip, never a silent drop.
  private readonly onPaste: EventListener = (value) => {
    const event = value as ClipboardEvent;
    if (this.destroyed || !this.stageImage) return;
    const data = event.clipboardData;
    if (!data) return;
    const triage = triageClipboardItems(Array.from(data.items));
    if (!triage.captured) return;
    event.preventDefault();
    const text = data.getData("text/plain");
    if (text !== "") this.insertTextAtSelection(text);
    for (const file of triage.allowed) this.stageFile(file);
    for (const refused of triage.refused) this.addRefusedAttachment(refused);
  };
  private insertTextAtSelection(text: string): void {
    const start = this.textarea.selectionStart ?? this.textarea.value.length;
    const end = this.textarea.selectionEnd ?? this.textarea.value.length;
    this.textarea.setRangeText(text, start, end, "end");
    this.setText(this.textarea.value);
    this.contentState = this.hasInsertableContent() ? "draft" : "empty";
    this.scheduleStoredDraft();
  }
  private readonly onToggleMode: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    this.mode = this.mode === "prose" ? "code" : "prose";
    this.applyMode();
    this.persistComposer();
    this.scheduleStoredDraft();
    this.render();
  };
  private readonly onToggleTypography: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    event.preventDefault();
    this.setTypographyOpen(!this.typographyOpen);
  };
  private readonly onTypographyMinus: EventListener = (event) => {
    if (!event.isTrusted) return;
    event.preventDefault();
    this.requestTypography(this.typographyState.size - 1);
  };
  private readonly onTypographyPlus: EventListener = (event) => {
    if (!event.isTrusted) return;
    event.preventDefault();
    this.requestTypography(this.typographyState.size + 1);
  };
  private readonly onTypographyReset: EventListener = (event) => {
    if (!event.isTrusted) return;
    event.preventDefault();
    this.requestTypography(DEFAULT_COMPOSER_FONT_SIZE);
  };
  private readonly onDocumentPointerDown: EventListener = (value) => {
    if (!this.typographyOpen || !this.typographyRoot) return;
    const target = value.target;
    if (target instanceof Node
      && !this.typographyRoot.contains(target)
      && this.typographyPopover?.contains(target) !== true) this.setTypographyOpen(false);
  };
  private readonly onDocumentKeydown: EventListener = (value) => {
    const event = value as KeyboardEvent;
    if (!event.isTrusted || !this.typographyOpen || event.key !== "Escape") return;
    const focusWasInside = this.typographyPopover?.contains(document.activeElement) === true;
    event.preventDefault();
    event.stopPropagation();
    this.setTypographyOpen(false);
    if (focusWasInside) this.typographyTrigger?.focus({ preventScroll: true });
  };
  private readonly onTypographyViewportChange: EventListener = () => {
    if (this.typographyOpen) this.positionTypographyPopover();
    this.scheduleTypographyPosition();
  };
  private readonly onToggleSize: EventListener = (event) => {
    if (!event.isTrusted || this.destroyed) return;
    this.size = this.size === "compact" ? "expanded" : "compact";
    const target = this.options.preferences().composer.heightPercent[this.density][this.size];
    this.resizePercent = target ?? 0;
    if (this.resizePercent === 0 && this.density === "standard") {
      this.setResizePercent(this.size === "expanded" ? 45 : 25);
    } else if (this.resizePercent === 0) {
      this.setResizePercent(this.size === "expanded" ? 35 : 20);
    }
    else this.persistComposer();
    this.applySize();
    this.scheduleAutoGrow();
    this.scheduleStoredDraft();
    this.render();
  };
  private readonly onVisibilityChange = (): void => {
    if (document.visibilityState === "hidden") this.flushStoredDraft();
  };
  private readonly onPageHide = (): void => { this.flushStoredDraft(); };
  // iOS zooms the page when a text field takes focus with a computed font size
  // under 16px, and it takes that decision at the moment of focus only. So on a
  // coarse pointer the textarea wears a 16px face for that instant — armed on
  // the pointer that is about to focus it and before any programmatic focus —
  // and returns to the chosen face on the frame after focus. Page zoom itself
  // is never capped (unrestricted-zoom: a `maximum-scale` cap defeats WCAG 1.4.4).
  private focusZoomGuardRelease?: ReturnType<typeof setTimeout>;
  private armFocusZoomGuard(): void {
    if (this.destroyed || !window.matchMedia("(pointer: coarse)").matches) return;
    this.textarea.style.fontSize = "16px";
    if (this.focusZoomGuardRelease !== undefined) clearTimeout(this.focusZoomGuardRelease);
    // A pointer that never focuses (cancelled, or the field already focused)
    // must not leave the 16px face behind.
    this.focusZoomGuardRelease = setTimeout(() => this.releaseFocusZoomGuard(), 1_000);
  }
  private releaseFocusZoomGuard(): void {
    if (this.focusZoomGuardRelease !== undefined) {
      clearTimeout(this.focusZoomGuardRelease);
      this.focusZoomGuardRelease = undefined;
    }
    this.textarea.style.removeProperty("font-size");
  }
  private readonly onTextareaPointerDown: EventListener = (event) => {
    this.textareaPointers.add((event as PointerEvent).pointerId);
    this.noteTypographyInteraction();
    if (document.activeElement !== this.textarea) this.armFocusZoomGuard();
  };
  private readonly onTextareaPointerEnd: EventListener = (event) => {
    if (this.textareaPointers.delete((event as PointerEvent).pointerId)) this.noteTypographyInteraction();
  };
  private readonly onTextareaTouchStart: EventListener = (event) => {
    for (const touch of Array.from((event as TouchEvent).changedTouches)) this.textareaTouches.add(touch.identifier);
    this.noteTypographyInteraction();
    if (document.activeElement !== this.textarea) this.armFocusZoomGuard();
  };
  private readonly onTextareaTouchEnd: EventListener = (event) => {
    for (const touch of Array.from((event as TouchEvent).changedTouches)) {
      if (this.textareaTouches.delete(touch.identifier)) this.noteTypographyInteraction();
    }
  };
  private textareaGestureActive(): boolean {
    return this.textareaPointers.size !== 0 || this.textareaTouches.size !== 0;
  }
  // A native thumb drag can continue without another input event. During a
  // gesture even a layout-generated notification must yield to user intent.
  // Without input intent, font reflow and layout offset writes still need the
  // pending repair. Product navigation declares intent through scrollDraftTo.
  private readonly onTextareaScroll: EventListener = () => {
    if (this.textareaGestureActive()) this.noteTypographyInteraction();
  };
  private readonly onTextareaScrollIntent: EventListener = () => {
    if (!this.destroyed) this.noteTypographyInteraction();
  };
  private readonly onTextareaFocus: EventListener = () => {
    if (this.focusZoomGuardRelease !== undefined) {
      window.requestAnimationFrame(() => this.releaseFocusZoomGuard());
    }
    if (this.focusScrollListeners.size !== 0) {
      this.emitFocusScroll(Object.freeze({ kind: "composer-focus-trigger" }));
    }
    if (this.destroyed) return;
    const scroller = document.scrollingElement;
    if (scroller && scroller.scrollTop !== 0) scroller.scrollTop = 0;
    if (this.focusRevealFrame !== undefined) window.cancelAnimationFrame(this.focusRevealFrame);
    this.focusRevealFrame = window.requestAnimationFrame(() => {
      this.focusRevealFrame = undefined;
      if (!this.destroyed) this.options.revealLiveEdgeForOverlay();
    });
  };

  private restoreTextareaFocus(): void {
    if (this.destroyed) return;
    if (document.activeElement !== this.textarea) this.armFocusZoomGuard();
    this.textarea.focus({ preventScroll: true });
  }
  private noteTypographyInteraction(): void {
    this.typographyInteractionGeneration += 1;
  }
  private readonly onGripPointerDown: EventListener = (value) => {
    const event = value as PointerEvent;
    if (!event.isTrusted || this.destroyed || event.button !== 0) return;
    event.preventDefault();
    const startPercent = this.resizePercent || Math.round(this.textarea.getBoundingClientRect().height / Math.max(1, this.options.page.clientHeight) * 100);
    this.drag = Object.freeze({ pointerId: event.pointerId, startY: event.clientY, startPercent });
    this.grip.setPointerCapture(event.pointerId);
  };
  private readonly onGripPointerMove: EventListener = (value) => {
    const event = value as PointerEvent;
    if (!this.drag || this.drag.pointerId !== event.pointerId || this.destroyed) return;
    event.preventDefault();
    this.setResizePercent(this.drag.startPercent + (this.drag.startY - event.clientY) / Math.max(1, this.options.page.clientHeight) * 100);
  };
  private readonly onGripPointerEnd: EventListener = (value) => {
    const event = value as PointerEvent;
    if (!this.drag || this.drag.pointerId !== event.pointerId) return;
    this.drag = undefined;
    if (this.grip.hasPointerCapture(event.pointerId)) this.grip.releasePointerCapture(event.pointerId);
  };
  private readonly onGripKeydown: EventListener = (value) => {
    const event = value as KeyboardEvent;
    if (!event.isTrusted || this.destroyed) return;
    const current = this.resizePercent || this.defaultResizePercent();
    let next: number | undefined;
    if (event.key === "ArrowUp") next = current + 5;
    else if (event.key === "ArrowDown") next = current - 5;
    else if (event.key === "Home") next = 10;
    else if (event.key === "End") next = this.maximumResizePercent();
    if (next === undefined) return;
    event.preventDefault();
    this.setResizePercent(next);
  };
}
