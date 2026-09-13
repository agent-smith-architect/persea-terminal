import type { ComposerSegment } from "./composer";

// Pure attachment-domain logic for the composer's image staging: the
// attachment model, clipboard triage, serialization inputs, and the closed
// operator copy. No DOM and no transport live here, so every rule the design
// packet names is unit-testable under plain node.

export type ComposerStagedImage = Readonly<{
  id: string;
  path: string;
  bytes: number;
  mediaType: string;
  expiresAt: string;
}>;

// The closed refusal vocabulary the upload client maps HTTP status onto. The
// server's message body is never echoed into the page; these kinds are the
// entire surface the UI renders from.
export type ImageStagingFailureKind =
  | "too_large"
  | "unsupported_type"
  | "rate_limited"
  | "capacity"
  | "unavailable"
  | "network";

export class ImageStagingError extends Error {
  constructor(readonly kind: ImageStagingFailureKind) {
    super(kind);
    this.name = "ImageStagingError";
  }
}

export type ComposerAttachmentStatus = "uploading" | "staged" | "failed";

export type ComposerAttachment = {
  readonly localId: string;
  // Held for Retry; released with the attachment. A clipboard item whose
  // File was not obtainable still renders a visible failed chip without one.
  readonly file: File | undefined;
  // Display only, derived in the browser, never transmitted.
  readonly label: string;
  readonly bytes: number;
  status: ComposerAttachmentStatus;
  path?: string;
  serverId?: string;
  expiresAt?: string;
  error?: string;
  abort?: AbortController;
};

// The four types the receiving CLIs detect, enumerated explicitly and never
// image/*: the explicit list is the iOS lever that makes Safari transcode
// camera-roll HEIC to JPEG on the way out of the picker.
export const ALLOWED_IMAGE_TYPES: readonly string[] = Object.freeze([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
]);
export const IMAGE_ACCEPT_ATTRIBUTE = ALLOWED_IMAGE_TYPES.join(",");

// A UX bound, not a security bound (the server owns those): a paste of more
// paths than this is not a usable prompt.
export const MAX_COMPOSER_ATTACHMENTS = 8;
export const ATTACHMENT_LIMIT_REASON = "8 images is the limit for one insertion";

// A pasted image outside the allow-list must be a visible refusal, never a
// silent drop.
export const PASTED_IMAGE_REFUSAL_COPY =
  "HEIC images are not supported — use Attach, which converts them";

export function imageStagingFailureCopy(kind: ImageStagingFailureKind): string {
  switch (kind) {
    case "too_large": return "Image is larger than the 10 MB limit";
    case "unsupported_type": return "That file is not a PNG, JPEG, GIF or WebP";
    case "rate_limited": return "Too many uploads at once — try again";
    case "capacity": return "The server's image staging space is full";
    case "unavailable": return "Image staging is unavailable";
    case "network": return "Upload failed — Retry";
  }
}

// segments derivation: only STAGED attachments become artifact segments, so a
// failed or uploading chip structurally cannot contribute to a paste.
export function stagedArtifactSegments(attachments: readonly ComposerAttachment[]): ComposerSegment[] {
  return attachments
    .filter((attachment) => attachment.status === "staged" && typeof attachment.path === "string")
    .map((attachment) => Object.freeze({
      kind: "artifact" as const,
      id: attachment.localId,
      label: attachment.label,
      resolve: () => attachment.path ?? "",
    }));
}

export function uploadingCount(attachments: readonly ComposerAttachment[]): number {
  return attachments.filter((attachment) => attachment.status === "uploading").length;
}

export function stagedCount(attachments: readonly ComposerAttachment[]): number {
  return attachments.filter((attachment) => attachment.status === "staged").length;
}

export function failedCount(attachments: readonly ComposerAttachment[]): number {
  return attachments.filter((attachment) => attachment.status === "failed").length;
}

// Insert is disabled while any upload is in flight, with a persistent reason
// exposed before the press, never after.
export function attachmentUploadBlockReason(attachments: readonly ComposerAttachment[]): string | undefined {
  const uploading = uploadingCount(attachments);
  if (uploading === 0) return undefined;
  return `Waiting for ${uploading.toLocaleString()} image upload${uploading === 1 ? "" : "s"}`;
}

// The aggregate failure line that joins the footer status chain when any chip
// failed; the chip itself carries the per-image reason.
export function attachmentFailureStatus(attachments: readonly ComposerAttachment[]): string {
  const failed = failedCount(attachments);
  if (failed === 0) return "";
  return `${failed.toLocaleString()} of ${attachments.length.toLocaleString()} image${attachments.length === 1 ? "" : "s"} failed to upload`;
}

// Display-only label from File.name, truncated to 32 graphemes with a middle
// ellipsis; a clipboard file with no name reads "pasted image". Never
// transmitted anywhere.
export function attachmentDisplayLabel(name: string | undefined): string {
  const trimmed = (name ?? "").trim();
  if (trimmed === "") return "pasted image";
  const graphemes = splitGraphemes(trimmed);
  if (graphemes.length <= 32) return trimmed;
  return `${graphemes.slice(0, 15).join("")}…${graphemes.slice(-16).join("")}`;
}

function splitGraphemes(value: string): string[] {
  if (typeof Intl !== "undefined" && "Segmenter" in Intl) {
    return Array.from(new Intl.Segmenter().segment(value), (segment) => segment.segment);
  }
  return Array.from(value);
}

export function attachmentSizeLabel(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${Math.max(1, Math.ceil(bytes / 1024)).toLocaleString()} KB`;
}

export type ClipboardImageTriage = Readonly<{
  // Whether at least one image file item was present, which is the ONLY
  // condition under which the paste event is captured: a plain text paste
  // must proceed natively.
  captured: boolean;
  allowed: readonly File[];
  refused: ReadonlyArray<File | undefined>;
}>;

type ClipboardItemLike = Readonly<{ kind: string; type: string; getAsFile(): File | null }>;

// Two-stage filter (design amendment): ANY image file item captures the
// event; items outside the allow-list become immediately-failed chips so the
// operator always sees where their image went. Mixed clipboards keep both
// halves: capture never discards the text/plain part (the caller re-inserts
// it).
export function triageClipboardItems(items: readonly ClipboardItemLike[]): ClipboardImageTriage {
  const allowed: File[] = [];
  const refused: Array<File | undefined> = [];
  for (const item of items) {
    if (item.kind !== "file" || !item.type.startsWith("image/")) continue;
    if (ALLOWED_IMAGE_TYPES.includes(item.type)) {
      const file = item.getAsFile();
      if (file) allowed.push(file);
      else refused.push(undefined);
    } else {
      refused.push(item.getAsFile() ?? undefined);
    }
  }
  return Object.freeze({
    captured: allowed.length > 0 || refused.length > 0,
    allowed: Object.freeze(allowed),
    refused: Object.freeze(refused),
  });
}
