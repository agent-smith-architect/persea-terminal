import { UnifiedTerminalPage } from "../src/unified_terminal_page";

// The terminal's ordered renderer and its resume point, driven through the
// real page with a fake port. Frames are delivered in one task, as a socket
// burst delivers them, so xterm has not parsed any of them yet; each case then
// reads what the emulator actually shows.
const SOURCE = "resume-order-source";
const EPOCH = 7n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const STREAM = "ABCDEFGHIJKLMNOPQRSTUVWXYZ";
const OTHER_STREAM = "BBCDEFGHIJKLMNOPQRSTUVWXYZ";
const encoder = new TextEncoder();

type Position = { seq: number; offset: number };
let page: UnifiedTerminalPage | undefined;
let generation = 0;
let cut = 30n;
const sent: Array<{ generation: number; type: string }> = [];
const finalized: Array<{ generation: number; cause: string }> = [];

const frame = (extra: Record<string, unknown>): Record<string, unknown> => ({ version: 1, source: SOURCE, epoch: EPOCH, ...extra });

function mounted(): UnifiedTerminalPage {
  if (!page) throw new Error("no page is mounted");
  return page;
}

function mount(): void {
  page?.destroy();
  sent.splice(0);
  finalized.splice(0);
  const host = document.getElementById("resume-host");
  if (!(host instanceof HTMLElement)) throw new Error("resume host missing");
  host.replaceChildren();
  generation = 0;
  page = new UnifiedTerminalPage({
    root: host,
    port: {
      trySend: (frameGeneration, value) => { sent.push({ generation: frameGeneration, type: value.type }); return "ACCEPTED"; },
      finalize: (intent) => { finalized.push({ generation: intent.generation, cause: intent.cause }); },
      detach: () => undefined,
      attachAgain: () => undefined,
    },
    capabilityMode: "control",
    styleNonce: NONCE,
    sessionName: "resume-order",
    composerStorageScope: "resume-order",
    rememberSource: () => undefined,
    claimFocusOnCommit: () => false,
  });
}

// What the emulator shows: every buffer line, trimmed on the right.
function lines(): string[] {
  const buffer = (mounted() as any).terminal.buffer.active;
  const result: string[] = [];
  for (let row = 0; row < buffer.length; row++) result.push(buffer.getLine(row)?.translateToString(true) ?? "");
  while (result.length > 0 && result[result.length - 1] === "") result.pop();
  return result;
}

function state(): Record<string, unknown> {
  const terminal = (mounted() as any).terminal;
  return {
    lines: lines(),
    columns: terminal.cols,
    rows: terminal.rows,
    offer: mounted().resumeProtocol() ?? null,
    finalized: finalized.slice(),
    ready: sent.filter((entry) => entry.type === "READY" && entry.generation === generation).length,
  };
}

// Waits until xterm has parsed everything queued to it.
function drained(): Promise<void> {
  return new Promise((resolve) => (mounted() as any).terminal.write("", () => setTimeout(resolve, 0)));
}

// A new connection's socket opens; the old one has ended.
function open(): void {
  generation += 1;
  cut += 1n;
  mounted().openTransport(generation);
}

function close(): void {
  mounted().transportClosed(generation, "websocket_1006");
}

function prepare(admission: Record<string, unknown>): void {
  mounted().receiveDecoded(generation, frame({ type: "PREPARE", cut, kind: "INITIAL", history: [], truncated: false, ...admission }));
}

// A connection: its PREPARE, the replay's READY, COMMIT and the control grant.
async function admit(admission: Record<string, unknown>): Promise<Record<string, unknown>> {
  const current = mounted();
  open();
  prepare(admission);
  await drained();
  if (finalized.length === 0) {
    current.receiveDecoded(generation, frame({ type: "COMMIT", cut }));
    current.receiveDecoded(generation, frame({ type: "MODE", mode: "CONTROL" }));
    await drained();
  }
  return state();
}

function live(text: string | Uint8Array, position?: Position): void {
  mounted().receiveDecoded(generation, frame({ type: "LIVE", cut, data: typeof text === "string" ? encoder.encode(text) : text, ...(position ? { position } : {}) }));
}

function geometry(columns: number, rows: number, position?: Position): void {
  mounted().receiveDecoded(generation, frame({ type: "PREPARE", cut, kind: "RESIZE", columns, rows, history: [], truncated: false, replay: new Uint8Array(), ...(position ? { position } : {}) }));
}

// A full admission of STREAM: 80x24, "first" at the stream's start.
function whole(): Promise<Record<string, unknown>> {
  return admit({ columns: 80, rows: 24, replay: encoder.encode("first"), stream: STREAM, position: { seq: 1, offset: 5 } });
}

(window as any).__resume = {
  // Output, a geometry and output again, delivered before xterm parsed any
  // of them: the geometry must apply between them. "\x1b[999C" moves the
  // cursor to the right edge, so X lands in the last column of the width in
  // force when it is parsed.
  async geometryBetweenOutput() {
    mount();
    await whole();
    live("\r\n\x1b[999CX", { seq: 2, offset: 14 });
    geometry(120, 24, { seq: 3, offset: 14 });
    live("\r\n\x1b[999CY", { seq: 4, offset: 23 });
    const queued = mounted().resumeProtocol() ?? null;
    await drained();
    return { queued, ...state() };
  },
  // A new connection's full admission resets the terminal only after the
  // output the old one queued has been parsed, so nothing old survives it.
  async resetAfterQueuedOutput() {
    mount();
    await whole();
    live("\r\nOLD-OUTPUT", { seq: 2, offset: 17 });
    return await admit({ columns: 80, rows: 24, replay: encoder.encode("NEW"), stream: OTHER_STREAM, position: { seq: 1, offset: 3 } });
  },
  // A reconnect that resumes: the page offers its point, and the resumed
  // admission continues the screen instead of replacing it.
  async resumeContinues() {
    mount();
    await whole();
    live("-second", { seq: 2, offset: 12 });
    await drained();
    const offer = mounted().resumeProtocol();
    mounted().transportClosed(generation, "websocket_1006");
    const resumed = await admit({ columns: 80, rows: 24, replay: encoder.encode("-third"), stream: STREAM, resumed: true, position: { seq: 3, offset: 18 } });
    live("-fourth", { seq: 3, offset: 25 });
    await drained();
    return { offer, resumed, after: state() };
  },
  // Resumed admissions the page did not offer, or that do not continue what
  // it shows, end the attachment and drop the point.
  async resumeRefused(variant: string) {
    mount();
    await whole();
    await drained();
    mounted().resumeProtocol();
    if (variant === "point moved") {
      live("-late", { seq: 2, offset: 10 });
      await drained();
    }
    if (variant === "output queued") live("-late", { seq: 2, offset: 10 });
    if (variant === "never offered") mounted().forgetResumePoint();
    mounted().transportClosed(generation, "websocket_1006");
    const admission: Record<string, unknown> = { columns: 80, rows: 24, replay: new Uint8Array(), stream: STREAM, resumed: true, position: { seq: 1, offset: 5 } };
    if (variant === "other stream") admission.stream = OTHER_STREAM;
    if (variant === "other geometry") admission.columns = 100;
    if (variant === "position behind") admission.position = { seq: 0, offset: 0 };
    if (variant === "replay does not follow") Object.assign(admission, { replay: encoder.encode("abc"), position: { seq: 2, offset: 7 } });
    return await admit(admission);
  },
  // An offer is made only when nothing is queued to the terminal.
  async offerWaitsForTheRenderer() {
    mount();
    await whole();
    live("-more", { seq: 2, offset: 10 });
    const queued = mounted().resumeProtocol() ?? null;
    await drained();
    return { queued, drained: mounted().resumeProtocol() ?? null };
  },
  // A geometry of a connection that ended before xterm parsed it still moves
  // the point, so the next offer names the geometry the screen has.
  async supersededGeometry() {
    mount();
    await whole();
    live("\r\nX", { seq: 2, offset: 8 });
    geometry(120, 30, { seq: 3, offset: 8 });
    close();
    open();
    await drained();
    close();
    const offer = mounted().resumeProtocol() ?? null;
    const resumed = await admit({ columns: 120, rows: 30, replay: new Uint8Array(), stream: STREAM, resumed: true, position: { seq: 3, offset: 8 } });
    return { offer, resumed };
  },
  // A resumed admission whose connection ends before its replay is parsed:
  // the replay still reaches the screen, so the page must not offer the
  // position before it, or the next resume would send the replay again.
  async resumedReplaySuperseded() {
    mount();
    await whole();
    live("-a", { seq: 2, offset: 7 });
    await drained();
    mounted().resumeProtocol();
    close();
    open();
    prepare({ columns: 80, rows: 24, replay: encoder.encode("-b"), stream: STREAM, resumed: true, position: { seq: 3, offset: 9 } });
    const accepted = finalized.length === 0;
    close();
    open();
    await drained();
    close();
    return { accepted, offer: mounted().resumeProtocol() ?? null, ...state() };
  },
  // A connection lost inside an escape sequence or a UTF-8 character: the
  // next full replay starts clean.
  async cutShortThenWholeStream(cut: string) {
    mount();
    await whole();
    live(cut === "utf-8" ? new Uint8Array([0xc3]) : cut === "osc" ? "\x1b]0;tit" : "\x1b[", { seq: 2, offset: 6 });
    await drained();
    close();
    // The stream starts with an orphan UTF-8 continuation byte, which a clean
    // decoder drops; one still holding the cut character would join them.
    const replay = new Uint8Array([0xa9, ...encoder.encode("HELLO\r\nWORLD")]);
    return await admit({ columns: 80, rows: 24, replay, stream: OTHER_STREAM, position: { seq: 1, offset: replay.byteLength } });
  },
  // A frame whose position does not follow the point leaves the screen as
  // it is but drops the point.
  async positionGap() {
    mount();
    await whole();
    live("-more", { seq: 2, offset: 11 });
    await drained();
    return state();
  },
};
document.body.dataset.resumeReady = "true";
