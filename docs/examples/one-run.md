# One run

This is an illustration of the contract in [agents.md](../agents.md) and
[http.md](../http.md). It does not add a matcher, a journal file, or a
status code.

The files below are the shared-UID shape from the live check: no `user`,
so non-root `listen` can load them. The committed
`agents.d/workspace-janitor.yaml` adds `user`, `home: /home/workspace-janitor`,
and `setup.workspace: private`. Non-root launch refuses that file. Its rule
is `rules.d/example.yaml` (`type: dev.genesis.run`, `source:
urn:genesis:example`, `subject: hello`).

## Agent

`workspace-janitor.yaml`:

```yaml
instructions: |
  You are the workspace janitor for this lab.
  Read the CloudEvent, do only the work it asks, write results in the workspace, then stop.
cwd: /tmp/genesis-live-work
home: /tmp/genesis-live-home
env:
  LANG: C.UTF-8
  PATH: /usr/local/bin:/usr/bin:/bin
```

## Rule

`live.yaml`:

```yaml
match:
  type: dev.genesis.live
agent: workspace-janitor
```

## CloudEvent

`POST /events` with `Content-Type: application/cloudevents+json`:

```json
{
  "specversion": "1.0",
  "id": "live-1",
  "source": "urn:genesis:live",
  "type": "dev.genesis.live",
  "data": {"task": "Create hello.txt containing hello, then report success."}
}
```

A match returns `202` and one object per rule. The documented body, copied
from [http.md](../http.md), is:

```json
{
  "runs": [
    {
      "rule": "example.yaml",
      "agent": "workspace-janitor",
      "run_id": "gen_0123456789abcdef0123456789abcdef"
    }
  ]
}
```

That sample is the committed `example.yaml` rule, not `live.yaml`. `rule` in
that object is the rule filename. The hex in `run_id` is the documented
shape, not a fixed id.

## Journal files

Under `-data` (here `/tmp/genesis-live-data`), after the run is accepted:

```
sessions/<root gen_>/dsh_home/
runs/gen_<hex>/events.jsonl
runs/gen_<hex>/stderr.log
runs/gen_<hex>/result.json
runs/gen_<hex>/session.json
```

`queue.json` appears on a run that is waiting because that agent is already
at `max_parallel`. Lifecycle types and the rule that the run directory
exists before `202` are in [http.md](../http.md).
