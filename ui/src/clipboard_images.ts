import { csrfToken } from "./csrf_refresh";
import { deviceOrigin, sanitizeSnippetOrigin } from "./snippet_client";
import { isClipboardRetentionSeconds, type ClipboardRetentionSeconds } from "./clipboard_retention";

export const CLIPBOARD_IMAGE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"] as const;
export const CLIPBOARD_IMAGE_MAX_BYTES = 10 << 20;
export type ClipboardImage = Readonly<{
  id: string;
  mediaType: string;
  byteSize: number;
  createdAt: string;
  updatedAt: string;
  revision: number;
  retentionSeconds: ClipboardRetentionSeconds;
  expiresAt: string | null;
  origin: string;
}>;
export type ClipboardImagesSnapshot = Readonly<{
  status: "idle" | "loading" | "ready" | "unavailable";
  items: readonly ClipboardImage[];
}>;

export class ClipboardImageError extends Error {
  constructor(readonly status: number) { super("Clipboard image request failed"); }
}

export function clipboardImageErrorMessage(error: unknown): string {
  const status = error instanceof ClipboardImageError ? error.status : 0;
  if (status === 404) return "This image has expired or was deleted. Choose another item.";
  if (status === 409 || status === 412) return "This image changed on another device. The list has been refreshed; try again.";
  if (status === 413) return "This image is too large. The maximum is 10 MB; this server may have a lower limit.";
  if (status === 415 || status === 400) return "Choose a valid PNG, JPEG, GIF or WebP image.";
  if (status === 507) return "The image clipboard is full. Delete an item or wait for it to expire.";
  if (status === 401 || status === 403) return "Clipboard access was refused. Reload the page and try again.";
  if (status === 503) return "Shared images are unavailable on this server.";
  return "The image could not be transferred. Check your connection and try again.";
}

function parseImage(value: unknown): ClipboardImage {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid clipboard image");
  const wire = value as Record<string, unknown>;
  const updatedAt = wire.updated_at === undefined ? wire.created_at : wire.updated_at;
  const revision = wire.revision === undefined ? 1 : wire.revision;
  const retentionSeconds = wire.retention_seconds === undefined ? (wire.expires_at === null ? 0 : 1800) : wire.retention_seconds;
  if (typeof wire.id !== "string" || !/^[0-9a-f]{32}$/.test(wire.id)
    || typeof wire.media_type !== "string" || !CLIPBOARD_IMAGE_TYPES.some((type) => type === wire.media_type)
    || typeof wire.byte_size !== "number" || !Number.isSafeInteger(wire.byte_size) || wire.byte_size < 1 || wire.byte_size > CLIPBOARD_IMAGE_MAX_BYTES
    || typeof wire.created_at !== "string" || !Number.isFinite(Date.parse(wire.created_at))
    || typeof updatedAt !== "string" || !Number.isFinite(Date.parse(updatedAt)) || Date.parse(updatedAt) < Date.parse(wire.created_at)
    || typeof revision !== "number" || !Number.isSafeInteger(revision) || revision < 1
    || !isClipboardRetentionSeconds(retentionSeconds) || (retentionSeconds === 0) !== (wire.expires_at === null)
    || (wire.expires_at !== null && (typeof wire.expires_at !== "string" || !Number.isFinite(Date.parse(wire.expires_at)) || Date.parse(wire.expires_at) <= Date.parse(wire.created_at)))
    || typeof wire.origin !== "string" || wire.origin !== sanitizeSnippetOrigin(wire.origin)) throw new Error("Invalid clipboard image");
  return Object.freeze({ id: wire.id, mediaType: wire.media_type, byteSize: wire.byte_size, createdAt: wire.created_at, updatedAt, revision, retentionSeconds, expiresAt: wire.expires_at, origin: wire.origin });
}

export function clipboardImageURL(image: ClipboardImage): string {
  if (!/^[0-9a-f]{32}$/.test(image.id)) throw new Error("Invalid clipboard image id");
  return `/api/clipboard/images/${image.id}`;
}

export function clipboardImageFilename(image: ClipboardImage): string {
  const extension = image.mediaType === "image/jpeg" ? "jpg" : image.mediaType.split("/")[1];
  return `clipboard-${image.id.slice(0,8)}.${extension}`;
}

type Options = Readonly<{
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
  csrf?: () => string;
  origin?: string;
  pollMs?: number;
}>;

/** Binary clipboard content has its own bounded store. One service per document
 * shares the live read across workspace panes; closing the last view stops it. */
export class ClipboardImages {
  private current: ClipboardImagesSnapshot = Object.freeze({ status: "idle", items: Object.freeze([]) });
  private readonly listeners = new Set<(snapshot: ClipboardImagesSnapshot) => void>();
  private live = 0;
  private timer?: ReturnType<typeof setTimeout>;
  private inFlight?: Promise<void>;
  private disposed = false;
  private generation = 0;
  private readonly fetcher: NonNullable<Options["fetch"]>;
  private readonly csrf: NonNullable<Options["csrf"]>;
  private readonly origin: string;
  private readonly foreground = (): void => {
    if (document.visibilityState === "visible" && this.live > 0) void this.refresh();
    else this.cancelTimer();
  };
  constructor(private readonly options: Options = {}) {
    this.fetcher = options.fetch ?? ((input, init) => fetch(input, init));
    this.csrf = options.csrf ?? csrfToken;
    this.origin = sanitizeSnippetOrigin(options.origin ?? (typeof window === "undefined" ? "" : deviceOrigin(window)));
    if (typeof document !== "undefined") document.addEventListener("visibilitychange", this.foreground);
  }
  snapshot(): ClipboardImagesSnapshot { return this.current; }
  subscribe(listener: (snapshot: ClipboardImagesSnapshot) => void): () => void {
    this.listeners.add(listener); listener(this.current); return () => this.listeners.delete(listener);
  }
  retainLive(): () => void {
    this.live++;
    if (this.live === 1) void this.refresh();
    let released = false;
    return () => { if (released) return; released = true; this.live--; if (!this.live) this.cancelTimer(); };
  }
  private publish(snapshot: ClipboardImagesSnapshot): void {
    if (this.disposed) return;
    this.current = Object.freeze(snapshot); this.listeners.forEach((listener) => listener(this.current));
  }
  private cancelTimer(): void { if (this.timer !== undefined) clearTimeout(this.timer); this.timer = undefined; }
  private schedule(): void {
    this.cancelTimer();
    if (this.disposed || !this.live || (typeof document !== "undefined" && document.visibilityState !== "visible")) return;
    this.timer = setTimeout(() => { this.timer = undefined; void this.refresh(); }, this.options.pollMs ?? 4_000);
  }
  async refresh(afterMutation = false): Promise<void> {
    if (this.disposed) return;
    if (this.inFlight) {
      await this.inFlight;
      if (!afterMutation || this.disposed) return;
      return this.refresh();
    }
    this.cancelTimer();
    if (this.current.status === "idle") this.publish({ ...this.current, status: "loading" });
    this.inFlight = this.readList();
    try { await this.inFlight; } finally { this.inFlight = undefined; this.schedule(); }
  }
  private async readList(): Promise<void> {
    const generation = this.generation;
    try {
      const response = await this.request("/api/clipboard/images", "GET");
      const value: unknown = await response.json();
      if (!value || typeof value !== "object" || !Array.isArray((value as Record<string, unknown>).items)) throw new Error("Invalid clipboard images");
      const items = ((value as Record<string, unknown>).items as unknown[]).map(parseImage);
      if (items.length > 20 || new Set(items.map((item) => item.id)).size !== items.length) throw new Error("Invalid clipboard images");
      if (generation === this.generation) this.publish({ status: "ready", items: Object.freeze(items) });
    } catch { if (generation === this.generation) this.publish({ status: "unavailable", items: this.current.items }); }
  }
  async add(file: Blob, seconds?: ClipboardRetentionSeconds): Promise<void> {
    if (seconds !== undefined && !isClipboardRetentionSeconds(seconds)) throw new ClipboardImageError(400);
    if (!CLIPBOARD_IMAGE_TYPES.some((type) => type === file.type)) throw new ClipboardImageError(415);
    if (file.size > CLIPBOARD_IMAGE_MAX_BYTES) throw new ClipboardImageError(413);
    const response = await this.request("/api/clipboard/images", "POST", file, seconds === undefined ? {} : { "X-Persea-Clipboard-Retention": String(seconds) });
    parseImage(await response.json());
    this.generation++;
    await this.refresh(true);
  }
  async setRetention(image: ClipboardImage, seconds: ClipboardRetentionSeconds): Promise<void> {
    if (!isClipboardRetentionSeconds(seconds)) throw new ClipboardImageError(400);
    try {
      const response = await this.request(clipboardImageURL(image), "PATCH", JSON.stringify({ retention_seconds: seconds, revision: image.revision }));
      parseImage(await response.json());
    } catch (error) {
      if (error instanceof ClipboardImageError && (error.status === 409 || error.status === 412)) {
        this.generation++; await this.refresh(true);
      }
      throw error;
    }
    this.generation++; await this.refresh(true);
  }
  async remove(image: ClipboardImage): Promise<void> {
    await this.request(clipboardImageURL(image), "DELETE", "{}");
    this.generation++;
    await this.refresh(true);
  }
  async file(image: ClipboardImage): Promise<File> {
    if (image.expiresAt !== null && Date.parse(image.expiresAt) <= Date.now()) throw new ClipboardImageError(404);
    const response = await this.request(clipboardImageURL(image), "GET");
    const blob = await response.blob();
    if (blob.type !== image.mediaType || blob.size !== image.byteSize) throw new ClipboardImageError(415);
    return new File([blob], clipboardImageFilename(image), { type: image.mediaType });
  }
  private async request(path: string, method: string, body?: Blob | string, extraHeaders: Record<string, string> = {}): Promise<Response> {
    const headers: Record<string,string> = { ...extraHeaders };
    if (body !== undefined) {
      headers["X-Persea-CSRF"] = this.csrf();
      headers["Content-Type"] = typeof body === "string" ? "application/json" : body.type;
      if (method === "POST" && this.origin) headers["X-Persea-Clipboard-Origin"] = this.origin;
    }
    const response = await this.fetcher(path, { method, headers, cache: "no-store", credentials: "same-origin", ...(body === undefined ? {} : { body }) });
    if (!response.ok) throw new ClipboardImageError(response.status);
    return response;
  }
  dispose(): void {
    this.disposed = true; this.cancelTimer(); this.listeners.clear();
    if (typeof document !== "undefined") document.removeEventListener("visibilitychange", this.foreground);
  }
}

const documentServices = new WeakMap<Document, ClipboardImages>();
export function documentClipboardImages(): ClipboardImages {
  let service = documentServices.get(document);
  if (!service) {
    service = new ClipboardImages(); documentServices.set(document, service);
    window.addEventListener("pagehide", (event) => { if (!event.persisted) service?.dispose(); });
  }
  return service;
}
