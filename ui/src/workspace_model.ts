// Workspace layout model — the split tree, pure and DOM-free.
//
// A workspace arrangement is a recursive split tree whose leaves name tmux
// sessions by selector (realm, server, name). Weights size CELLS only: they
// are CSS track proportions and never reach tmux.
// Nothing in this module knows about attachments, transports, or geometry —
// there is no RESIZE_REQUEST path here by construction, and the unit suite
// pins that with a source scan.
//
// Validation fails closed with typed refusals, mirroring the alias store's
// load discipline: a malformed or over-cap tree is refused
// as a whole, never partially accepted.

export const WORKSPACE_STORE_VERSION = 1 as const;
export const PANE_CAP = 6;
export const SPLIT_MIN_CHILDREN = 2;
export const SPLIT_MAX_CHILDREN = 4;
export const SPLIT_MAX_DEPTH = 3;
export const WEIGHT_MIN = 1;
export const WEIGHT_MAX = 100;
// Drag and nudge operations rescale a split's weights to this total first so a
// 1-unit step is 1 % of the split; the integer 1..100 range then holds by
// construction (each child keeps at least one unit).
export const WEIGHT_SCALE = 100;
export const DIVIDER_NUDGE_STEP = 5;
// Same trim / length / control-character refusal as aliases
// (internal/frontdoor/alias_store.go normalizeAlias) and fragment labels
// (ui/src/app.ts labelFromFragment).
export const SESSION_LABEL_MAX_LENGTH = 128;

export type SplitDirection = "row" | "column";
export type OnMissing = "offer" | "create" | "skip";
export const ON_MISSING_DEFAULT: OnMissing = "offer";
export const ON_MISSING_VALUES: readonly OnMissing[] = Object.freeze(["offer", "create", "skip"]);

export type SessionSelector = Readonly<{ realm: string; server: string; name: string }>;
export type WorkspaceLeaf = Readonly<{ kind: "leaf"; session: SessionSelector; onMissing: OnMissing; aliasHint?: string }>;
export type WorkspaceSplit = Readonly<{ kind: "split"; direction: SplitDirection; weights: readonly number[]; children: readonly WorkspaceNode[] }>;
export type WorkspaceNode = WorkspaceLeaf | WorkspaceSplit;
export type NodePath = readonly number[];
export type LeafEntry = Readonly<{ path: NodePath; leaf: WorkspaceLeaf; key: string }>;

export type WorkspaceRefusal =
  | Readonly<{ code: "malformed"; path: string; detail: string }>
  | Readonly<{ code: "invalid_session"; path: string; field: "realm" | "server" | "name" | "alias_hint"; detail: string }>
  | Readonly<{ code: "split_arity"; path: string; count: number }>
  | Readonly<{ code: "weights_mismatch"; path: string }>
  | Readonly<{ code: "weight_out_of_range"; path: string; weight: number }>
  | Readonly<{ code: "depth_exceeded"; path: string; depth: number }>
  | Readonly<{ code: "pane_cap_exceeded"; count: number; cap: number }>
  | Readonly<{ code: "duplicate_leaf"; path: string; session: SessionSelector }>
  | Readonly<{ code: "path_not_found"; path: string }>
  | Readonly<{ code: "wrong_node_kind"; path: string; expected: "leaf" | "split" }>
  | Readonly<{ code: "last_pane" }>
  | Readonly<{ code: "divider_out_of_range"; path: string; divider: number }>
  | Readonly<{ code: "unsupported_version"; version: unknown }>;

export type WorkspaceResult<T> = Readonly<{ ok: true; value: T }> | Readonly<{ ok: false; refusal: WorkspaceRefusal }>;

function ok<T>(value: T): WorkspaceResult<T> { return Object.freeze({ ok: true, value }); }
function refuse<T>(refusal: WorkspaceRefusal): WorkspaceResult<T> { return Object.freeze({ ok: false, refusal: Object.freeze(refusal) }); }

export function pathLabel(path: NodePath): string { return path.length === 0 ? "root" : `root/${path.join("/")}`; }
export function sessionKey(session: SessionSelector): string { return JSON.stringify([session.realm, session.server, session.name]); }
export function sameSession(a: SessionSelector, b: SessionSelector): boolean { return a.realm === b.realm && a.server === b.server && a.name === b.name; }

function unicodeScalars(value: string): readonly number[] | undefined {
  const scalars: number[] = [];
  for (const character of value) {
    const scalar = character.codePointAt(0);
    if (scalar === undefined || (scalar >= 0xd800 && scalar <= 0xdfff)) return undefined;
    scalars.push(scalar);
  }
  return scalars;
}

function unicodeWhitespace(scalar: number): boolean {
  return (scalar >= 0x0009 && scalar <= 0x000d)
    || scalar === 0x0020 || scalar === 0x0085 || scalar === 0x00a0 || scalar === 0x1680
    || (scalar >= 0x2000 && scalar <= 0x200a) || scalar === 0x2028 || scalar === 0x2029
    || scalar === 0x202f || scalar === 0x205f || scalar === 0x3000;
}

function unicodeControl(scalar: number): boolean {
  return scalar <= 0x001f || (scalar >= 0x007f && scalar <= 0x009f);
}

// Refuses (never rewrites) a label that is not a session identity: untrimmed,
// empty, over-long, or carrying control characters. A silently rewritten name
// would resolve to a session the operator never named.
export function sessionLabelProblem(value: unknown): string | undefined {
  if (typeof value !== "string") return "must be a string";
  if (value.length === 0) return "must be nonempty";
  const scalars = unicodeScalars(value);
  if (scalars === undefined) return "must contain only Unicode scalar values";
  if (unicodeWhitespace(scalars[0]!) || unicodeWhitespace(scalars[scalars.length - 1]!)) return "must not carry leading or trailing whitespace";
  if (scalars.length > SESSION_LABEL_MAX_LENGTH) return `must be at most ${SESSION_LABEL_MAX_LENGTH} characters`;
  if (scalars.some(unicodeControl)) return "must not contain control characters";
  return undefined;
}

export function leaf(session: SessionSelector, onMissing: OnMissing = ON_MISSING_DEFAULT, aliasHint?: string): WorkspaceLeaf {
  return Object.freeze({ kind: "leaf", session: Object.freeze({ ...session }), onMissing, ...(aliasHint !== undefined ? { aliasHint } : {}) });
}
export function split(direction: SplitDirection, children: readonly WorkspaceNode[], weights?: readonly number[]): WorkspaceSplit {
  return Object.freeze({ kind: "split", direction, weights: Object.freeze([...(weights ?? children.map(() => 1))]), children: Object.freeze([...children]) });
}

export function nodeAt(root: WorkspaceNode, path: NodePath): WorkspaceNode | undefined {
  let node: WorkspaceNode | undefined = root;
  for (const index of path) {
    if (node === undefined || node.kind !== "split") return undefined;
    node = node.children[index];
  }
  return node;
}

function replaceAt(root: WorkspaceNode, path: NodePath, replacement: WorkspaceNode): WorkspaceNode {
  if (path.length === 0) return replacement;
  if (root.kind !== "split") throw new Error(`replaceAt: ${pathLabel(path)} is not reachable`);
  const [index, ...rest] = path;
  const child = root.children[index];
  if (child === undefined) throw new Error(`replaceAt: ${pathLabel(path)} is not reachable`);
  const children = root.children.map((item, i) => (i === index ? replaceAt(item, rest, replacement) : item));
  return split(root.direction, children, root.weights);
}

export function leafEntries(root: WorkspaceNode): readonly LeafEntry[] {
  const entries: LeafEntry[] = [];
  const walk = (node: WorkspaceNode, path: NodePath): void => {
    if (node.kind === "leaf") { entries.push(Object.freeze({ path: Object.freeze([...path]), leaf: node, key: sessionKey(node.session) })); return; }
    node.children.forEach((child, index) => walk(child, [...path, index]));
  };
  walk(root, []);
  return Object.freeze(entries);
}
export function leafCount(root: WorkspaceNode): number { return leafEntries(root).length; }

// Fail-closed structural validation of a typed tree. The typed shape does not
// guarantee runtime integrity (weights can be fractional, children can exceed
// the arity), so every tree invariant is checked here.
export function validateWorkspaceTree(root: WorkspaceNode): WorkspaceResult<readonly LeafEntry[]> {
  const seen = new Map<string, string>();
  let leaves = 0;
  const walk = (node: WorkspaceNode, path: NodePath, depth: number): WorkspaceRefusal | undefined => {
    const label = pathLabel(path);
    if (node === null || typeof node !== "object") return { code: "malformed", path: label, detail: "node must be an object" };
    if (node.kind === "leaf") {
      const session = node.session;
      if (session === null || typeof session !== "object") return { code: "malformed", path: label, detail: "leaf session must be an object" };
      for (const field of ["realm", "server", "name"] as const) {
        const problem = sessionLabelProblem(session[field]);
        if (problem) return { code: "invalid_session", path: label, field, detail: `${field} ${problem}` };
      }
      if (!ON_MISSING_VALUES.includes(node.onMissing)) return { code: "malformed", path: label, detail: "on_missing must be offer, create, or skip" };
      if (node.aliasHint !== undefined) {
        const problem = sessionLabelProblem(node.aliasHint);
        if (problem) return { code: "invalid_session", path: label, field: "alias_hint", detail: `alias_hint ${problem}` };
      }
      leaves += 1;
      if (leaves > PANE_CAP) return { code: "pane_cap_exceeded", count: leaves, cap: PANE_CAP };
      const key = sessionKey(session);
      const prior = seen.get(key);
      if (prior !== undefined) return { code: "duplicate_leaf", path: label, session: Object.freeze({ ...session }) };
      seen.set(key, label);
      return undefined;
    }
    if (node.kind !== "split") return { code: "malformed", path: label, detail: "kind must be split or leaf" };
    if (depth > SPLIT_MAX_DEPTH) return { code: "depth_exceeded", path: label, depth };
    if (node.direction !== "row" && node.direction !== "column") return { code: "malformed", path: label, detail: "direction must be row or column" };
    if (!Array.isArray(node.children) || !Array.isArray(node.weights)) return { code: "malformed", path: label, detail: "children and weights must be arrays" };
    if (node.children.length < SPLIT_MIN_CHILDREN || node.children.length > SPLIT_MAX_CHILDREN) return { code: "split_arity", path: label, count: node.children.length };
    if (node.weights.length !== node.children.length) return { code: "weights_mismatch", path: label };
    for (const weight of node.weights) {
      if (!Number.isInteger(weight) || weight < WEIGHT_MIN || weight > WEIGHT_MAX) return { code: "weight_out_of_range", path: label, weight };
    }
    for (let index = 0; index < node.children.length; index += 1) {
      const problem = walk(node.children[index], [...path, index], depth + 1);
      if (problem) return problem;
    }
    return undefined;
  };
  const problem = walk(root, [], 1);
  if (problem) return refuse(problem);
  if (leaves === 0) return refuse({ code: "malformed", path: "root", detail: "a workspace needs at least one pane" });
  return ok(leafEntries(root));
}

function validated(root: WorkspaceNode): WorkspaceResult<WorkspaceNode> {
  const verdict = validateWorkspaceTree(root);
  return verdict.ok ? ok(root) : refuse(verdict.refusal);
}

function parentOf(root: WorkspaceNode, path: NodePath): WorkspaceSplit | undefined {
  if (path.length === 0) return undefined;
  const parent = nodeAt(root, path.slice(0, -1));
  return parent?.kind === "split" ? parent : undefined;
}

function averageWeight(weights: readonly number[]): number {
  const sum = weights.reduce((total, weight) => total + weight, 0);
  return Math.max(WEIGHT_MIN, Math.min(WEIGHT_MAX, Math.round(sum / weights.length)));
}

// Splits the leaf at `path`, placing `addition` beside it. When the parent
// already runs in the requested direction and has room, the addition joins as
// a sibling (keeping depth flat, tmux-style); otherwise the leaf becomes a
// two-child split. Refused when the result breaks the cap, depth, or the
// duplicate-leaf rule.
export function splitLeaf(root: WorkspaceNode, path: NodePath, direction: SplitDirection, addition: WorkspaceLeaf): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "leaf") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "leaf" });
  const parent = parentOf(root, path);
  if (parent && parent.direction === direction && parent.children.length < SPLIT_MAX_CHILDREN) {
    const index = path[path.length - 1];
    const children = [...parent.children]; const weights = [...parent.weights];
    children.splice(index + 1, 0, addition); weights.splice(index + 1, 0, parent.weights[index]);
    return validated(replaceAt(root, path.slice(0, -1), split(direction, children, weights)));
  }
  return validated(replaceAt(root, path, split(direction, [target, addition], [1, 1])));
}

// Removes the leaf at `path`; a parent left with one child collapses into it.
// The last pane cannot be removed — a workspace with no panes is not a
// workspace (validation refuses it too).
export function removeLeaf(root: WorkspaceNode, path: NodePath): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "leaf") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "leaf" });
  if (path.length === 0) return refuse({ code: "last_pane" });
  const parent = parentOf(root, path);
  if (parent === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  const index = path[path.length - 1];
  const children = parent.children.filter((_child, i) => i !== index);
  const weights = parent.weights.filter((_weight, i) => i !== index);
  const replacement = children.length === 1 ? children[0] : split(parent.direction, children, weights);
  return validated(replaceAt(root, path.slice(0, -1), replacement));
}

// Collapses the split at `path` into one of its children; the other children
// are dropped. Explicit keep index, so nothing is discarded by accident.
export function unsplit(root: WorkspaceNode, path: NodePath, keepIndex: number): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "split") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "split" });
  const kept = target.children[keepIndex];
  if (kept === undefined) return refuse({ code: "path_not_found", path: pathLabel([...path, keepIndex]) });
  return validated(replaceAt(root, path, kept));
}

// Adds a pane at the root level: joins the root split when it runs in the
// requested direction and has room, otherwise wraps the whole tree.
export function addLeaf(root: WorkspaceNode, addition: WorkspaceLeaf, direction: SplitDirection): WorkspaceResult<WorkspaceNode> {
  if (root.kind === "split" && root.direction === direction && root.children.length < SPLIT_MAX_CHILDREN) {
    return validated(split(direction, [...root.children, addition], [...root.weights, averageWeight(root.weights)]));
  }
  return validated(split(direction, [root, addition], [1, 1]));
}

// Display-only quick rename. The session selector is copied byte-for-byte;
// the draft cannot turn an alias into tmux/session authority.
export function setLeafAliasHint(root: WorkspaceNode, path: NodePath, aliasHint: string | undefined): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "leaf") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "leaf" });
  const replacement = leaf(target.session, target.onMissing, aliasHint);
  return validated(replaceAt(root, path, replacement));
}

export function setWeights(root: WorkspaceNode, path: NodePath, weights: readonly number[]): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "split") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "split" });
  return validated(replaceAt(root, path, split(target.direction, target.children, weights)));
}

// Largest-remainder rescale to `total` with every entry at least WEIGHT_MIN.
// Invariants: same length, sum === total, order of magnitude preserved.
export function normalizeWeights(weights: readonly number[], total: number = WEIGHT_SCALE): readonly number[] {
  const count = weights.length;
  if (count === 0) return Object.freeze([]);
  const sum = weights.reduce((acc, weight) => acc + weight, 0);
  // Idempotent on an already-normalized split: re-apportioning a split whose
  // child sits at WEIGHT_MIN would otherwise drift a unit between children on
  // every drag or nudge, moving weight that the operator never touched.
  if (sum === total && weights.every((weight) => Number.isInteger(weight) && weight >= WEIGHT_MIN)) return Object.freeze([...weights]);
  const available = total - count * WEIGHT_MIN;
  const exact = weights.map((weight) => (sum > 0 ? (weight / sum) * available : available / count));
  const floors = exact.map((value) => Math.floor(value));
  let remainder = available - floors.reduce((acc, value) => acc + value, 0);
  const order = exact.map((value, index) => ({ index, fraction: value - floors[index] })).sort((a, b) => b.fraction - a.fraction || a.index - b.index);
  for (const { index } of order) { if (remainder <= 0) break; floors[index] += 1; remainder -= 1; }
  return Object.freeze(floors.map((value) => value + WEIGHT_MIN));
}

function pairAdjust(root: WorkspaceNode, path: NodePath, divider: number, leftFor: (pairTotal: number, currentLeft: number) => number): WorkspaceResult<WorkspaceNode> {
  const target = nodeAt(root, path);
  if (target === undefined) return refuse({ code: "path_not_found", path: pathLabel(path) });
  if (target.kind !== "split") return refuse({ code: "wrong_node_kind", path: pathLabel(path), expected: "split" });
  if (!Number.isInteger(divider) || divider < 0 || divider >= target.children.length - 1) return refuse({ code: "divider_out_of_range", path: pathLabel(path), divider });
  const normalized = [...normalizeWeights(target.weights)];
  const pairTotal = normalized[divider] + normalized[divider + 1];
  const left = Math.max(WEIGHT_MIN, Math.min(pairTotal - WEIGHT_MIN, leftFor(pairTotal, normalized[divider])));
  normalized[divider] = left; normalized[divider + 1] = pairTotal - left;
  return validated(replaceAt(root, path, split(target.direction, target.children, normalized)));
}

// Divider drag: `fraction` is the pointer position across the two adjacent
// cells (0 = fully left/top, 1 = fully right/bottom). Only the pair changes;
// every other child of the split keeps its normalized share.
export function dragDivider(root: WorkspaceNode, path: NodePath, divider: number, fraction: number): WorkspaceResult<WorkspaceNode> {
  const bounded = Number.isFinite(fraction) ? Math.max(0, Math.min(1, fraction)) : 0.5;
  return pairAdjust(root, path, divider, (pairTotal) => Math.round(bounded * pairTotal));
}

// +/- fallback: moves `delta` units from the right/bottom cell to the
// left/top cell (negative delta moves the other way).
export function nudgeDivider(root: WorkspaceNode, path: NodePath, divider: number, delta: number = DIVIDER_NUDGE_STEP): WorkspaceResult<WorkspaceNode> {
  const step = Number.isFinite(delta) ? Math.trunc(delta) : 0;
  return pairAdjust(root, path, divider, (_pairTotal, currentLeft) => currentLeft + step);
}

// --- Serialization: the version-1 tree shape (snake_case wire
// keys; strict — unknown fields and unknown kinds fail closed, mirroring the
// alias store's DisallowUnknownFields walker).

export type SerializedLeaf = Readonly<{ kind: "leaf"; session: SessionSelector; on_missing: OnMissing; alias_hint?: string }>;
export type SerializedSplit = Readonly<{ kind: "split"; direction: SplitDirection; weights: readonly number[]; children: readonly SerializedNode[] }>;
export type SerializedNode = SerializedLeaf | SerializedSplit;
export type SerializedWorkspace = Readonly<{ version: typeof WORKSPACE_STORE_VERSION; root: SerializedNode }>;

export function serializeWorkspaceTree(node: WorkspaceNode): SerializedNode {
  if (node.kind === "leaf") {
    return Object.freeze({ kind: "leaf", session: Object.freeze({ realm: node.session.realm, server: node.session.server, name: node.session.name }), on_missing: node.onMissing, ...(node.aliasHint !== undefined ? { alias_hint: node.aliasHint } : {}) });
  }
  return Object.freeze({ kind: "split", direction: node.direction, weights: Object.freeze([...node.weights]), children: Object.freeze(node.children.map(serializeWorkspaceTree)) });
}
export function serializeWorkspace(root: WorkspaceNode): SerializedWorkspace {
  return Object.freeze({ version: WORKSPACE_STORE_VERSION, root: serializeWorkspaceTree(root) });
}

function objectKeys(value: unknown, path: string, allowed: readonly string[]): WorkspaceResult<Record<string, unknown>> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return refuse({ code: "malformed", path, detail: "must be an object" });
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) return refuse({ code: "malformed", path, detail: `unknown field ${JSON.stringify(key)}` });
  }
  return ok(value as Record<string, unknown>);
}

function parseNode(value: unknown, path: NodePath): WorkspaceResult<WorkspaceNode> {
  const label = pathLabel(path);
  if (value === null || typeof value !== "object" || Array.isArray(value)) return refuse({ code: "malformed", path: label, detail: "node must be an object" });
  const kind = (value as Record<string, unknown>).kind;
  if (kind === "leaf") {
    const record = objectKeys(value, label, ["kind", "session", "on_missing", "alias_hint"]);
    if (!record.ok) return record;
    const session = objectKeys(record.value.session, `${label}.session`, ["realm", "server", "name"]);
    if (!session.ok) return session;
    for (const field of ["realm", "server", "name"] as const) {
      const problem = sessionLabelProblem(session.value[field]);
      if (problem) return refuse({ code: "invalid_session", path: label, field, detail: `${field} ${problem}` });
    }
    const onMissing = record.value.on_missing === undefined ? ON_MISSING_DEFAULT : record.value.on_missing;
    if (!ON_MISSING_VALUES.includes(onMissing as OnMissing)) return refuse({ code: "malformed", path: label, detail: "on_missing must be offer, create, or skip" });
    const aliasHint = record.value.alias_hint;
    if (aliasHint !== undefined) {
      const problem = sessionLabelProblem(aliasHint);
      if (problem) return refuse({ code: "invalid_session", path: label, field: "alias_hint", detail: `alias_hint ${problem}` });
    }
    return ok(leaf({ realm: session.value.realm as string, server: session.value.server as string, name: session.value.name as string }, onMissing as OnMissing, aliasHint as string | undefined));
  }
  if (kind === "split") {
    const record = objectKeys(value, label, ["kind", "direction", "weights", "children"]);
    if (!record.ok) return record;
    const direction = record.value.direction;
    if (direction !== "row" && direction !== "column") return refuse({ code: "malformed", path: label, detail: "direction must be row or column" });
    const weights = record.value.weights; const children = record.value.children;
    if (!Array.isArray(weights) || !Array.isArray(children)) return refuse({ code: "malformed", path: label, detail: "children and weights must be arrays" });
    // Typed refusal before recursing: a hostile deeply nested document must
    // not reach the call-stack limit and surface as a crash.
    if (path.length + 1 > SPLIT_MAX_DEPTH) return refuse({ code: "depth_exceeded", path: label, depth: path.length + 1 });
    const parsedChildren: WorkspaceNode[] = [];
    for (let index = 0; index < children.length; index += 1) {
      const child = parseNode(children[index], [...path, index]);
      if (!child.ok) return child;
      parsedChildren.push(child.value);
    }
    for (const weight of weights) {
      if (typeof weight !== "number") return refuse({ code: "malformed", path: label, detail: "weights must be numbers" });
    }
    return ok(split(direction, parsedChildren, weights as number[]));
  }
  return refuse({ code: "malformed", path: label, detail: "kind must be split or leaf" });
}

export function parseWorkspaceTree(value: unknown): WorkspaceResult<WorkspaceNode> {
  const parsed = parseNode(value, []);
  return parsed.ok ? validated(parsed.value) : parsed;
}

// Envelope parser for the future store record's `root` under a file-level
// version. Unknown versions fail closed; the tree schema is versioned by
// the file version, so a v1 reader never guesses at unknown fields.
export function parseWorkspace(value: unknown): WorkspaceResult<WorkspaceNode> {
  const record = objectKeys(value, "workspace", ["version", "root"]);
  if (!record.ok) return record;
  if (record.value.version !== WORKSPACE_STORE_VERSION) return refuse({ code: "unsupported_version", version: record.value.version });
  return parseWorkspaceTree(record.value.root);
}

// Operator wording for a refusal, closed-set (every code has reviewed copy);
// the code stays visible alongside so a report remains diagnosable.
export function workspaceRefusalMessage(refusal: WorkspaceRefusal): string {
  switch (refusal.code) {
    case "malformed": return `This workspace record is malformed (${refusal.path}: ${refusal.detail}).`;
    case "invalid_session": return `A pane names an invalid session (${refusal.path}: ${refusal.detail}).`;
    case "split_arity": return `A split at ${refusal.path} has ${refusal.count} panes; splits hold ${SPLIT_MIN_CHILDREN} to ${SPLIT_MAX_CHILDREN}.`;
    case "weights_mismatch": return `A split at ${refusal.path} has a weight count that does not match its panes.`;
    case "weight_out_of_range": return `A split at ${refusal.path} carries weight ${refusal.weight}; weights are integers ${WEIGHT_MIN} to ${WEIGHT_MAX}.`;
    case "depth_exceeded": return `Splits nest ${refusal.depth} deep at ${refusal.path}; the limit is ${SPLIT_MAX_DEPTH}.`;
    case "pane_cap_exceeded": return `This workspace has more than ${refusal.cap} panes.`;
    case "duplicate_leaf": return `The session ${refusal.session.name} appears twice; a workspace opens each session once.`;
    case "path_not_found": return `No pane exists at ${refusal.path}.`;
    case "wrong_node_kind": return `The node at ${refusal.path} is not a ${refusal.expected}.`;
    case "last_pane": return "The last pane cannot be removed.";
    case "divider_out_of_range": return `No divider ${refusal.divider} exists at ${refusal.path}.`;
    case "unsupported_version": return `This workspace record uses an unsupported version (${String(refusal.version)}).`;
  }
}
