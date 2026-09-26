declare const require: (name: string) => unknown;
declare const process: { env: Record<string, string | undefined>; cwd(): string; stdout: { write(value: string): void } };

const fs = require("node:fs") as { readFileSync(path: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };
const sourcePath = process.env.PERSEA_TERMINAL_INTERACTION_SOURCE
  ?? path.join(process.cwd(), "src", "unified_terminal_page.ts");
const source = fs.readFileSync(sourcePath, "utf8")
  + ["unified_terminal_options", "unified_terminal_geometry_types", "unified_terminal_state_types"]
    .map((name) => fs.readFileSync(path.join(process.cwd(), "src", `${name}.ts`), "utf8")).join("\n");
const controller = fs.readFileSync(path.join(process.cwd(), "src", "unified_pane_controller.ts"), "utf8");
const packageSource = fs.readFileSync(path.join(process.cwd(), "package.json"), "utf8");
const packageJSON = JSON.parse(packageSource) as { scripts?: Record<string, string> };
const terminal_interactionBrowser = fs.readFileSync(path.join(process.cwd(), "test", "run_terminal_interaction_browser.cjs"), "utf8");
const composerSource = fs.readFileSync(path.join(process.cwd(), "src", "composer.ts"), "utf8");
const sessionSwitcherSource = fs.readFileSync(path.join(process.cwd(), "src", "session_switcher.ts"), "utf8");
const tapActivationSource = fs.readFileSync(path.join(process.cwd(), "src", "tap_activation.ts"), "utf8");

const assert = (condition: boolean, message: string): void => {
  if (!condition) throw new Error(message);
};

// Persistent button owners choose one of
// three explicit paths: scroll-safe tap, terminal-focus-preserving key, or
// native-focus generation-fenced click. A raw click registration or a fake
// zero/optional authority is therefore a source-level regression, while the
// helper implementations remain the only place DOM click listeners exist.
for (const [owner, ownerSource] of Object.entries({
  unified: source,
  composer: composerSource,
  sessionSwitcher: sessionSwitcherSource,
})) {
  assert(!/addEventListener\(\s*["']click["']/.test(ownerSource), `${owner} persistent activation bypassed the classified helpers`);
  assert(!ownerSource.includes("() => 0"), `${owner} persistent activation silently substituted generation zero`);
  assert(!ownerSource.includes("interactionGeneration?"), `${owner} interaction generation became optional`);
}
assert(composerSource.includes("interactionGeneration(): number;")
  && composerSource.includes("this.options.interactionGeneration"), "Composer must require and consume owner interaction authority");
assert(sessionSwitcherSource.includes("interactionGeneration(): number;")
  && sessionSwitcherSource.includes("this.options.interactionGeneration"), "SessionSwitcher must require and consume owner interaction authority");
assert((source.match(/new SessionSwitcherView\(\{/g) ?? []).length === 2
  && (source.match(/interactionGeneration: \(\) => this\.keyInteractionGeneration/g) ?? []).length >= 3,
"both unified SessionSwitcher instances and Composer must receive page interaction authority");
assert(/bindTapActivation\(\s*composerToggle,/.test(source), "toolbar Composer must use the scroll-safe generation fence");
assert(tapActivationSource.includes("const armedPointers = new Map<number,")
  && tapActivationSource.includes("armedPointers.get(event.pointerId)")
  && tapActivationSource.includes("capture.generation !== interactionGeneration()"), "tap authority must remain keyed by pointer identity and checked against the owner generation");

// ⇄ is absent from the row; Copy occupies its primary-row slot. The tag
// popover is a neutral shell with identity facts and a second view over the
// one pane-local inventory/select authority.
assert(!source.includes('className = "persea-unified-session-switch"'), "top-bar session switch must be absent");
assert(source.includes("controls.append(selectContext, paste, geometryReadout, quickActions, composerToggle)"), "the compact primary row must contain contextual Select/Copy, Paste, committed size, Quick actions and Composer in order");
assert(source.includes('document.createElement("div");\n    identityDetails.className = "persea-unified-identity__details"'), "tag popover must be a neutral shell");
assert(source.includes("currentDetails.append(currentSummary, identityFacts)")
  && source.includes("identityDetails.append(currentDetails, identitySessionStatus, identitySessionList)"), "disclosed facts and switcher must have separate hosts");
assert(source.includes("private sessionInventoryRequest?: Promise<void>"), "inventory loading must be one shared single flight");
assert(source.includes("this.identitySessionSwitcher?.setInventory(inventory)"), "tag and sheet must share the same inventory projection");
assert(/bindTapActivation\(\s*tag,/.test(source), "the tag opener must preserve terminal focus");
assert(source.includes("bindTapActivation(dashboard,") && !source.includes("bindTapActivation(previous,") && !source.includes("bindTapActivation(next,"),
  "tag navigation must offer Dashboard without arbitrary Prev/Next cycling");
assert(/bindTapActivation\(\s*switcher\.refresh,/.test(source), "tag Refresh must preserve terminal focus");

// the capability-free root navigation exists only in a standalone pane.
assert(/tile\("[^"]+", "Dashboard", "Open the dashboard"\)/.test(source), "the task menu must provide Dashboard");
assert(source.includes('window.location.assign("/")'), "Dashboard must navigate to the clean root URL");
assert(source.includes("if (!this.options.workspaceCell)"), "workspace cells must omit Dashboard rather than disable it");

// the committed geometry button is the one View & appearance disclosure
// and the moved controllers have exactly one construction site.
assert(source.includes('geometryReadout.className = "persea-unified-geometry persea-unified-view-disclosure"'), "committed geometry must be a disclosure button");
assert(source.includes('geometryReadout.setAttribute("aria-controls", viewPopover.id)') && source.includes('geometryReadout.setAttribute("aria-expanded", "false")'), "geometry disclosure must own the popover state");
assert((source.match(/this\.createGeometryForm\("view"\)/g) ?? []).length === 1, "Terminal size must render once in View & appearance");
// theme and composer text are dashboard settings; the terminal's
// View popover no longer renders either.
assert(!source.includes('this.preferencePicker("Theme")') && !source.includes("this.composerFontPicker()") && !source.includes("persea-unified-view-popover__appearance"), "Theme and Composer text must not render in the terminal's View popover");
assert(!source.includes('className = "persea-unified-phone-view"') && !source.includes('className = "persea-unified-fit-height"') && !source.includes('className = "persea-unified-toolbar__more"'), "standalone Aa, Fit rows and overflow controls must be absent");
assert(!source.includes('section("View"') && !source.includes('section("Terminal size"') && !source.includes('section("Appearance"'), "Quick actions must not duplicate view controls");
assert(!source.includes('section("Selection & clipboard"'), "Quick actions must not duplicate the primary Select/Copy and Paste controls");
assert(!source.includes('className = "persea-unified-select__strip"') && !source.includes('className = "persea-unified-select__copy"') && !source.includes('className = "persea-unified-select__copy-screen"'),
  "the frozen overlay must not duplicate the contextual Select/Copy owner or render a Done strip");
// Select is a pure toggle (Select ⇄ Selecting); the Paste slot is
// the one control that morphs Paste → Copy → Copied ✓ / Copy ✗ → Paste.
assert(source.includes('button.dataset.selectState = "selecting"') && source.includes('button.dataset.selectState = "select"')
  && !source.includes('button.dataset.selectState = "copy"') && !source.includes('button.dataset.selectState = "copied"'),
  "Select must expose only its Select and Selecting states");
assert(source.includes('button.dataset.pasteState = "copy"') && source.includes('button.dataset.pasteState = "copied"')
  && source.includes('button.dataset.pasteState = "failed"') && source.includes('button.dataset.pasteState = "paste"'),
  "the Paste slot must expose Paste, Copy, Copied and failed states");
assert(source.includes("private activateContextualSelection(): void") && source.includes("private copyContextualSelection(): void")
  && source.includes("private activatePasteSlot(event: Event): void") && source.includes("if (this.selectMode) this.exitSelectMode();"),
  "one trusted Select toggle must own enter and cancel; the Paste slot must own exact-range copy");
// Clipboard list, editor and input behavior are exercised in the browser gate.
assert(source.includes('"Clipboard", "Open shared clipboard"') && source.includes("this.openClipboard();"),
  "The terminal menu must open the shared Clipboard owner");
assert(!source.includes('tile("‹", "Prev"') && !source.includes('tile("›", "Next"'), "quick actions must not expose arbitrary Prev/Next cycling");

// both controls converge on one rows-only request constructor.
assert((source.match(/type: "RESIZE_REQUEST"/g) ?? []).length === 1, "there must be exactly one direct resize-frame construction site");
assert(source.includes("private requestRowsOnly(rows: number, explicit = false)"), "geometry emitter must be rows-only (explicit typed requests skip only the keyboard guard)");
assert((source.match(/this\.requestRowsOnly\(/g) ?? []).length === 2, "only trusted Fit rows and Apply activations may call the resize helper");
assert(source.includes("private requestVerticalFit(): void") && source.includes("private applyTypedGeometry(columnsText: string, rowsText: string): void"),
  "Fit rows and the single Apply must be the two rows-only callers");
assert(source.includes("columns: this.committedColumns, rows,"), "the resize frame must carry the witnessed column count, never an editable width");
// one geometry block — Fit rows and Fit width above one form row of
// Columns, Rows and a single Apply. A column change goes through the
// generation refit and carries the typed rows; it never becomes a resize frame.
assert(source.includes('const input = document.createElement("input")')
  && source.includes('actions.className = "persea-unified-size__actions"') && source.includes('form.className = "persea-unified-size__form"')
  && source.includes('fitWidth.textContent = "↔ Fit width"')
  && source.includes('fitWidth.setAttribute("aria-label", "Fit width")')
  && source.includes('apply.textContent = "Apply"')
  && !source.includes('refit.textContent = "Refit"'), "the geometry block must expose Fit width and a single Apply");
// the typed Apply passes `explicit` so only the keyboard guard is
// skipped; the measured Fit width keeps every guard.
assert(source.includes("private requestWidthRefit(columns: number, rows?: number, explicit = false): void")
  && source.includes("this.requestWidthRefit(columns, rows === this.committedRows ? undefined : rows, true)")
  && (source.match(/this\.requestWidthRefit\(/g) ?? []).length === 2, "only Fit width and Apply may request a width refit");
assert(source.includes('const request = this.options.refitWidth') && source.includes('const attempt = request(columns, rows)') && source.includes('void attempt.result.then') && !source.includes('type: "RESIZE_REQUEST", version: 1, source: active.source, epoch: active.epoch,\n      columns,'), "width must use the out-of-band refit owner, never a column resize frame");
assert(controller.includes("...(rows === undefined ? {} : { rows })"), "the refit request must carry typed rows only when the operator changed them");
assert(controller.includes('window.fetch("/api/session-refits"') && controller.includes("operationToken = ++this.operationToken"), "width refit is not bound to the exact controller operation");
assert(controller.includes("private currentSource: string | null = null") && controller.includes("this.currentSource = source")
  && controller.includes("const source = this.currentSource") && controller.includes("identity !== this.currentIdentity.incarnationKey || source !== this.currentSource"),
  "width refit must present the current PREPARE source while settling against stable incarnation plus operation token");
// the out-of-band result names the one broker
// successor that may settle the operation. Every terminal path converges on
// one idempotent dropped-input owner instead of assigning pendingRefit away.
const refitBoundaryFailures = [
	!source.includes("export type WidthRefitResult") || !source.includes("successorSource: string") ? "result lacks broker successor identity" : "",
  !controller.includes("fields.successor_source") ? "HTTP result does not validate successor identity" : "",
  !source.includes("successorSource: string") ? "pending operation lacks exact successor" : "",
	!source.includes("frame.source !== pendingRefit.successorSource") ? "PREPARE does not require the broker successor" : "",
  !source.includes("private settlePendingRefit(") ? "dropped-input settlement is not centralized" : "",
].filter((value) => value !== "");
assert(refitBoundaryFailures.length === 0, `refit boundary gaps: ${refitBoundaryFailures.join(", ")}`);
assert(source.includes('/^(?:0|[1-9][0-9]*)$/'), "size inputs must use strict ASCII-decimal validation");

// primary action and explanation use one composed recognizer, and the
// browser lane is a package-owned member of the canonical suite.
assert(source.includes("bindExplainedTapActivation"), "explainer controls must use the composed gesture state machine");
assert(source.includes('this.openExplainer("select"') && source.includes('this.openExplainer("view"'), "contextual Select/Copy and moved disclosure explanations must be wired");
assert(packageJSON.scripts?.["test:terminal-interaction-browser"]?.includes("run_terminal_interaction_browser.cjs") === true, "terminal interaction browser lane must have a package script");
assert(packageJSON.scripts?.["test:view-disclosure-browser"]?.includes("run_view_disclosure_browser.cjs") === true, "view disclosure browser lane must have a package script");
assert(packageJSON.scripts?.["test:refit-accounting-browser"]?.includes("run_refit_accounting_browser.cjs") === true,
  "refit refit-accounting browser lane must have a package script");
// ci_reachability.cjs enforces these suites' transitive ownership by CI.
assert(!terminal_interactionBrowser.includes('if (false && shape.name === "desktop" && refitLifecycleEnabled("c3"))'),
  "refit lifecycle coverage must not remain behind a compile-time-disabled terminal interaction block");

// The frozen overlay publishes the exact xterm presentation it
// captured, and passive readiness rendering cannot retire a copy outcome.
assert(source.includes('"--persea-terminal-font-family", snapshot.fontFamily'), "frozen selection must publish xterm font family");
assert(source.includes('"--persea-terminal-font-size", `${snapshot.fontSizePixels}px`'), "frozen selection must publish xterm font size");
assert(source.includes('"--persea-terminal-row-height", `${snapshot.rowHeightPixels}px`'), "frozen selection must publish measured row height");
assert(source.includes("this.selectSnapshot.viewportY * this.selectSnapshot.rowHeightPixels"), "initial frozen scroll must use the captured row metric");
assert(!source.includes('fragment.append(document.createTextNode("\\n"))'), "block-row overlay must not render separator newline nodes");
assert(!source.includes("this.snippetCopyButton") && source.includes("private copyContextualSelection(): void"), "the contextual Copy owner must replace the retired Saved-text selection copy control");
assert(source.includes("value !== this.selectSelectionValue") && source.includes("this.renderContextualSelectionState()"), "new frozen selections must synchronously update the contextual owner");

// view disclosure correction: Escape restores the disclosure only for focus that came
// from inside View, while every terminal lifecycle transition tears down the
// complete pane-local overlay owner set through one idempotent primitive.
assert(source.includes("if (restoreDisclosureFocus) geometryReadout.focus({ preventScroll: true })"), "View Escape must not steal an xterm/helper-textarea focus owner");
const closeOverlays = source.match(/private closePresentationOverlays\([^)]*\): void \{([\s\S]*?)\n  \}/)?.[1] ?? "";
for (const required of ["setViewPopover(false, true)", "setIdentityDetails(false, true)", "setSheet(false, true)", "closeExplainer(true)", "this.composer?.closeTypographyPopover(true)", 'this.popoverOwner = "none"']) {
  assert(closeOverlays.includes(required), `lifecycle overlay teardown is missing ${required}`);
}
// Offline and exhaustion render through the failure notice, which owns their
// teardown, so they add no call site of their own.
assert((source.match(/this\.closePresentationOverlays\([^)]*\);/g) ?? []).length === 6, "destroy, transport close, reconnect attempts, failure (including offline and exhaustion), replacement and explicit refit must share overlay teardown");
assert(/this\.pendingRefit = \{[\s\S]*?\};\n\s*this\.closePresentationOverlays\(\);/.test(source), "explicit refit must publish its exact owner and tear down presentation overlays before awaiting the width transaction");

// every programmatic text source converges on the one xterm paste
// sink, and only that sink performs the post-paste router reconciliation.
const deliverText = source.match(/private deliverText\([\s\S]*?\n  \}/)?.[0] ?? "";
assert(deliverText.includes("this.terminal.paste(text);\n    this.iosBackspace.onXtermOperationComplete();"),
  "programmatic paste must reconcile the iOS router synchronously at the sink");
assert((source.match(/this\.iosBackspace\.onXtermOperationComplete\(\);/g) ?? []).length === 3
  && source.includes("const onRouterPaste = () => this.iosBackspace.onXtermOperationComplete();")
  && source.includes("Selection/copy changes document selection"),
  "native paste, the one text-delivery sink, and coarse Select focus restoration must be the only router reconciliations");
assert(source.includes("return this.deliverText(text, true)") && source.includes("return this.deliverText(normalized, !coarse)"),
  "Composer and explicit Clipboard Send must share the corrected delivery sink");

process.stdout.write("terminal interaction structural contracts: PASS\n");
