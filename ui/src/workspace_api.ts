import { csrfToken } from "./csrf_refresh";
import { parseWorkspaceTree, serializeWorkspace, WORKSPACE_STORE_VERSION, type WorkspaceNode } from "./workspace_model";
import { boundedWorkspaceName } from "./workspace_url";

export type WorkspaceRecord = Readonly<{
  workspaceId: string;
  name: string;
  normalizedName: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
  tree: WorkspaceNode;
}>;

export type WorkspaceList = Readonly<{ version: 1; items: readonly WorkspaceRecord[] }>;
export type WorkspaceAPIErrorCode = "unavailable" | "not_found" | "conflict" | "rate_limited" | "capacity" | "invalid";

export class WorkspaceAPIError extends Error {
  constructor(readonly code: WorkspaceAPIErrorCode, readonly status: number, readonly current?: WorkspaceRecord) {
    super(code);
    this.name = "WorkspaceAPIError";
  }
}

export function workspaceAPIErrorCode(status: number): WorkspaceAPIErrorCode {
  if (status === 404) return "not_found";
  if (status === 429) return "rate_limited";
  if (status === 507) return "capacity";
  if (status === 400 || status === 413 || status === 428) return "invalid";
  return "unavailable";
}

type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
type ObjectValue = Record<string, unknown>;

function object(value: unknown, keys: readonly string[], label: string): ObjectValue {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  const record = value as ObjectValue;
  const actual = Object.keys(record).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) throw new Error(`${label} has unknown fields`);
  return record;
}

function text(value: unknown, label: string): string {
  if (typeof value !== "string" || value === "") throw new Error(`${label} must be a nonempty string`);
  return value;
}

function integer(value: unknown, label: string, minimum = 0): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) throw new Error(`${label} must be an integer`);
  return value;
}

function parseRecord(value: unknown, label: string): WorkspaceRecord {
  const wire = object(value, ["workspace_id", "name", "normalized_name", "revision", "created_at", "updated_at", "tree"], label);
  const workspaceId = text(wire.workspace_id, `${label}.workspace_id`);
  if (!/^[0-9a-f]{32}$/.test(workspaceId)) throw new Error(`${label}.workspace_id is invalid`);
  const name = text(wire.name, `${label}.name`);
  if (boundedWorkspaceName(name) !== name) throw new Error(`${label}.name is invalid`);
  const normalizedName = text(wire.normalized_name, `${label}.normalized_name`);
  if (boundedWorkspaceName(normalizedName) !== normalizedName) throw new Error(`${label}.normalized_name is invalid`);
  const parsed = parseWorkspaceTree(wire.tree);
  if (!parsed.ok) throw new Error(`${label}.tree is invalid`);
  const createdAt = text(wire.created_at, `${label}.created_at`);
  const updatedAt = text(wire.updated_at, `${label}.updated_at`);
  if (!Number.isFinite(Date.parse(createdAt)) || !Number.isFinite(Date.parse(updatedAt))) throw new Error(`${label} timestamps are invalid`);
  return Object.freeze({ workspaceId, name, normalizedName, revision: integer(wire.revision, `${label}.revision`, 1), createdAt, updatedAt, tree: parsed.value });
}

export function parseWorkspaceList(value: unknown): WorkspaceList {
  const root = object(value, ["version", "items"], "workspace list");
  if (root.version !== WORKSPACE_STORE_VERSION || !Array.isArray(root.items) || root.items.length > 64) throw new Error("workspace list is invalid");
  const items = root.items.map((item, index) => parseRecord(item, `workspace list.items[${index}]`));
  const ids = new Set<string>(); const names = new Set<string>();
  for (const item of items) {
    if (ids.has(item.workspaceId) || names.has(item.normalizedName)) throw new Error("workspace list contains duplicates");
    ids.add(item.workspaceId); names.add(item.normalizedName);
  }
  return Object.freeze({ version: WORKSPACE_STORE_VERSION, items: Object.freeze(items) });
}

function mutationBody(name: string, tree: WorkspaceNode): string {
  if (boundedWorkspaceName(name) !== name) throw new WorkspaceAPIError("invalid", 400);
  return JSON.stringify({ version: WORKSPACE_STORE_VERSION, name, tree: serializeWorkspace(tree).root });
}

async function failure(response: Response): Promise<WorkspaceAPIError> {
  if (response.status === 409) {
    try { return new WorkspaceAPIError("conflict", 409, parseRecord(await response.json(), "workspace conflict")); } catch { return new WorkspaceAPIError("unavailable", 409); }
  }
  return new WorkspaceAPIError(workspaceAPIErrorCode(response.status), response.status);
}

export class WorkspaceAPI {
  constructor(private readonly fetcher: FetchLike = (input, init) => window.fetch(input, init)) {}

  async list(signal?: AbortSignal): Promise<WorkspaceList> {
    const response = await this.fetcher("/api/workspaces", { cache: "no-store", credentials: "same-origin", signal });
    if (!response.ok) throw await failure(response);
    try { return parseWorkspaceList(await response.json()); } catch { throw new WorkspaceAPIError("unavailable", response.status); }
  }

  async create(name: string, tree: WorkspaceNode, signal?: AbortSignal): Promise<WorkspaceRecord> {
    return this.mutate("/api/workspaces", "POST", name, tree, undefined, signal);
  }

  async update(record: Pick<WorkspaceRecord, "workspaceId" | "revision">, name: string, tree: WorkspaceNode, signal?: AbortSignal): Promise<WorkspaceRecord> {
    return this.mutate(`/api/workspaces/${encodeURIComponent(record.workspaceId)}`, "PUT", name, tree, record.revision, signal);
  }

  async delete(record: Pick<WorkspaceRecord, "workspaceId" | "revision">, signal?: AbortSignal): Promise<void> {
    const response = await this.fetcher(`/api/workspaces/${encodeURIComponent(record.workspaceId)}`, {
      method: "DELETE", cache: "no-store", credentials: "same-origin", signal,
      headers: { "If-Match": `"${record.revision}"`, "X-Persea-CSRF": csrfToken() },
    });
    if (!response.ok) throw await failure(response);
  }

  private async mutate(url: string, method: "POST" | "PUT", name: string, tree: WorkspaceNode, revision: number | undefined, signal?: AbortSignal): Promise<WorkspaceRecord> {
    const response = await this.fetcher(url, {
      method, cache: "no-store", credentials: "same-origin", signal,
      headers: {
        "Content-Type": "application/json", "X-Persea-CSRF": csrfToken(),
        ...(revision === undefined ? {} : { "If-Match": `"${revision}"` }),
      },
      body: mutationBody(name, tree),
    });
    if (!response.ok) throw await failure(response);
    try { return parseRecord(await response.json(), "workspace response"); } catch { throw new WorkspaceAPIError("unavailable", response.status); }
  }
}

export function workspaceAPIMessage(error: unknown, action = "loaded"): Readonly<{ headline: string; detail: string; code: string }> {
  if (error instanceof WorkspaceAPIError) {
    switch (error.code) {
      case "not_found": return { headline: "This workspace no longer exists", detail: "Return to the dashboard and choose another workspace.", code: "workspace_not_found" };
      case "conflict": return { headline: "This workspace changed elsewhere", detail: "Reload the saved version or keep editing your local draft.", code: "workspace_conflict" };
      case "rate_limited": return { headline: "Workspace changes are waiting", detail: "The server is busy. Your draft is still here; retry this action shortly.", code: "workspace_rate_limited" };
      case "capacity": return { headline: "Workspace capacity is full", detail: "Delete an unused workspace, then retry. Your current draft and saved workspaces are unchanged.", code: "workspace_capacity" };
      case "invalid": return { headline: "This workspace change was refused", detail: "The name or layout is not valid. Your saved workspace was not changed.", code: "workspace_invalid" };
      case "unavailable": return { headline: "Workspaces are unavailable", detail: "The durable workspace store could not be read safely. No fallback workspace was opened.", code: "workspace_unavailable" };
    }
  }
  return { headline: `This workspace could not be ${action}`, detail: "The durable workspace request did not complete. No saved workspace was changed.", code: "workspace_unavailable" };
}
