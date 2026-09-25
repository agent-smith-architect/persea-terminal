// Public terminal page options, viewport integration, and insertion results.
import type { AttachmentPagePort } from "./attachment_port";
import type { ComposerStagedImage } from "./composer_attachments";
import type { SnippetServicePort } from "./snippet_client";
import type { ScrollbackRows } from "./scrollback_preferences";
import type { OperatorPreferencePort } from "./operator_preferences";
import type { CommitFocusContext } from "./unified_focus_claim";
import type { SessionSwitcherInventory } from "./session_switcher";
import type { DashboardSession } from "./dashboard";
import type { WidthRefitAttempt } from "./unified_terminal_geometry_types";

// The slice of VisualViewport the page consumes. Injected so a harness can
// drive keyboard open/close; production passes nothing and the page reads
// window.visualViewport.
export type UnifiedViewportInsetSource = Readonly<{
  readonly height: number;
  readonly scale: number;
  readonly offsetLeft: number;
  readonly offsetTop: number;
  addEventListener(type: "resize" | "scroll", listener: () => void): void;
  removeEventListener(type: "resize" | "scroll", listener: () => void): void;
}>;

// How an insert ended, in the operator's terms. The sheet renders one fixed
// sentence for each: nothing is ever dropped silently.
export type InsertTextResult = "SENT" | "COMPOSER" | "REFUSED_NO_CONTROL" | "REFUSED_EMPTY" | "REFUSED_DESTROYED";

export type UnifiedTerminalPageOptions = Readonly<{
  root: HTMLElement;
  port: AttachmentPagePort;
  capabilityMode: "observe" | "control";
  styleNonce: string;
  historyRows?: ScrollbackRows;
  scrollbackScope?: string;
  onScrollbackChanged?(rows: ScrollbackRows): void;
  reloadRecordedHistory?(): boolean;
  // Display-only session name from the URL fragment, shown in the reconnecting
  // strip ("Reconnecting to <name>…") so a reopen names what it is restoring.
  // No authority: the attached session is fixed by the handle.
  sessionName?: string;
  // Display-only alias from the URL fragment / the pane controller's resolved
  // identity, shown beside the name in the top bar's session tag. Like
  // sessionName it carries no authority: the attached session is fixed by the
  // handle. Absent when the session has no alias.
  aliasLabel?: string;
  viewportInset?: UnifiedViewportInsetSource;
  // Draft-persistence scope for the composer, carried by the terminal URL's
  // display-only draft_scope fragment field (same plumbing as the legacy
  // page). Absent, the composer runs storage-degraded: drafts live only in
  // this tab.
  composerStorageScope?: string;
  // Image staging for the composer, present exactly when the dashboard's
  // inventory advertised the capability for this session's realm (the
  // display/UX-only image_realm fragment field). The server re-authorizes
  // every upload regardless; absence means the composer has no image
  // affordance at all.
  stageImage?(file: File, signal: AbortSignal): Promise<ComposerStagedImage>;
  rememberSource(source: string): void;
  // `rows` rides the same generation refit when the operator typed both; an
  // absent rows keeps the predecessor's height (the engine's default).
  refitWidth?(columns: number, rows?: number): WidthRefitAttempt;
  // Control-takeover claim: resolves a fresh takeover endpoint for this
  // session (given the last prepared source when one exists) which the page
  // hands to its port. Present only in control mode.
  takeControl?(source: string | undefined, signal: AbortSignal): Promise<Readonly<{ url: string; protocols: readonly string[] }>>;
  // First COMMIT of this page's lifetime; the app clears its one-shot
  // takeover offer here, mirroring the legacy page.
  onFirstCommit?(): void;
  // Every successful admission commit. Unlike onFirstCommit, this runs again
  // after an explicit session switch so session memory memory can record the new exact
  // identity only after the new session is genuinely live.
  onCommit?(generation: number): void;
  sessionSwitch?: Readonly<{
    currentDraftScope(): string | null;
    inventory(refresh: boolean, signal: AbortSignal): Promise<SessionSwitcherInventory>;
    blockedMessage(session: DashboardSession): string;
    select(session: DashboardSession): Promise<Readonly<{ ok: boolean; message: string }>>;
  }>;
  // Focus-claim policy consulted on every COMMIT, AFTER the MODE_REQUEST has
  // been sent (that order is load-bearing, abd2dce) and only for the one
  // terminal.focus() the admission path makes. Absent, the page applies its
  // pointer rule (context.pointerRuleClaims). Returning true focuses xterm's
  // helper textarea — on a phone, that raises the keyboard.
  claimFocusOnCommit?(context: CommitFocusContext): boolean;
  // The ONE document-global snippets/clips service, supplied by the consumer
  // that owns the document (app.ts for a single terminal, WorkspacePage for a
  // workspace). The page is a subscriber and a caller; it never constructs
  // one, so six panes still poll once. Without a service the Clipboard control
  // is unavailable and no shared-text request is made.
  snippets?: SnippetServicePort;
  // The ONE document-global operator preference service. It is loaded by the
  // document owner before page construction, so its current theme and font
  // baseline are available before xterm opens and performs its first fit.
  preferences?: OperatorPreferencePort;
  // Workspace owns document navigation; a pane-local sheet must not offer a
  // Dashboard action that would tear down its siblings.
  workspaceCell?: boolean;
}>;
