# Contributing

The repository owner reviews and merges pull requests. There is no reviewer
pool, `CODEOWNERS` file, or required CI approval gate.

Check the change with the commands already used for credential-free
verification. Build prerequisites are in [docs/build.md](docs/build.md).

```sh
uv lock --check
uv sync --locked
test -z "$(gofmt -l cmd/genesis/*.go)"
uv run --locked ruff format --check cmd/genesis/runner.py cmd/genesis/test_runner.py
uv run --locked ruff check cmd/genesis/runner.py cmd/genesis/test_runner.py
go vet ./...
go test ./...
uv run --locked python -m unittest discover -s cmd/genesis -p 'test_*.py'
npm ci --prefix web
npm test --prefix web
npm run check --prefix web
```

`go test -race ./...` and `npm run build --prefix web` are in the same
[docs/build.md](docs/build.md) list when the change touches concurrency or
the UI. First start is [docs/quickstart.md](docs/quickstart.md). The
contract index is [docs/run.md](docs/run.md). Log locations and the live
check are in [docs/troubleshooting.md](docs/troubleshooting.md).

## Issues

Open a GitHub issue. A bug report should say what you ran, what you expected, and what happened. A feature request can be the same issue type: state the problem and the constraint.

Write reproduction as numbered steps a second person can run:

- the command and flags (`genesis listen`, `genesis launch`, or `genesis control`)
- the CloudEvent body if the bug is on `POST /events`
- the agent and rule YAML, with secret values removed
- the commit you built

Attach logs, not a screenshot of them. Paths and the failure modes around
them are in [docs/troubleshooting.md](docs/troubleshooting.md):

- the listener's stdout (JSON `slog` lines)
- the run directory under `-data` (default `genesis-data`): `events.jsonl`, `stderr.log`, and `result.json` for the `gen_` id
- if you used Compose, `docker compose logs` for the `genesis` and `control` services

The control process reads those journals at `http://127.0.0.1:8790/`
(`GET /api/runs/{id}/events`). It does not keep a separate log of the run.