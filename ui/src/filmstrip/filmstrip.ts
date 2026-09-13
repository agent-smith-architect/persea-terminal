import { Terminal, type ITerminalOptions } from "@xterm/xterm";
import type {
  CellDimensions,
  FilmstripModelState,
  FilmstripOptions,
  ReplicaSource,
  RotationResult,
} from "./types";

const FIXED_SCROLLBACK = 5000;
const OVERSCAN_ROWS = 16;
const MAX_ROWS_PER_WRITE = 192;
const MAX_WRITE_WORK_MS = 8;

type ChunkCommit = (rowCount: number) => void;

/**
 * Browser-native filmstrip composition. The replica buffer is disposable
 * presentation state; historyRows is the authoritative virtual history.
 */
export class Filmstrip {
  private readonly root = document.createElement("div");
  private readonly scroller = document.createElement("div");
  private readonly spacer = document.createElement("div");
  private readonly historyHost = document.createElement("div");
  private readonly liveHost: HTMLElement;
  private readonly source: ReplicaSource;
  private readonly live: Terminal;
  private readonly createHistoryTerminal: (options: ITerminalOptions) => Terminal;
  private readonly historyCapacity: number;
  private readonly onScrollBound = () => this.syncWindow();

  private history: Terminal | undefined;
  private currentHistoryHost: HTMLElement;
  private historyRows: string[] = [];
  private cellDimensions: CellDimensions;
  private windowRows: number;
  private renderedWindowStart = 0;
  private bufferOffset = 0;
  private bufferLength = 0;
  private droppedRows = 0;
  private mounted = false;

  public constructor(live: Terminal, source: ReplicaSource, options: FilmstripOptions) {
    if (source.width <= 0 || source.rowCount < 0) {
      throw new Error("ReplicaSource metadata is invalid");
    }
    if (live.cols !== source.width) {
      throw new Error("Injected live terminal width must match ReplicaSource width");
    }

    this.live = live;
    this.source = source;
    this.liveHost = options.liveHost;
    this.windowRows = options.windowRows ?? live.rows + OVERSCAN_ROWS * 2;
    if (this.windowRows <= 0) {
      throw new Error("windowRows must be positive");
    }
    this.historyCapacity = Math.min(options.historyCapacity ?? source.rowCount, FIXED_SCROLLBACK);
    if (this.historyCapacity <= 0) {
      throw new Error("historyCapacity must be positive");
    }
    this.createHistoryTerminal = options.createHistoryTerminal ?? ((terminalOptions) => new Terminal(terminalOptions));
    this.cellDimensions = options.cellDimensions ?? this.deriveCellDimensions();
    this.currentHistoryHost = this.historyHost;

    this.root.dataset.filmstripRoot = "true";
    this.scroller.dataset.filmstripScroller = "true";
    this.historyHost.dataset.filmstripTerminal = "history";
    this.configureElements(options.host);
  }

  /** Chunked, offscreen preload. Visibility changes only in the final callback. */
  public async mount(): Promise<void> {
    this.assertNotMounted();
    const initialStart = Math.max(0, this.source.rowCount - this.historyCapacity);
    this.historyRows = [...this.source.rows(initialStart, this.source.rowCount - initialStart)];
    this.droppedRows = initialStart;
    this.history = this.createHistory(this.windowRows, this.historyHost);
    this.historyHost.style.visibility = "hidden";
    this.layout();
    await this.writeRowsChunked(this.history, this.historyRows);
    this.bufferLength = this.historyRows.length;
    this.bufferOffset = 0;
    this.renderedWindowStart = this.maxWindowStart();
    this.positionHistoryWindow(true);
    this.historyHost.style.visibility = "visible";
    this.mounted = true;
    this.scrollToBottom();
  }

  /**
   * Reads new fixed-width rows through the source seam. At capacity the
   * accounting source, not the renderer, determines logical eviction.
   */
  public async appendFromSource(start: number, count: number): Promise<RotationResult> {
    this.assertMounted();
    const rows = this.source.rows(start, count);
    if (rows.length !== count) {
      throw new Error("ReplicaSource returned an unexpected row count");
    }
    if (count === 0) {
      return { appended: 0, dropped: 0 };
    }

    const dropped = Math.max(0, this.historyRows.length + rows.length - this.historyCapacity);
    let physicallyTrimmed = 0;
    await this.writeRowsChunked(this.requireHistory(), rows, (chunkRows) => {
      const overflow = Math.max(0, this.bufferLength + chunkRows - this.bufferCapacity());
      this.bufferLength = Math.min(this.bufferCapacity(), this.bufferLength + chunkRows);
      if (overflow > 0) {
        physicallyTrimmed += overflow;
        this.compensateScroll(overflow);
        this.syncWindow();
      }
    });

    this.historyRows = [...this.historyRows.slice(dropped), ...rows];
    this.droppedRows += dropped;
    const deferredTrim = dropped - physicallyTrimmed;
    if (deferredTrim < 0) {
      throw new Error("Renderer trim exceeded authoritative rotation accounting");
    }
    this.bufferOffset += deferredTrim;
    if (deferredTrim > 0) {
      this.compensateScroll(deferredTrim);
    }
    this.layout();
    this.positionHistoryWindow(true);
    return { appended: rows.length, dropped };
  }

  /**
   * A row-window change always creates an offscreen terminal. The old
   * renderer is never resized; its fixed scrollback configuration remains
   * untouched until disposal after the atomic handoff.
   */
  public async rebuildForWindowRows(nextWindowRows: number): Promise<void> {
    this.assertMounted();
    if (!Number.isInteger(nextWindowRows) || nextWindowRows <= 0) {
      throw new Error("nextWindowRows must be a positive integer");
    }
    if (nextWindowRows === this.windowRows) {
      return;
    }

    const nextHost = document.createElement("div");
    nextHost.dataset.filmstripTerminal = "history";
    nextHost.style.visibility = "hidden";
    this.spacer.append(nextHost);
    const nextHistory = this.createHistory(nextWindowRows, nextHost);
    await this.writeRowsChunked(nextHistory, this.historyRows);

    const previousHistory = this.requireHistory();
    const previousHost = this.currentHistoryHost;
    this.windowRows = nextWindowRows;
    this.history = nextHistory;
    this.currentHistoryHost = nextHost;
    this.bufferLength = this.historyRows.length;
    this.bufferOffset = 0;
    this.moveHistoryHost(nextHost);
    this.layout();
    this.positionHistoryWindow(true);
    nextHost.style.visibility = "visible";
    previousHost.style.visibility = "hidden";
    previousHistory.dispose();
    previousHost.remove();
  }

  /** Public model-only diagnostics; it deliberately exposes no rendered text. */
  public modelState(): FilmstripModelState {
    return {
      historyRows: this.historyRows.length,
      historyCapacity: this.historyCapacity,
      droppedRows: this.droppedRows,
      windowRows: this.windowRows,
      renderedWindowStart: this.renderedWindowStart,
      cellDimensions: { ...this.cellDimensions },
    };
  }

  public refreshCellDimensions(dimensions?: CellDimensions): void {
    this.cellDimensions = dimensions ?? this.deriveCellDimensions();
    if (this.mounted) {
      this.layout();
      this.positionHistoryWindow(true);
    }
  }

  public dispose(): void {
    this.scroller.removeEventListener("scroll", this.onScrollBound);
    this.history?.dispose();
    this.root.remove();
  }

  private configureElements(host: HTMLElement): void {
    Object.assign(this.root.style, {
      position: "relative",
      width: "100%",
      height: "100%",
      overflow: "hidden",
    });
    Object.assign(this.scroller.style, {
      position: "absolute",
      inset: "0",
      overflowY: "scroll",
      overflowX: "hidden",
      scrollbarGutter: "stable",
      overscrollBehavior: "contain",
    });
    Object.assign(this.spacer.style, {
      position: "relative",
      minWidth: "100%",
    });
    this.liveHost.style.position = "absolute";
    this.liveHost.style.left = "0";
    this.liveHost.style.width = "100%";
    this.historyHost.style.position = "absolute";
    this.historyHost.style.left = "0";
    this.historyHost.style.width = "100%";
    this.scroller.append(this.spacer);
    this.spacer.append(this.historyHost, this.liveHost);
    this.root.append(this.scroller);
    host.append(this.root);
    this.scroller.addEventListener("scroll", this.onScrollBound, { passive: true });
    this.live.attachCustomWheelEventHandler(() => false);
  }

  private createHistory(rows: number, host: HTMLElement): Terminal {
    const terminal = this.createHistoryTerminal({
      scrollback: FIXED_SCROLLBACK,
      convertEol: false,
    });
    terminal.resize(this.source.width, rows);
    terminal.attachCustomWheelEventHandler(() => false);
    terminal.open(host);
    return terminal;
  }

  private async writeRowsChunked(terminal: Terminal, rows: readonly string[], onCommit?: ChunkCommit): Promise<void> {
    let index = 0;
    while (index < rows.length) {
      await this.nextFrame();
      const startedAt = performance.now();
      const chunk: string[] = [];
      while (
        index < rows.length &&
        chunk.length < MAX_ROWS_PER_WRITE &&
        performance.now() - startedAt <= MAX_WRITE_WORK_MS
      ) {
        chunk.push(rows[index]);
        index += 1;
      }
      await new Promise<void>((resolve) => {
        // Fixed-width physical rows must not acquire a renderer-only wrap row.
        terminal.write(`\x1b[?7l${chunk.join("\r\n")}\r\n`, () => {
          onCommit?.(chunk.length);
          resolve();
        });
      });
    }
  }

  private layout(): void {
    const cellHeight = this.cellDimensions.height;
    const historyHeight = this.historyRows.length * cellHeight;
    const liveHeight = this.live.rows * cellHeight;
    this.spacer.style.height = `${historyHeight + liveHeight}px`;
    this.liveHost.style.top = `${historyHeight}px`;
    this.liveHost.style.height = `${liveHeight}px`;
    this.activeHistoryHost().style.height = `${this.windowRows * cellHeight}px`;
  }

  private syncWindow(): void {
    if (!this.mounted || this.scroller.scrollTop >= this.historyRows.length * this.cellDimensions.height) {
      return;
    }
    const maximum = this.maxWindowStart();
    const row = this.clamp(Math.floor(this.scroller.scrollTop / this.cellDimensions.height), 0, maximum);
    if (row === maximum) {
      if (this.renderedWindowStart !== maximum) {
        this.renderedWindowStart = maximum;
        this.positionHistoryWindow(false);
      }
      return;
    }
    const lower = this.renderedWindowStart + OVERSCAN_ROWS;
    const upper = this.renderedWindowStart + this.windowRows - OVERSCAN_ROWS;
    if (row >= lower && row <= upper) {
      return;
    }
    this.renderedWindowStart = this.clamp(row - OVERSCAN_ROWS, 0, this.maxWindowStart());
    this.positionHistoryWindow(false);
  }

  private positionHistoryWindow(force: boolean): void {
    const history = this.requireHistory();
    const target = this.renderedWindowStart + this.bufferOffset;
    if (force || target >= 0) {
      history.scrollToLine(target);
    }
    this.activeHistoryHost().style.top = `${this.renderedWindowStart * this.cellDimensions.height}px`;
  }

  private compensateScroll(rows: number): void {
    this.scroller.scrollTop = Math.max(0, this.scroller.scrollTop - rows * this.cellDimensions.height);
  }

  private scrollToBottom(): void {
    this.scroller.scrollTop = Math.max(0, this.spacer.offsetHeight - this.scroller.clientHeight);
  }

  private deriveCellDimensions(): CellDimensions {
    const element = this.live.element;
    if (!element) {
      throw new Error("Injected live terminal must be opened before Filmstrip construction");
    }
    const bounds = element.getBoundingClientRect();
    const width = bounds.width / this.live.cols;
    const height = bounds.height / this.live.rows;
    if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) {
      throw new Error("Unable to derive terminal cell dimensions from public element bounds");
    }
    return { width, height };
  }

  private activeHistoryHost(): HTMLElement {
    return this.currentHistoryHost;
  }

  private moveHistoryHost(host: HTMLElement): void {
    host.style.position = "absolute";
    host.style.left = "0";
    host.style.width = "100%";
  }

  private bufferCapacity(): number {
    return FIXED_SCROLLBACK + this.windowRows;
  }

  private maxWindowStart(): number {
    return Math.max(0, this.historyRows.length - this.windowRows);
  }

  private clamp(value: number, minimum: number, maximum: number): number {
    return Math.min(maximum, Math.max(minimum, value));
  }

  private requireHistory(): Terminal {
    if (!this.history) {
      throw new Error("History renderer is unavailable");
    }
    return this.history;
  }

  private assertMounted(): void {
    if (!this.mounted) {
      throw new Error("Filmstrip must be mounted first");
    }
  }

  private assertNotMounted(): void {
    if (this.mounted) {
      throw new Error("Filmstrip is already mounted");
    }
  }

  private nextFrame(): Promise<void> {
    return new Promise((resolve) => requestAnimationFrame(() => resolve()));
  }
}
