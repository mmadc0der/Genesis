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
from typing import ClassVar
from unittest import mock

RUNNER_PATH = Path(__file__).with_name("runner.py")
SPEC = importlib.util.spec_from_file_location("genesis_runner", RUNNER_PATH)
assert SPEC is not None and SPEC.loader is not None
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)

INSTRUCTIONS = "You are the workspace janitor for this lab."
AGENT_HOME = "/tmp/genesis-agent-home"


class FakeHarness:
    instances: ClassVar[list] = []

    def __init__(self, **kwargs):
        self.kwargs = kwargs
        self.environment = dict(os.environ)
        self.message = None
        self.home_existed = False
        self.patch_existed = False
        self.patch_contents = None
        self.__class__.instances.append(self)

    def __enter__(self):
        self.home_existed = Path(self.kwargs["dsh_home"]).is_dir()
        patch_path = Path(self.kwargs["patches"][0])
        self.patch_existed = patch_path.is_file()
        self.patch_contents = patch_path.read_text(encoding="utf-8")
        return self

    def __exit__(self, _kind, _error, _traceback):
        return None

    def run(self, message):
        self.message = message
        return SimpleNamespace(
            session_id="deepseek-session-1",
            finish_reason="completed",
            final_response="finished",
        )


class ErrorHarness(FakeHarness):
    def run(self, message):
        self.message = message
        return SimpleNamespace(
            session_id="deepseek-session-error",
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

    def test_execute_uses_complete_event_environment_and_sdk_minimal(self):
        workspace = str(Path.cwd().resolve())
        event = {
            "specversion": "1.0",
            "id": "event-1",
            "source": "urn:test",
            "type": "dev.genesis.test",
            "data": {"unicode": "Привет", "nested": [1, True]},
        }
        invocation = sample_invocation(event=event, cwd=workspace)
        with mock.patch.dict(
            os.environ,
            {
                "DEEPSEEK_API_KEY": "ambient-key-must-not-win",
                "AMBIENT_ONLY": "must-not-leak",
                "HOME": "/ambient/home",
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
                "model": "deepseek-v4-flash",
                "cwd": workspace,
                "profile": "sdk-minimal",
            },
        )
        self.assertEqual(
            harness.kwargs["dsh_home"],
            harness.kwargs["runtime_cwd"],
        )
        self.assertNotEqual(harness.kwargs["dsh_home"], AGENT_HOME)
        self.assertNotEqual(harness.kwargs["dsh_home"], workspace)
        self.assertTrue(harness.home_existed)
        self.assertFalse(Path(harness.kwargs["dsh_home"]).exists())
        self.assertEqual(harness.environment["DEEPSEEK_API_KEY"], "genesis-key")
        self.assertNotIn("AMBIENT_ONLY", harness.environment)

    def test_execute_sets_home_and_system_prompt_from_agent_snapshot(self):
        invocation = sample_invocation(
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
            sample_invocation(
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
            sample_invocation(
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
        self.assertEqual(
            harness.patch_contents,
            """\
- id: session-log-deepseek
  name: '@deepseek-ai/dsh-session-log-deepseek'
  config:
    enabled: false
""",
        )
        self.assertFalse(patch_path.exists())

    def test_error_finish_returns_diagnostics_and_structured_failure(self):
        result = runner.execute(
            sample_invocation(
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
        self.assertEqual(json.loads(stdout.getvalue()), result)

    def test_validation_rejects_non_string_environment_values(self):
        with self.assertRaisesRegex(TypeError, "env"):
            runner.validate_invocation(sample_invocation(env={"COUNT": 1}))

    def test_validation_requires_agent_home_and_instructions(self):
        with self.assertRaisesRegex(ValueError, "agent"):
            runner.validate_invocation(sample_invocation(agent=""))
        with self.assertRaisesRegex(ValueError, "home"):
            runner.validate_invocation(sample_invocation(home="relative-home"))
        with self.assertRaisesRegex(ValueError, "instructions"):
            runner.validate_invocation(sample_invocation(instructions="   "))

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
        self.assertEqual(
            json.loads(stdout.getvalue()),
            {
                "deepseek_session_id": None,
                "finish_reason": None,
                "final_response": None,
                "diagnostics": None,
                "error": {
                    "type": "RuntimeError",
                    "message": "DEEPSEEK_API_KEY is not set",
                },
            },
        )

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

    def test_temporary_dsh_home_is_not_agent_home(self):
        with tempfile.TemporaryDirectory() as agent_home:
            runner.execute(
                sample_invocation(home=agent_home),
                harness_factory=FakeHarness,
            )
            harness = FakeHarness.instances[0]
            self.assertNotEqual(harness.kwargs["dsh_home"], agent_home)
            self.assertTrue(Path(agent_home).is_dir())
            self.assertFalse(Path(harness.kwargs["dsh_home"]).exists())


if __name__ == "__main__":
    unittest.main()
