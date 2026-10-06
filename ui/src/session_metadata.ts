import type { DashboardSession } from "./dashboard";

// The output time in seconds, or undefined when it is absent, invalid or in
// the future. The label and the Output order use this same rule.
export function knownOutputTime(activity: number | undefined, now = Date.now()): number | undefined {
  return activity === undefined || !Number.isSafeInteger(activity) || activity <= 0 || activity * 1000 > now + 60_000 ? undefined : activity;
}

export function outputActivityLabel(activity: number | undefined, now = Date.now()): string {
  const known = knownOutputTime(activity, now);
  if (known === undefined) return "Output time unknown";
  const seconds = Math.max(0, Math.floor(now / 1000) - known);
  if (seconds < 60) return "Output just now";
  if (seconds < 3600) return `Output ${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `Output ${Math.floor(seconds / 3600)}h ago`;
  return `Output ${Math.floor(seconds / 86_400)}d ago`;
}

export function sessionMetadata(session: Pick<DashboardSession, "width" | "height" | "attached" | "outputActivity">, now = Date.now()): string {
  return `${session.width}×${session.height} · ${session.attached} attached · ${outputActivityLabel(session.outputActivity, now)}`;
}

export const SESSION_METADATA_HELP = "Size in columns × rows. Attached counts tmux clients. Output time is the latest output in any of the session's windows (a window without output counts from its creation). It does not indicate whether a task is running or finished.";

export function compareSessionNames(a: Pick<DashboardSession, "name">, b: Pick<DashboardSession, "name">): number {
  return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }) || a.name.localeCompare(b.name);
}
