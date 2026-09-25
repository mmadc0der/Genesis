# GitHub App webhook ingress

Genesis accepts GitHub App webhook deliveries on the listener and, for an
adopted repository, turns one delivery into one CloudEvent. The existing rule
matcher then starts runs. This path does not create repository webhooks, mint
a per-run App token, or clone a workspace.

## Endpoint

`POST /webhooks/github` on the listener (default `127.0.0.1:8787`).

GitHub must be able to reach that path. Compose does not publish the listener
port. The control process does not proxy this route and does not receive the
webhook secret.

The body is the raw JSON payload, `Content-Type: application/json`, at most
512 KiB. GitHub signs that raw body.

## Signature

`X-Hub-Signature-256` must be `sha256=` plus 64 lowercase hex characters.
Genesis computes HMAC-SHA256 over the raw body and compares the digest with
`hmac.Equal`. A missing, malformed, or mismatched signature is `401`
`signature rejected` before any rule runs and before a delivery record or run
journal is written.

Root `launch` holds the secret. The listener sends the body and header to the
root coordinator over the private socketpair. The secret is not copied into
the listener, control, or an agent. Standalone `listen` cannot verify and
returns `503` `webhook verification is unavailable` for a well-formed
signature.

## Secret file

The webhook secret is a root-only file beside the reconciler PEM:

`/var/lib/genesis/secrets/GITHUB_APP_WEBHOOK_SECRET`

The secrets directory is mode `0700`. The file is mode `0600`, owned by root.
Local `-secrets` (default `genesis-secrets`) uses the same filename. The file
contents are the exact secret bytes. Do not add a trailing newline.

```sh
umask 077
printf '%s' "$GITHUB_APP_WEBHOOK_SECRET" > /var/lib/genesis/secrets/GITHUB_APP_WEBHOOK_SECRET
chown root:root /var/lib/genesis/secrets/GITHUB_APP_WEBHOOK_SECRET
chmod 0600 /var/lib/genesis/secrets/GITHUB_APP_WEBHOOK_SECRET
```

`GITHUB_APP_WEBHOOK_SECRET` is not a provider `secret` reference. Do not put
the value in `repos.d`, `providers.d`, rule YAML, logs, CloudEvents, or API
responses. A missing file, or a file that is not a private regular file,
fails closed. Deploy-key registration still reads only
`GITHUB_APP_RECONCILER_PEM`.

## Delivery id

`X-GitHub-Delivery` is the CloudEvent `id`. An empty or non-GUID value is
`400` and starts nothing. After the signature, event, and repository binding
all pass, Genesis records the id under `-data/deliveries/<id>` (directory
mode `0700`, file mode `0600`) before it runs rules. The file contains only
the event name. A repeated id that was already recorded returns `200` and
does not start a second run. If the id cannot be recorded, the response is
`500` `delivery was not recorded` and no run starts.

## Events

Accepted `X-GitHub-Event` values are `issues`, `pull_request`, `push`, and
`check_run`. Any other event, including `ping`, returns `200` and starts
nothing.

Dispatch also requires the payload `repository.id` to equal
`repository_id` on a binding in `-data/observations/repositories.json`.
That journal is what an adopted repository sync persists. An unknown
repository returns `200` and starts nothing. A journal that cannot be read
is `500`. No payload field is interpolated into a command.

## CloudEvent

A dispatched delivery is one CloudEvents 1.0 JSON object. Rules match exact
top-level strings, the same way as `POST /events`.

| Attribute | Exact value |
|---|---|
| `specversion` | `1.0` |
| `id` | `X-GitHub-Delivery` |
| `source` | `urn:genesis:github` |
| `type` | `dev.genesis.github.issues`, `dev.genesis.github.pull_request`, `dev.genesis.github.push`, or `dev.genesis.github.check_run` |
| `subject` | `issues`, `pull_request`, `push`, or `check_run` |
| `event` | the same event name as `subject` |
| `action` | the GitHub action when the payload has one, for example `opened` or `completed`. `push` has no `action` attribute |
| `repositoryid` | the decimal repository id |
| `datacontenttype` | `application/json` |

`data` repeats `event`, `repository_id`, and `action` when an action is
present. It does not include the webhook secret, a token, the raw payload,
or a command.

```yaml
match:
  type: dev.genesis.github.issues
  source: urn:genesis:github
  subject: issues
  action: opened
agent: workspace-janitor
```

A match returns `202` with the same run list as `POST /events`. No matching
rule returns `204` after the delivery id is recorded. While a sync is in
progress the response is `503` and the id is not recorded.

## GitHub App setting

In the App's webhook settings, set webhook active for `issues`,
`pull_request`, `push`, and `check_run`.

- Webhook URL: the reachable listener `POST /webhooks/github`
- Secret: the same bytes as `GITHUB_APP_WEBHOOK_SECRET`
- Events: Issues, Pull requests, Pushes, and Check runs

Saving the webhook sends `ping`. Genesis returns success and starts nothing.
The deploy-key and read-only probes leave this webhook inactive on purpose.
Those checklists are not the ingress setup.
