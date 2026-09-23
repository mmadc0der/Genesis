import { describe, expect, it } from "vitest";
import { applyPanelLoad, buildMessage, driftLabel, eventLine, maxCursor, mergeEvents, repositoryLabel, repositoryPolicy, repositorySyncNotice, secretNames, shortDigest } from "./model";
import type { Agent, ControlState, LifecycleEvent, Repository, RunSummary } from "./types";

function event(sequence: string, type: string, data?: Record<string, unknown>): LifecycleEvent {
  return { sequence, type, data };
}

describe("control panel model", () => {
  it("shortens digests and names drift", () => {
    expect(shortDigest("sha256:0123456789abcdef")).toBe("0123456789ab");
    expect(shortDigest(undefined)).toBe("—");
    expect(driftLabel("in_sync")).toBe("Active");
    expect(driftLabel("draft")).toBe("Draft");
    expect(driftLabel("listener_unavailable")).toBe("Listener down");
    expect(driftLabel("generation_too_large")).toBe("Generation too large");
  });

  it("merges journal events by sequence without duplicates", () => {
    const first = [event("1", "dev.genesis.run.accepted"), event("2", "dev.genesis.run.turn", { phase: "start" })];
    const merged = mergeEvents(first, [
      event("2", "dev.genesis.run.turn", { phase: "start" }),
      event("3", "dev.genesis.run.result", { final_response: "done" }),
    ]);
    expect(merged.map((item) => item.sequence)).toEqual(["1", "2", "3"]);
    expect(maxCursor(merged)).toBe("3");
  });

  it("renders settled results and does not invent token text from raw notifications", () => {
    expect(eventLine(event("4", "dev.genesis.run.result", { final_response: "bench is clean" })).text).toBe(
      "bench is clean",
    );
    const raw = eventLine(
      event("5", "dev.genesis.run.tool", {
        phase: "call",
        name: "read",
        raw: { method: "session.event", payload: { event: { type: "assistant/message", text: "hidden stream" } } },
      }),
    );
    expect(raw.text).toBe("Tool call read");
    expect(raw.text).not.toContain("hidden stream");
  });

  it("builds a message for a selected rule without a bearer token", () => {
    const body = buildMessage({
      message: "summarize the workspace",
      rule: "example.yaml",
      type: "dev.genesis.run",
      source: "urn:genesis:example",
    });
    expect(body).toEqual({
      message: "summarize the workspace",
      rule: "example.yaml",
      type: "dev.genesis.run",
      source: "urn:genesis:example",
    });
    expect(JSON.stringify(body)).not.toContain("Bearer");
    expect(JSON.stringify(body)).not.toContain("sync-token");
  });

  it("builds the dedicated designer user-message event from the selected rule", () => {
    const body = buildMessage({
      message: "add a shared-uid lab agent",
      rule: "designer.yaml",
      type: "dev.genesis.user.message",
      source: "urn:genesis:control",
      subject: "designer",
    });
    expect(body).toEqual({
      message: "add a shared-uid lab agent",
      rule: "designer.yaml",
      type: "dev.genesis.user.message",
      source: "urn:genesis:control",
      subject: "designer",
    });
    expect(JSON.stringify(body)).not.toContain("GENESIS_SYNC_TOKEN");
  });

  it("renders state and runs when the agent and rule catalogs fail", () => {
    const state = {
      drift: "desired_invalid",
      desired_error: "agents are invalid",
      listener: { reachable: true, ok: true, sync_configured: true, syncing: false },
      active: null,
      desired: null,
    } satisfies ControlState;
    const run = {
      run_id: "gen_1",
      agent: "workspace-janitor",
      rule: "example.yaml",
      state: "open",
      last_seq: "1",
    } satisfies RunSummary;
    const view = applyPanelLoad(
      { state: null, agents: [], rules: [], repositories: [], runs: [], problems: [] },
      {
        state: { ok: true, value: state },
        agents: { ok: false, error: "agents are invalid" },
        rules: { ok: false, error: "rules are invalid" },
        repositories: { ok: false, error: "repositories are invalid" },
        runs: { ok: true, value: { runs: [run] } },
      },
    );
    expect(view.state?.drift).toBe("desired_invalid");
    expect(view.runs.map((item) => item.run_id)).toEqual(["gen_1"]);
    expect(view.problems.join(" ")).toContain("agents are invalid");
    expect(view.problems.join(" ")).toContain("rules are invalid");
    expect(view.problems.join(" ")).toContain("repositories are invalid");

    const previousAgent: Agent = {
      id: "workspace-janitor",
      instructions: "keep",
      cwd: "/work",
      home: "/home",
      user: "workspace-janitor",
      env: {},
      secrets: [],
      presence: "active",
    };
    const previousRepository: Repository = {
      id: "lab",
      provider: "github",
      org: "octo-org",
      name: "lab-widget",
      lifecycle: { remove: "retain", existing: "adopt" },
      settings: {
        visibility: "private",
        default_branch: "main",
        features: { issues: true, wiki: false, projects: false },
        merge: {
          allow_squash: true,
          allow_merge_commit: false,
          allow_rebase: false,
          delete_branch_on_merge: true,
        },
      },
      actions: { enabled: true, allowed: "selected", selected: ["actions/checkout@v4"] },
      secrets: { repository: ["DEEPSEEK_API_KEY"], environments: [{ name: "ci", secrets: ["CI_BOT_TOKEN"] }] },
      presence: "active",
    };
    const kept = applyPanelLoad(
      { state: null, agents: [previousAgent], rules: [], repositories: [previousRepository], runs: [], problems: [] },
      {
        state: { ok: true, value: state },
        agents: { ok: false, error: "agents are invalid" },
        rules: { ok: true, value: { rules: [] } },
        repositories: { ok: false, error: "repositories are invalid" },
        runs: { ok: true, value: { runs: [run] } },
      },
    );
    expect(kept.agents.map((item) => item.id)).toEqual(["workspace-janitor"]);
    expect(kept.repositories.map((item) => item.id)).toEqual(["lab"]);
    expect(repositoryLabel(previousRepository)).toBe("octo-org/lab-widget");
    expect(repositoryPolicy(previousRepository)).toBe("adopt, remove retain");
    expect(secretNames(previousRepository)).toEqual(["DEEPSEEK_API_KEY", "ci:CI_BOT_TOKEN"]);
    expect(repositorySyncNotice(200, JSON.stringify({ repository_plan: { remote_mutation: "none", applied: [] } }))).toContain(
      "not applied",
    );
    expect(repositorySyncNotice(200, JSON.stringify({ repository_plan: { remote_mutation: "applied", applied: ["ensure_repository:lab"] } }))).toContain(
      "did not confirm",
    );
    expect(repositorySyncNotice(200, "not-json")).toContain("not JSON");
    expect(repositorySyncNotice(500, "repositories are invalid")).toBe("repositories are invalid");
    expect(JSON.stringify(secretNames(previousRepository))).not.toContain("ghp_");
    expect(kept.state?.drift).toBe("desired_invalid");
    expect(kept.runs.map((item) => item.run_id)).toEqual(["gen_1"]);
  });
});
