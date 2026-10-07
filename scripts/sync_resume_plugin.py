#!/usr/bin/env python3
from pathlib import Path

root = Path(__file__).resolve().parents[1]
mjs = (root / "cmd/genesis/genesis_session_resume_plugin.mjs").read_text(encoding="utf-8")
runner_path = root / "cmd/genesis/runner.py"
runner = runner_path.read_text(encoding="utf-8")
marker = 'RESUME_PLUGIN = r"""'
start = runner.index(marker)
end = runner.index('"""', start + len(marker)) + 3
runner_path.write_text(
    runner[:start] + marker + mjs + '"""' + runner[end:],
    encoding="utf-8",
)
print(f"synced {len(mjs)} bytes into runner.py")
