import { UnifiedTerminalPage } from "../src/unified_terminal_page";

// The typing pause, driven through the real page with a fake port. Every
// frame the page hands its port is recorded, so a check can prove that a
// keystroke, paste or composer Insert made while paused never left the page.
const SOURCE = "typing_pause-source";
const EPOCH = 43n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const encoder = new TextEncoder();
const decoder = new TextDecoder();

type Sent = { generation: number; type: string; data?: string };
const sent: Sent[] = [];
const finalized: string[] = [];
// What the fake port answers a send with, and how many of each generation's
// INPUT frames have been given a result.
let portResult: "ACCEPTED" | "SATURATED" = "ACCEPTED";
const answered = new Map<number, number>();
let page: UnifiedTerminalPage | undefined;
let generation = 0;
let cut = 20n;
let sessionName = "pause-one";

const frame = (extra: Record<string, unknown>): Record<string, unknown> => ({ version: 1, source: `${SOURCE}-${sessionName}`, epoch: EPOCH, ...extra });

const box = (node: Element | null): { x: number; y: number; width: number; height: number; top: number; bottom: number; left: number; right: number } | null => {
  if (!node) return null;
  const rect = node.getBoundingClientRect();
  return { x: rect.x, y: rect.y, width: rect.width, height: rect.height, top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right };
};
const shown = (node: HTMLElement | null): boolean => Boolean(node && !node.hidden && getComputedStyle(node).display !== "none" && node.getClientRects().length > 0);

function mounted(): UnifiedTerminalPage {
  if (!page) throw new Error("no page is mounted");
  return page;
}

function snapshot(): Record<string, unknown> {
  const root = document.getElementById("typing-host")!;
  const shell = root.querySelector<HTMLElement>(".persea-unified-terminal");
  const paused = root.querySelector<HTMLElement>(".persea-unified-paused");
  const message = root.querySelector<HTMLElement>(".persea-unified-paused__message");
  const resume = root.querySelector<HTMLButtonElement>(".persea-unified-paused__resume");
  const strip = root.querySelector<HTMLElement>(".persea-unified-refusal");
  const toast = root.querySelector<HTMLElement>(".persea-unified-toast");
  const composerStatus = root.querySelector<HTMLOutputElement>(".attachment-page__composer-status");
  const composerSend = root.querySelector<HTMLButtonElement>(".attachment-page__composer-send");
  const textarea = root.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
  const pausedStyle = paused ? getComputedStyle(paused) : undefined;
  const resumeStyle = resume ? getComputedStyle(resume) : undefined;
  const messageStyle = message ? getComputedStyle(message) : undefined;
  return {
    inputs: sent.filter((entry) => entry.type === "INPUT").map((entry) => entry.data ?? ""),
    sentTypes: sent.map((entry) => entry.type),
    paused: {
      hidden: paused?.hidden ?? null,
      shown: shown(paused),
      text: paused?.textContent ?? "",
      message: message?.textContent ?? "",
      messageRole: message?.getAttribute("role") ?? null,
      messageLive: message?.getAttribute("aria-live") ?? null,
      messageClipped: message ? message.scrollWidth > message.clientWidth + 1 || message.scrollHeight > message.clientHeight + 1 : null,
      buttonText: resume?.textContent ?? "",
      buttonType: resume?.type ?? null,
      box: box(paused),
      buttonBox: box(resume),
      colors: pausedStyle && resumeStyle && messageStyle ? {
        background: pausedStyle.backgroundColor,
        color: messageStyle.color,
        buttonBackground: resumeStyle.backgroundColor,
        buttonColor: resumeStyle.color,
        buttonBorder: resumeStyle.borderTopColor,
        pointerEvents: pausedStyle.pointerEvents,
      } : null,
    },
    strip: { hidden: strip?.hidden ?? null, shown: shown(strip), text: strip?.textContent ?? "", box: box(strip) },
    toast: { shown: shown(toast), text: toast?.textContent ?? "", box: box(toast) },
    shell: box(shell),
    connection: root.querySelector(".persea-unified-connection")?.textContent ?? "",
    failureShown: shown(root.querySelector<HTMLElement>(".persea-unified-notice")),
    composer: {
      status: composerStatus?.value ?? "", sendDisabled: composerSend?.disabled ?? null,
      draft: root.querySelector<HTMLTextAreaElement>(".attachment-page__composer-textarea")?.value ?? "",
      restoreShown: shown(root.querySelector<HTMLElement>(".attachment-page__composer-restore")),
    },
    finalized: finalized.slice(),
    activeIsTerminal: textarea !== null && document.activeElement === textarea,
    activeClass: (document.activeElement as HTMLElement | null)?.className ?? "",
    viewport: { width: window.innerWidth, height: window.innerHeight },
  };
}

function reset(name = "pause-one"): Record<string, unknown> {
  page?.destroy();
  sent.splice(0);
  finalized.splice(0);
  answered.clear();
  portResult = "ACCEPTED";
  sessionName = name;
  const host = document.getElementById("typing-host");
  if (!(host instanceof HTMLElement)) throw new Error("typing host missing");
  host.replaceChildren();
  generation = 1;
  cut = 20n;
  page = new UnifiedTerminalPage({
    root: host,
    port: {
      trySend: (frameGeneration, value) => {
        if (value.type === "INPUT" && portResult !== "ACCEPTED") return portResult;
        sent.push({ generation: frameGeneration, type: value.type, ...(value.type === "INPUT" ? { data: decoder.decode(value.data) } : {}) });
        return "ACCEPTED";
      },
      finalize: (intent) => { finalized.push(intent.cause); },
      detach: () => undefined,
      attachAgain: () => undefined,
    },
    capabilityMode: "control",
    styleNonce: NONCE,
    sessionName,
    composerStorageScope: `typing-pause-${sessionName}`,
    rememberSource: () => undefined,
    claimFocusOnCommit: () => false,
  });
  page.openTransport(generation);
  return snapshot();
}

// One admission of the current session: PREPARE, the replay's READY, COMMIT
// and the control grant, exactly as the broker sends them.
async function admit(): Promise<Record<string, unknown>> {
  const current = mounted();
  const readyBefore = sent.filter((entry) => entry.type === "READY").length;
  current.receiveDecoded(generation, frame({ type: "PREPARE", cut, kind: "INITIAL", columns: 80, rows: 24, history: [], truncated: false, replay: encoder.encode(`TYPING-PAUSE-REPLAY-${sessionName}\r\n$ `) }));
  const deadline = performance.now() + 5_000;
  while (sent.filter((entry) => entry.type === "READY").length === readyBefore) {
    if (performance.now() > deadline) throw new Error("replay write did not settle");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  current.receiveDecoded(generation, frame({ type: "COMMIT", cut }));
  current.receiveDecoded(generation, frame({ type: "MODE", mode: "CONTROL" }));
  return snapshot();
}

const INPUT_CODES = new Set(["input_paused", "input_refused", "input_dropped", "input_partial"]);
const inputsSent = (): number => sent.filter((entry) => entry.type === "INPUT" && entry.generation === generation).length;

// A broker refusal. Input codes arrive as the result of the next INPUT frame
// still waiting for one, as the server reports them; there must be one.
function refuse(code: string, generationOffset = 0): Record<string, unknown> {
  if (!INPUT_CODES.has(code)) {
    mounted().operationalRefusal(generation + generationOffset, code);
    return snapshot();
  }
  if (generationOffset !== 0) {
    mounted().inputResult(generation + generationOffset, 1, code as never);
    return snapshot();
  }
  const through = (answered.get(generation) ?? 0) + 1;
  if (through > inputsSent()) throw new Error(`no INPUT frame waits for a result to refuse with ${code}`);
  answered.set(generation, through);
  mounted().inputResult(generation, through, code as never);
  return snapshot();
}

// Results for INPUT frames of the current generation, as the server sends
// them: every frame still waiting except the last `keep`, with one code
// ("" = written).
function answer(code = "", keep = 0): Record<string, unknown> {
  const from = answered.get(generation) ?? 0;
  const through = inputsSent() - keep;
  if (through <= from || through > inputsSent()) throw new Error(`cannot answer through ${through} (answered ${from}, sent ${inputsSent()})`);
  answered.set(generation, through);
  mounted().inputResult(generation, through, code as never);
  return snapshot();
}

// A result the server should never send: through `through`, as given.
function rawResult(through: number, code = ""): Record<string, unknown> {
  mounted().inputResult(generation, through, code as never);
  return snapshot();
}

function setPortResult(value: "ACCEPTED" | "SATURATED"): void {
  portResult = value;
}

// Terminal output on the live attachment.
function live(text: string): Record<string, unknown> {
  mounted().receiveDecoded(generation, frame({ type: "LIVE", cut, data: encoder.encode(text) }));
  return snapshot();
}

function closeTransport(reason: string): Record<string, unknown> {
  mounted().transportClosed(generation, reason);
  return snapshot();
}

// The transport's next attempt on the same session: a new generation and cut.
async function readmit(): Promise<Record<string, unknown>> {
  generation += 1;
  cut += 10n;
  mounted().openTransport(generation);
  return admit();
}

// An in-place session switch: the identity commit, then the new session's
// first admission.
async function switchSession(name: string): Promise<Record<string, unknown>> {
  const current = mounted();
  current.replaceSessionPresentation({ sessionName: name, composerStorageScope: `typing-pause-${name}` });
  const between = snapshot();
  sessionName = name;
  generation += 1;
  cut += 10n;
  current.openTransport(generation);
  return { between, after: await admit() };
}

// A paste as the browser delivers it to the terminal's input element; xterm
// reads it and emits it through the same data path a keystroke takes.
function paste(text: string): Record<string, unknown> {
  const textarea = document.querySelector<HTMLTextAreaElement>("#typing-host .xterm-helper-textarea");
  if (!textarea) throw new Error("terminal input element missing");
  const data = new DataTransfer();
  data.setData("text/plain", text);
  textarea.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }));
  return snapshot();
}

// The insert path shared by snippets, clips and the toolbar Paste.
function insertText(text: string): Record<string, unknown> {
  return { result: mounted().insertText(text, "paste"), ...snapshot() };
}

type PagePrivates = { composerAvailability(): { canInject: boolean; reason: string }; injectComposerText(text: string, onDelivery: () => void): string };
const privates = (): PagePrivates => mounted() as unknown as PagePrivates;

function composerAvailability(): { canInject: boolean; reason: string } {
  const availability = privates().composerAvailability();
  return { canInject: availability.canInject, reason: availability.reason };
}

// The composer's own delivery hook, called as its Insert would call it.
function injectComposer(text: string): Record<string, unknown> {
  return { result: privates().injectComposerText(text, () => undefined), ...snapshot() };
}

function setTheme(theme: string): Record<string, unknown> {
  const shell = document.querySelector<HTMLElement>("#typing-host .persea-unified-terminal");
  if (!shell) throw new Error("terminal shell missing");
  shell.dataset.theme = theme;
  return snapshot();
}

function destroy(): Record<string, unknown> {
  mounted().destroy();
  const result = snapshot();
  page = undefined;
  return result;
}

declare global {
  interface Window {
    __typing_pause: {
      reset(name?: string): Record<string, unknown>;
      admit(): Promise<Record<string, unknown>>;
      refuse(code: string, generationOffset?: number): Record<string, unknown>;
      answer(code?: string, keep?: number): Record<string, unknown>;
      rawResult(through: number, code?: string): Record<string, unknown>;
      setPortResult(value: "ACCEPTED" | "SATURATED"): void;
      live(text: string): Record<string, unknown>;
      closeTransport(reason: string): Record<string, unknown>;
      readmit(): Promise<Record<string, unknown>>;
      switchSession(name: string): Promise<Record<string, unknown>>;
      paste(text: string): Record<string, unknown>;
      insertText(text: string): Record<string, unknown>;
      composerAvailability(): { canInject: boolean; reason: string };
      injectComposer(text: string): Record<string, unknown>;
      setTheme(theme: string): Record<string, unknown>;
      destroy(): Record<string, unknown>;
      snapshot(): Record<string, unknown>;
    };
  }
}

window.__typing_pause = { reset, admit, refuse, answer, rawResult, setPortResult, live, closeTransport, readmit, switchSession, paste, insertText, composerAvailability, injectComposer, setTheme, destroy, snapshot };
document.body.dataset.typingPauseReady = "true";
