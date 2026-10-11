# Genesis

Genesis is a local listener that turns a [CloudEvent](https://cloudevents.io/) into one pinned [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) session for the agent a YAML rule names. Rules match event attributes exactly. `subject` is the exception: a `*` matches one non-empty path segment, so `worker/*` matches `worker/run-1` and does not match `worker/run-1/extra`.

A run does not need an outside sender. `genesis launch` serves the matcher. The first cause can be `genesis schedule`, `genesis job`, a control message, or an agent posting to `POST /events`. When a session ends, the listener feeds `dev.genesis.agent.finished` into the same matcher, so a rule such as the shipped oracle can start the next run. Schedules and job exits come back through that matcher too. During a turn the runner can dispatch `genesis.emit` in-process; it stamps id and source and refuses `dev.genesis.agent.finished`.

There is no optimizer that rewrites prompts, rules, or weights. Self-improvement is the oracle review and continue path, plus agents editing YAML that stays inactive until an authorized `POST /sync`. The designer may edit that YAML; the bearer stays in `genesis control`.

Sessions use the official Python SDK, standalone `sdk-minimal` profile. The lockfile pins `deepseek-harness-sdk` and `deepseek-harness-runtime-bin` to `0.1.5rc1`; the runtime wheel supplies `dsh`.

## Run

```sh
genesis launch -listen 127.0.0.1:8787 -agents agents.d -rules rules.d -data genesis-data
```

That serves the matcher on `127.0.0.1:8787`. A missing `repos.d` stays inactive. An empty sync token disables `/sync`. The next step is in the [quickstart](docs/quickstart.md). Flags are in the [CLI](docs/cli.md).

With no external sender, start a tick or a command that posts when it exits:

```sh
genesis schedule --each=15m --emit '{"type":"dev.genesis.schedule.fired"}'
genesis job start --emit '{"type":"dev.genesis.job.exited"}' -- ./your-command
```

The schedule waits one interval, then posts on a loop (source `urn:genesis:schedule`, from one minute to 168 hours). The job stays up after the turn and posts on exit. Jobs and schedules cannot emit finish, `dev.genesis.session.continue`, or `dev.genesis.publication.submitted`.

The oracle matches finishes except the agent that just finished, writes a report, and files it with `genesis publish` only if `setup.groups` contains `reporter`. If the exit criterion is unmet, it is instructed to post `dev.genesis.session.continue` on the same session. Continuations stop at hop 9. A transport failure may resume the same session at most three times.

## Docs

- [Quickstart](docs/quickstart.md)
- [Build](docs/build.md)
- [CLI](docs/cli.md)
- [Agents and rules](docs/agents.md)
- [HTTP](docs/http.md)
- [Run](docs/run.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Contributing](CONTRIBUTING.md)

Also: [one run](docs/examples/one-run.md), [orchestrator](docs/orchestrator.md), [designer](docs/designer.md), [control](docs/control.md), [GitHub webhooks](docs/webhooks.md), [repositories](docs/repositories.md), [providers](docs/providers.md), [publications](docs/publications.md).
