# Publications

A publication is one short story about the loop that an agent files for the
operator. The Wire shows them as the front page. They are not run output and
not a log: a reporter writes them on purpose.

## Who may publish

Only an agent whose `setup.groups` contains `reporter` in the active
generation. The shipped `oracle` is the reporter (`groups: [shared,
reporter]`). The listener reads the agent id from the event `source`
(`urn:genesis:agent:<id>`) and answers `403` for anyone else.

This is a policy check, not a capability boundary. `genesis publish` takes
the source from `GENESIS_AGENT`, but a caller who posts to the listener by
hand can write any `source`. Treat the register as the operator's feed, not
as evidence.

Control does not write the register. `POST /api/events` proxies to the
listener, which applies the same check to whatever `source` the event carries.

## Filing one

```sh
genesis publish --kind report \
  --headline 'Baseline reproduced' \
  --lede 'The first loop matched the reference within noise.' \
  --run gen_0123456789abcdef0123456789abcdef \
  --body-file reports/gen_0123456789abcdef0123456789abcdef.md
```

It prints the new `pub_` id, and exits nonzero with the listener's reason
when the call is refused. It posts one `dev.genesis.publication.submitted`
CloudEvent. Genesis stamps the id, source, and time; jobs and schedules
cannot emit that type.

| Field | Flag | Rule |
| --- | --- | --- |
| `kind` | `--kind` | `report` (routine), `progress`, `breakthrough`, or `impasse` |
| `headline` | `--headline` | one line, at most 120 characters |
| `lede` | `--lede` | one line, at most 400 characters |
| `body` | `--body-file` | optional markdown, UTF-8, at most 16 KiB |
| `run` | `--run` | optional `gen_` run id the story is about |
| `refs` | `--ref` (repeatable) | optional, at most 8 `gen_` or `pub_` ids |
| `to` | `--to` | optional agent id in the active generation |
| `supersedes` | `--supersedes` | optional `pub_` id of the same agent's earlier story |

Unknown fields are refused. Secret values known to the listener are redacted
before the line is written.

## Register

`<data>/publications/register.jsonl`, written only by the listener, one JSON
object per line, append-only, with a `seq` that starts at 1 and never
repeats. The directory is mode `0755` and the file `0644` so the control
process, which runs as another uid, can read it. A torn last line from a crash
is dropped on the next start, as with the run journals.

An entry is never edited. A correction is a new publication with
`supersedes`. Only the author may supersede a story. By default the read API
hides superseded stories and marks them with `superseded_by` when asked.

## Read API

`GET /api/publications?limit=&before=&after=&include_superseded=1`

- Newest first; `limit` defaults to 20 and is at most 100.
- `before=<seq>` returns older stories. Use `next_before` from the last page.
  It is omitted on the last page.
- `after=<seq>` returns only newer stories, for polling. Use `last_seq`.
- `last_seq` is the newest sequence in the register.

`GET /api/publications/{id}` returns one story, superseded or not.

The Wire loads the first page, loads the next older page when the reader
scrolls near the end of the front page, and polls with `after` for new
stories.
