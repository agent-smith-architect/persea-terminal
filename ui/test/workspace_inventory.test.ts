import { nextInventoryReadOrder } from "../src/inventory_read_order";
import { WorkspaceInventory } from "../src/workspace_page";
import type { DashboardInventory } from "../src/dashboard";

const assert = (value: unknown, message: string): void => { if (!value) throw new Error(message); };

// Each fetch is held until the test resolves it, so the order in which reads
// start and finish is chosen by the test.
function heldReads() {
  const reads: { resolve: (inventory: DashboardInventory) => void; signal: AbortSignal }[] = [];
  const fetch = (signal: AbortSignal) => new Promise<DashboardInventory>((resolve) => { reads.push({ resolve, signal }); });
  return { reads, fetch };
}
const inventory = (label: string): DashboardInventory => ({ realms: [{ name: label, displayName: label, uid: 0, servers: [] }], detachedAliases: [] } as unknown as DashboardInventory);
const label = (snapshot: { inventory: DashboardInventory }) => snapshot.inventory.realms[0]?.name;

async function main(): Promise<void> {
  {
    // Callers without a save share one read and its snapshot.
    const { reads, fetch } = heldReads();
    const shared = new WorkspaceInventory(fetch);
    const a = shared.snapshot(undefined, 0), b = shared.snapshot(undefined, 0);
    assert(reads.length === 1, "two callers did not share one read");
    reads[0]!.resolve(inventory("first"));
    const [first, second] = await Promise.all([a, b]);
    assert(first === second && label(first) === "first", "shared callers received different snapshots");
    assert(await shared.snapshot(undefined, -1) === first, "a newer snapshot was not reused");
  }
  {
    // A snapshot read before a save is not reused for a caller that needs a
    // read after it, even when it is newer than that caller's last snapshot.
    const { reads, fetch } = heldReads();
    const shared = new WorkspaceInventory(fetch);
    const before = shared.snapshot(undefined, 0);
    reads[0]!.resolve(inventory("before-save"));
    await before;
    const fence = nextInventoryReadOrder();
    const after = shared.snapshot(undefined, -1, fence);
    assert(reads.length === 2, "a snapshot read before the save was reused");
    reads[1]!.resolve(inventory("after-save"));
    const snapshot = await after;
    assert(label(snapshot) === "after-save" && snapshot.readOrder > fence, "the snapshot does not follow the save");
  }
  {
    // A read in flight since before a save is not joined. It still serves its
    // own callers, and when it finishes last it does not replace the later read.
    const { reads, fetch } = heldReads();
    const shared = new WorkspaceInventory(fetch);
    const sibling = shared.snapshot(undefined, 0);
    const fence = nextInventoryReadOrder();
    const waiter = new AbortController();
    const after = shared.snapshot(waiter.signal, 0, fence);
    assert(reads.length === 2, "a read started before the save was joined");
    reads[1]!.resolve(inventory("after-save"));
    assert(label(await after) === "after-save", "the caller after the save did not receive the later read");
    reads[0]!.resolve(inventory("before-save"));
    const siblingSnapshot = await sibling;
    assert(!reads[0]!.signal.aborted, "the shared read was cancelled for its other callers");
    assert(label(siblingSnapshot) === "after-save", "the earlier read's callers did not receive the later result");
    assert(label(shared.latest()!) === "after-save", "an earlier read replaced a later one");
  }
  {
    // Detaching one waiter never cancels the shared read.
    const { reads, fetch } = heldReads();
    const shared = new WorkspaceInventory(fetch);
    const waiter = new AbortController();
    const detached = shared.snapshot(waiter.signal, 0).then(() => "settled", () => "detached");
    const other = shared.snapshot(undefined, 0);
    waiter.abort();
    assert(await detached === "detached" && !reads[0]!.signal.aborted, "detaching a waiter cancelled the shared read");
    reads[0]!.resolve(inventory("shared"));
    assert(label(await other) === "shared", "the other caller lost the shared read");
  }
  console.log("workspace inventory read order tests passed");
}

// An unhandled rejection fails the run.
void main();
