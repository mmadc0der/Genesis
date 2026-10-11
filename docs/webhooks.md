# Webhooks

An outside system can start a Genesis run without posting a CloudEvent
itself. It delivers a webhook. A connector checks that delivery and maps it
to one CloudEvent. The listener then uses the same matcher as
`POST /events`. A rule names the agent. The vendor stays outside the loop.

That is the cross-source path. GitHub, or any later source, is only a
connector in front of the matcher. Agents and rules do not speak the
vendor's payload. They match `type`, `source`, and `subject` the same way
as a schedule, a job exit, or a hand-posted event. See
[agents.md](agents.md) and [http.md](http.md).

A connector does not create the remote subscription, mint a credential for
the run, or clone a workspace. Those are other layers
([providers.md](providers.md), [repositories.md](repositories.md)).

## What a connector does

Each source has its own listener route. Verification stays in root
`launch`. The listener forwards the raw body and the signature header over
the private socketpair. The secret is not copied into the listener, the
control process, or an agent. Standalone `listen` cannot verify.

A bad signature is rejected before a rule runs and before a delivery record
or a run journal is written. A delivery id that was already recorded is
acknowledged and does not start a second run. An event the connector does
not care about is acknowledged and starts nothing. While a sync is in
progress the response is `503` and the id is not recorded.

A delivery that passes becomes one CloudEvents 1.0 object. `source` is a
stable URN for that connector, so rules can tell platforms apart. `data`
carries the fields the connector chose to expose. It does not carry the
webhook secret, a token, or the raw payload. No payload field is
interpolated into a command.

A match returns `202` with the same run list as `POST /events`. No matching
rule returns `204` after the delivery id is recorded. Compose does not
publish the listener port, and the control process does not proxy webhook
routes.

## GitHub

GitHub is the connector that exists today. The route is
`POST /webhooks/github` on the listener (default `127.0.0.1:8787`).

The secret file is `GITHUB_APP_WEBHOOK_SECRET` in the root secrets
directory (`genesis-secrets`, or `runtime/secrets` in Compose). Placement
and mode are in [build.md](build.md). The bytes are the exact secret, with
no trailing newline. This name is not a provider `secret` reference. A
missing file, or a file that is not a private regular file, fails closed.
Deploy-key registration still reads only `GITHUB_APP_RECONCILER_PEM`.

`X-Hub-Signature-256` must be `sha256=` plus 64 lowercase hex characters.
Genesis computes HMAC-SHA256 over the raw body. A missing, malformed, or
mismatched signature is `401` `signature rejected`. A well-formed signature
when verification cannot run is `503` `webhook verification is unavailable`.

The body is JSON, `Content-Type: application/json`, at most 512 KiB.
`X-GitHub-Delivery` becomes the CloudEvent `id`. An empty or non-GUID value
is `400` and starts nothing. After the signature, the event, and the
repository binding all pass, Genesis records the id under
`-data/deliveries/<id>` before it runs rules. The file contains only the
event name. If the id cannot be recorded, the response is `500`
`delivery was not recorded` and no run starts.

Accepted `X-GitHub-Event` values are `issues`, `pull_request`, `push`, and
`check_run`. Any other event, including `ping`, returns `200` and starts
nothing. Dispatch also requires the payload `repository.id` to equal
`repository_id` on a binding in `-data/observations/repositories.json`,
which an adopted repository sync persists. An unknown repository returns
`200` and starts nothing. A journal that cannot be read is `500`.

The mapped event uses `source` `urn:genesis:github`. `type` is
`dev.genesis.github.issues`, `dev.genesis.github.pull_request`,
`dev.genesis.github.push`, or `dev.genesis.github.check_run`. `subject` is
the event name (`issues`, `pull_request`, `push`, or `check_run`). `action`
is set when the payload has one (`push` has none). `repositoryid` is the
decimal repository id. `data` repeats `event`, `repository_id`, and
`action` when an action is present.

```yaml
match:
  type: dev.genesis.github.issues
  source: urn:genesis:github
  subject: issues
  action: opened
agent: workspace-janitor
```

In the GitHub App, set the webhook URL to the reachable
`POST /webhooks/github`, use the same secret bytes, and subscribe to
Issues, Pull requests, Pushes, and Check runs. Saving the webhook sends
`ping`, which Genesis acknowledges without starting a run.
