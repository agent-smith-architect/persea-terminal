// Browser harness for the unified terminal's scroll projection geometry.
// It mounts the real UnifiedTerminalPage against a stub port and asks the
// questions the shipped page answered wrongly: is everything the grid renders
// reachable by scrolling, and does the tail still show the live screen? No
// transcript is fabricated beyond the replay bytes the broker would have sent.
import { ImageStagingError, type ComposerStagedImage, type ImageStagingFailureKind } from "../src/composer_attachments";
import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const SOURCE = "harness-source";
const EPOCH = 1n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const ROWS = 24;
const encoder = new TextEncoder();

let page: UnifiedTerminalPage | undefined;
let generation = 0;
let cut = 9n;
let tailLine = "";
// Every frame the page tries to send. The vertical-fit contract is mostly a
// contract about what is NOT sent, so the record has to be complete.
const sentFrames: Array<Record<string, unknown>> = [];
// What the transport reports back for the next send. A refused send must clear
// the pending state without replaying anything the seal dropped.
let portResult: "ACCEPTED" | "SATURATED" | "CLOSED" | "TRANSPORT_LOST" = "ACCEPTED";
// Decoded INPUT payloads, so the image-insert scenario can assert the exact
// text a composer Insert delivered.
const inputTexts: string[] = [];
// Image staging mock: the page and composer run their real plumbing; only
// the network call is replaced with a deferred the scenarios settle.
type PendingUpload = { file: File; resolve(staged: ComposerStagedImage): void; reject(error: unknown): void };
const pendingUploads: PendingUpload[] = [];
let stagedImageSerial = 0;
function stageImageMock(file: File, signal: AbortSignal): Promise<ComposerStagedImage> {
  return new Promise<ComposerStagedImage>((resolve, reject) => {
    pendingUploads.push({ file, resolve, reject });
    signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
  });
}
// Failure-surface records: every detach the page asked its port for, every
// takeover claim it resolved, and every endpoint it handed the port.
const detachCalls: string[] = [];
const takeoverClaims: Array<string | null> = [];
const portTakeovers: Array<Record<string, unknown>> = [];
// Injected visual viewport. It mirrors the window until a scenario shrinks
// it, so everything that predates it sees the platform's own state; the
// keyboard scenarios shrink it and fire the listeners, the way a real
// visualViewport announces the software keyboard.
const fakeViewportState: { height: number | null; scale: number; offsetLeft: number; offsetTop: number } = { height: null, scale: 1, offsetLeft: 0, offsetTop: 0 };
const fakeViewportListeners = new Set<() => void>();
const fakeViewport = {
  get height(): number { return fakeViewportState.height ?? window.innerHeight; },
  get scale(): number { return fakeViewportState.scale; },
  get offsetLeft(): number { return fakeViewportState.offsetLeft; },
  get offsetTop(): number { return fakeViewportState.offsetTop; },
  addEventListener(_type: "resize" | "scroll", listener: () => void): void { fakeViewportListeners.add(listener); },
  removeEventListener(_type: "resize" | "scroll", listener: () => void): void { fakeViewportListeners.delete(listener); },
};
function fireFakeViewport(): void {
  for (const listener of [...fakeViewportListeners]) listener();
}
// Spy on window.scrollTo: the page's residual-pan restore is otherwise
// unobservable here, because the harness document cannot actually scroll.
const scrollToCalls: Array<{ x: number; y: number }> = [];
const realScrollTo = window.scrollTo.bind(window);
(window as any).scrollTo = (...args: unknown[]) => {
  const [first, second] = args;
  if (typeof first === "object" && first !== null) {
    const options = first as ScrollToOptions;
    scrollToCalls.push({ x: options.left ?? 0, y: options.top ?? 0 });
  } else {
    scrollToCalls.push({ x: Number(first ?? 0), y: Number(second ?? 0) });
  }
  return realScrollTo(...(args as [number, number]));
};

function frame(extra: Record<string, unknown>): Record<string, unknown> {
  return { version: 1, source: SOURCE, epoch: EPOCH, ...extra };
}

function settle(): Promise<void> {
  return new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 40))));
}

function nodes(): { outer: HTMLElement; host: HTMLElement; screen: HTMLElement } {
  const outer = document.querySelector<HTMLElement>(".persea-unified-scroll");
  const host = document.querySelector<HTMLElement>(".persea-unified-xterm");
  const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen");
  if (!outer || !host || !screen) throw new Error("unified terminal geometry is not mounted");
  return { outer, host, screen };
}

function toolbarButton(label: string): HTMLButtonElement {
  const button = Array.from(document.querySelectorAll<HTMLButtonElement>(".persea-unified-toolbar button"))
    .find((node) => node.textContent === label);
  if (!button) throw new Error(`toolbar button ${label} is absent`);
  return button;
}

function fitHeightButton(): HTMLButtonElement {
  const button = Array.from(document.querySelectorAll<HTMLButtonElement>(".persea-unified-view-popover .persea-unified-size button"))
    .find((node) => node.textContent?.includes("Fit rows"));
  if (!button) throw new Error("fit-terminal-height control is absent");
  return button;
}

function viewDisclosureButton(): HTMLButtonElement {
  const button = document.querySelector<HTMLButtonElement>(".persea-unified-view-disclosure");
  if (!button) throw new Error("committed-size disclosure is absent");
  return button;
}

function ensureViewPopoverOpen(): void {
  if (viewDisclosureButton().getAttribute("aria-expanded") !== "true") (page as any).setViewPopover(true);
}

function resizeRequests(): Array<Record<string, unknown>> {
  return sentFrames.filter((frame) => frame.type === "RESIZE_REQUEST");
}

// Ground truth from the emulator itself, not from the geometry under test.
function buffer(): any {
  return (page as any).terminal.buffer.active;
}

function renderedRows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows > div"));
}

function rowText(index: number): string {
  const rows = renderedRows();
  const row = index < 0 ? rows[rows.length + index] : rows[index];
  return (row?.textContent ?? "").trim();
}

function state(): Record<string, unknown> {
  const { outer, host, screen } = nodes();
  const outerRect = outer.getBoundingClientRect();
  const screenRect = screen.getBoundingClientRect();
  const rows = renderedRows();
  const active = buffer();
  return {
    baseY: active.baseY,
    viewportY: active.viewportY,
    cursorY: active.cursorY,
    bufferType: active.type,
    renderedRowCount: rows.length,
    projectedRow: Number.parseFloat(host.style.top) / (screenRect.height / ROWS),
    hostTop: Number.parseFloat(host.style.top),
    cellHeight: screenRect.height / ROWS,
    gridWidth: screenRect.width,
    gridHeight: screenRect.height,
    clientWidth: outer.clientWidth,
    clientHeight: outer.clientHeight,
    scrollWidth: outer.scrollWidth,
    scrollHeight: outer.scrollHeight,
    scrollLeft: outer.scrollLeft,
    scrollTop: outer.scrollTop,
    horizontalRange: outer.scrollWidth - outer.clientWidth,
    verticalRange: outer.scrollHeight - outer.clientHeight,
    rightOvershoot: screenRect.right - outerRect.right,
    bottomOvershoot: screenRect.bottom - outerRect.bottom,
    overflowX: getComputedStyle(outer).overflowX,
    topRowText: rowText(0),
    lastRowText: rowText(-1),
    tailLine,
    // The topmost buffer row the range can still project. Every row the buffer
    // holds must be reachable, so this has to cover baseY.
    maxProjectableRow: Math.floor((outer.scrollHeight - outer.clientHeight) / (screenRect.height / ROWS)),
    // Drift diagnostics: what a tail scrollTop would have to be for the
    // top-accumulated row math to resolve to baseY, and how far short it lands.
    tailTopForBaseY: active.baseY * (screenRect.height / ROWS),
    tailShortfall: active.baseY * (screenRect.height / ROWS) - outer.scrollTop,
    devicePixelRatio: window.devicePixelRatio,
  };
}

async function boot(lines: number, columns = 80, rows = ROWS): Promise<void> {
  const root = document.getElementById("root");
  if (!root) throw new Error("harness root is absent");
  if (!page) {
    page = new UnifiedTerminalPage({
      root,
      port: {
        trySend: (_generation: number, frame: any) => {
          if (frame.type === "INPUT" && frame.data instanceof Uint8Array) inputTexts.push(new TextDecoder().decode(frame.data));
          sentFrames.push(JSON.parse(JSON.stringify(frame, (_key, value) => (typeof value === "bigint" ? value.toString() : value))));
          return portResult;
        },
        finalize: () => undefined,
        detach: (reason?: string) => { detachCalls.push(reason ?? ""); },
        takeControl: (endpoint: Readonly<{ url: string; protocols: readonly string[] }>) => {
          portTakeovers.push({ url: endpoint.url, protocols: [...endpoint.protocols] });
        },
      },
      capabilityMode: "control",
      styleNonce: NONCE,
      viewportInset: fakeViewport,
      stageImage: stageImageMock,
      rememberSource: () => undefined,
      takeControl: async (source: string | undefined) => {
        takeoverClaims.push(source ?? null);
        return { url: "wss://harness.invalid/ws", protocols: ["persea-terminal.v2"] };
      },
    });
    (window as any).__page = page;
  }
  const transcript: string[] = [];
  for (let line = 1; line <= lines; line++) transcript.push(`HARNESS-ROW-${String(line).padStart(4, "0")}`);
  tailLine = `HARNESS-TAIL-${String(lines).padStart(4, "0")}`;
  transcript.push(tailLine);
  generation += 1;
  cut += 1n;
  page.openTransport(generation);
  page.receiveDecoded(generation, frame({
    type: "PREPARE", cut, kind: "INITIAL", columns, rows,
    history: [], truncated: false, replay: encoder.encode(transcript.join("\r\n")),
  }));
  page.receiveDecoded(generation, frame({ type: "COMMIT", cut }));
  // The broker's control grant, delivered as the real broker does in response
  // to the page's MODE_REQUEST at COMMIT. The page seals input until it lands.
  page.receiveDecoded(generation, frame({ type: "MODE", mode: "CONTROL" }));
  await settle();
  await settle();
}

async function live(text: string): Promise<void> {
  if (!page) throw new Error("harness is not booted");
  page.receiveDecoded(generation, frame({ type: "LIVE", cut, data: encoder.encode(text) }));
  await settle();
  await settle();
}

function inputFrames(): Array<Record<string, unknown>> {
  return sentFrames.filter((frame) => frame.type === "INPUT");
}

function decodeInput(frame: Record<string, unknown>): string {
  const data = frame.data as Record<string, number> | number[] | undefined;
  const values = Array.isArray(data) ? data : Object.values(data ?? {});
  return values.map((value) => String.fromCharCode(value)).join("");
}

// The whole input record, in send order, plus the count of bytes the seal
// dropped. A queue would show up as bytes reappearing after the seal opens;
// a drop shows up as bytes that are counted and never sent.
function inputState(): Record<string, unknown> {
  return {
    sentText: inputFrames().map(decodeInput).join(""),
    inputFrameCount: inputFrames().length,
    frameOrder: sentFrames.map((frame) => frame.type),
    sealedInputBytes: (page as any)?.sealedInputBytes ?? -1,
    fitPending: (page as any)?.fitPending ?? null,
    resizeRequests: resizeRequests(),
  };
}

function fitState(): Record<string, unknown> {
  const button = fitHeightButton();
  const rect = button.getBoundingClientRect();
  const readout = document.querySelector<HTMLElement>(".persea-unified-geometry");
  const terminal = (page as any)?.terminal;
  const active = terminal?.buffer?.active;
  return {
    readout: readout?.textContent ?? "",
    ariaLive: readout?.getAttribute("aria-live") ?? "",
    accessibleName: button.getAttribute("aria-label") ?? "",
    title: button.title,
    symbol: button.textContent ?? "",
    disabled: button.disabled,
    ariaDisabled: button.getAttribute("aria-disabled") ?? "",
    x: rect.x + rect.width / 2,
    y: rect.y + rect.height / 2,
    width: rect.width,
    height: rect.height,
    columns: terminal?.cols ?? 0,
    rows: terminal?.rows ?? 0,
    bufferType: active?.type ?? "",
    cursorX: active?.cursorX ?? -1,
    cursorY: active?.cursorY ?? -1,
    selection: terminal?.getSelection?.() ?? "",
    resizeRequests: resizeRequests(),
  };
}

// The visible failure/reconnect surface: what the notice panel and connection
// strip actually show, plus the port traffic the page generated getting there.
function failureState(): Record<string, unknown> {
  const notice = document.querySelector<HTMLElement>(".persea-unified-notice");
  const headline = document.querySelector<HTMLElement>(".persea-unified-notice__headline");
  const detail = document.querySelector<HTMLElement>(".persea-unified-notice__detail");
  const codeLine = document.querySelector<HTMLElement>(".persea-unified-notice__code");
  const dashboard = document.querySelector<HTMLAnchorElement>(".persea-unified-notice__dashboard");
  const takeControl = document.querySelector<HTMLButtonElement>(".persea-unified-notice__take-control");
  const connection = document.querySelector<HTMLElement>(".persea-unified-connection");
  const takeRect = takeControl?.getBoundingClientRect();
  return {
    noticeHidden: notice?.hidden ?? true,
    noticeText: (notice?.textContent ?? "").trim(),
    headline: headline?.textContent ?? "",
    detail: detail?.textContent ?? "",
    codeLine: codeLine?.textContent ?? "",
    dashboardHref: dashboard?.getAttribute("href") ?? "",
    takeControlHidden: takeControl?.hidden ?? true,
    takeControlDisabled: takeControl?.disabled ?? true,
    takeControlText: takeControl?.textContent ?? "",
    takeControlPoint: takeRect ? { x: takeRect.x + takeRect.width / 2, y: takeRect.y + takeRect.height / 2 } : null,
    connectionText: connection?.textContent ?? "",
    connectionVisible: connection ? getComputedStyle(connection).display !== "none" : false,
    detachCalls: detachCalls.slice(),
    takeoverClaims: takeoverClaims.slice(),
    portTakeovers: portTakeovers.length,
  };
}

// The keyboard-inset surface: the shell's pinned height, the key bar's dock
// position, the fitted font, and the whole scroll state — everything the
// inset must move and everything it must leave alone.
function insetState(): Record<string, unknown> {
  const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
  const keybar = document.querySelector<HTMLElement>(".persea-unified-keybar");
  if (!shell || !keybar) throw new Error("unified shell or key bar is absent");
  const shellRect = shell.getBoundingClientRect();
  const keybarRect = keybar.getBoundingClientRect();
  const terminal = (page as any)?.terminal;
  return {
    ...state(),
    styleHeight: shell.style.height,
    shellHeight: shellRect.height,
    keybarHidden: keybar.hidden,
    keybarTop: keybarRect.top,
    keybarBottom: keybarRect.bottom,
    fontSize: terminal?.options?.fontSize ?? -1,
    visualHeight: fakeViewport.height,
    windowInnerHeight: window.innerHeight,
    maximumTop: (() => { const { outer } = nodes(); return Math.max(0, outer.scrollHeight - outer.clientHeight); })(),
    scrollToCalls: scrollToCalls.slice(),
  };
}

// The primary key row's rendered geometry, plus every horizontal-overflow
// figure a stretched bar would show up in.
function keyBarState(): Record<string, unknown> {
  const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
  const keybar = document.querySelector<HTMLElement>(".persea-unified-keybar");
  const row = document.querySelector<HTMLElement>(".persea-unified-keybar-row");
  if (!shell || !keybar || !row) throw new Error("unified key bar is absent");
  const keys = Array.from(keybar.querySelectorAll<HTMLButtonElement>("button")).map((button) => {
    const rect = button.getBoundingClientRect();
    return { label: button.textContent ?? "", left: rect.left, right: rect.right, width: rect.width, height: rect.height };
  });
  return {
    hidden: keybar.hidden,
    viewportWidth: window.innerWidth,
    keyCount: keys.length,
    keys,
    ctrl: keys.find((key) => key.label === "Ctrl") ?? null,
    toggle: keys.find((key) => key.label === "⋯") ?? null,
    rowScrollWidth: row.scrollWidth,
    rowClientWidth: row.clientWidth,
    rowScrollLeft: row.scrollLeft,
    shellScrollWidth: shell.scrollWidth,
    shellClientWidth: shell.clientWidth,
    pageScrollWidth: document.documentElement.scrollWidth,
    // The iOS auto-zoom trigger under test: a focused input below 16px zooms
    // the page. xterm's helper textarea otherwise carries the UA's default
    // ~13px monospace, so the pinned cure is asserted here.
    helperFontSize: (() => {
      const textarea = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-helper-textarea");
      return textarea ? getComputedStyle(textarea).fontSize : "";
    })(),
  };
}

// The composer dock: the panel's in-flow geometry between the viewport and
// the key bar, the toggles' reflected state, and the font — everything the
// dock must move and everything (the fitted font, the tail) it must not.
function composerState(): Record<string, unknown> {
  const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
  const dock = document.querySelector<HTMLElement>(".persea-unified-composer-dock");
  const panel = document.querySelector<HTMLElement>(".persea-unified-composer-dock .attachment-page__composer");
  const textarea = document.querySelector<HTMLTextAreaElement>(".persea-unified-composer-dock .attachment-page__composer-textarea");
  const keybar = document.querySelector<HTMLElement>(".persea-unified-keybar");
  const toolbarToggle = document.querySelector<HTMLButtonElement>(".persea-unified-toolbar .persea-unified-composer-toggle");
  if (!shell || !dock || !panel || !textarea || !keybar || !toolbarToggle) {
    throw new Error("unified composer dock is not mounted");
  }
  const { outer } = nodes();
  const outerRect = outer.getBoundingClientRect();
  const dockRect = dock.getBoundingClientRect();
  const panelRect = panel.getBoundingClientRect();
  const keybarRect = keybar.getBoundingClientRect();
  const terminal = (page as any)?.terminal;
  return {
    dockDataset: dock.dataset.composer ?? "",
    panelHidden: panel.hidden,
    panelPosition: getComputedStyle(panel).position,
    dockTop: dockRect.top,
    dockBottom: dockRect.bottom,
    dockHeight: dockRect.height,
    panelTop: panelRect.top,
    panelBottom: panelRect.bottom,
    panelHeight: panelRect.height,
    viewportBottom: outerRect.bottom,
    keybarHidden: keybar.hidden,
    keybarTop: keybarRect.top,
    keybarBottom: keybarRect.bottom,
    shellStyleHeight: shell.style.height,
    fontSize: terminal?.options?.fontSize ?? -1,
    textareaFontSize: getComputedStyle(textarea).fontSize,
    textareaFocused: document.activeElement === textarea,
    toolbarToggleExpanded: toolbarToggle.getAttribute("aria-expanded") ?? "",
    scrollTop: outer.scrollTop,
    maximumTop: Math.max(0, outer.scrollHeight - outer.clientHeight),
    pageScrollWidth: document.documentElement.scrollWidth,
  };
}

// A rendered box with the visibility judgement the phone-chrome assertions
// need: a display:none, visibility:hidden, or visually-hidden (1px clipped)
// element is not something a thumb can land on.
type RenderedBox = {
  text: string; left: number; right: number; top: number; bottom: number; width: number; height: number;
  visible: boolean; scrollWidth: number; clientWidth: number;
};
function renderedBox(element: Element | null | undefined): RenderedBox | null {
  if (!element) return null;
  const rect = element.getBoundingClientRect();
  const style = getComputedStyle(element);
  return {
    text: (element.textContent ?? "").trim(),
    left: rect.left,
    right: rect.right,
    top: rect.top,
    bottom: rect.bottom,
    width: rect.width,
    height: rect.height,
    visible: style.display !== "none" && style.visibility !== "hidden" && rect.width > 1 && rect.height > 1,
    scrollWidth: element.scrollWidth,
    clientWidth: element.clientWidth,
  };
}

// The phone chrome. Every control row's overflow figures plus each control's
// rendered box, so "every essential control on screen and tappable, no row
// scrolling, no page overflow" is a measurement and not a reading of the
// stylesheet. The grid's own width and the scroller's overflow are reported
// alongside, so a full-width TUI's horizontal range can be attributed: the
// grid's inherent width inside its scroller, or a page-level source.
function chromeState(): Record<string, unknown> {
  const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
  const toolbar = document.querySelector<HTMLElement>(".persea-unified-toolbar");
  const secondary = document.querySelector<HTMLElement>(".persea-unified-view-popover");
  const more = viewDisclosureButton();
  if (!shell || !toolbar || !secondary || !more) throw new Error("unified toolbar is absent");
  const { outer, host, screen } = nodes();
  const hostStyle = getComputedStyle(host);
  const hostPadding = Number.parseFloat(hostStyle.paddingLeft) + Number.parseFloat(hostStyle.paddingRight);
  const screenRect = screen.getBoundingClientRect();
  const terminal = (page as any)?.terminal;
  const columns: number = terminal?.cols ?? 0;
  const panel = document.querySelector<HTMLElement>(".persea-unified-composer-dock .attachment-page__composer");
  // The chrome row: the footer while it lays out as a row; when the narrow
  // layout dissolves it (display: contents) so the status can take its own
  // line, the row is the action group inside it.
  const footerElement = panel?.querySelector<HTMLElement>(".attachment-page__composer-footer") ?? null;
  const actions = panel?.querySelector<HTMLElement>(".attachment-page__composer-actions") ?? null;
  const footer = footerElement && getComputedStyle(footerElement).display === "contents" && actions ? actions : footerElement;
  const header = panel?.querySelector<HTMLElement>(".attachment-page__composer-header") ?? null;
  const footerItem = (selector: string) => renderedBox(panel?.querySelector(selector));
  const opener = document.querySelector<HTMLButtonElement>(".persea-unified-quick-actions");
  const sheet = document.querySelector<HTMLElement>(".persea-unified-sheet");
  return {
    puckPresent: !!document.querySelector(".persea-unified-puck"),
    opener: opener ? { ...renderedBox(opener), expanded: opener.getAttribute("aria-expanded") ?? "", controls: opener.getAttribute("aria-controls") ?? "" } : null,
    primaryControls: Array.from(toolbar.querySelectorAll<HTMLButtonElement>(".persea-unified-toolbar__controls > button")).map((button) => ({
      ...renderedBox(button), text: button.textContent ?? "", label: button.getAttribute("aria-label") ?? "", disabled: button.disabled,
    })),
    // `layer` is the position of the overlay the sheet lives in (the
    // quick-actions dock): the sheet is out of flow when that layer is.
    sheet: sheet ? { ...renderedBox(sheet), id: sheet.id, position: getComputedStyle(sheet).position, layer: getComputedStyle(sheet.closest(".persea-unified-quick") ?? sheet).position } : null,
    sheetTiles: sheet ? Array.from(sheet.querySelectorAll<HTMLButtonElement>(".persea-unified-sheet__tile")).map((button) => ({
      ...renderedBox(button), text: button.querySelector(".persea-unified-sheet__label")?.textContent ?? "", disabled: button.disabled,
    })) : [],
    viewportWidth: window.innerWidth,
    fontSize: terminal?.options?.fontSize ?? -1,
    columns,
    cellWidth: columns > 0 ? screenRect.width / columns : 0,
    gridWidth: screenRect.width,
    hostPadding,
    scrollerClientWidth: outer.clientWidth,
    scrollerScrollWidth: outer.scrollWidth,
    pageScrollWidth: document.documentElement.scrollWidth,
    pageClientWidth: document.documentElement.clientWidth,
    shellScrollWidth: shell.scrollWidth,
    shellClientWidth: shell.clientWidth,
    shellBottom: shell.getBoundingClientRect().bottom,
    keybarTop: (() => { const keybar = document.querySelector<HTMLElement>(".persea-unified-keybar"); return keybar && !keybar.hidden ? keybar.getBoundingClientRect().top : null; })(),
    toolbar: { ...renderedBox(toolbar), overflowX: getComputedStyle(toolbar).overflowX, scrollLeft: toolbar.scrollLeft },
    tag: renderedBox(toolbar.querySelector(".persea-unified-tag")),
    readout: renderedBox(toolbar.querySelector(".persea-unified-geometry")),
    phoneView: renderedBox(more),
    phoneReadout: renderedBox(more),
    fitHeight: renderedBox(fitHeightButton()),
    composerToggle: renderedBox(toolbar.querySelector(".persea-unified-composer-toggle")),
    more: { ...renderedBox(more), expanded: more.getAttribute("aria-expanded") ?? "", controls: more.getAttribute("aria-controls") ?? "" },
    secondary: { ...renderedBox(secondary), id: secondary.id, open: more.getAttribute("aria-expanded") ?? "", position: getComputedStyle(secondary).position },
    secondaryButtons: Array.from(secondary.querySelectorAll("button")).map((button) => renderedBox(button)),
    composer: panel && footer && header ? {
      hidden: panel.hidden,
      content: panel.dataset.content ?? "",
      footer: { ...renderedBox(footer), overflowX: getComputedStyle(footer).overflowX, scrollLeft: footer.scrollLeft },
      header: renderedBox(header),
      counter: footerItem(".attachment-page__composer-counter"),
      status: footerItem(".attachment-page__composer-status"),
      modeToggle: footerItem(".attachment-page__composer-mode-toggle"),
      fix: footerItem(".attachment-page__composer-fix"),
      restore: footerItem(".attachment-page__composer-restore"),
      attach: footerItem(".attachment-page__composer-attach"),
      clear: footerItem(".attachment-page__composer-clear"),
      send: footerItem(".attachment-page__composer-send"),
      close: footerItem(".attachment-page__composer-close"),
      headerButtons: Array.from(header.querySelectorAll("button")).map((button) => renderedBox(button)),
    } : null,
  };
}

// The composer's image-attachment surface: chips, gating, and the affordances
// whose presence is capability-gated.
function composerImageState(): Record<string, unknown> {
  const panel = document.querySelector<HTMLElement>(".persea-unified-composer-dock .attachment-page__composer");
  const textarea = document.querySelector<HTMLTextAreaElement>(".persea-unified-composer-dock .attachment-page__composer-textarea");
  const send = document.querySelector<HTMLButtonElement>(".persea-unified-composer-dock .attachment-page__composer-send");
  const status = document.querySelector<HTMLOutputElement>(".persea-unified-composer-dock .attachment-page__composer-status");
  const attach = document.querySelector<HTMLButtonElement>(".persea-unified-composer-dock .attachment-page__composer-attach");
  const input = document.querySelector<HTMLInputElement>(".persea-unified-composer-dock .attachment-page__composer input[type=file]");
  const strip = document.querySelector<HTMLUListElement>(".persea-unified-composer-dock .attachment-page__composer-chips");
  if (!panel || !textarea || !send || !status) throw new Error("unified composer is not mounted");
  const chips = Array.from(strip?.querySelectorAll<HTMLLIElement>(".attachment-page__composer-chip") ?? []);
  return {
    attachPresent: attach !== null,
    inputAccept: input?.getAttribute("accept") ?? null,
    stripHidden: strip?.hidden ?? null,
    panelAttachments: panel.dataset.attachments ?? "",
    chipCount: chips.length,
    chipStatuses: chips.map((chip) => chip.dataset.status ?? ""),
    chipNotes: chips.map((chip) => chip.querySelector(".attachment-page__composer-chip-note")?.textContent ?? ""),
    chipRemoveLabels: chips.map((chip) => Array.from(chip.querySelectorAll("button")).at(-1)?.getAttribute("aria-label") ?? ""),
    sendDisabled: send.disabled,
    statusText: status.value,
    statusHidden: status.hidden,
    textareaValue: textarea.value,
    pendingUploads: pendingUploads.length,
  };
}

function composerTextarea(): HTMLTextAreaElement {
  const textarea = document.querySelector<HTMLTextAreaElement>(".persea-unified-composer-dock .attachment-page__composer-textarea");
  if (!textarea) throw new Error("unified composer textarea is absent");
  return textarea;
}

function syntheticImageFile(type: string, name: string): File {
  return new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])], name, { type });
}

function dispatchPaste(text: string | undefined, files: readonly File[]): { defaultPrevented: boolean } {
  const data = new DataTransfer();
  if (text !== undefined) data.setData("text/plain", text);
  for (const file of files) data.items.add(file);
  const event = new ClipboardEvent("paste", { clipboardData: data, cancelable: true, bubbles: true });
  composerTextarea().dispatchEvent(event);
  return { defaultPrevented: event.defaultPrevented };
}

(window as any).__harness = {
  async boot(lines: number) { await boot(lines); },
  failureState() { return failureState(); },
  async closeTransport(reason: string) {
    if (!page) throw new Error("harness is not booted");
    page.transportClosed(generation, reason);
    await settle();
    return failureState();
  },
  async reconnectStatus(state: string, attempt: number, reason?: string) {
    if (!page) throw new Error("harness is not booted");
    (page as any).reconnectStatus({ state, attempt, ...(reason === undefined ? {} : { reason }) });
    await settle();
    return failureState();
  },
  clearFailureRecords() {
    detachCalls.length = 0;
    takeoverClaims.length = 0;
    portTakeovers.length = 0;
  },
  async bootWithGeometry(lines: number, columns: number, rows: number) { await boot(lines, columns, rows); },
  // The committed geometry event, delivered exactly as the broker delivers it:
  // a typed CutResize PREPARE on the same ordered stream as output.
  async applyCommittedGeometry(columns: number, rows: number) {
    if (!page) throw new Error("harness is not booted");
    page.receiveDecoded(generation, frame({
      type: "PREPARE", cut, kind: "RESIZE", columns, rows,
      history: [], truncated: false, replay: new Uint8Array(),
    }));
    await settle();
    await settle();
    return fitState();
  },
  fitState() { return fitState(); },
  fitClickState() { ensureViewPopoverOpen(); return fitState(); },
  setViewPopover(open: boolean) { (page as any)?.setViewPopover(open); return chromeState(); },
  // Type through xterm's own input path, exactly as a keyboard would: onData is
  // what the page listens to, so this cannot bypass the seal under test.
  async type(text: string) {
    const terminal = (page as any).terminal;
    for (const character of text) terminal.input(character, true);
    await settle();
    return inputState();
  },
  inputState() { return inputState(); },
  setPortResult(result: "ACCEPTED" | "SATURATED" | "CLOSED" | "TRANSPORT_LOST") { portResult = result; },
  async transportClosed() {
    if (!page) throw new Error("harness is not booted");
    page.transportClosed(generation, "harness");
    await settle();
    return inputState();
  },
  clearSentFrames() { sentFrames.length = 0; },
  sentFrames() { return sentFrames.slice(); },
  resizeRequests() { return resizeRequests(); },
  // A synthetic click is not a trusted click. The geometry action must refuse
  // it, because a script-driven resize is exactly the automatic authority this
  // design does not grant.
  async clickFitHeightUntrusted() {
    ensureViewPopoverOpen();
    fitHeightButton().click();
    await settle();
    return fitState();
  },
  async select(text: string) {
    const terminal = (page as any).terminal;
    terminal.selectAll();
    await settle();
    return { selection: terminal.getSelection().includes(text) };
  },
  async settle() { await settle(); return fitState(); },
  async live(text: string) { await live(text); },
  async zoom(label: string, times: number) {
    for (let press = 0; press < times; press++) {
      toolbarButton(label).click();
      await settle();
    }
  },
  async scrollTo(top: number, left = 0) {
    const { outer } = nodes();
    outer.scrollTop = top;
    outer.scrollLeft = left;
    await settle();
    return state();
  },
  async toFarCorner() {
    const { outer } = nodes();
    outer.scrollLeft = outer.scrollWidth;
    outer.scrollTop = outer.scrollHeight;
    await settle();
    return state();
  },
  async toTop() {
    const { outer } = nodes();
    outer.scrollTop = 0;
    outer.scrollLeft = 0;
    await settle();
    return state();
  },
  // Enter and leave the alternate screen the only way the page can see it:
  // through the live byte stream.
  async enterAlternate() {
    await live("\x1b[?1049h\x1b[HALTERNATE-SCREEN-ACTIVE");
    return state();
  },
  async leaveAlternate() {
    await live("\x1b[?1049l");
    return state();
  },
  // The shell is 100dvh in production, so the scroller's content box is
  // viewport height minus a toolbar whose height depends on font metrics. That
  // subtraction lands on a fraction on real machines, while clientHeight is
  // reported as a whole number. Driving the shell height directly reproduces
  // every sub-pixel offset the product can actually be laid out at.
  async setShellHeight(px: number) {
    const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
    if (!shell) throw new Error("unified terminal shell is absent");
    shell.style.height = `${px}px`;
    await settle();
    const { outer } = nodes();
    const rect = outer.getBoundingClientRect();
    return { shellHeight: px, contentHeight: rect.height, clientHeight: outer.clientHeight };
  },
  async setViewportHeight(px: number) {
    const { outer } = nodes();
    outer.style.height = `${px}px`;
    outer.style.flex = `0 0 ${px}px`;
    await settle();
    return { clientHeight: outer.clientHeight };
  },
  async resetViewportHeight() {
    const { outer } = nodes();
    outer.style.removeProperty("height");
    outer.style.removeProperty("flex");
    await settle();
    return { clientHeight: outer.clientHeight };
  },
  // Browser page zoom, the way the browser itself implements it: lengths are
  // scaled in layout, so the toolbar and the rendered rows stop landing on
  // whole pixels. This is the condition a device-pixel-ratio override cannot
  // produce, because DPR leaves layout in whole CSS pixels.
  async setZoom(factor: number) {
    const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
    if (!shell) throw new Error("unified terminal shell is absent");
    (shell.style as any).zoom = `${factor}`;
    await settle();
    await settle();
    return state();
  },
  state() { return state(); },
  // Drives the injected visual viewport the way the platform does: mutate,
  // then fire the resize/scroll listeners. null restores the mirror of the
  // window (keyboard closed).
  async setViewportInset(height: number | null, scale = 1) {
    fakeViewportState.height = height;
    fakeViewportState.scale = scale;
    fireFakeViewport();
    await settle();
    await settle();
    return insetState();
  },
  // A residual visual-viewport pan, the way Safari leaves one: offsets move,
  // a scroll event fires, height and scale stay honest.
  async setViewportOffsets(left: number, top: number) {
    fakeViewportState.offsetLeft = left;
    fakeViewportState.offsetTop = top;
    fireFakeViewport();
    await settle();
    return insetState();
  },
  clearScrollToCalls() { scrollToCalls.length = 0; },
  insetState() { return insetState(); },
  keyBarState() { return keyBarState(); },
  composerState() { return composerState(); },
  composerImageState() { return composerImageState(); },
  // Draft text through the textarea's own input path (onInput carries no
  // trust gate; only Insert does).
  async setComposerText(text: string) {
    const textarea = composerTextarea();
    textarea.value = text;
    textarea.dispatchEvent(new Event("input", { bubbles: true }));
    await settle();
    return composerImageState();
  },
  // Synthetic clipboard events, sanctioned by the design's own test items:
  // interception is not an authority action (uploads are server-authorized,
  // Insert still needs a trusted click).
  async pasteText(text: string) {
    const result = dispatchPaste(text, []);
    await settle();
    return { ...result, ...composerImageState() };
  },
  async pasteImage(type: string, name: string, text?: string) {
    const result = dispatchPaste(text, [syntheticImageFile(type, name)]);
    await settle();
    return { ...result, ...composerImageState() };
  },
  // The file-picker path: files land on the hidden input and change fires,
  // exactly the shape the picker produces.
  async attachFiles(types: readonly string[]) {
    const input = document.querySelector<HTMLInputElement>(".persea-unified-composer-dock .attachment-page__composer input[type=file]");
    if (!input) throw new Error("composer file input is absent");
    const data = new DataTransfer();
    types.forEach((type, index) => data.items.add(syntheticImageFile(type, `picked-${index}.png`)));
    input.files = data.files;
    input.dispatchEvent(new Event("change", { bubbles: true }));
    await settle();
    return composerImageState();
  },
  async resolveUpload() {
    const pending = pendingUploads.shift();
    if (!pending) throw new Error("no pending upload to resolve");
    const serial = (stagedImageSerial += 1);
    const id = String(serial).padStart(32, "0");
    pending.resolve(Object.freeze({
      id,
      path: `/var/lib/persea-terminal-staging/harness/img-${id}.png`,
      bytes: pending.file.size,
      mediaType: pending.file.type,
      expiresAt: "2026-08-20T18:00:00Z",
    }));
    await settle();
    return composerImageState();
  },
  async rejectUpload(kind: ImageStagingFailureKind) {
    const pending = pendingUploads.shift();
    if (!pending) throw new Error("no pending upload to reject");
    pending.reject(new ImageStagingError(kind));
    await settle();
    return composerImageState();
  },
  async clickChipButton(chipIndex: number, label: string) {
    const chips = Array.from(document.querySelectorAll<HTMLLIElement>(".persea-unified-composer-dock .attachment-page__composer-chip"));
    const chip = chips[chipIndex];
    if (!chip) throw new Error(`chip ${chipIndex} is absent`);
    const button = Array.from(chip.querySelectorAll<HTMLButtonElement>("button")).find((node) => node.textContent === label);
    if (!button) throw new Error(`chip ${chipIndex} has no ${label} button`);
    button.click();
    await settle();
    return composerImageState();
  },
  // Center of the composer's Insert button, for a trusted CDP tap.
  composerSendPoint() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-composer-dock .attachment-page__composer-send");
    if (!button) throw new Error("composer send button is absent");
    const rect = button.getBoundingClientRect();
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, disabled: button.disabled };
  },
  chromeState() { return chromeState(); },
  // Center of the quick-actions opener, for a trusted CDP tap.
  openerPoint() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-quick-actions");
    if (!button) throw new Error("quick-actions opener is absent");
    const rect = button.getBoundingClientRect();
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, visible: !button.hidden && getComputedStyle(button).display !== "none" };
  },
  // Center of the toolbar's ⋯ overflow toggle, for a trusted CDP tap.
  toolbarMorePoint() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-toolbar__more");
    if (!button) throw new Error("toolbar overflow toggle is absent");
    const rect = button.getBoundingClientRect();
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, visible: getComputedStyle(button).display !== "none" };
  },
  // A ratatui-shaped frame on the alternate screen: a box drawn across every
  // committed column, so the rendered grid is as wide as a full-width TUI
  // paints it. Leave with leaveAlternate().
  async paintFullWidthFrame() {
    const terminal = (page as any)?.terminal;
    if (!terminal) throw new Error("harness is not booted");
    const columns: number = terminal.cols;
    const rows: number = terminal.rows;
    const lines = [`┌${"─".repeat(columns - 2)}┐`];
    for (let row = 1; row < rows - 1; row++) lines.push(`│${" ".repeat(columns - 2)}│`);
    lines.push(`└${"─".repeat(columns - 2)}┘`);
    await live(`\x1b[?1049h\x1b[H${lines.join("\r\n")}`);
    return chromeState();
  },
  // Center of the toolbar composer toggle, shared by phone and desktop.
  toolbarComposerTogglePoint() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-toolbar .persea-unified-composer-toggle");
    if (!button) throw new Error("toolbar composer toggle is absent");
    const rect = button.getBoundingClientRect();
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, visible: rect.width > 0 && rect.height > 0 };
  },
  clearInputTexts() { inputTexts.length = 0; },
  inputTexts() { return inputTexts.slice(); },
  // Announces the CURRENT at-rest viewport, the way a real visualViewport
  // fires resize when the window itself resizes. The fake cannot observe
  // Emulation metrics changes, so scenarios call this after changing them to
  // let the page seed its resting high-water mark.
  async announceViewport() {
    fireFakeViewport();
    await settle();
    return insetState();
  },
  // Mutates the visual viewport WITHOUT firing any listener: the dismissal
  // paths iOS forgets to announce. Only blur, the watchdog, or a later event
  // can surface this change to the page.
  setViewportInsetSilently(height: number | null, scale = 1) {
    fakeViewportState.height = height;
    fakeViewportState.scale = scale;
    return insetState();
  },
  // Focus then blur xterm's textarea: the focusout a real iOS dismissal
  // delivers even when the visual viewport resize goes missing.
  async blurTerminal() {
    const textarea = document.querySelector<HTMLTextAreaElement>(".persea-unified-xterm .xterm-helper-textarea");
    if (!textarea) throw new Error("xterm helper textarea is absent");
    textarea.focus({ preventScroll: true });
    textarea.blur();
    await settle();
    return insetState();
  },
  // Center of the Ctrl latch key, for a trusted CDP tap.
  ctrlLatchPoint() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-keybar-key--latch");
    if (!button) throw new Error("ctrl latch key is absent");
    const rect = button.getBoundingClientRect();
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2, visible: !button.closest(".persea-unified-keybar")?.hasAttribute("hidden") };
  },
  ctrlState() {
    const button = document.querySelector<HTMLButtonElement>(".persea-unified-keybar-key--latch");
    if (!button) throw new Error("ctrl latch key is absent");
    return { latched: button.dataset.latched === "true", ariaPressed: button.getAttribute("aria-pressed") };
  },
  // Clears the inline height earlier sweeps drove through setShellHeight, so
  // the keyboard scenarios start from the stylesheet's own 100dvh.
  async resetShellHeight() {
    const shell = document.querySelector<HTMLElement>(".persea-unified-terminal");
    if (!shell) throw new Error("unified terminal shell is absent");
    shell.style.removeProperty("height");
    await settle();
    return insetState();
  },
};
