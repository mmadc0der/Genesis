import importlib.util
import io
import json
import os
import re
import sys
import tempfile
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
        self.assertEqual(
            harness.patch_contents,
            """\
- id: session-log-deepseek
  name: '@deepseek-ai/dsh-session-log-deepseek'
  config:
    enabled: false
- insert:
    - id: genesis-assistant-stream
      name: """
            + json.dumps(str(plugin_path))
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

    def test_execute_uses_supplied_dsh_home_and_start_session(self):
        created = []
        result = runner.execute(
            self.invocation(),
            harness_factory=FakeHarness,
            on_session_created=created.append,
        )
        self.assertEqual(result["deepseek_session_id"], "deepseek-session-1")
        self.assertEqual(created, ["deepseek-session-1"])
        self.assertEqual(FakeHarness.instances[0].kwargs["dsh_home"], self.dsh_home)
        self.assertTrue(Path(self.dsh_home).is_dir())

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
