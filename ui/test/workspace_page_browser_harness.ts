// Browser harness for the REAL workspace page (structural-update-preservation): the page
// class, its inventory, its per-pane controllers, and real attachments to the
// fixture, with one extra seam a product document does not expose — a
// structural `updateTree` call. Workspace edit mode drives that method from
// the UI; the runtime gate drives it from here and checks what the page preserves
// across it: cell elements, controllers and their sockets, focus, selection,
// and the designated pane.
import { DEFAULT_HISTORY_CHOICE } from "../src/dashboard";
import { WorkspaceInventory, WorkspacePage } from "../src/workspace_page";
import { parseWorkspace, type WorkspaceNode } from "../src/workspace_model";

// Trees arrive in the store's serialized envelope (the same bytes the
// ephemeral arrangement carries) and pass through the shared parser, so the
// harness never hand-builds an in-memory tree the product would not accept.
const parseEnvelope = (serialized: string): WorkspaceNode | undefined => {
  let value: unknown;
  try { value = JSON.parse(serialized); } catch { return undefined; }
  const parsed = parseWorkspace(value);
  return parsed.ok ? parsed.value : undefined;
};

type HarnessAPI = {
  open(serialized: string): Promise<void>;
  updateTree(serialized: string): boolean;
  refreshPresentation(): Promise<void>;
  savePresentationSnapshot(): void;
  refreshSavedPresentation(): Promise<void>;
  panes(): readonly { key: string; state: string; detail: string | null; generation: number; controllerDetail: string | null; counters?: object }[];
  designated(): string | undefined;
  inventoryRequests(): number;
};
declare global {
  interface Window { __wsPage: HarnessAPI; }
}

const nonce = document.querySelector('meta[name="persea-style-nonce"]')?.getAttribute("content") ?? "";
const root = document.getElementById("ws-host");
if (!(root instanceof HTMLElement)) throw new Error("harness host missing");
document.body.classList.add("ws-document");
const inventory = new WorkspaceInventory();
const page = new WorkspacePage({ root, styleNonce: nonce, name: "harness", win: window, inventory, historyRows: DEFAULT_HISTORY_CHOICE });
let savedPresentationSnapshot: ReturnType<WorkspaceInventory["latest"]>;

window.__wsPage = {
  async open(serialized: string): Promise<void> {
    const tree = parseEnvelope(serialized);
    if (tree === undefined) throw new Error("harness: the arrangement did not parse");
    await inventory.snapshot(undefined, -1);
    await page.open(tree);
  },
  updateTree(serialized: string): boolean {
    const tree = parseEnvelope(serialized);
    return tree === undefined ? false : page.updateTree(tree);
  },
  refreshPresentation(): Promise<void> { return page.refreshPresentation(); },
  savePresentationSnapshot(): void { savedPresentationSnapshot = inventory.latest(); },
  refreshSavedPresentation(): Promise<void> { return page.refreshPresentation(savedPresentationSnapshot); },
  panes(): readonly { key: string; state: string; detail: string | null; generation: number; controllerDetail: string | null; counters?: object }[] {
    return page.panesSnapshot().map((pane) => ({
      key: pane.key, state: pane.state.kind, detail: "detail" in pane.state ? pane.state.detail ?? null : null, generation: pane.generation,
      controllerDetail: pane.counters?.projectionDetail ?? null,
      ...(pane.counters ? { counters: pane.counters.counters } : {}),
    }));
  },
  designated(): string | undefined { return page.designatedKey(); },
  inventoryRequests(): number { return page.inventoryRequestCount(); },
};
document.body.dataset.wsHarnessReady = "true";
