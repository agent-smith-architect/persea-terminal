// M9 W1a — workspace model unit suite: cap, duplicates (OQ1), structural
// refusals, split/unsplit/remove/add, reweight invariants, serialization
// round-trip, and the "no geometry path" source scan (FW3 static half).
declare const require: (name: string) => any;
declare const process: { stdout: { write(value: string): void } };
declare const __dirname: string;
const fs = require("node:fs");
const path = require("node:path");
import {
  DIVIDER_NUDGE_STEP, PANE_CAP, SPLIT_MAX_CHILDREN, WEIGHT_MAX, WEIGHT_MIN, WEIGHT_SCALE, WORKSPACE_STORE_VERSION,
  addLeaf, dragDivider, leaf, leafCount, leafEntries, nodeAt, normalizeWeights, nudgeDivider, parseWorkspace, parseWorkspaceTree,
  removeLeaf, serializeWorkspace, serializeWorkspaceTree, sessionLabelProblem, setLeafAliasHint, setWeights, split, splitLeaf, unsplit, validateWorkspaceTree,
  workspaceRefusalMessage,
  type SessionSelector, type WorkspaceNode, type WorkspaceRefusal, type WorkspaceResult,
} from "../src/workspace_model";
import { WorkspaceAPIError, parseWorkspaceList, workspaceAPIErrorCode, workspaceAPIMessage } from "../src/workspace_api";
import { boundedWorkspaceName } from "../src/workspace_url";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  deepEqual(actual: unknown, expected: unknown, message = "values differ"): void { if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
  throws(fn: () => unknown, message = "expected function to throw"): void { try { fn(); } catch { return; } throw new Error(message); },
};

function sel(name: string, realm = "main", server = "default"): SessionSelector { return { realm, server, name }; }
function expectOk<T>(result: WorkspaceResult<T>, message: string): T { if (!result.ok) throw new Error(`${message}: refused ${JSON.stringify(result.refusal)}`); return result.value; }
function expectRefusal<T>(result: WorkspaceResult<T>, code: WorkspaceRefusal["code"], message: string): WorkspaceRefusal {
  if (result.ok) throw new Error(`${message}: accepted ${JSON.stringify(result.value)}`);
  assert.equal(result.refusal.code, code, `${message}: refusal code`);
  return result.refusal;
}

type WorkspaceNameLawCase = Readonly<{
  id: string;
  value?: string;
  repeat?: string;
  count?: number;
  utf8_hex?: string;
  utf16_units?: readonly number[];
  valid: boolean;
  normalized_name?: string;
}>;
type WorkspaceNameLawFixture = Readonly<{
  version: number;
  maximum_scalars: number;
  control_ranges: readonly Readonly<{ first: number; last: number }>[];
  cases: readonly WorkspaceNameLawCase[];
}>;

const workspaceNameLaw = JSON.parse(fs.readFileSync(path.resolve(__dirname, "..", "..", "..", "internal", "frontdoor", "testdata", "workspace_name_law.json"), "utf8")) as WorkspaceNameLawFixture;

function javascriptFixtureValue(fixture: WorkspaceNameLawCase): string | undefined {
  if (fixture.value !== undefined) return fixture.value;
  if (fixture.repeat !== undefined && fixture.count !== undefined) return fixture.repeat.repeat(fixture.count);
  if (fixture.utf16_units !== undefined) return String.fromCharCode(...fixture.utf16_units);
  return undefined;
}

// The packet's §2a example, extended to the six-pane cap: row [A | column [B, C] | column [D, row [E, F]]].
const six: WorkspaceNode = split("row", [
  leaf(sel("qt20"), "offer", "meta-advisor"),
  split("column", [leaf(sel("build"), "create"), leaf(sel("scratch"), "skip")]),
  split("column", [leaf(sel("logs")), split("row", [leaf(sel("e")), leaf(sel("f"))])]),
], [2, 1, 1]);

// --- one cross-runtime workspace-name law
{
  assert.equal(workspaceNameLaw.version, 1, "name-law fixture version");
  assert.equal(workspaceNameLaw.maximum_scalars, 128, "name-law scalar ceiling");
  for (const fixture of workspaceNameLaw.cases) {
    const value = javascriptFixtureValue(fixture);
    if (value === undefined) continue;
    assert.equal(sessionLabelProblem(value) === undefined, fixture.valid, `${fixture.id}: model validity`);
    assert.equal(boundedWorkspaceName(value) === value, fixture.valid, `${fixture.id}: URL validity`);
  }
  for (const range of workspaceNameLaw.control_ranges) {
    for (let scalar = range.first; scalar <= range.last; scalar += 1) {
      assert.ok(sessionLabelProblem(`x${String.fromCodePoint(scalar)}y`) !== undefined, `U+${scalar.toString(16).padStart(4, "0")}: control refused`);
    }
  }
  const parsed = parseWorkspaceList({
    version: 1,
    items: [{
      workspace_id: "00000000000000000000000000000001",
      name: "İ",
      normalized_name: "i",
      revision: 1,
      created_at: "2026-08-28T00:00:00Z",
      updated_at: "2026-08-28T00:00:00Z",
      tree: serializeWorkspaceTree(leaf(sel("one"))),
    }],
  });
  assert.equal(parsed.items[0]?.normalizedName, "i", "authority-supplied normalized name is accepted");
}

// --- closed workspace API failure classes and recoverable copy
{
  assert.equal(workspaceAPIErrorCode(507), "capacity", "507 is capacity, not invalid input");
  for (const status of [400, 413, 428]) assert.equal(workspaceAPIErrorCode(status), "invalid", `${status} remains invalid input`);
  const copy = workspaceAPIMessage(new WorkspaceAPIError("capacity", 507), "saved");
  assert.equal(copy.code, "workspace_capacity", "capacity copy code");
  assert.ok(/Delete an unused workspace/.test(copy.detail) && /draft/.test(copy.detail), "capacity copy is honest and recoverable");
  assert.ok(workspaceAPIMessage(new WorkspaceAPIError("invalid", 400)).code !== copy.code, "invalid and capacity copy stay distinct");
}

// --- validation
{
  const entries = expectOk(validateWorkspaceTree(six), "six-pane tree validates");
  assert.equal(entries.length, 6);
  assert.deepEqual(entries.map((entry) => entry.leaf.session.name), ["qt20", "build", "scratch", "logs", "e", "f"], "tree order");
  assert.deepEqual(entries.map((entry) => entry.path), [[0], [1, 0], [1, 1], [2, 0], [2, 1, 0], [2, 1, 1]], "leaf paths");
  assert.equal(leafCount(six), 6);
  assert.equal(nodeAt(six, [2, 1, 1])?.kind, "leaf");
  assert.equal(nodeAt(six, [9]), undefined);
  assert.equal(nodeAt(six, [0, 0]), undefined, "a leaf has no children");

  // PANE_CAP is six (OQ2): a seventh leaf is refused as a whole.
  const seven = split("row", [six.kind === "split" ? six.children[0] : six, ...(six.kind === "split" ? six.children.slice(1) : []), leaf(sel("g"))], [2, 1, 1, 1]);
  const cap = expectRefusal(validateWorkspaceTree(seven), "pane_cap_exceeded", "seventh pane");
  assert.equal(cap.code === "pane_cap_exceeded" && cap.cap, PANE_CAP);

  // OQ1: the same session twice is refused, with the offending path.
  const duplicate = split("row", [leaf(sel("qt20")), split("column", [leaf(sel("x")), leaf(sel("qt20"))])]);
  const dup = expectRefusal(validateWorkspaceTree(duplicate), "duplicate_leaf", "duplicate leaf");
  assert.equal(dup.code === "duplicate_leaf" && dup.path, "root/1/1");
  // Identity is the full selector: same name on another server is distinct.
  expectOk(validateWorkspaceTree(split("row", [leaf(sel("qt20")), leaf(sel("qt20", "main", "other"))])), "same name on another server is not a duplicate");

  // Structural refusals.
  expectRefusal(validateWorkspaceTree(split("row", [leaf(sel("a"))], [1])), "split_arity", "one child");
  expectRefusal(validateWorkspaceTree(split("row", [leaf(sel("a")), leaf(sel("b")), leaf(sel("c")), leaf(sel("d")), leaf(sel("e"))])), "split_arity", "five children");
  expectRefusal(validateWorkspaceTree(split("row", [leaf(sel("a")), leaf(sel("b"))], [1])), "weights_mismatch", "weights count");
  for (const bad of [0, 101, 1.5, Number.NaN, -3]) expectRefusal(validateWorkspaceTree(split("row", [leaf(sel("a")), leaf(sel("b"))], [bad, 1])), "weight_out_of_range", `weight ${bad}`);
  const deep = split("row", [leaf(sel("a")), split("column", [leaf(sel("b")), split("row", [leaf(sel("c")), split("column", [leaf(sel("d")), leaf(sel("e"))])])])]);
  const depth = expectRefusal(validateWorkspaceTree(deep), "depth_exceeded", "depth four");
  assert.equal(depth.code === "depth_exceeded" && depth.depth, 4);
  expectOk(validateWorkspaceTree(split("row", [leaf(sel("a")), split("column", [leaf(sel("b")), split("row", [leaf(sel("c")), leaf(sel("d"))])])])), "depth three is allowed");
  expectRefusal(validateWorkspaceTree({ kind: "split", direction: "diagonal" as "row", weights: [1, 1], children: [leaf(sel("a")), leaf(sel("b"))] }), "malformed", "direction");
  expectRefusal(validateWorkspaceTree({ kind: "leaf", session: sel("a"), onMissing: "guess" as "offer" }), "malformed", "on_missing");
  expectRefusal(validateWorkspaceTree({ kind: "other" as "leaf", session: sel("a"), onMissing: "offer" }), "malformed", "kind");

  // Session labels: refused, never rewritten.
  for (const [value, field] of [["", "name"], [" qt20", "name"], ["qt20 ", "name"], ["a\u0000b", "name"], ["x".repeat(129), "name"], ["", "realm"], ["\tmain", "realm"], ["", "server"]] as const) {
    const bad = field === "name" ? sel(value) : field === "realm" ? sel("n", value) : sel("n", "main", value);
    const refusal = expectRefusal(validateWorkspaceTree(leaf(bad)), "invalid_session", `label ${JSON.stringify(value)} in ${field}`);
    assert.equal(refusal.code === "invalid_session" && refusal.field, field);
  }
  expectOk(validateWorkspaceTree(leaf(sel("x".repeat(128)))), "128 characters is the bound");
  assert.equal(sessionLabelProblem("ok name"), undefined);
  expectRefusal(validateWorkspaceTree(leaf(sel("a"), "offer", " hint")), "invalid_session", "alias hint follows the same rule");
  expectOk(validateWorkspaceTree(leaf(sel("only"))), "a single leaf is a workspace");
}

// --- split / remove / unsplit / add
{
  // A full six-pane workspace refuses a seventh pane through every editing entry point.
  expectRefusal(splitLeaf(six, [0], "row", leaf(sel("g"))), "pane_cap_exceeded", "sibling split at the cap");
  expectRefusal(splitLeaf(six, [0], "column", leaf(sel("g"))), "pane_cap_exceeded", "nested split at the cap");
}
{
  const five: WorkspaceNode = split("row", [leaf(sel("a")), split("column", [leaf(sel("b")), leaf(sel("c"))])], [3, 1]);
  const sibling = expectOk(splitLeaf(five, [0], "row", leaf(sel("d"))), "sibling split");
  assert.ok(sibling.kind === "split" && sibling.children.length === 3, "joined the parent row");
  assert.deepEqual(sibling.kind === "split" ? sibling.weights : [], [3, 3, 1], "new sibling copies the target's weight");
  assert.equal(sibling.kind === "split" && sibling.children[1].kind === "leaf" && sibling.children[1].session.name, "d");
  // A different direction nests a two-child split with equal weights.
  const nested = expectOk(splitLeaf(five, [0], "column", leaf(sel("d"))), "nested split");
  const node = nodeAt(nested, [0]);
  assert.ok(node?.kind === "split" && node.direction === "column" && node.children.length === 2, "nested column split");
  assert.deepEqual(node?.kind === "split" ? node.weights : [], [1, 1]);
  assert.deepEqual(leafEntries(nested).map((entry) => entry.leaf.session.name), ["a", "d", "b", "c"], "tree order after nesting");
  // Refusals.
  expectRefusal(splitLeaf(five, [0], "row", leaf(sel("b"))), "duplicate_leaf", "OQ1 at split time");
  expectRefusal(splitLeaf(five, [7], "row", leaf(sel("z"))), "path_not_found", "unknown path");
  expectRefusal(splitLeaf(five, [1], "row", leaf(sel("z"))), "wrong_node_kind", "split at a split path");
  const full = split("row", [leaf(sel("a")), leaf(sel("b")), leaf(sel("c")), leaf(sel("d"))]);
  const wrapped = expectOk(splitLeaf(full, [3], "row", leaf(sel("e"))), "arity-full parent nests instead");
  assert.ok(wrapped.kind === "split" && wrapped.children.length === SPLIT_MAX_CHILDREN && wrapped.children[3].kind === "split", "fourth child became a split");
  expectRefusal(splitLeaf(six, [2, 1, 1], "column", leaf(sel("g"))), "depth_exceeded", "depth holds through splitLeaf (a fourth nesting level)");
  const threeDeep = split("row", [leaf(sel("a")), split("column", [leaf(sel("b")), split("row", [leaf(sel("c")), leaf(sel("d"))])])]);
  expectRefusal(splitLeaf(threeDeep, [1, 1, 1], "column", leaf(sel("e"))), "depth_exceeded", "nesting under a depth-three split is refused");
  expectOk(splitLeaf(threeDeep, [1, 1, 1], "row", leaf(sel("e"))), "a sibling under a depth-three split is fine");

  // Remove collapses a two-child parent into the survivor; the last pane is refused.
  const removed = expectOk(removeLeaf(five, [1, 0]), "remove b");
  assert.ok(nodeAt(removed, [1])?.kind === "leaf", "column collapsed into c");
  assert.deepEqual(leafEntries(removed).map((entry) => entry.leaf.session.name), ["a", "c"]);
  const removedRow = expectOk(removeLeaf(five, [0]), "remove a");
  assert.ok(removedRow.kind === "split" && removedRow.direction === "column", "root collapsed into the column split");
  expectRefusal(removeLeaf(leaf(sel("solo")), []), "last_pane", "last pane");
  expectRefusal(removeLeaf(five, [1]), "wrong_node_kind", "remove a split");
  const three = split("row", [leaf(sel("a")), leaf(sel("b")), leaf(sel("c"))], [10, 20, 30]);
  const dropMiddle = expectOk(removeLeaf(three, [1]), "remove middle");
  assert.deepEqual(dropMiddle.kind === "split" ? dropMiddle.weights : [], [10, 30], "weights follow their children");

  // Unsplit keeps the named child only.
  const kept = expectOk(unsplit(five, [1], 1), "unsplit keeping c");
  assert.deepEqual(leafEntries(kept).map((entry) => entry.leaf.session.name), ["a", "c"]);
  expectRefusal(unsplit(five, [0], 0), "wrong_node_kind", "unsplit a leaf");
  expectRefusal(unsplit(five, [1], 5), "path_not_found", "keep index out of range");

  // Add joins the root when it runs in that direction, else wraps.
  const joined = expectOk(addLeaf(five, leaf(sel("z")), "row"), "add joins root row");
  assert.ok(joined.kind === "split" && joined.children.length === 3, "joined");
  assert.deepEqual(joined.kind === "split" ? joined.weights : [], [3, 1, 2], "new pane gets the average share");
  const wrappedRoot = expectOk(addLeaf(five, leaf(sel("z")), "column"), "add wraps in a column");
  assert.ok(wrappedRoot.kind === "split" && wrappedRoot.direction === "column" && wrappedRoot.children.length === 2, "wrapped");
  expectOk(addLeaf(leaf(sel("solo")), leaf(sel("z")), "row"), "add to a single leaf");
  expectRefusal(addLeaf(six, leaf(sel("g")), "row"), "pane_cap_exceeded", "cap holds through addLeaf");
  expectRefusal(addLeaf(five, leaf(sel("a")), "row"), "duplicate_leaf", "OQ1 through addLeaf");

  // W2 quick rename is display-only: selector and missing policy cannot move.
  const aliased = expectOk(setLeafAliasHint(five, [0], "primary"), "set display alias");
  const aliasedLeaf = nodeAt(aliased, [0]);
  assert.deepEqual(aliasedLeaf?.kind === "leaf" ? aliasedLeaf.session : undefined, sel("a"), "alias preserves selector authority");
  assert.equal(aliasedLeaf?.kind === "leaf" ? aliasedLeaf.aliasHint : undefined, "primary");
  const cleared = expectOk(setLeafAliasHint(aliased, [0], undefined), "clear display alias");
  assert.equal(nodeAt(cleared, [0])?.kind === "leaf" && (nodeAt(cleared, [0]) as { aliasHint?: string }).aliasHint, undefined);
  expectRefusal(setLeafAliasHint(five, [1], "bad"), "wrong_node_kind", "alias requires a leaf");
  expectRefusal(setLeafAliasHint(five, [0], ""), "invalid_session", "empty alias is refused");

  // setWeights validates the range.
  expectOk(setWeights(five, [], [50, 50]), "explicit weights");
  expectRefusal(setWeights(five, [], [50]), "weights_mismatch", "count");
  expectRefusal(setWeights(five, [], [0, 100]), "weight_out_of_range", "range");
  expectRefusal(setWeights(five, [0], [1, 1]), "wrong_node_kind", "weights on a leaf");
}

// --- reweight invariants (drag + nudge)
{
  assert.deepEqual(normalizeWeights([1, 1]), [50, 50]);
  assert.deepEqual(normalizeWeights([2, 1]), [66, 34], "largest remainder: 65.33/32.67 of the 98 free units rounds the larger fraction up");
  assert.deepEqual(normalizeWeights([100, 100, 100, 100]), [25, 25, 25, 25]);
  assert.deepEqual(normalizeWeights([1, 100]), [2, 98], "every child keeps at least one unit");
  assert.deepEqual(normalizeWeights([]), []);
  for (const weights of [[1, 1], [2, 1], [7, 3, 90], [1, 100], [100, 1, 1, 1], [33, 33, 34], [49, 1, 50], [98, 1, 1]]) {
    const normalized = normalizeWeights(weights);
    assert.deepEqual(normalizeWeights(normalized), normalized, `idempotent for ${JSON.stringify(weights)}`);
    assert.equal(normalized.length, weights.length, "length");
    assert.equal(normalized.reduce((a, b) => a + b, 0), WEIGHT_SCALE, `sum for ${JSON.stringify(weights)}`);
    assert.ok(normalized.every((weight) => Number.isInteger(weight) && weight >= WEIGHT_MIN && weight <= WEIGHT_MAX), "range");
    for (let i = 0; i + 1 < weights.length; i += 1) if (weights[i] >= weights[i + 1]) assert.ok(normalized[i] >= normalized[i + 1], "order preserved");
  }

  const three = split("row", [leaf(sel("a")), leaf(sel("b")), leaf(sel("c"))], [1, 1, 2]);
  const base = normalizeWeights([1, 1, 2]);
  let previousLeft = 0;
  for (const fraction of [0, 0.1, 0.25, 0.5, 0.75, 0.9, 1, 2, -1, Number.NaN]) {
    const dragged = expectOk(dragDivider(three, [], 0, fraction), `drag ${fraction}`);
    const weights = dragged.kind === "split" ? dragged.weights : [];
    assert.equal(weights.length, 3, "count unchanged");
    assert.equal(weights.reduce((a, b) => a + b, 0), WEIGHT_SCALE, "sum preserved");
    assert.ok(weights.every((weight) => Number.isInteger(weight) && weight >= WEIGHT_MIN && weight <= WEIGHT_MAX), "range");
    assert.equal(weights[2], base[2], "the non-pair child keeps its share");
    assert.equal(weights[0] + weights[1], base[0] + base[1], "pair total preserved");
    if (Number.isFinite(fraction) && fraction >= 0 && fraction <= 1) { assert.ok(weights[0] >= previousLeft, "monotonic in the fraction"); previousLeft = weights[0]; }
  }
  const atZero = expectOk(dragDivider(three, [], 0, 0), "drag to zero");
  assert.equal(atZero.kind === "split" && atZero.weights[0], WEIGHT_MIN, "left never collapses below one unit");
  const atOne = expectOk(dragDivider(three, [], 0, 1), "drag to one");
  assert.equal(atOne.kind === "split" && atOne.weights[1], WEIGHT_MIN, "right never collapses below one unit");
  expectRefusal(dragDivider(three, [], 2, 0.5), "divider_out_of_range", "no divider after the last child");
  expectRefusal(dragDivider(three, [], -1, 0.5), "divider_out_of_range", "negative divider");
  expectRefusal(dragDivider(three, [0], 0, 0.5), "wrong_node_kind", "drag on a leaf");

  const nudged = expectOk(nudgeDivider(three, [], 1), "nudge +step");
  assert.deepEqual(nudged.kind === "split" ? nudged.weights : [], [base[0], base[1] + DIVIDER_NUDGE_STEP, base[2] - DIVIDER_NUDGE_STEP]);
  const back = expectOk(nudgeDivider(nudged, [], 1, -DIVIDER_NUDGE_STEP), "nudge back");
  assert.deepEqual(back.kind === "split" ? back.weights : [], base, "+step then -step is the identity");
  let pinned: WorkspaceNode = three;
  for (let i = 0; i < 60; i += 1) pinned = expectOk(nudgeDivider(pinned, [], 0, 7), `nudge ${i}`);
  assert.equal(pinned.kind === "split" && pinned.weights[1], WEIGHT_MIN, "nudging saturates at one unit for the neighbour");
  assert.equal(pinned.kind === "split" && pinned.weights[0], base[0] + base[1] - WEIGHT_MIN);
  // Nested split drags leave the rest of the tree untouched.
  const nestedDrag = expectOk(dragDivider(six, [1], 0, 0.2), "nested drag");
  assert.deepEqual(nestedDrag.kind === "split" ? nestedDrag.weights : [], [2, 1, 1], "root weights untouched");
  assert.deepEqual(serializeWorkspaceTree(nodeAt(nestedDrag, [2])!), serializeWorkspaceTree(nodeAt(six, [2])!), "sibling subtree untouched");
}

// --- serialization (§3a version-1 shape)
{
  const wire = serializeWorkspace(six);
  assert.equal(wire.version, WORKSPACE_STORE_VERSION);
  const expectedRoot = {
    kind: "split", direction: "row", weights: [2, 1, 1], children: [
      { kind: "leaf", session: { realm: "main", server: "default", name: "qt20" }, on_missing: "offer", alias_hint: "meta-advisor" },
      { kind: "split", direction: "column", weights: [1, 1], children: [
        { kind: "leaf", session: { realm: "main", server: "default", name: "build" }, on_missing: "create" },
        { kind: "leaf", session: { realm: "main", server: "default", name: "scratch" }, on_missing: "skip" },
      ] },
      { kind: "split", direction: "column", weights: [1, 1], children: [
        { kind: "leaf", session: { realm: "main", server: "default", name: "logs" }, on_missing: "offer" },
        { kind: "split", direction: "row", weights: [1, 1], children: [
          { kind: "leaf", session: { realm: "main", server: "default", name: "e" }, on_missing: "offer" },
          { kind: "leaf", session: { realm: "main", server: "default", name: "f" }, on_missing: "offer" },
        ] },
      ] },
    ],
  };
  assert.deepEqual(wire.root, expectedRoot, "wire shape is the packet's snake_case tree");
  const parsed = expectOk(parseWorkspace(JSON.parse(JSON.stringify(wire))), "round trip parses");
  assert.deepEqual(parsed, six, "round trip is lossless");
  assert.deepEqual(serializeWorkspace(parsed), wire, "serialize ∘ parse is the identity on the wire");
  const noDefault = expectOk(parseWorkspaceTree({ kind: "leaf", session: sel("x") }), "on_missing defaults to offer (OQ5)");
  assert.equal(noDefault.kind === "leaf" && noDefault.onMissing, "offer");

  // Fail closed: unknown version, unknown fields, unknown kinds, malformed shapes.
  expectRefusal(parseWorkspace({ version: 2, root: wire.root }), "unsupported_version", "version 2");
  expectRefusal(parseWorkspace({ version: 1, root: wire.root, extra: true }), "malformed", "unknown envelope field");
  expectRefusal(parseWorkspaceTree({ ...expectedRoot.children[0], handle: "h" }), "malformed", "unknown leaf field (never a handle)");
  expectRefusal(parseWorkspaceTree({ kind: "leaf", session: { ...sel("x"), session_id: "$1" }, on_missing: "offer" }), "malformed", "unknown session field");
  expectRefusal(parseWorkspaceTree({ kind: "tab", children: [] }), "malformed", "unknown kind");
  expectRefusal(parseWorkspaceTree({ kind: "split", direction: "row", weights: [1, "1"], children: [expectedRoot.children[0], { kind: "leaf", session: sel("y") }] }), "malformed", "string weight");
  expectRefusal(parseWorkspaceTree({ kind: "split", direction: "row", weights: [1, 1], children: [expectedRoot.children[0], { kind: "leaf", session: sel("qt20") }] }), "duplicate_leaf", "OQ1 at load");
  expectRefusal(parseWorkspaceTree({ kind: "split", direction: "row", weights: [1, 1], children: [expectedRoot.children[0], { kind: "leaf", session: sel("y"), on_missing: "maybe" }] }), "malformed", "on_missing value");
  expectRefusal(parseWorkspaceTree(null), "malformed", "null");
  // Hostile nesting is a typed refusal, never a call-stack crash.
  let hostile: unknown = { kind: "leaf", session: sel("deep") };
  for (let level = 0; level < 20_000; level += 1) hostile = { kind: "split", direction: "row", weights: [1, 1], children: [hostile, { kind: "leaf", session: sel(`l${level}`) }] };
  const deep = expectRefusal(parseWorkspaceTree(hostile), "depth_exceeded", "20000-deep document");
  assert.equal(deep.code === "depth_exceeded" && deep.depth, 4, "refused at the first level past the limit");
  expectRefusal(parseWorkspaceTree([]), "malformed", "array");
  expectRefusal(parseWorkspaceTree({ kind: "leaf", session: sel("x"), on_missing: "offer", alias_hint: "" }), "invalid_session", "empty alias hint");
  expectRefusal(parseWorkspace({ version: 1, root: { ...expectedRoot, children: [...expectedRoot.children, { kind: "leaf", session: sel("g") }], weights: [2, 1, 1, 1] } }), "pane_cap_exceeded", "cap at load");
}

// --- every refusal code has reviewed wording
{
  const refusals: WorkspaceRefusal[] = [
    { code: "malformed", path: "root", detail: "d" }, { code: "invalid_session", path: "root", field: "name", detail: "d" }, { code: "split_arity", path: "root", count: 1 },
    { code: "weights_mismatch", path: "root" }, { code: "weight_out_of_range", path: "root", weight: 0 }, { code: "depth_exceeded", path: "root", depth: 4 },
    { code: "pane_cap_exceeded", count: 7, cap: PANE_CAP }, { code: "duplicate_leaf", path: "root/1", session: sel("x") }, { code: "path_not_found", path: "root/9" },
    { code: "wrong_node_kind", path: "root", expected: "leaf" }, { code: "last_pane" }, { code: "divider_out_of_range", path: "root", divider: 3 }, { code: "unsupported_version", version: 2 },
  ];
  for (const refusal of refusals) assert.ok(workspaceRefusalMessage(refusal).length > 0, `wording for ${refusal.code}`);
}

// --- FW3 (static half): no geometry path exists in the workspace code. The
// workspace modules never name a RESIZE_REQUEST, a send port, the terminal
// page's fit request, xterm's resize, a WebSocket, or any transport module.
{
  const src = path.resolve(__dirname, "..", "..", "src");
  const files = ["workspace_model.ts", "workspace_layout.ts", "workspace_url.ts", "workspace.css"];
  const forbidden = ["RESIZE_REQUEST", "trySend", "requestVerticalFit", "Terminal.resize", ".resize(", "WebSocket", "websocket_attachment_transport", "unified_terminal_page", "attachment_page", "attachment_protocol", "attachment_wire", "attachment_reducer", "style.setProperty", "setAttribute(\"style\"", "innerHTML"];
  // Comments may name the invariant; code may not name the path. Strip block
  // and line comments before scanning so the prose does not mask the code.
  const stripComments = (source: string): string => source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
  for (const file of files) {
    const code = stripComments(fs.readFileSync(path.join(src, file), "utf8"));
    for (const token of forbidden) assert.ok(!code.includes(token), `${file} must not contain ${token}`);
  }
  // Class-based styling only: the renderer never writes a style attribute.
  assert.ok(!/\.style\b/.test(stripComments(fs.readFileSync(path.join(src, "workspace_layout.ts"), "utf8"))), "workspace_layout.ts must not touch element.style");
}

process.stdout.write("workspace_model.test: PASS\n");
