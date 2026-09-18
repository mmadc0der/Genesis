#!/usr/bin/env python3
"""Run one Genesis invocation through the official DeepSeek Harness SDK."""

from __future__ import annotations

import json
import os
import sys
import tempfile
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
        raise ValueError("invocation must be a JSON object")
    if not isinstance(value.get("event"), dict):
        raise ValueError("event must be a JSON object")
    for field in ("rule", "run_id", "cwd"):
        if not isinstance(value.get(field), str) or not value[field]:
            raise ValueError(f"{field} must be a non-empty string")
    if not Path(value["cwd"]).is_absolute():
        raise ValueError("cwd must be absolute")
    environment = value.get("env")
    if not isinstance(environment, dict) or not all(
        isinstance(key, str) and isinstance(item, str)
        for key, item in environment.items()
    ):
        raise ValueError("env must be an object of string values")
    return value


def execute(
    invocation: dict[str, Any],
    harness_factory: Callable[..., Any] | None = None,
) -> dict[str, Any]:
    invocation = validate_invocation(invocation)
    if harness_factory is None:
        from deepseek_harness import DeepSeekHarness

        harness_factory = DeepSeekHarness

    message = json.dumps(
        invocation["event"],
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    )
    with tempfile.TemporaryDirectory(prefix=f"{invocation['run_id']}-") as dsh_home:
        patch_path = Path(dsh_home) / "session-log-off.patch.yml"
        patch_path.write_text(SESSION_LOG_OFF_PATCH, encoding="utf-8")
        with complete_environment(invocation["env"]):
            with harness_factory(
                provider="deepseek-official",
                model="deepseek-v4-flash",
                cwd=invocation["cwd"],
                runtime_cwd=dsh_home,
                dsh_home=dsh_home,
                profile="sdk-minimal",
                patches=(str(patch_path),),
            ) as harness:
                result = harness.run(message)

    finish_reason = result.finish_reason
    final_response = result.final_response
    if finish_reason == "completed":
        error = None
        diagnostics = None
    else:
        error_type = (
            "DeepSeekRunError"
            if finish_reason == "error"
            else "DeepSeekRunIncomplete"
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
        and (
            "error" in event["type"].lower()
            or event["type"] == "turn/end"
        )
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


def main() -> int:
    try:
        invocation = json.load(sys.stdin)
        output = execute(invocation)
        status = 0 if output["error"] is None else 1
    except Exception as error:  # The Go parent records this structured failure.
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

    json.dump(
        output,
        sys.stdout,
        ensure_ascii=False,
        separators=(",", ":"),
        default=str,
    )
    sys.stdout.write("\n")
    sys.stdout.flush()
    return status


if __name__ == "__main__":
    raise SystemExit(main())
