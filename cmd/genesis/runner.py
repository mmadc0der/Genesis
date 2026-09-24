"""Run one Genesis invocation through the official DeepSeek Harness SDK."""

from __future__ import annotations

import json
import os
import sys
from collections.abc import Callable, Iterator, Mapping
from contextlib import contextmanager
from pathlib import Path
from typing import Any

# This row and id are coupled to the pinned 0.1.5rc1 sdk-minimal profile.
SESSION_LOG_OFF_PATCH = """\
- id: session-log-deepseek
  name: '@deepseek-ai/dsh-session-log-deepseek'
  config:
    enabled: false
"""

MAX_STDOUT_FRAME_BYTES = 16 * 1024 * 1024
SESSION_CREATED = "session.created"
NOTIFICATION = "notification"
RESULT = "result"


@contextmanager
def complete_environment(environment: Mapping[str, str]) -> Iterator[None]:
    previous = os.environ.copy()
    os.environ.clear()
    os.environ.update(environment)
    try:
        yield
    finally:
        os.environ.clear()
        os.environ.update(previous)


def validate_invocation(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise TypeError("invocation must be a JSON object")
    if not isinstance(value.get("event"), dict):
        raise TypeError("event must be a JSON object")
    for field in (
        "rule",
        "agent",
        "run_id",
        "cwd",
        "home",
        "instructions",
        "dsh_home",
    ):
        if not isinstance(value.get(field), str) or not value[field]:
            raise ValueError(f"{field} must be a non-empty string")
    if not str(value["instructions"]).strip():
        raise ValueError("instructions must be a non-empty string")
    for field in ("cwd", "home", "dsh_home"):
        if not Path(value[field]).is_absolute():
            raise ValueError(f"{field} must be absolute")
    run_dir = value.get("run_dir")
    if run_dir is not None:
        if not isinstance(run_dir, str) or not run_dir:
            raise ValueError("run_dir must be a non-empty string")
        if not Path(run_dir).is_absolute():
            raise ValueError("run_dir must be absolute")
    environment = value.get("env")
    if not isinstance(environment, dict) or not all(
        isinstance(key, str) and isinstance(item, str)
        for key, item in environment.items()
    ):
        raise TypeError("env must be an object of string values")
    return value


def execute(
    invocation: dict[str, Any],
    harness_factory: Callable[..., Any] | None = None,
    on_session_created: Callable[[str], None] | None = None,
    on_notification: Callable[[Any], None] | None = None,
) -> dict[str, Any]:
    invocation = validate_invocation(invocation)
    if harness_factory is None:
        from deepseek_harness import DeepSeekHarness

        harness_factory = DeepSeekHarness

    # Standing identity is DSH_SYSTEM_PROMPT (sdk-minimal personaPrefix hook).
    # The user message stays the compact CloudEvent JSON and is not prepended.
    # GITHUB_TOKEN and SSH_AUTH_SOCK come only from the process environment
    # that root set for this run. Invocation JSON cannot supply them.
    delivered: dict[str, str] = {}
    for key in ("SSH_AUTH_SOCK", "GITHUB_TOKEN"):
        value = os.environ.get(key) or ""
        if value:
            delivered[key] = value
    environment = dict(invocation["env"])
    for key in (
        "GITHUB_TOKEN",
        "GH_TOKEN",
        "SSH_AUTH_SOCK",
        "GIT_SSH",
        "GIT_SSH_COMMAND",
        "SSH_AGENT_PID",
    ):
        environment.pop(key, None)
    environment["HOME"] = invocation["home"]
    environment["DSH_SYSTEM_PROMPT"] = invocation["instructions"]
    environment.update(delivered)

    message = json.dumps(
        invocation["event"],
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    )
    dsh_home = invocation["dsh_home"]
    home_path = Path(dsh_home)
    if not home_path.is_dir():
        raise ValueError("dsh_home must be an existing directory")
    if home_path.resolve() == Path(invocation["home"]).resolve():
        raise ValueError("dsh_home must be distinct from agent home")
    if home_path.resolve() == Path(invocation["cwd"]).resolve():
        raise ValueError("dsh_home must be distinct from cwd")

    patch_path = home_path / "session-log-off.patch.yml"
    patch_path.write_text(SESSION_LOG_OFF_PATCH, encoding="utf-8")
    with (
        complete_environment(environment),
        harness_factory(
            provider="deepseek-official",
            model="deepseek-flash",
            cwd=invocation["cwd"],
            runtime_cwd=dsh_home,
            dsh_home=dsh_home,
            profile="sdk-minimal",
            patches=(str(patch_path),),
        ) as harness,
    ):
        session = harness.start_session()
        if on_session_created is not None:
            on_session_created(session.id)
        result = session.run(message, on_notification=on_notification)

    finish_reason = result.finish_reason
    final_response = result.final_response
    if finish_reason == "completed":
        error = None
        diagnostics = None
    else:
        error_type = (
            "DeepSeekRunError" if finish_reason == "error" else "DeepSeekRunIncomplete"
        )
        reason = repr(finish_reason)
        empty_response = " with an empty final response" if not final_response else ""
        error = {
            "type": error_type,
            "message": (
                f"DeepSeek run finished with finish_reason={reason}{empty_response}"
            ),
        }
        diagnostics = extract_diagnostics(result)

    return {
        "deepseek_session_id": result.session_id,
        "finish_reason": finish_reason,
        "final_response": final_response,
        "error": error,
        "diagnostics": diagnostics,
    }


def extract_diagnostics(result: Any) -> dict[str, Any]:
    events = [
        event
        for event in (getattr(result, "events", None) or [])
        if isinstance(event, dict)
    ]
    turn_end = next(
        (event for event in reversed(events) if event.get("type") == "turn/end"),
        None,
    )
    interesting = [
        event
        for event in events
        if isinstance(event.get("type"), str)
        and ("error" in event["type"].lower() or event["type"] == "turn/end")
    ]
    selected_events = interesting if interesting else events[-8:]

    notifications = []
    for notification in getattr(result, "notifications", None) or []:
        method = getattr(notification, "method", None)
        if method != "session.event":
            notifications.append(
                {
                    "method": method,
                    "payload": getattr(notification, "payload", None),
                }
            )
    return {
        "turn_end": turn_end,
        "events": selected_events,
        "notifications": notifications,
    }


def emit_frame(obj: dict[str, Any]) -> None:
    line = json.dumps(
        obj,
        ensure_ascii=False,
        separators=(",", ":"),
        default=str,
    )
    encoded = (line + "\n").encode("utf-8")
    if len(encoded) > MAX_STDOUT_FRAME_BYTES:
        sys.stderr.write("skipped oversized stdout frame\n")
        sys.stderr.flush()
        return
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def emit_notification(notification: Any) -> None:
    try:
        emit_frame(
            {
                "v": 1,
                "type": NOTIFICATION,
                "method": getattr(notification, "method", None),
                "payload": getattr(notification, "payload", None),
            }
        )
    except Exception as error:  # noqa: BLE001 - tee must not fail the run.
        sys.stderr.write(f"skipped notification: {error}\n")
        sys.stderr.flush()


def result_frame(output: dict[str, Any]) -> dict[str, Any]:
    return {"v": 1, "type": RESULT, **output}


def main() -> int:
    output: dict[str, Any] | None = None
    try:
        invocation = json.load(sys.stdin)
        run_id = invocation.get("run_id") if isinstance(invocation, dict) else None

        def on_session_created(session_id: str) -> None:
            emit_frame(
                {
                    "v": 1,
                    "type": SESSION_CREATED,
                    "run_id": run_id,
                    "session_id": session_id,
                }
            )

        output = execute(
            invocation,
            on_session_created=on_session_created,
            on_notification=emit_notification,
        )
        status = 0 if output["error"] is None else 1
    except Exception as error:  # noqa: BLE001 - return process errors to Go.
        output = {
            "deepseek_session_id": None,
            "finish_reason": None,
            "final_response": None,
            "diagnostics": None,
            "error": {
                "type": type(error).__name__,
                "message": str(error),
            },
        }
        status = 1

    emit_frame(result_frame(output))
    return status


if __name__ == "__main__":
    raise SystemExit(main())
