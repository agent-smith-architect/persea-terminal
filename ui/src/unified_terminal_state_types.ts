// Terminal copy, popover, scroll, typography, and input trace state types.
import type { SnippetOutcome } from "./snippet_client";
import type { OperatorPreferencePreviewToken } from "./operator_preferences";

export type CopyToClipsResult = Readonly<{
  local: boolean;
  store: "not-attempted" | "too-large" | "pending" | SnippetOutcome;
}>;

export type UnifiedPopoverOwner = "none" | "view" | "tag" | "sheet" | "explainer" | "typography";

export type UnifiedExplainerTopic = "copy" | "select" | "paste" | "fit" | "view" | "size" | "sessions";

export type ScrollAnchor = Readonly<{
  bufferType: "normal" | "alternate";
  row: number;
  fraction: number;
  scalarRemainder: number;
  following: boolean;
}>;

// value is the font-size tri-state the page intends to store: an explicit size, or
// `null` for auto. `null` is a real intent, never "no intent", so every read of
// it tests `fontIntent !== undefined` rather than using `??`.
export type FontIntent = Readonly<{
  id: number;
  value: number | null;
}>;

export type ComposerFontIntent = Readonly<{
  id: number;
  value: number;
  preview?: OperatorPreferencePreviewToken;
}>;

export type InputTraceEntry = Readonly<Record<string, unknown> & { t: number; kind: string }>;
