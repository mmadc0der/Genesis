import { isDrifted } from "./hints";
import type { AgentRow, RuleRow } from "./live";

export interface DriftItem {
  kind: "agent" | "rule";
  name: string;
  // note says what differs: new or edited, or removed on disk.
  note: string;
}

function noteFor(presence: string | undefined) {
  return presence === "active_only" ? "removed on disk" : "new or edited";
}

// driftItems lists what a Sync would change, agents first and each group in
// name order.
export function driftItems(agents: AgentRow[], rules: RuleRow[]): DriftItem[] {
  const byName = (left: DriftItem, right: DriftItem) => left.name.localeCompare(right.name);
  const agentItems = agents
    .filter((agent) => isDrifted(agent.presence))
    .map((agent): DriftItem => ({ kind: "agent", name: agent.id, note: noteFor(agent.presence) }))
    .sort(byName);
  const ruleItems = rules
    .filter((rule) => isDrifted(rule.presence))
    .map((rule): DriftItem => ({ kind: "rule", name: rule.name, note: noteFor(rule.presence) }))
    .sort(byName);
  return [...agentItems, ...ruleItems];
}

export interface SyncResult {
  ok: boolean;
  message: string;
}

// postSync asks control to sync agents, rules, and repos. Control adds the
// bearer; the browser never holds the sync token.
export async function postSync(): Promise<SyncResult> {
  let response: Response;
  try {
    response = await fetch("/api/sync", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
  } catch {
    return { ok: false, message: "Control did not answer." };
  }
  if (response.ok) return { ok: true, message: "Synced." };
  const text = (await response.text().catch(() => "")).trim();
  return { ok: false, message: text ? text.slice(0, 240) : `Sync failed (${response.status}).` };
}
