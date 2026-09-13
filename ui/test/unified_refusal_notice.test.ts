import { REFUSAL_NOTICE_MS, TRANSPORT_REFUSAL_PREFIX, UNIFIED_OPERATIONAL_CODES, decodeServerRefusalFrame, refusalReleasesFit, unifiedRefusalNotice } from "../src/unified_refusal_notice";
import { classifyUnifiedClose } from "../src/unified_close_policy";

declare const require: (name: string) => unknown;
const fs = require("node:fs") as { readFileSync(file: string, encoding: "utf8"): string };
const path = require("node:path") as { join(...parts: string[]): string };
const nodeProcess = require("node:process") as { cwd(): string };

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void { if (actual !== expected) throw new Error(`${message}: ${String(actual)} !== ${String(expected)}`); },
  ok(value: unknown, message = "expected truthy value"): void { if (!value) throw new Error(message); },
};

// --- wire decode -----------------------------------------------------------
assert.equal(decodeServerRefusalFrame(`${TRANSPORT_REFUSAL_PREFIX}resize_failed`).type, "REFUSAL");
assert.equal((decodeServerRefusalFrame(`${TRANSPORT_REFUSAL_PREFIX}resize_failed`) as { code: string }).code, "resize_failed");
assert.equal(decodeServerRefusalFrame(`{"type":"LIVE"}`).type, "ORDINARY");
assert.equal(decodeServerRefusalFrame("PERSEA-LIVENESS/1 PONG 0123").type, "ORDINARY");
for (const bad of ["", "Resize_Failed", "a b", "<img src=x>", "x".repeat(65)]) {
  assert.equal(decodeServerRefusalFrame(`${TRANSPORT_REFUSAL_PREFIX}${bad}`).type, "VIOLATION", `unreadable code must be a violation: ${JSON.stringify(bad)}`);
}

// --- notice copy ------------------------------------------------------------
assert.equal(unifiedRefusalNotice("resize_failed"), "Fit didn't apply (resize_failed)");
assert.equal(unifiedRefusalNotice("resize_rejected"), "Fit was refused (resize_rejected)");
assert.equal(unifiedRefusalNotice("input_refused"), "Input was refused — try again (input_refused)");
assert.equal(unifiedRefusalNotice("observe_mode"), "This view is read-only (observe_mode)");
assert.equal(unifiedRefusalNotice("some_future_code"), "Request refused (some_future_code)");
assert.equal(unifiedRefusalNotice("<script>"), "Request refused (refused)");
for (const code of UNIFIED_OPERATIONAL_CODES) assert.ok(unifiedRefusalNotice(code).length > 0, `notice for ${code}`);
assert.ok(REFUSAL_NOTICE_MS >= 2_000 && REFUSAL_NOTICE_MS <= 8_000, "a refusal notice is readable and passing");

// --- the Fit seal opens only for the answer to a Fit --------------------------
assert.ok(refusalReleasesFit("resize_failed") && refusalReleasesFit("resize_rejected"));
assert.ok(!refusalReleasesFit("input_refused") && !refusalReleasesFit("observe_mode"));

// --- belt and braces: every operational code is still classified for the
// close path, so a front door that closes on one cannot dead-end the page.
for (const code of UNIFIED_OPERATIONAL_CODES) {
  const cls = classifyUnifiedClose(code);
  assert.ok(cls === "reattach" || cls === "terminal", `operational code ${code} has no close classification`);
}

// --- source-to-policy: the UI operational set IS the Go authority --------------
{
  const uiRoot = nodeProcess.cwd(); // npm test runs from ui/
  const goProto = fs.readFileSync(path.join(uiRoot, "..", "internal", "proto", "attachment_errors.go"), "utf8");
  const block = goProto.slice(goProto.indexOf("var operationalAttachmentCodes"), goProto.indexOf("var fatalAttachmentCodes"));
  const goCodes = new Set([...block.matchAll(/"([a-z0-9_]+)":\s*\{\}/g)].map((match) => match[1]));
  assert.ok(goCodes.size >= 3, "failed to extract the Go operational set");
  assert.equal([...goCodes].sort().join(","), [...UNIFIED_OPERATIONAL_CODES].sort().join(","), "UI operational set drifted from internal/proto");
  const wirePrefix = fs.readFileSync(path.join(uiRoot, "..", "internal", "attachmentwire", "transport_refusal.go"), "utf8").match(/TransportRefusalPrefix = "([^"]+)"/);
  assert.equal(wirePrefix?.[1], TRANSPORT_REFUSAL_PREFIX, "refusal wire prefix drifted from attachmentwire");
}
