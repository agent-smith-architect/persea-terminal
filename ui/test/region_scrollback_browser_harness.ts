// Browser harness for scroll-region history preservation in the unified
// terminal. Codex's TUI (Ratatui) preserves prior screen content by scrolling
// it out of a TOP-ANCHORED DECSTBM region (CSI 1;Nr + CSI N S); tmux archives
// those lines into pane history, while stock xterm.js splices them away — its
// own FIXME at InputHandler.scrollUp says "scrolled out lines at top = 1
// should add to scrollback (xterm)". The unified terminal's browser xterm is
// the ONLY interpreter of the pane's original bytes, so the emulation must
// match tmux or the pre-TUI transcript is silently destroyed.
//
// Every expectation below is pinned by tmux 3.4 control probes (evidence:
// 2026-08-19_scrollup_history_loss/tmux_probes): SU archives
// min(param, region height) top lines of the region for ANY region top on the
// normal buffer; the alternate buffer never archives; DL (CSI M) never
// archives regardless of region.
import { UnifiedTerminalPage } from "../src/unified_terminal_page";

const SOURCE = "harness-source";
const EPOCH = 1n;
const NONCE = "AAAAAAAAAAAAAAAAAAAAAA";
const ROWS = 24;
const encoder = new TextEncoder();

let page: UnifiedTerminalPage | undefined;
let generation = 0;
let cut = 9n;

function frame(extra: Record<string, unknown>): Record<string, unknown> {
  return { version: 1, source: SOURCE, epoch: EPOCH, ...extra };
}

function settle(): Promise<void> {
  return new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 40))));
}

// Ground truth from the emulator itself, not from the geometry under test.
function buffer(): any {
  return (page as any).terminal.buffer.active;
}

function lineText(absoluteRow: number): string {
  return (buffer().getLine(absoluteRow)?.translateToString(true) ?? "").trim();
}

function allBufferText(): string {
  const active = buffer();
  const lines: string[] = [];
  for (let row = 0; row < active.length; row++) lines.push(lineText(row));
  return lines.join("\n");
}

async function boot(lines: string[]): Promise<void> {
  const root = document.getElementById("root");
  if (!root) throw new Error("harness root is absent");
  if (!page) {
    page = new UnifiedTerminalPage({
      root,
      port: { trySend: () => "ACCEPTED", finalize: () => undefined },
      capabilityMode: "control",
      // Keep the full-buffer stress case at its explicit 10,000-row capacity.
      historyRows: 10_000,
      styleNonce: NONCE,
      rememberSource: () => undefined,
    });
    (window as any).__page = page;
  }
  generation += 1;
  cut += 1n;
  page.openTransport(generation);
  page.receiveDecoded(generation, frame({
    type: "PREPARE", cut, kind: "INITIAL", columns: 80, rows: ROWS,
    history: [], truncated: false, replay: encoder.encode(lines.join("\r\n")),
  }));
  page.receiveDecoded(generation, frame({ type: "COMMIT", cut }));
  // The broker's control grant in response to the page's MODE_REQUEST at
  // COMMIT; the page seals input until it lands.
  page.receiveDecoded(generation, frame({ type: "MODE", mode: "CONTROL" }));
  // xterm consumes large writes in chunks across frames, so wait until the
  // replay's last line has actually landed on the screen bottom row.
  const lastLine = lines[lines.length - 1];
  for (let attempt = 0; attempt < 400; attempt++) {
    await settle();
    if (lineText(buffer().baseY + ROWS - 1) === lastLine) return;
  }
  throw new Error(`replay never settled: bottom row is ${JSON.stringify(lineText(buffer().baseY + ROWS - 1))}`);
}

async function live(text: string): Promise<void> {
  if (!page) throw new Error("harness is not booted");
  page.receiveDecoded(generation, frame({ type: "LIVE", cut, data: encoder.encode(text) }));
  await settle();
}

function seedScreen(): string[] {
  const lines: string[] = [];
  for (let row = 1; row <= ROWS; row++) lines.push(`S${String(row).padStart(2, "0")}`);
  return lines;
}

type CaseResult = { name: string; pass: boolean; detail: Record<string, unknown> };

function judge(name: string, expectations: Record<string, [unknown, unknown]>): CaseResult {
  const detail: Record<string, unknown> = {};
  let pass = true;
  for (const [key, [actual, expected]] of Object.entries(expectations)) {
    const ok = actual === expected;
    if (!ok) pass = false;
    detail[key] = ok ? actual : { actual, expected };
  }
  return { name, pass, detail };
}

// SU inside a top-anchored partial region (tmux probe p2): the lines that
// leave the region top become the newest pane history, directly above the
// surviving screen.
async function suTopPartialRegion(): Promise<CaseResult> {
  await boot(seedScreen());
  const before = buffer().baseY;
  await live("\x1b[1;10r\x1b[3S\x1b[r");
  const baseY = buffer().baseY;
  return judge("su_top_partial_region", {
    archivedDelta: [baseY - before, 3],
    history0: [lineText(baseY - 3), "S01"],
    history1: [lineText(baseY - 2), "S02"],
    history2: [lineText(baseY - 1), "S03"],
    screenTop: [lineText(baseY), "S04"],
    belowRegionUntouched: [lineText(baseY + 10), "S11"],
    screenBottom: [lineText(baseY + ROWS - 1), "S24"],
    viewportPinned: [buffer().viewportY, baseY],
  });
}

// SU inside an INNER region (tmux probe p3): tmux archives the region-top
// lines even when the region does not start at the screen top; they appear in
// history directly above the (unmoved) screen top.
async function suInnerRegion(): Promise<CaseResult> {
  await boot(seedScreen());
  const before = buffer().baseY;
  await live("\x1b[5;10r\x1b[3S\x1b[r");
  const baseY = buffer().baseY;
  return judge("su_inner_region", {
    archivedDelta: [baseY - before, 3],
    history0: [lineText(baseY - 3), "S05"],
    history1: [lineText(baseY - 2), "S06"],
    history2: [lineText(baseY - 1), "S07"],
    screenTopUnmoved: [lineText(baseY), "S01"],
    aboveRegionUntouched: [lineText(baseY + 3), "S04"],
    regionScrolled: [lineText(baseY + 4), "S08"],
    belowRegionUntouched: [lineText(baseY + 10), "S11"],
  });
}

// The exact shape codex emits at startup (tmux probe p8): CSI 1;4r + CSI 4S
// with the region bottom far above the last row.
async function suCodexShape(): Promise<CaseResult> {
  await boot(seedScreen());
  const before = buffer().baseY;
  await live("\x1b[1;4r\x1b[4S\x1b[r");
  const baseY = buffer().baseY;
  return judge("su_codex_shape_1_4", {
    archivedDelta: [baseY - before, 4],
    history0: [lineText(baseY - 4), "S01"],
    history3: [lineText(baseY - 1), "S04"],
    regionBlank: [lineText(baseY), ""],
    belowRegionUntouched: [lineText(baseY + 4), "S05"],
  });
}

// The alternate buffer never archives (tmux probe p4), and returning to the
// normal buffer restores the untouched screen.
async function suAltBuffer(): Promise<CaseResult> {
  await boot(seedScreen());
  const normalBefore = buffer().baseY;
  const altFill = [] as string[];
  for (let row = 1; row <= ROWS; row++) altFill.push(`A${String(row).padStart(2, "0")}`);
  await live(`\x1b[?1049h\x1b[2J\x1b[H${altFill.join("\r\n")}`);
  const duringType = buffer().type;
  const duringBaseY = buffer().baseY;
  await live("\x1b[1;10r\x1b[3S\x1b[r");
  const afterScrollBaseY = buffer().baseY;
  await live("\x1b[?1049l");
  const baseY = buffer().baseY;
  return judge("su_alt_buffer_never_archives", {
    altActive: [duringType, "alternate"],
    altBaseYBefore: [duringBaseY, 0],
    altBaseYAfter: [afterScrollBaseY, 0],
    normalBaseYUnchanged: [baseY, normalBefore],
    normalRestoredTop: [lineText(baseY), "S01"],
    normalRestoredBottom: [lineText(baseY + ROWS - 1), "S24"],
    altLinesNotInNormal: [allBufferText().includes("A01"), false],
  });
}

// DL never archives (tmux probes p5/p6): deleted lines are gone from the
// entire buffer, exactly as before the patch.
async function dlNeverArchives(): Promise<CaseResult> {
  await boot(seedScreen());
  const before = buffer().baseY;
  await live("\x1b[1;10r\x1b[1;1H\x1b[3M\x1b[r");
  const baseY = buffer().baseY;
  const withRegion = judge("dl_top_partial_region", {
    archivedDelta: [baseY - before, 0],
    deletedGone: [allBufferText().includes("S01"), false],
    screenTop: [lineText(baseY), "S04"],
    belowRegionUntouched: [lineText(baseY + 10), "S11"],
  });
  await boot(seedScreen());
  const before2 = buffer().baseY;
  await live("\x1b[1;1H\x1b[3M");
  const baseY2 = buffer().baseY;
  const withoutRegion = judge("dl_no_region", {
    archivedDelta: [baseY2 - before2, 0],
    deletedGone: [allBufferText().includes("S01"), false],
    screenTop: [lineText(baseY2), "S04"],
    shifted: [lineText(baseY2 + 20), "S24"],
  });
  return {
    name: "dl_never_archives",
    pass: withRegion.pass && withoutRegion.pass,
    detail: { withRegion: withRegion.detail, withoutRegion: withoutRegion.detail },
  };
}

// SU beyond the region height archives exactly the region height (tmux probe
// p9 clamps CSI 15S in a 10-row region to 10 archived lines).
async function suOverscrollClamp(): Promise<CaseResult> {
  await boot(seedScreen());
  const before = buffer().baseY;
  await live("\x1b[1;10r\x1b[15S\x1b[r");
  const baseY = buffer().baseY;
  return judge("su_overscroll_clamp", {
    archivedDelta: [baseY - before, 10],
    historyOldest: [lineText(baseY - 10), "S01"],
    historyNewest: [lineText(baseY - 1), "S10"],
    regionBlank: [lineText(baseY), ""],
    belowRegionUntouched: [lineText(baseY + 10), "S11"],
  });
}

// With the scrollback full, archiving must rotate: the oldest history line is
// trimmed, the archived line still lands as newest history, and the absolute
// buffer length and baseY hold steady (mirrors BufferService.scroll's trimmed
// branch, which the LF path already exercises).
async function suFullBufferTrim(): Promise<CaseResult> {
  const filler: string[] = [];
  for (let row = 1; row <= 10_150; row++) filler.push(`F${String(row).padStart(5, "0")}`);
  await boot(filler);
  const active = buffer();
  const beforeLength = active.length;
  const beforeBaseY = active.baseY;
  const beforeOldest = lineText(0);
  const beforeThird = lineText(2);
  const screenTopBefore = lineText(beforeBaseY);
  const screenNextBefore = lineText(beforeBaseY + 1);
  const screenThirdBefore = lineText(beforeBaseY + 2);
  await live("\x1b[1;10r\x1b[2S\x1b[r");
  const baseY = buffer().baseY;
  return judge("su_full_buffer_trim", {
    fullBefore: [beforeLength, 10_000 + ROWS],
    lengthStable: [buffer().length, beforeLength],
    baseYStable: [baseY, beforeBaseY],
    oldestTrimmed: [lineText(0), beforeThird],
    oldestGone: [allBufferText().includes(beforeOldest), false],
    archivedNewest0: [lineText(baseY - 2), screenTopBefore],
    archivedNewest1: [lineText(baseY - 1), screenNextBefore],
    screenTop: [lineText(baseY), screenThirdBefore],
    viewportPinned: [buffer().viewportY, baseY],
  });
}

// The unified page's scroll projection reads buffer.baseY; history created by
// region scrolling must be reachable exactly like history created by ordinary
// LF scrolling: the range covers baseY, the top of the range shows the oldest
// archived line, and the tail still shows the live screen.
async function projectionCoversArchivedHistory(): Promise<CaseResult> {
  await boot(seedScreen());
  await live("\x1b[1;10r\x1b[10S\x1b[r");
  await live("\x1b[24;1H\x1b[2KTAIL-LIVE-MARKER");
  const baseY = buffer().baseY;
  const outer = document.querySelector<HTMLElement>(".persea-unified-scroll");
  const screen = document.querySelector<HTMLElement>(".persea-unified-xterm .xterm-screen");
  if (!outer || !screen) throw new Error("unified terminal geometry is not mounted");
  const cellHeight = screen.getBoundingClientRect().height / ROWS;
  const maxProjectableRow = Math.floor((outer.scrollHeight - outer.clientHeight) / cellHeight);
  outer.scrollTop = 0;
  await settle();
  await settle();
  const rows = () => Array.from(document.querySelectorAll<HTMLElement>(".xterm-rows > div"));
  const topRowText = (rows()[0]?.textContent ?? "").trim();
  outer.scrollTop = outer.scrollHeight;
  await settle();
  await settle();
  const tailText = rows().map((row) => (row.textContent ?? "").trim()).join("\n");
  return judge("projection_covers_archived_history", {
    archivedDelta: [baseY, 10],
    rangeCoversBaseY: [maxProjectableRow >= baseY, true],
    topShowsOldestArchived: [topRowText, "S01"],
    tailShowsLiveScreen: [tailText.includes("TAIL-LIVE-MARKER"), true],
  });
}

(window as any).__regionScrollbackHarness = {
  async run(): Promise<{ results: CaseResult[]; failures: string[] }> {
    const results: CaseResult[] = [];
    results.push(await suTopPartialRegion());
    results.push(await suInnerRegion());
    results.push(await suCodexShape());
    results.push(await suAltBuffer());
    results.push(await dlNeverArchives());
    results.push(await suOverscrollClamp());
    results.push(await suFullBufferTrim());
    results.push(await projectionCoversArchivedHistory());
    const failures = results.filter((result) => !result.pass)
      .map((result) => `${result.name}: ${JSON.stringify(result.detail)}`);
    return { results, failures };
  },
};
(window as any).regionScrollbackHarnessReady = true;
