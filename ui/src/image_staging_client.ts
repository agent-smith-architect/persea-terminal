import { ImageStagingError, type ComposerStagedImage, type ImageStagingFailureKind } from "./composer_attachments";
import { csrfToken, refreshCSRFToken } from "./csrf_refresh";

// Browser client for POST /api/session-images. Same code shape as the
// diagnostic autosave client: raw body (no multipart, no filename field — the
// client filename never leaves the browser), current CSRF token with exactly
// one refresh-and-retry on 403, and full validation of status, headers, and
// parsed shape before the returned path is trusted.

export type ImageStagingResponse = Readonly<{
  status: number;
  headers: Readonly<{ get(name: string): string | null }>;
  json(): Promise<unknown>;
}>;

export type ImageStagingFetch = (input: string, init: RequestInit) => Promise<ImageStagingResponse>;

export type CSRFSource = Readonly<{
  token(): string;
  refresh(signal: AbortSignal): Promise<string>;
}>;

const defaultCSRF: CSRFSource = Object.freeze({
  token: () => csrfToken(),
  refresh: (signal: AbortSignal) => refreshCSRFToken(signal),
});

// The 201 path is validated against the staging-path shape the server chain
// guarantees: staging root + realm label + server-chosen basename. The root
// itself is host-level configuration (deployment manifest front.staging_root,
// default /var/lib/persea-terminal-staging), so the browser checks the
// structural shape — absolute, bounded, safe path charset, every segment
// starting alphanumeric (which excludes "." and "..") — while the front door
// remains the authority that confines the path to the exact configured root.
// Anything else is treated as a malformed response, never inserted.
const STAGED_IMAGE_PATH = /^(?:\/[A-Za-z0-9][A-Za-z0-9_.-]{0,63})+\/img-[0-9a-f]{32}\.(png|jpg|gif|webp)$/;
const STAGED_IMAGE_PATH_MAX = 512;
const STAGED_IMAGE_ID = /^[0-9a-f]{32}$/;
const STAGED_MEDIA_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);

function refusalKind(status: number): ImageStagingFailureKind {
  switch (status) {
    case 413: return "too_large";
    case 415: return "unsupported_type";
    case 429: return "rate_limited";
    case 507: return "capacity";
    default: return "unavailable";
  }
}

export function parseStagedImageResponse(value: unknown): ComposerStagedImage {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new ImageStagingError("network");
  }
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record).sort().join(",");
  if (keys !== "bytes,expires_at,id,media_type,path"
    || typeof record.id !== "string" || !STAGED_IMAGE_ID.test(record.id)
    || typeof record.path !== "string" || record.path.length > STAGED_IMAGE_PATH_MAX || !STAGED_IMAGE_PATH.test(record.path)
    || typeof record.bytes !== "number" || !Number.isSafeInteger(record.bytes) || record.bytes <= 0
    || typeof record.media_type !== "string" || !STAGED_MEDIA_TYPES.has(record.media_type)
    || typeof record.expires_at !== "string" || record.expires_at === "") {
    throw new ImageStagingError("network");
  }
  return Object.freeze({
    id: record.id,
    path: record.path,
    bytes: record.bytes,
    mediaType: record.media_type,
    expiresAt: record.expires_at,
  });
}

export async function stageSessionImage(
  realm: string,
  file: File,
  signal: AbortSignal,
  fetcher: ImageStagingFetch = (input, init) => window.fetch(input, init),
  csrf: CSRFSource = defaultCSRF,
): Promise<ComposerStagedImage> {
  const post = (token: string): Promise<ImageStagingResponse> => fetcher(
    `/api/session-images?realm=${encodeURIComponent(realm)}`,
    {
      method: "POST",
      cache: "no-store",
      credentials: "same-origin",
      signal,
      headers: { "Content-Type": file.type, "X-Persea-CSRF": token },
      // The raw bytes and nothing else: no multipart wrapper, no filename.
      body: file,
    },
  );
  let response: ImageStagingResponse;
  try {
    response = await post(csrf.token());
    if (response.status === 403) {
      response = await post(await csrf.refresh(signal));
    }
  } catch (error) {
    // A removal-driven abort propagates untranslated so the caller can tell
    // it apart from a failure worth a chip.
    if (signal.aborted) throw error;
    throw new ImageStagingError("network");
  }
  if (response.status !== 201) throw new ImageStagingError(refusalKind(response.status));
  if (response.headers.get("Cache-Control") !== "no-store"
    || response.headers.get("Content-Type")?.split(";", 1)[0]?.trim() !== "application/json") {
    throw new ImageStagingError("network");
  }
  let parsed: unknown;
  try {
    parsed = await response.json();
  } catch {
    throw new ImageStagingError("network");
  }
  return parseStagedImageResponse(parsed);
}
