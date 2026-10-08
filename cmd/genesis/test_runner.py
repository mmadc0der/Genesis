import importlib.util
import io
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from typing import Any, ClassVar
from unittest import mock

RUNNER_PATH = Path(__file__).with_name("runner.py")
SPEC = importlib.util.spec_from_file_location("genesis_runner", RUNNER_PATH)
assert SPEC is not None and SPEC.loader is not None
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)

INSTRUCTIONS = "You are the workspace janitor for this lab."
AGENT_HOME = "/tmp/genesis-agent-home"


class FakeSession:
    def __init__(self, harness: "FakeHarness", session_id: str):
        self.harness = harness
        self.id = session_id

    def run(self, message, on_notification=None):
        return self.harness.complete(message, on_notification)


class FakeHarness:
    instances: ClassVar[list] = []

    def __init__(self, **kwargs):
        self.kwargs = kwargs
        self.environment = dict(os.environ)
        self.message = None
        self.home_existed = False
        self.patch_existed = False
        self.patch_contents = None
        self.session_id = "deepseek-session-1"
        self.notifications: list[Any] = []
        self.__class__.instances.append(self)

    def __enter__(self):
        self.home_existed = Path(self.kwargs["dsh_home"]).is_dir()
        patch_path = Path(self.kwargs["patches"][0])
        self.patch_existed = patch_path.is_file()
        self.patch_contents = patch_path.read_text(encoding="utf-8")
        return self

    def __exit__(self, _kind, _error, _traceback):
        return None

    def start_session(self, session_id=None):
        self.supplied_session_id = session_id
        self.session_id = session_id or self.session_id
        return FakeSession(self, self.session_id)

    def complete(self, message, on_notification=None):
        self.message = message
        if on_notification is not None:
            for notification in self.notifications:
                on_notification(notification)
        return SimpleNamespace(
            session_id=self.session_id,
            finish_reason="completed",
            final_response="finished",
        )


class ErrorHarness(FakeHarness):
    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.session_id = "deepseek-session-error"

    def complete(self, message, on_notification=None):
        self.message = message
        return SimpleNamespace(
            session_id=self.session_id,
            finish_reason="error",
            final_response="",
            events=[
                {"type": "message/start", "data": {"ignored": True}},
                {
                    "type": "agent/error",
                    "data": {"message": "provider request failed"},
                },
                {
                    "type": "turn/end",
                    "data": {
                        "reason": {
                            "kind": "error",
                            "message": "upstream unavailable",
                        }
                    },
                },
            ],
            notifications=[
                SimpleNamespace(method="session.event", payload={"ignored": True}),
                SimpleNamespace(
                    method="runtime.warning",
                    payload={"message": "connection closed"},
                ),
            ],
        )


class TurnFailureHarness(ErrorHarness):
    def complete(self, message, on_notification=None):
        self.message = message
        return SimpleNamespace(
            session_id=self.session_id,
            finish_reason="error",
            final_response="",
            events=[
                {
                    "type": "turn/end",
                    "data": {
                        "reason": {
                            "kind": "error",
                            "error": {
                                "message": "bad key",
                                "code": "AUTH",
                                "status": 401,
                            },
                        }
                    },
                }
            ],
            notifications=[
                SimpleNamespace(
                    method="on_chunk",
                    payload={
                        "type": "chunk",
                        "sessionId": self.session_id,
                        "chunk": {"type": "text-delta", "text": "x"},
                    },
                )
            ],
        )


class EmittingHarness(FakeHarness):
    def complete(self, message, on_notification=None):
        self.message = message
        if on_notification is not None:
            on_notification(
                SimpleNamespace(
                    method="genesis.emit",
                    payload={
                        "type": "com.example.note",
                        "subject": "lab",
                        "source": "urn:spoofed",
                        "id": "spoof",
                        "data": {"n": 1},
                    },
                )
            )
        return SimpleNamespace(
            session_id=self.session_id,
            finish_reason="completed",
            final_response="finished",
        )


class NotifyingHarness(FakeHarness):
    def complete(self, message, on_notification=None):
        self.message = message
        notifications = [
            SimpleNamespace(
                method="session.event",
                payload={
                    "sessionId": self.session_id,
                    "event": {"type": "turn/start", "seq": 1, "data": {}},
                },
            ),
            SimpleNamespace(
                method="session.status",
                payload={"sessionId": self.session_id, "status": "idle"},
            ),
        ]
        if on_notification is not None:
            for notification in notifications:
                on_notification(notification)
        return SimpleNamespace(
            session_id=self.session_id,
            finish_reason="completed",
            final_response="finished",
        )


def sample_invocation(**overrides):
    workspace = str(Path.cwd().resolve())
    document = {
        "event": {
            "specversion": "1.0",
            "id": "event-1",
            "source": "urn:test",
            "type": "dev.genesis.test",
            "data": {"unicode": "Привет", "nested": [1, True]},
        },
        "rule": "test.yaml",
        "agent": "workspace-janitor",
        "run_id": "gen_test",
        "cwd": workspace,
        "home": AGENT_HOME,
        "instructions": INSTRUCTIONS,
        "env": {
            "DEEPSEEK_API_KEY": "genesis-key",
            "ONLY_DECLARED": "yes",
        },
    }
    document.update(overrides)
    return document


class RunnerTests(unittest.TestCase):
    def setUp(self):
        FakeHarness.instances.clear()
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dsh_home = str(Path(self.temp.name) / "dsh_home")
        self.run_dir = str(Path(self.temp.name) / "run")
        Path(self.dsh_home).mkdir()
        Path(self.run_dir).mkdir()

    def invocation(self, **overrides):
        document = sample_invocation(dsh_home=self.dsh_home, run_dir=self.run_dir)
        document.update(overrides)
        return document

    def test_execute_uses_complete_event_environment_and_sdk_minimal(self):
        workspace = str(Path.cwd().resolve())
        event = {
            "specversion": "1.0",
            "id": "event-1",
            "source": "urn:test",
            "type": "dev.genesis.test",
            "data": {"unicode": "Привет", "nested": [1, True]},
        }
        invocation = self.invocation(event=event, cwd=workspace)
        with mock.patch.dict(
            os.environ,
            {
                "DEEPSEEK_API_KEY": "ambient-key-must-not-win",
                "AMBIENT_ONLY": "must-not-leak",
                "HOME": "/ambient/home",
                "SSH_AUTH_SOCK": "",
                "GITHUB_TOKEN": "",
            },
        ):
            environment_before = dict(os.environ)
            result = runner.execute(invocation, harness_factory=FakeHarness)
            self.assertEqual(dict(os.environ), environment_before)

        self.assertEqual(
            result,
            {
                "deepseek_session_id": "deepseek-session-1",
                "finish_reason": "completed",
                "final_response": "finished",
                "error": None,
                "diagnostics": None,
            },
        )
        harness = FakeHarness.instances[0]
        self.assertEqual(json.loads(harness.message), event)
        self.assertNotIn(INSTRUCTIONS, harness.message)
        self.assertEqual(
            harness.environment,
            {
                "DEEPSEEK_API_KEY": "genesis-key",
                "ONLY_DECLARED": "yes",
                "HOME": AGENT_HOME,
                "DSH_SYSTEM_PROMPT": INSTRUCTIONS,
            },
        )
        self.assertEqual(
            {
                key: value
                for key, value in harness.kwargs.items()
                if key not in {"dsh_home", "runtime_cwd", "patches"}
            },
            {
                "provider": "deepseek-official",
                "model": "deepseek-flash",
                "cwd": workspace,
                "profile": "sdk-minimal",
            },
        )
        self.assertEqual(harness.kwargs["dsh_home"], self.dsh_home)
        self.assertEqual(harness.kwargs["runtime_cwd"], self.dsh_home)
        self.assertNotEqual(harness.kwargs["dsh_home"], AGENT_HOME)
        self.assertNotEqual(harness.kwargs["dsh_home"], workspace)
        self.assertNotIn("reasoning_effort", harness.kwargs)
        self.assertTrue(harness.home_existed)
        self.assertTrue(Path(harness.kwargs["dsh_home"]).is_dir())
        self.assertEqual(harness.environment["DEEPSEEK_API_KEY"], "genesis-key")
        self.assertNotIn("AMBIENT_ONLY", harness.environment)

    def test_execute_keeps_root_delivered_socket_and_token_only(self):
        invocation = self.invocation(
            env={
                "ONLY_DECLARED": "yes",
                "GITHUB_TOKEN": "ghs_from_invocation_0123456789",
                "GH_TOKEN": "ghp_from_invocation_0123456789",
                "SSH_AUTH_SOCK": "/tmp/invocation.sock",
            }
        )
        with mock.patch.dict(
            os.environ,
            {
                "GITHUB_TOKEN": "ghs_from_process_0123456789",
                "SSH_AUTH_SOCK": "/tmp/run.sock",
                "GH_TOKEN": "ghp_from_process_0123456789",
            },
        ):
            runner.execute(invocation, harness_factory=FakeHarness)
        harness = FakeHarness.instances[-1]
        self.assertEqual(
            harness.environment["GITHUB_TOKEN"], "ghs_from_process_0123456789"
        )
        self.assertEqual(harness.environment["SSH_AUTH_SOCK"], "/tmp/run.sock")
        self.assertNotIn("GH_TOKEN", harness.environment)
        self.assertNotIn("ghs_from_invocation_0123456789", harness.environment.values())
        self.assertNotIn("/tmp/invocation.sock", harness.environment.values())

    def test_execute_sets_home_and_system_prompt_from_agent_snapshot(self):
        invocation = self.invocation(
            env={"ONLY_DECLARED": "yes"},
            home="/tmp/explicit-agent-home",
            instructions="Standing identity only.",
        )
        runner.execute(invocation, harness_factory=FakeHarness)
        harness = FakeHarness.instances[0]
        self.assertEqual(harness.environment["HOME"], "/tmp/explicit-agent-home")
        self.assertEqual(
            harness.environment["DSH_SYSTEM_PROMPT"],
            "Standing identity only.",
        )
        self.assertEqual(json.loads(harness.message), invocation["event"])
        self.assertNotIn("Standing identity only.", harness.message)

    def test_execute_allows_missing_api_key_for_credential_free_tests(self):
        result = runner.execute(
            self.invocation(
                event={
                    "specversion": "1.0",
                    "id": "event-no-key",
                    "source": "urn:test",
                    "type": "dev.genesis.test",
                },
                rule="no-key.yaml",
                run_id="gen_no_key",
                env={"ONLY_DECLARED": "yes"},
            ),
            harness_factory=FakeHarness,
        )

        self.assertEqual(result["finish_reason"], "completed")
        self.assertEqual(
            FakeHarness.instances[0].environment,
            {
                "ONLY_DECLARED": "yes",
                "HOME": AGENT_HOME,
                "DSH_SYSTEM_PROMPT": INSTRUCTIONS,
            },
        )

    def test_execute_passes_reasoning_effort_when_set(self):
        runner.execute(self.invocation(reasoning_effort="low"), harness_factory=FakeHarness)
        self.assertEqual(FakeHarness.instances[0].kwargs["reasoning_effort"], "low")
        with self.assertRaises(ValueError):
            runner.execute(self.invocation(reasoning_effort="turbo"), harness_factory=FakeHarness)

    def test_execute_supplies_version_coupled_session_log_privacy_patch(self):
        runner.execute(
            self.invocation(
                event={
                    "specversion": "1.0",
                    "id": "event-privacy",
                    "source": "urn:test",
                    "type": "dev.genesis.test",
                },
                rule="privacy.yaml",
                run_id="gen_privacy",
                env={"DEEPSEEK_API_KEY": "test-key"},
            ),
            harness_factory=FakeHarness,
        )

        harness = FakeHarness.instances[0]
        patch_path = Path(harness.kwargs["dsh_home"]) / "session-log-off.patch.yml"
        self.assertEqual(harness.kwargs["patches"], (str(patch_path),))
        self.assertTrue(harness.patch_existed)
        plugin_path = Path(harness.kwargs["dsh_home"]) / "genesis-assistant-stream.mjs"
        emit_path = Path(harness.kwargs["dsh_home"]) / "genesis-emit.mjs"
        resume_path = Path(harness.kwargs["dsh_home"]) / "genesis-session-resume.mjs"
        self.assertEqual(runner.HARNESS_MAX_RETRIES, 10)
        self.assertEqual(runner.HARNESS_MAX_BACKOFF_MS, 60_000)
        self.assertEqual(
            harness.patch_contents,
            """\
- id: session-log-deepseek
  name: '@deepseek-ai/dsh-session-log-deepseek'
  config:
    enabled: false
- id: llm-deepseek
  name: '@deepseek-ai/dsh-llm-deepseek'
  config:
    apiKeyEnv: DEEPSEEK_API_KEY
    defaultContextWindow: !!js Number(process.env.DSH_CONTEXT_WINDOW ?? 1000000)
    streamIdleTimeoutMs: 172800000
    retryPolicy:
      mode: normal
      maxRetries: 10
      backoff:
        initialDelayMs: 500
        maxDelayMs: 60000
        jitterRatio: 0.1
- insert:
    - id: genesis-assistant-stream
      name: """
            + json.dumps(str(plugin_path))
            + "\n"
            + "    - id: genesis-emit\n"
            + "      name: "
            + json.dumps(str(emit_path))
            + "\n"
            + "    - id: genesis-session-resume\n"
            + "      name: "
            + json.dumps(str(resume_path))
            + "\n",
        )
        self.assertTrue(patch_path.is_file())
        self.assertEqual(
            plugin_path.read_text(encoding="utf-8"), runner.ASSISTANT_STREAM_PLUGIN
        )
        source = (
            Path(__file__)
            .with_name("assistant_stream_plugin.mjs")
            .read_text(encoding="utf-8")
        )
        self.assertEqual(source, runner.ASSISTANT_STREAM_PLUGIN)
        self.assertEqual(emit_path.read_text(encoding="utf-8"), runner.EMIT_PLUGIN)
        emit_source = (
            Path(__file__)
            .with_name("genesis_emit_plugin.mjs")
            .read_text(encoding="utf-8")
        )
        self.assertEqual(emit_source, runner.EMIT_PLUGIN)
        self.assertEqual(resume_path.read_text(encoding="utf-8"), runner.RESUME_PLUGIN)
        resume_source = (
            Path(__file__)
            .with_name("genesis_session_resume_plugin.mjs")
            .read_text(encoding="utf-8")
        )
        self.assertEqual(resume_source, runner.RESUME_PLUGIN)
        self.assertNotIn("process.env.GENESIS_SYNC_TOKEN", runner.EMIT_PLUGIN)
        self.assertNotIn("127.0.0.1", runner.EMIT_PLUGIN)

    def test_execute_uses_supplied_dsh_home_and_start_session(self):
        created = []
        result = runner.execute(
            self.invocation(),
            harness_factory=FakeHarness,
            on_session_created=created.append,
        )
        self.assertEqual(result["deepseek_session_id"], "deepseek-session-1")
        self.assertEqual(created, ["deepseek-session-1"])
        self.assertIsNone(FakeHarness.instances[0].supplied_session_id)
        self.assertEqual(FakeHarness.instances[0].kwargs["dsh_home"], self.dsh_home)
        self.assertTrue(Path(self.dsh_home).is_dir())

    def test_continuation_passes_session_id_and_user_message(self):
        result = runner.execute(
            self.invocation(session_id="session-kept", user_message="follow up"),
            harness_factory=FakeHarness,
        )
        harness = FakeHarness.instances[0]
        self.assertEqual(harness.supplied_session_id, "session-kept")
        self.assertEqual(harness.message, "follow up")
        self.assertEqual(result["deepseek_session_id"], "session-kept")

    def test_transport_restart_does_not_send_the_event_as_the_user_message(self):
        event = {
            "specversion": "1.0",
            "id": "event-restart",
            "source": "urn:test",
            "type": "dev.genesis.test",
        }
        result = runner.execute(
            self.invocation(
                event=event,
                session_id="session-kept",
                transport_restart=True,
            ),
            harness_factory=FakeHarness,
        )
        harness = FakeHarness.instances[0]
        self.assertEqual(harness.supplied_session_id, "session-kept")
        self.assertEqual(harness.message, runner.TRANSPORT_RESTART_PROMPT)
        self.assertNotIn("event-restart", harness.message)
        self.assertEqual(harness.environment.get("GENESIS_TRANSPORT_RESTART"), "1")
        self.assertEqual(result["deepseek_session_id"], "session-kept")

    def test_transport_restart_requires_session_id(self):
        with self.assertRaises(ValueError):
            runner.execute(
                self.invocation(transport_restart=True),
                harness_factory=FakeHarness,
            )

    def test_resume_plugin_appends_decision_instead_of_create(self):
        node = shutil.which("node")
        if node is None:
            self.skipTest("node is not installed; resume decision was not executed")
        plugin = Path(__file__).with_name("genesis_session_resume_plugin.mjs")
        home = Path(self.temp.name) / "dsh-home"
        session_id = "session-existing"
        log = home / "sessions" / "proj" / session_id / "session.v3.jsonl"
        log.parent.mkdir(parents=True)
        log.write_text('{"type":"session"}\n', encoding="utf-8")
        script = r"""
import { apply } from process.env.PLUGIN_PATH;

try {
  apply({});
  console.error("apply succeeded without agents.create");
  process.exit(2);
} catch (error) {
  if (!String(error && error.message).includes("agents.create")) {
    console.error(error);
    process.exit(2);
  }
}

const calls = [];
const agents = {
  async create(options) {
    calls.push(["create", options && options.sessionId]);
    return { created: true };
  },
  async resume(options) {
    calls.push(["resume", options && options.resumeSessionId]);
    return { resumed: true };
  },
};
apply({ agents });
const resumed = await agents.create({
  sessionId: process.env.SESSION_ID,
  agentOptions: { provider: "deepseek-official", model: "deepseek-flash" },
});
if (!resumed || !resumed.resumed || calls.some((call) => call[0] === "create")) {
  console.error(JSON.stringify(calls));
  process.exit(1);
}

const fresh = [];
const clean = {
  async create() {
    fresh.push("create");
    return { created: true };
  },
  async resume() {
    fresh.push("resume");
    return { resumed: true };
  },
};
const previous = process.env.DSH_HOME;
delete process.env.DSH_HOME;
apply({ agents: clean });
process.env.DSH_HOME = previous;
await clean.create({ sessionId: "session-new" });
if (fresh.join(",") !== "create") {
  console.error(fresh.join(","));
  process.exit(1);
}
"""
        # The import path cannot be a normal string inside the module graph
        # when the script is stdin. Write a file that imports the plugin URL.
        module = Path(self.temp.name) / "resume-check.mjs"
        module.write_text(
            script.replace(
                "process.env.PLUGIN_PATH",
                json.dumps(plugin.as_uri()),
            ),
            encoding="utf-8",
        )
        completed = subprocess.run(
            [node, str(module)],
            check=False,
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "DSH_HOME": str(home),
                "SESSION_ID": session_id,
            },
        )
        if completed.returncode != 0:
            self.fail(completed.stderr or completed.stdout)

    def test_resume_plugin_transport_restart_drops_the_prompt(self):
        node = shutil.which("node")
        if node is None:
            self.skipTest("node is not installed; resume decision was not executed")
        plugin = Path(__file__).with_name("genesis_session_resume_plugin.mjs")
        home = Path(self.temp.name) / "restart-home"
        session_id = "session-restart"
        log = home / "sessions" / "proj" / session_id / "session.v3.jsonl"
        log.parent.mkdir(parents=True)
        log.write_text('{"type":"step/start","seq":1,"data":{"turn":2}}\n', encoding="utf-8")
        script = r"""
import { apply } from process.env.PLUGIN_PATH;

const calls = [];
const agents = {
  async create() {
    calls.push("create");
    return { created: true };
  },
  async resume() {
    const agent = {
      followup(message) {
        calls.push(["followup", message && message.id]);
      },
      wakeDriver() {
        calls.push("wake");
      },
      async preStep() {
        calls.push("preStep");
        return { kind: "enter", messages: [] };
      },
      session: {
        append(type, data) {
          calls.push(["append", type, data && data.id]);
          return { seq: 1 };
        },
      },
    };
    return { agent };
  },
};
apply({ agents });
const handle = await agents.create({ sessionId: process.env.SESSION_ID });
handle.agent.followup({ id: "user-1", role: "user", content: [{ type: "text", text: "keep going" }] });
handle.agent.session.append("turn/end", { reason: { kind: "completed" } });
await new Promise((resolve) => queueMicrotask(resolve));
if (calls.includes("create")) {
  console.error(JSON.stringify(calls));
  process.exit(1);
}
const followups = calls.filter((call) => Array.isArray(call) && call[0] === "followup");
if (followups.length !== 1 || followups[0][1] !== "user-1") {
  console.error(JSON.stringify({ followups, calls }));
  process.exit(1);
}
if (!calls.includes("wake")) {
  console.error(JSON.stringify(calls));
  process.exit(1);
}
"""
        module = Path(self.temp.name) / "restart-check.mjs"
        module.write_text(
            script.replace("process.env.PLUGIN_PATH", json.dumps(plugin.as_uri())),
            encoding="utf-8",
        )
        completed = subprocess.run(
            [node, str(module)],
            check=False,
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "DSH_HOME": str(home),
                "SESSION_ID": session_id,
                "GENESIS_TRANSPORT_RESTART": "1",
            },
        )
        if completed.returncode != 0:
            self.fail(completed.stderr or completed.stdout)

    def test_resume_plugin_transport_restart_idle_turn_acks_prompt(self):
        node = shutil.which("node")
        if node is None:
            self.skipTest("node is not installed; resume decision was not executed")
        plugin = Path(__file__).with_name("genesis_session_resume_plugin.mjs")
        home = Path(self.temp.name) / "idle-restart-home"
        session_id = "session-idle-restart"
        log = home / "sessions" / "proj" / session_id / "session.v3.jsonl"
        log.parent.mkdir(parents=True)
        log.write_text(
            '{"type":"turn/end","seq":9,"data":{"reason":{"kind":"completed"}}}\n'
            '{"type":"agent/inbox/spliced","seq":10,"data":{"inserted":[]}}\n',
            encoding="utf-8",
        )
        script = r"""
import { apply } from process.env.PLUGIN_PATH;

const calls = [];
const agents = {
  async create() {
    calls.push("create");
    return { created: true };
  },
  async resume() {
    const agent = {
      followup(message) {
        calls.push(["followup", message && message.id]);
      },
      wakeDriver() {
        calls.push("wake");
      },
      async preStep() {
        return { kind: "enter", messages: [] };
      },
      session: {
        append(type, data) {
          calls.push(["append", type]);
          return { seq: 1 };
        },
      },
    };
    return { agent };
  },
};
apply({ agents });
const handle = await agents.create({ sessionId: process.env.SESSION_ID });
handle.agent.followup({ id: "user-1", role: "user", content: [{ type: "text", text: " " }] });
const followups = calls.filter((call) => Array.isArray(call) && call[0] === "followup");
if (followups.length !== 1 || followups[0][1] !== "user-1") {
  console.error(JSON.stringify({ followups, calls }));
  process.exit(1);
}
if (calls.includes("wake")) {
  console.error(JSON.stringify(calls));
  process.exit(1);
}
"""
        module = Path(self.temp.name) / "idle-restart-check.mjs"
        module.write_text(
            script.replace("process.env.PLUGIN_PATH", json.dumps(plugin.as_uri())),
            encoding="utf-8",
        )
        completed = subprocess.run(
            [node, str(module)],
            check=False,
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "DSH_HOME": str(home),
                "SESSION_ID": session_id,
                "GENESIS_TRANSPORT_RESTART": "1",
            },
        )
        if completed.returncode != 0:
            self.fail(completed.stderr or completed.stdout)

    def test_dsh_resume_appends_existing_session_log(self):
        root = Path(__file__).resolve().parents[2]
        binary = (
            root
            / ".venv/lib/python3.10/site-packages/deepseek_harness_runtime/runtime/deepseek-harness-sdk-runtime-linux-x64"
        )
        if not binary.is_file():
            self.skipTest(
                "dsh runtime is not installed; live dsh process test was not run"
            )
        try:
            import deepseek_harness  # noqa: F401
        except ImportError:
            self.skipTest(
                "deepseek_harness is not installed; live dsh process test was not run"
            )

        home = Path(self.temp.name) / "live-home"
        work = Path(self.temp.name) / "live-work"
        home.mkdir()
        work.mkdir()
        session_id = "session-live"
        child = Path(self.temp.name) / "live_resume.py"
        child.write_text(
            """
import importlib.util
import sys
from pathlib import Path

runner_path, home, cwd, session_id, phase = sys.argv[1:6]
spec = importlib.util.spec_from_file_location("genesis_runner", runner_path)
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
patch = runner.write_runtime_patch(Path(home))
from deepseek_harness import DeepSeekHarness

message = "hello" if phase == "1" else "follow up"
try:
    with DeepSeekHarness(
        provider="deepseek-official",
        model="deepseek-flash",
        cwd=cwd,
        runtime_cwd=home,
        dsh_home=home,
        profile="sdk-minimal",
        patches=(str(patch),),
        api_key="invalid-test-key",
        initialize_timeout_seconds=25,
    ) as harness:
        session = harness.start_session(session_id)
        session.run(message)
except Exception as error:
    text = f"{type(error).__name__}: {error}"
    sys.stderr.write(text + "\\n")
    lowered = text.lower()
    if "already exists" in lowered or "open it instead" in lowered:
        sys.exit(2)
    sys.exit(0)
""",
            encoding="utf-8",
        )

        def spawn(phase: str) -> subprocess.Popen[str]:
            return subprocess.Popen(
                [
                    sys.executable,
                    str(child),
                    str(RUNNER_PATH),
                    str(home),
                    str(work),
                    session_id,
                    phase,
                ],
                start_new_session=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )

        def stop(proc: subprocess.Popen[str]) -> tuple[str, str]:
            if proc.poll() is None:
                try:
                    os.killpg(proc.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
            try:
                out, err = proc.communicate(timeout=15)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                out, err = proc.communicate()
            return out, err

        first = spawn("1")
        log = None
        deadline = time.time() + 40
        while time.time() < deadline:
            found = list(home.glob("sessions/*/*/session.v3.jsonl"))
            if found and found[0].stat().st_size > 0:
                log = found[0]
                break
            if first.poll() is not None:
                break
            time.sleep(0.2)
        first_out, first_err = stop(first)
        if log is None:
            self.skipTest(
                "live dsh process did not create session.v3.jsonl; append was not proved\n"
                + first_err
            )
        before = log.read_bytes()
        second = spawn("2")
        grew = False
        already = False
        deadline = time.time() + 40
        while time.time() < deadline:
            if log.stat().st_size > len(before):
                grew = True
                break
            code = second.poll()
            if code is not None:
                already = code == 2
                break
            time.sleep(0.2)
        second_out, second_err = stop(second)
        if (
            already
            or "already exists" in second_err.lower()
            or "open it instead" in second_err.lower()
        ):
            self.fail("resume still created over an existing log\n" + second_err)
        if not grew and second.returncode not in (0, -15, None):
            self.fail(
                "live dsh resume did not append\n"
                + second_err
                + "\n"
                + second_out
                + "\nfirst\n"
                + first_err
                + first_out
            )
        if not grew:
            self.skipTest(
                "live dsh process exited before appending; append was not proved\n"
                + second_err
            )
        sessions = [path for path in home.glob("sessions/*/*") if path.is_dir()]
        self.assertEqual(len(sessions), 1)
        self.assertEqual(sessions[0].name, session_id)

    def test_error_finish_returns_diagnostics_and_structured_failure(self):
        result = runner.execute(
            self.invocation(
                event={
                    "specversion": "1.0",
                    "id": "event-error",
                    "source": "urn:test",
                    "type": "dev.genesis.test",
                },
                rule="error.yaml",
                run_id="gen_error",
                env={"DEEPSEEK_API_KEY": "test-key"},
            ),
            harness_factory=ErrorHarness,
        )

        self.assertEqual(result["deepseek_session_id"], "deepseek-session-error")
        self.assertEqual(result["finish_reason"], "error")
        self.assertEqual(result["final_response"], "")
        self.assertEqual(result["error"]["type"], "DeepSeekRunError")
        self.assertIn("finish_reason='error'", result["error"]["message"])
        self.assertIn("empty final response", result["error"]["message"])
        self.assertEqual(
            result["diagnostics"]["turn_end"],
            {
                "type": "turn/end",
                "data": {
                    "reason": {
                        "kind": "error",
                        "message": "upstream unavailable",
                    }
                },
            },
        )
        self.assertEqual(
            [event["type"] for event in result["diagnostics"]["events"]],
            ["agent/error", "turn/end"],
        )
        self.assertEqual(
            result["diagnostics"]["notifications"],
            [
                {
                    "method": "runtime.warning",
                    "payload": {"message": "connection closed"},
                }
            ],
        )
        stdin = io.StringIO("{}")
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(runner, "execute", return_value=result),
        ):
            status = runner.main()
        self.assertEqual(status, 1)
        frames = [json.loads(line) for line in stdout.getvalue().splitlines()]
        self.assertEqual(frames[-1]["type"], "result")
        self.assertEqual(frames[-1]["error"]["type"], "DeepSeekRunError")

    def test_turn_end_failure_omits_empty_response_wrapper(self):
        result = runner.execute(
            self.invocation(
                event={
                    "specversion": "1.0",
                    "id": "event-turn-failure",
                    "source": "urn:test",
                    "type": "dev.genesis.test",
                },
                rule="error.yaml",
                run_id="gen_turn_failure",
                env={"DEEPSEEK_API_KEY": "test-key"},
            ),
            harness_factory=TurnFailureHarness,
        )
        self.assertEqual(result["finish_reason"], "error")
        self.assertIsNone(result["error"])
        self.assertEqual(
            result["diagnostics"]["turn_end"]["data"]["reason"]["error"]["code"], "AUTH"
        )
        self.assertEqual(result["diagnostics"]["notifications"], [])
        stdin = io.StringIO(json.dumps(self.invocation()))
        stdout = io.StringIO()
        original = runner.execute

        def wrapped(invocation, harness_factory=None, **kwargs):
            return original(invocation, harness_factory=TurnFailureHarness, **kwargs)

        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(runner, "execute", wrapped),
        ):
            status = runner.main()
        self.assertEqual(status, 1)
        frames = [json.loads(line) for line in stdout.getvalue().splitlines()]
        self.assertIsNone(frames[-1]["error"])
        self.assertEqual(frames[-1]["finish_reason"], "error")

    def test_validation_rejects_non_string_environment_values(self):
        with self.assertRaisesRegex(TypeError, "env"):
            runner.validate_invocation(self.invocation(env={"COUNT": 1}))

    def test_validation_requires_agent_home_instructions_and_dsh_home(self):
        with self.assertRaisesRegex(ValueError, "agent"):
            runner.validate_invocation(self.invocation(agent=""))
        with self.assertRaisesRegex(ValueError, "home"):
            runner.validate_invocation(self.invocation(home="relative-home"))
        with self.assertRaisesRegex(ValueError, "instructions"):
            runner.validate_invocation(self.invocation(instructions="   "))
        with self.assertRaisesRegex(ValueError, "dsh_home"):
            runner.validate_invocation(self.invocation(dsh_home="relative-dsh"))

    def test_main_emits_structured_error(self):
        stdin = io.StringIO("{}")
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(
                runner,
                "execute",
                side_effect=RuntimeError("DEEPSEEK_API_KEY is not set"),
            ),
        ):
            status = runner.main()

        self.assertEqual(status, 1)
        frames = [json.loads(line) for line in stdout.getvalue().splitlines()]
        self.assertEqual(len(frames), 1)
        self.assertEqual(frames[0]["type"], "result")
        self.assertEqual(
            frames[0]["error"],
            {
                "type": "RuntimeError",
                "message": "DEEPSEEK_API_KEY is not set",
            },
        )

    def test_emit_notification_writes_one_emit_frame(self):
        stdout = io.StringIO()
        notification = SimpleNamespace(
            method="genesis.emit",
            payload={
                "type": "com.example.note",
                "subject": "lab",
                "source": "urn:spoofed",
                "id": "spoof",
                "data": {"ok": True},
            },
        )
        with mock.patch.object(sys, "stdout", stdout):
            runner.emit_notification(notification)
        frame = json.loads(stdout.getvalue())
        self.assertEqual(frame["type"], "emit")
        self.assertEqual(frame["event"]["type"], "com.example.note")
        self.assertEqual(frame["event"]["subject"], "lab")
        self.assertEqual(frame["event"]["source"], "urn:spoofed")
        self.assertEqual(frame["event"]["id"], "spoof")
        self.assertEqual(frame["event"]["data"], {"ok": True})
        self.assertNotIn("sessionId", frame["event"])

    def test_main_forwards_genesis_emit_as_emit_frame(self):
        original = runner.execute

        def wrapped(invocation, harness_factory=None, **kwargs):
            return original(
                invocation,
                harness_factory=EmittingHarness,
                **kwargs,
            )

        stdin = io.StringIO(json.dumps(self.invocation()))
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(runner, "execute", wrapped),
        ):
            status = runner.main()
        self.assertEqual(status, 0)
        frames = [json.loads(line) for line in stdout.getvalue().splitlines()]
        emits = [frame for frame in frames if frame["type"] == "emit"]
        self.assertEqual(len(emits), 1)
        self.assertEqual(emits[0]["event"]["source"], "urn:spoofed")
        self.assertEqual(emits[0]["event"]["type"], "com.example.note")
        self.assertEqual(frames[-1]["type"], "result")

    def test_main_tees_session_created_notifications_and_one_result(self):
        original = runner.execute

        def wrapped(invocation, harness_factory=None, **kwargs):
            return original(
                invocation,
                harness_factory=NotifyingHarness,
                **kwargs,
            )

        stdin = io.StringIO(json.dumps(self.invocation()))
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(runner, "execute", wrapped),
        ):
            status = runner.main()

        self.assertEqual(status, 0)
        frames = [json.loads(line) for line in stdout.getvalue().splitlines()]
        self.assertEqual(frames[0]["type"], "session.created")
        self.assertEqual(frames[0]["session_id"], "deepseek-session-1")
        self.assertEqual(frames[1]["type"], "notification")
        self.assertEqual(frames[1]["method"], "session.event")
        self.assertEqual(frames[2]["type"], "notification")
        self.assertEqual(frames[2]["method"], "session.status")
        self.assertEqual(frames[3]["type"], "result")
        self.assertEqual(frames[3]["finish_reason"], "completed")
        self.assertEqual(sum(1 for frame in frames if frame["type"] == "result"), 1)
        self.assertTrue(Path(self.dsh_home).is_dir())

    def test_official_sdk_and_runtime_versions_are_paired(self):
        root = Path(__file__).resolve().parents[2]
        pyproject = (root / "pyproject.toml").read_text(encoding="utf-8")
        dependencies = re.search(
            r"(?ms)^dependencies\s*=\s*\[(.*?)^\]",
            pyproject,
        )
        self.assertIsNotNone(dependencies)
        pins = dict(
            re.findall(
                r'"([^"]+)==([^"]+)"',
                dependencies.group(1),
            )
        )
        self.assertEqual(
            pins,
            {
                "deepseek-harness-runtime-bin": "0.1.5rc1",
                "deepseek-harness-sdk": "0.1.5rc1",
            },
        )

    def test_supplied_dsh_home_is_not_agent_home(self):
        with tempfile.TemporaryDirectory() as agent_home:
            runner.execute(
                self.invocation(home=agent_home),
                harness_factory=FakeHarness,
            )
            harness = FakeHarness.instances[0]
            self.assertNotEqual(harness.kwargs["dsh_home"], agent_home)
            self.assertTrue(Path(agent_home).is_dir())
            self.assertTrue(Path(harness.kwargs["dsh_home"]).is_dir())


if __name__ == "__main__":
    unittest.main()
