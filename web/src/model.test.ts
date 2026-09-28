import { describe, expect, it } from "vitest";
import { ACTIVITY_PREVIEW_LIMIT, activityLines, activityPreview, activityView, applyPanelLoad, buildMessage, continueActivity, driftLabel, driftLine, eventLine, githubGrantSummary, maxCursor, mergeEvents, readJournal, repositoryLabel, repositoryObservationSummary, repositoryPolicy, repositorySyncNotice, secretNames, shortDigest } from "./model";
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
    const appended = mergeEvents(merged, [event("4", "dev.genesis.run.end")]);
    expect(appended.slice(0, 3)).toEqual(merged);
    expect(appended.map((item) => item.sequence)).toEqual(["1", "2", "3", "4"]);
  });

  it("loads the finished journal before the caller follows new tokens", async () => {
    const journal = readJournal(async (after) => {
      if (after === "0") {
        return { events: [event("1", "dev.genesis.run.accepted"), event("2", "dev.genesis.run.chunk")], cursor: "2", has_more: true };
      }
      if (after === "2") {
        return { events: [event("3", "dev.genesis.run.assistant")], cursor: "3", has_more: false };
      }
      throw new Error(`unexpected after ${after}`);
    });
    await expect(journal).resolves.toEqual({
      events: [event("1", "dev.genesis.run.accepted"), event("2", "dev.genesis.run.chunk"), event("3", "dev.genesis.run.assistant")],
      cursor: "3",
    });
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
    expect(raw.text).toBe("read");
    expect(raw.text).not.toContain("hidden stream");
    expect(raw.text).not.toBe("Tool call");
  });

  it("shows assistant text, a bash command, turn errors, and retries", () => {
    const assistant = eventLine(
      event("6", "dev.genesis.run.assistant", {
        phase: "message",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "assistant/message",
              data: {
                message: {
                  content: [
                    { type: "reasoning", text: "look at the tree" },
                    { type: "text", text: "the bench is clean" },
                  ],
                },
              },
            },
          },
        },
      }),
    );
    expect(assistant.text).toContain("look at the tree");
    expect(assistant.text).toContain("the bench is clean");
    expect(assistant.text).not.toBe("Tool call");

    const tool = eventLine(
      event("7", "dev.genesis.run.tool", {
        phase: "call",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "tool/call",
              data: { name: "bash", callId: "c1", arguments: '{"command":"pwd"}' },
            },
          },
        },
      }),
    );
    expect(tool.text).toContain("bash");
    expect(tool.text).toContain("pwd");
    expect(tool.text).not.toBe("Tool call");

    const result = eventLine(
      event("8", "dev.genesis.run.tool", {
        phase: "result",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "tool/result",
              data: {
                name: "bash",
                message: { content: [{ type: "tool-result", content: [{ type: "text", text: "total 1" }] }] },
              },
            },
          },
        },
      }),
    );
    expect(result.text).toContain("bash");
    expect(result.text).toContain("total 1");

    const failure = eventLine(
      event("9", "dev.genesis.run.turn", {
        phase: "end",
        reason_kind: "error",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "turn/end",
              data: { reason: { kind: "error", error: { message: "bad key", code: "AUTH", status: 401 } } },
            },
          },
        },
      }),
    );
    expect(failure.kind).toBe("error");
    expect(failure.text).toContain("bad key");
    expect(failure.text).toContain("AUTH");
    expect(failure.text).toContain("401");
    expect(failure.text).not.toContain("empty final response");
    expect(failure.text).not.toBe("Turn end");

    const retry = eventLine(
      event("10", "dev.genesis.run.retry", {
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "llm/retry",
              data: { retry: 2, failure: { code: "RATE_LIMIT", message: "slow down" } },
            },
          },
        },
      }),
    );
    expect(retry.text).toContain("retry");
    expect(retry.text).toContain("2");
    expect(retry.text).toContain("RATE_LIMIT");
    expect(retry.text).toContain("slow down");
  });

  it("appends live deltas onto the open row and does not paint usage or finish", () => {
    const lines = activityLines([
      event("1", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: { type: "chunk", attemptId: "a1", chunk: { type: "text-delta", index: 0, text: "hello " } },
        },
      }),
      event("2", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: { type: "chunk", attemptId: "a1", chunk: { type: "reasoning-delta", index: 0, text: "thinking" } },
        },
      }),
      event("3", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: { type: "chunk", attemptId: "a1", chunk: { type: "text-delta", index: 1, text: "bench" } },
        },
      }),
      event("4", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: {
            type: "chunk",
            attemptId: "a1",
            chunk: { type: "usage", usage: { inputTokens: 3, outputTokens: 4 } },
          },
        },
      }),
      event("5", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: {
            type: "chunk",
            attemptId: "a1",
            chunk: { type: "tool-call-delta", index: 1, id: "c1", name: "bash", argumentsDelta: '{"command":"pw' },
          },
        },
      }),
      event("6", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: {
            type: "chunk",
            attemptId: "a1",
            chunk: { type: "tool-call-delta", index: 1, id: "c1", argumentsDelta: 'd"}' },
          },
        },
      }),
      event("7", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: { type: "chunk", attemptId: "a1", chunk: { type: "finish", reason: { kind: "tool-calls" } } },
        },
      }),
      event("8", "dev.genesis.run.assistant", {
        phase: "message",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "assistant/message",
              data: {
                message: {
                  content: [
                    { type: "reasoning", text: "thinking" },
                    { type: "text", text: "hello bench" },
                  ],
                },
              },
            },
          },
        },
      }),
      event("9", "dev.genesis.run.tool", {
        phase: "call",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: { type: "tool/call", data: { name: "bash", arguments: '{"command":"pwd"}' } },
          },
        },
      }),
    ]);
    const assistant = lines.find((line) => line.kind === "assistant");
    const tool = lines.find((line) => line.kind === "tool");
    expect(assistant?.text).toContain("thinking");
    expect(assistant?.text).toContain("hello bench");
    expect(assistant?.text).not.toContain("hello hello");
    expect(tool?.text).toContain("bash");
    expect(tool?.text).toContain("pwd");
    expect(lines.some((line) => line.text === "Tool call")).toBe(false);
    expect(lines.some((line) => line.text.includes("inputTokens"))).toBe(false);
    expect(lines.some((line) => line.text.includes("tool-calls"))).toBe(false);

    const chunk = (sequence: string, text: string) =>
      event(sequence, "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: { type: "chunk", attemptId: "a1", chunk: { type: "text-delta", text } },
        },
      });
    const opened = [chunk("1", "one"), chunk("2", " two")];
    const cursor = continueActivity(null, opened);
    const extended = continueActivity(cursor, [...opened, chunk("3", " three")]);
    expect(extended).toBe(cursor);
    expect(activityView(extended).map((line) => line.text)).toEqual(["one two three"]);
    expect(activityLines([...opened, chunk("3", " three")]).map((line) => line.text)).toEqual(["one two three"]);
    const capped = activityLines(
      Array.from({ length: 200 }, (_, index) => chunk(String(index + 1), "y".repeat(100))),
    );
    expect(capped).toHaveLength(1);
    expect(capped[0].text.endsWith("\n… truncated")).toBe(true);
    expect(new TextEncoder().encode(capped[0].text.split("\n… truncated")[0]).length).toBeLessThanOrEqual(16 * 1024);

    const huge = eventLine(
      event("11", "dev.genesis.run.assistant", {
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "assistant/message",
              data: { message: { content: [{ type: "text", text: "x".repeat(20 * 1024) }] } },
            },
          },
        },
      }),
    );
    expect(huge.text.endsWith("\n… truncated")).toBe(true);
    expect(new TextEncoder().encode(huge.text.split("\n… truncated")[0]).length).toBeLessThanOrEqual(16 * 1024);
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
    expect(repositorySyncNotice(200, JSON.stringify({ repository_plan: { remote_mutation: "none", applied: [], observation: "observed" } }))).toContain(
      "was not applied",
    );
    expect(repositoryObservationSummary(previousRepository)).toContain("did not call github");
    expect(
      repositoryObservationSummary({
        provider: "github",
        observation: "observed",
        observed: {
          id: "lab",
          org: "octo-org",
          name: "lab-widget",
          repository_id: 4242,
          node_id: "R_testNode",
          visibility: "private",
          description: "",
          default_branch: "main",
          archived: false,
          features: { issues: true, wiki: false, projects: false },
          merge: { allow_squash: true, allow_merge_commit: false, allow_rebase: false, delete_branch_on_merge: true },
          actions_status: "observed",
          ruleset_status: "observed",
          branch_status: "observed",
        },
        drift: [{ id: "lab", field: "settings.visibility", desired: "private", observed: "public", status: "drift" }],
      }),
    ).toBe("Last observation of active octo-org/lab-widget (4242). 1 drifting field.");
    expect(driftLine({ field: "settings.visibility", status: "drift", desired: "private", observed: "public" })).toContain(
      "private → public",
    );
    expect(repositorySyncNotice(200, JSON.stringify({ repository_plan: { remote_mutation: "applied", applied: ["ensure_repository:lab"] } }))).toContain(
      "ensure_repository:lab",
    );
    expect(repositorySyncNotice(200, JSON.stringify({ repository_plan: { remote_mutation: "applied", applied: [] } }))).toContain(
      "did not confirm",
    );
    expect(repositorySyncNotice(200, "not-json")).toContain("not JSON");
    expect(repositorySyncNotice(500, "repositories are invalid")).toBe("repositories are invalid");
    const grantBody = JSON.stringify({
      repository_plan: { remote_mutation: "none", applied: [] },
      grant_plan: { credential_active: false, key_material: "pending", remote_registration: "unsupported" },
    });
    expect(repositorySyncNotice(200, grantBody)).toContain("GitHub credentials were not activated.");
    expect(
      repositorySyncNotice(
        200,
        JSON.stringify({
          repository_plan: { remote_mutation: "none", applied: [] },
          grant_plan: {
            credential_active: false,
            key_material: "pending",
            remote_registration: "unsupported",
            material: [{ grant_id: "gabc", fingerprint: "SHA256:abc" }],
          },
        }),
      ),
    ).toContain("The deploy key is not ready.");
    expect(
      repositorySyncNotice(
        200,
        JSON.stringify({
          repository_plan: { remote_mutation: "none", applied: [] },
          grant_plan: {
            credential_active: false,
            key_material: "pending",
            remote_registration: "unsupported",
            material: [{ grant_id: "gabc", remote_status: "ready", remote_key_id: "42" }],
          },
        }),
      ),
    ).toContain("SSH deploy key is registered.");
    expect(
      repositorySyncNotice(
        200,
        JSON.stringify({
          repository_plan: { remote_mutation: "none", applied: [] },
          grant_plan: { credential_active: true, key_material: "pending", remote_registration: "unsupported" },
        }),
      ),
    ).toContain("credential was reported active");
    const summary = githubGrantSummary({
      github: {
        repository: "lab-widget",
        git: "read",
        credential: "none",
        permissions: { metadata: "read", contents: "read" },
        secret: "GENESIS_PROVIDER_SECRET_REF_XYZ",
        identity: "prog-bot",
      } as Agent["github"],
    });
    expect(summary).toBe("GitHub lab-widget, git read, no credential, contents read, metadata read. Not activated.");
    expect(summary).not.toContain("GENESIS_PROVIDER_SECRET_REF_XYZ");
    expect(summary).not.toContain("prog-bot");
    expect(
      githubGrantSummary({
        github: { repository: "lab", git: "write", credential: "pending", permissions: { contents: "write" } },
      }),
    ).toBe("GitHub lab, git write, credential pending, contents write. Not activated.");
    const unexpected = githubGrantSummary({
      github: {
        repository: "lab",
        git: "read",
        credential: "GENESIS_PROVIDER_SECRET_REF_XYZ",
        permissions: { metadata: "read" },
      },
    });
    expect(unexpected).toBe("GitHub lab, git read, unrecognized credential, metadata read. Not activated.");
    expect(unexpected).not.toContain("GENESIS_PROVIDER_SECRET_REF_XYZ");
    expect(githubGrantSummary({ github: undefined })).toBe("");
    expect(JSON.stringify(secretNames(previousRepository))).not.toContain("ghp_");
    expect(kept.state?.drift).toBe("desired_invalid");
    expect(kept.runs.map((item) => item.run_id)).toEqual(["gen_1"]);
  });

  it("previews activity rows on one line and leaves the mapped text intact", () => {
    const assistant = eventLine(
      event("6", "dev.genesis.run.assistant", {
        phase: "message",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "assistant/message",
              data: {
                message: {
                  content: [
                    { type: "reasoning", text: "look at the tree" },
                    { type: "text", text: "the bench is clean" },
                  ],
                },
              },
            },
          },
        },
      }),
    );
    expect(assistant.text).toBe("look at the tree\n\nthe bench is clean");
    expect(activityPreview(assistant)).toBe("look at the tree the bench is clean");
    expect(activityPreview(assistant)).not.toContain("…");

    const call = eventLine(
      event("7", "dev.genesis.run.tool", {
        phase: "call",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "tool/call",
              data: { name: "bash", arguments: '{\n  "command": "pwd",\n  "cwd": "/tmp"\n}' },
            },
          },
        },
      }),
    );
    expect(call.toolCall).toBe(true);
    expect(call.text).toBe('bash\n{\n  "command": "pwd",\n  "cwd": "/tmp"\n}');
    expect(activityPreview(call)).toBe('bash {"command":"pwd","cwd":"/tmp"}');
    expect(activityPreview(call)).not.toContain("\n");

    const result = eventLine(
      event("8", "dev.genesis.run.tool", {
        phase: "result",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "tool/result",
              data: {
                name: "bash",
                message: { content: [{ type: "tool-result", content: [{ type: "text", text: '{\n  "ok": true\n}' }] }] },
              },
            },
          },
        },
      }),
    );
    expect(result.toolCall).toBeUndefined();
    expect(result.text).toContain("\n");
    expect(activityPreview(result)).toBe('bash { "ok": true }');

    const failure = eventLine(
      event("9", "dev.genesis.run.turn", {
        phase: "end",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "turn/end",
              data: { reason: { kind: "error", error: { message: "bad key", code: "AUTH", status: 401 } } },
            },
          },
        },
      }),
    );
    expect(failure.text).toBe("bad key\nAUTH\n401");
    expect(activityPreview(failure)).toBe("bad key AUTH 401");

    const retry = eventLine(
      event("10", "dev.genesis.run.retry", {
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "llm/retry",
              data: { retry: 2, failure: { code: "RATE_LIMIT", message: `slow down ${"x".repeat(200)}` } },
            },
          },
        },
      }),
    );
    const retryPreview = activityPreview(retry);
    expect(retry.text.startsWith("retry 2 RATE_LIMIT slow down")).toBe(true);
    expect(retry.text.length).toBeGreaterThan(ACTIVITY_PREVIEW_LIMIT);
    expect(retryPreview.startsWith("retry 2 RATE_LIMIT slow down")).toBe(true);
    expect(retryPreview.endsWith("…")).toBe(true);
    expect(retryPreview).not.toContain("\n");
    expect(Array.from(retryPreview).length).toBeLessThanOrEqual(ACTIVITY_PREVIEW_LIMIT);

    const longResult = eventLine(
      event("11", "dev.genesis.run.tool", {
        phase: "result",
        name: "bash",
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "tool/result",
              data: {
                name: "bash",
                message: {
                  content: [{ type: "tool-result", content: [{ type: "text", text: "line\n".repeat(40) }] }],
                },
              },
            },
          },
        },
      }),
    );
    const resultPreview = activityPreview(longResult);
    expect(longResult.text).toContain("\n");
    expect(resultPreview.startsWith("bash line")).toBe(true);
    expect(resultPreview.endsWith("…")).toBe(true);
    expect(resultPreview).not.toContain("\n");
    expect(Array.from(resultPreview).length).toBeLessThanOrEqual(ACTIVITY_PREVIEW_LIMIT);

    const huge = eventLine(
      event("12", "dev.genesis.run.assistant", {
        raw: {
          method: "session.event",
          payload: {
            event: {
              type: "assistant/message",
              data: { message: { content: [{ type: "text", text: "x".repeat(20 * 1024) }] } },
            },
          },
        },
      }),
    );
    expect(huge.text.endsWith("\n… truncated")).toBe(true);
    const hugePreview = activityPreview(huge);
    expect(hugePreview.endsWith("…")).toBe(true);
    expect(hugePreview).not.toContain("\n");
    expect(Array.from(hugePreview).length).toBeLessThanOrEqual(ACTIVITY_PREVIEW_LIMIT);

    const streamed = activityLines([
      event("1", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: {
            type: "chunk",
            attemptId: "a1",
            chunk: { type: "tool-call-delta", id: "c1", name: "bash", argumentsDelta: '{\n  "command": "' },
          },
        },
      }),
      event("2", "dev.genesis.run.chunk", {
        raw: {
          method: "on_chunk",
          payload: {
            type: "chunk",
            attemptId: "a1",
            chunk: { type: "tool-call-delta", id: "c1", argumentsDelta: 'pwd"\n}' },
          },
        },
      }),
    ]);
    const liveTool = streamed.find((line) => line.kind === "tool");
    expect(liveTool?.toolCall).toBe(true);
    expect(liveTool?.text).toBe('bash\n{\n  "command": "pwd"\n}');
    expect(activityPreview(liveTool!)).toBe('bash {"command":"pwd"}');
    expect(activityPreview({ text: 'bash\n{\n  "command": "pw', toolCall: true })).toBe('bash { "command": "pw');
  });
});
