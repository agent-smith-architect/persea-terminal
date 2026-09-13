/** Shared wire policy. Zero means no expiry; a month is exactly thirty days. */
export const RETENTION_SECONDS = [1800, 14400, 86400, 604800, 2592000, 0] as const;
export type ClipboardRetentionSeconds = (typeof RETENTION_SECONDS)[number];

export function isClipboardRetentionSeconds(value: unknown): value is ClipboardRetentionSeconds {
  return typeof value === "number" && RETENTION_SECONDS.some((seconds) => seconds === value);
}

export function clipboardRetentionLabel(seconds: ClipboardRetentionSeconds): string {
  switch (seconds) {
    case 1800: return "30 minutes";
    case 14400: return "4 hours";
    case 86400: return "1 day";
    case 604800: return "1 week";
    case 2592000: return "30 days";
    case 0: return "No expiry";
  }
}
