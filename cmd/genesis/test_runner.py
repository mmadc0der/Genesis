import importlib.util
import io
import json
import os
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


RUNNER_PATH = Path(__file__).with_name("runner.py")
SPEC = importlib.util.spec_from_file_location("genesis_runner", RUNNER_PATH)
assert SPEC is not None and SPEC.loader is not None
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


class FakeHarness:
    instances = []

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
        invocation = {
            "event": event,
            "rule": "test.yaml",
            "run_id": "gen_test",
            "cwd": workspace,
            "env": {
                "DEEPSEEK_API_KEY": "test-key",
                "ONLY_DECLARED": "yes",
            },
        }
        environment_before = dict(os.environ)

        result = runner.execute(invocation, harness_factory=FakeHarness)

        self.assertEqual(
            result,
            {
                "deepseek_session_id": "deepseek-session-1",
                "finish_reason": "completed",
                "final_response": "finished",
                "error": None,
            },
        )
        harness = FakeHarness.instances[0]
        self.assertEqual(json.loads(harness.message), event)
        self.assertEqual(harness.environment, invocation["env"])
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
        self.assertTrue(harness.home_existed)
        self.assertFalse(Path(harness.kwargs["dsh_home"]).exists())
        self.assertEqual(dict(os.environ), environment_before)

    def test_execute_supplies_version_coupled_session_log_privacy_patch(self):
        runner.execute(
            {
                "event": {
                    "specversion": "1.0",
                    "id": "event-privacy",
                    "source": "urn:test",
                    "type": "dev.genesis.test",
                },
                "rule": "privacy.yaml",
                "run_id": "gen_privacy",
                "cwd": str(Path.cwd().resolve()),
                "env": {"DEEPSEEK_API_KEY": "test-key"},
            },
            harness_factory=FakeHarness,
        )

        harness = FakeHarness.instances[0]
        patch_path = (
            Path(harness.kwargs["dsh_home"]) / "session-log-off.patch.yml"
        )
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

    def test_validation_rejects_non_string_environment_values(self):
        with self.assertRaisesRegex(ValueError, "env"):
            runner.validate_invocation(
                {
                    "event": {},
                    "rule": "test.yaml",
                    "run_id": "gen_test",
                    "cwd": str(Path.cwd().resolve()),
                    "env": {"COUNT": 1},
                }
            )

    def test_main_emits_structured_error(self):
        stdin = io.StringIO("{}")
        stdout = io.StringIO()
        with (
            mock.patch.object(sys, "stdin", stdin),
            mock.patch.object(sys, "stdout", stdout),
            mock.patch.object(runner, "execute", side_effect=RuntimeError("boom")),
        ):
            status = runner.main()

        self.assertEqual(status, 1)
        self.assertEqual(
            json.loads(stdout.getvalue()),
            {
                "deepseek_session_id": None,
                "finish_reason": None,
                "final_response": None,
                "error": {"type": "RuntimeError", "message": "boom"},
            },
        )

    def test_official_sdk_and_runtime_versions_are_paired(self):
        requirements = (
            Path(__file__).resolve().parents[2] / "requirements.txt"
        ).read_text(encoding="utf-8")
        pins = dict(
            line.split("==", 1)
            for line in requirements.splitlines()
            if line.strip()
        )
        self.assertEqual(pins["deepseek-harness-sdk"], "0.1.5rc1")
        self.assertEqual(
            pins["deepseek-harness-runtime-bin"],
            pins["deepseek-harness-sdk"],
        )


if __name__ == "__main__":
    unittest.main()
