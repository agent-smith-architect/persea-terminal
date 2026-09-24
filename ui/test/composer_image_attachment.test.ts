import { normalizeComposedText, serializeComposerSegments, type ComposerSegment } from "../src/composer";
import {
  ATTACHMENT_LIMIT_REASON,
  IMAGE_ACCEPT_ATTRIBUTE,
  ImageStagingError,
  MAX_COMPOSER_ATTACHMENTS,
  attachmentDisplayLabel,
  attachmentFailureStatus,
  attachmentSizeLabel,
  attachmentUploadBlockReason,
  imageStagingFailureCopy,
  stagedArtifactSegments,
  triageClipboardItems,
  type ComposerAttachment,
} from "../src/composer_attachments";
import { parseStagedImageResponse, stageSessionImage, type ImageStagingResponse } from "../src/image_staging_client";

const assert = {
  equal(actual: unknown, expected: unknown, message = "values differ"): void {
    if (actual !== expected) throw new Error(`${message}: ${JSON.stringify(actual)} !== ${JSON.stringify(expected)}`);
  },
  ok(value: unknown, message: string): void {
    if (!value) throw new Error(message);
  },
  throws(fn: () => unknown, message: string): void {
    try {
      fn();
    } catch {
      return;
    }
    throw new Error(message);
  },
};

const STAGED_PATH = `/var/lib/persea-terminal-staging/desk-a7/img-${"a".repeat(32)}.png`;
const artifact = (path: string, id = "art"): ComposerSegment =>
  Object.freeze({ kind: "artifact" as const, id, label: id, resolve: () => path });
const text = (value: string): ComposerSegment => Object.freeze({ kind: "text" as const, value });

// --- serialize(): the image serialization contract -----------------------------------

// Text-only serialization is the normalized text; artifacts-only has no
// leading space; text + artifacts joins each unit with exactly one space.
assert.equal(serializeComposerSegments([text("plain draft")]), "plain draft", "text-only serialization drifted");
assert.equal(serializeComposerSegments([artifact("/a.png"), artifact("/b.png")]), "/a.png /b.png", "artifacts-only grew a leading space");
assert.equal(
  serializeComposerSegments([text("review this"), artifact("/a.png"), artifact("/b.png")]),
  "review this /a.png /b.png",
  "text+artifacts spacing drifted",
);
// The trailing-newline hazard: normalization PRECEDES
// concatenation, so a dictation draft ending in newlines can never bury one
// mid-payload where xterm's \n-to-\r rewrite would submit the prompt.
const hazardous = serializeComposerSegments([text("look at this\n\n"), artifact("/path/a.png")]);
assert.equal(hazardous, "look at this /path/a.png", "trailing newlines were buried mid-payload");
assert.ok(!/[\r\n]/.test(hazardous), "serialized draft with images carries a newline");
// A trailing space on the text joins with exactly one space, never two.
assert.equal(serializeComposerSegments([text("before "), artifact("/staged/path")]), "before /staged/path", "trailing-space text double-spaced the join");
// The output is a fixed point of normalizeComposedText, so send()'s own
// normalization pass is a no-op rather than a second transformation.
for (const segments of [
  [text("a\nb\n\n"), artifact("/a.png")],
  [text(""), artifact("/a.png")],
  [text("plain\n")],
]) {
  const serialized = serializeComposerSegments(segments);
  assert.equal(normalizeComposedText(serialized), serialized, "serialization is not a normalize fixed point");
}

// --- derived segments: only staged attachments contribute -------------------

const attachmentFixture = (status: ComposerAttachment["status"], path?: string): ComposerAttachment => ({
  localId: `local-${status}-${path ?? "none"}`,
  file: undefined,
  label: "screenshot.png",
  bytes: 1024,
  status,
  ...(path === undefined ? {} : { path }),
});
const derived = stagedArtifactSegments([
  attachmentFixture("uploading"),
  attachmentFixture("staged", "/a.png"),
  attachmentFixture("failed"),
  attachmentFixture("staged", "/b.png"),
]);
assert.equal(derived.length, 2, "non-staged attachments leaked into segments");
assert.equal(derived.map((segment) => segment.kind === "artifact" ? segment.resolve() : "?").join(" "), "/a.png /b.png", "staged paths or their order drifted");

// --- Insert gating and aggregate failure copy -------------------------------

assert.equal(attachmentUploadBlockReason([]), undefined, "empty list blocks Insert");
assert.equal(attachmentUploadBlockReason([attachmentFixture("staged", "/a.png")]), undefined, "staged attachment blocks Insert");
assert.equal(attachmentUploadBlockReason([attachmentFixture("uploading")]), "Waiting for 1 image upload", "single-upload reason drifted");
assert.equal(
  attachmentUploadBlockReason([attachmentFixture("uploading"), attachmentFixture("uploading")]),
  "Waiting for 2 image uploads",
  "plural upload reason drifted",
);
assert.equal(attachmentFailureStatus([attachmentFixture("staged", "/a.png")]), "", "failure aggregate appears without failures");
assert.equal(
  attachmentFailureStatus([attachmentFixture("failed"), attachmentFixture("staged", "/a.png"), attachmentFixture("staged", "/b.png")]),
  "1 of 3 images failed to upload",
  "failure aggregate copy drifted",
);
assert.equal(MAX_COMPOSER_ATTACHMENTS, 8, "chip cap drifted");
assert.equal(ATTACHMENT_LIMIT_REASON, "8 images is the limit for one insertion", "cap reason drifted");

// --- labels, sizes, accept list ---------------------------------------------

assert.equal(attachmentDisplayLabel(undefined), "pasted image", "unnamed clipboard file label drifted");
assert.equal(attachmentDisplayLabel("shot.png"), "shot.png", "short label was rewritten");
const longLabel = attachmentDisplayLabel("a-very-long-screenshot-file-name-from-the-camera-roll.png");
assert.ok(longLabel.includes("…") && longLabel.length <= 33, `long label was not middle-truncated: ${longLabel}`);
assert.equal(attachmentSizeLabel(421_337), "412 KB", "KB size label drifted");
assert.equal(attachmentSizeLabel(2 * 1024 * 1024), "2.0 MB", "MB size label drifted");
assert.equal(IMAGE_ACCEPT_ATTRIBUTE, "image/png,image/jpeg,image/gif,image/webp", "accept list drifted (image/* would invite raw HEIC)");

// --- clipboard triage: two-stage filter --------------------------------------

const item = (kind: string, type: string, file: File | null) => ({ kind, type, getAsFile: () => file });
const fakeFile = (type: string, name = "clip.bin"): File => ({ type, name, size: 8 } as unknown as File);
assert.equal(triageClipboardItems([]).captured, false, "empty clipboard was captured");
assert.equal(triageClipboardItems([item("string", "text/plain", null)]).captured, false, "text-only clipboard was captured");
const allowedTriage = triageClipboardItems([item("string", "text/plain", null), item("file", "image/png", fakeFile("image/png"))]);
assert.ok(allowedTriage.captured && allowedTriage.allowed.length === 1 && allowedTriage.refused.length === 0, "mixed clipboard triage drifted");
const heicTriage = triageClipboardItems([item("file", "image/heic", fakeFile("image/heic"))]);
assert.ok(heicTriage.captured && heicTriage.allowed.length === 0 && heicTriage.refused.length === 1, "disallowed image type was silently dropped");

// --- failure copy is a closed set -------------------------------------------

assert.equal(imageStagingFailureCopy("too_large"), "Image is larger than the 10 MB limit", "413 copy drifted");
assert.equal(imageStagingFailureCopy("unsupported_type"), "That file is not a PNG, JPEG, GIF or WebP", "415 copy drifted");
assert.equal(imageStagingFailureCopy("rate_limited"), "Too many uploads at once — try again", "429 copy drifted");
assert.equal(imageStagingFailureCopy("capacity"), "The server's image staging space is full", "507 copy drifted");
assert.equal(imageStagingFailureCopy("unavailable"), "Image staging is unavailable", "503 copy drifted");
assert.equal(imageStagingFailureCopy("network"), "Upload failed — Retry", "network copy drifted");

// --- staged-image response shape --------------------------------------------

const validResponseBody = Object.freeze({
  id: "a".repeat(32),
  path: STAGED_PATH,
  bytes: 421_337,
  media_type: "image/png",
  expires_at: "2026-08-20T18:00:00.000000001Z",
});
const parsed = parseStagedImageResponse({ ...validResponseBody });
assert.equal(parsed.path, STAGED_PATH, "valid response path drifted");
assert.equal(parsed.mediaType, "image/png", "valid response media type drifted");
for (const [key, mutated] of Object.entries({
  id: "not-hex",
  path: "/etc/passwd",
  bytes: -1,
  media_type: "image/heic",
  expires_at: "",
})) {
  assert.throws(() => parseStagedImageResponse({ ...validResponseBody, [key]: mutated }), `parser accepted mutated ${key}`);
}
assert.throws(() => parseStagedImageResponse({ ...validResponseBody, filename: "x.png" }), "parser accepted an extra field");
assert.throws(() => parseStagedImageResponse({ ...validResponseBody, path: "/var/lib/persea-terminal-staging/desk-a7/../escape.png" }), "parser accepted a traversal path");

// The staging root is host-configurable (manifest front.staging_root): a
// non-default root with the same structural shape parses — the front door
// remains the confinement authority — while relative, dot-segment, empty-
// segment, unsafe-charset, and unbounded shapes never do.
const overriddenRootPath = `/srv/persea-staging/desk-a7/img-${"a".repeat(32)}.png`;
assert.equal(
  parseStagedImageResponse({ ...validResponseBody, path: overriddenRootPath }).path,
  overriddenRootPath,
  "an overridden staging root was refused",
);
for (const hostile of [
  `img-${"a".repeat(32)}.png`,
  `/desk-a7/./img-${"a".repeat(32)}.png`,
  `//desk-a7/img-${"a".repeat(32)}.png`,
  `/has space/desk-a7/img-${"a".repeat(32)}.png`,
  `/srv/persea-staging/desk-a7/img-${"a".repeat(32)}.png\n`,
  `/${"a/".repeat(300)}img-${"a".repeat(32)}.png`,
]) {
  assert.throws(() => parseStagedImageResponse({ ...validResponseBody, path: hostile }), `parser accepted hostile path ${JSON.stringify(hostile)}`);
}

// --- upload client contract ---------------------------------------------------

type RecordedRequest = { input: string; init: RequestInit };
function respond(status: number, headers: Record<string, string>, body: unknown): ImageStagingResponse {
  return {
    status,
    headers: { get: (name: string) => headers[name] ?? null },
    json: async () => body,
  };
}
const goodHeaders = { "Cache-Control": "no-store", "Content-Type": "application/json" };
function client(responses: ImageStagingResponse[]): { requests: RecordedRequest[]; refreshes: number; csrf: { token(): string; refresh(signal: AbortSignal): Promise<string> }; fetcher: (input: string, init: RequestInit) => Promise<ImageStagingResponse> } {
  const requests: RecordedRequest[] = [];
  const state = {
    requests,
    refreshes: 0,
    csrf: {
      token: () => "token-original",
      refresh: async () => {
        state.refreshes += 1;
        return "token-refreshed";
      },
    },
    fetcher: async (input: string, init: RequestInit) => {
      requests.push({ input, init });
      const next = responses.shift();
      if (!next) throw new Error("unexpected request");
      return next;
    },
  };
  return state;
}
const uploadFile = fakeFile("image/png", "IMG_4821.png");

async function main(): Promise<void> {
  // Success: raw body is the File itself — no multipart, no filename field —
  // and the realm travels only as the query parameter.
  {
    const c = client([respond(201, goodHeaders, { ...validResponseBody })]);
    const staged = await stageSessionImage("desk-a7", uploadFile, new AbortController().signal, c.fetcher, c.csrf);
    assert.equal(staged.path, STAGED_PATH, "upload did not yield the staged path");
    assert.equal(c.requests.length, 1, "success needed more than one request");
    assert.equal(c.requests[0].input, "/api/session-images?realm=desk-a7", "upload URL drifted");
    assert.ok(c.requests[0].init.body === uploadFile, "request body is not the raw File");
    const headers = c.requests[0].init.headers as Record<string, string>;
    assert.equal(headers["Content-Type"], "image/png", "declared media type drifted");
    assert.equal(headers["X-Persea-CSRF"], "token-original", "CSRF token was not attached");
    assert.equal(Object.keys(headers).sort().join(","), "Content-Type,X-Persea-CSRF", "request grew an extra header (filename surface?)");
    assert.equal(c.refreshes, 0, "success path refreshed CSRF");
  }
  // 403 triggers exactly one refresh-and-retry.
  {
    const c = client([respond(403, {}, null), respond(201, goodHeaders, { ...validResponseBody })]);
    await stageSessionImage("desk-a7", uploadFile, new AbortController().signal, c.fetcher, c.csrf);
    assert.equal(c.refreshes, 1, "403 did not refresh exactly once");
    assert.equal(c.requests.length, 2, "403 did not retry exactly once");
    assert.equal((c.requests[1].init.headers as Record<string, string>)["X-Persea-CSRF"], "token-refreshed", "retry reused the stale token");
  }
  // A second 403 is a refusal, never a loop.
  {
    const c = client([respond(403, {}, null), respond(403, {}, null)]);
    const error = await stageSessionImage("desk-a7", uploadFile, new AbortController().signal, c.fetcher, c.csrf).then(() => undefined, (e: unknown) => e);
    assert.ok(error instanceof ImageStagingError && error.kind === "unavailable", "double 403 did not refuse closed");
    assert.equal(c.refreshes, 1, "double 403 refreshed more than once");
  }
  // Status mapping is the closed set.
  for (const [status, kind] of [[413, "too_large"], [415, "unsupported_type"], [429, "rate_limited"], [507, "capacity"], [503, "unavailable"], [500, "unavailable"]] as const) {
    const c = client([respond(status, {}, null)]);
    const error = await stageSessionImage("desk-a7", uploadFile, new AbortController().signal, c.fetcher, c.csrf).then(() => undefined, (e: unknown) => e);
    assert.ok(error instanceof ImageStagingError && error.kind === kind, `status ${status} did not map to ${kind}`);
  }
  // Response headers and body shape are validated before the path is trusted.
  for (const bad of [
    respond(201, { "Content-Type": "application/json" }, { ...validResponseBody }),
    respond(201, { "Cache-Control": "no-store", "Content-Type": "text/html" }, { ...validResponseBody }),
    respond(201, goodHeaders, { ...validResponseBody, path: "/tmp/evil.png" }),
    respond(201, goodHeaders, "not-an-object"),
  ]) {
    const c = client([bad]);
    const error = await stageSessionImage("desk-a7", uploadFile, new AbortController().signal, c.fetcher, c.csrf).then(() => undefined, (e: unknown) => e);
    assert.ok(error instanceof ImageStagingError && error.kind === "network", "malformed 201 was trusted");
  }
  console.log("composer image attachment tests PASS");
}

main().catch((error: unknown) => {
  // Re-throw outside the promise chain so node exits nonzero.
  setTimeout(() => {
    throw error;
  });
});
