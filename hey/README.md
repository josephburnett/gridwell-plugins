# gridwell-plugin-hey

A read-only projection of one HEY account into Gridwell: three grids — the
Imbox, Reply Later and Set Aside — with one tile per email thread. A tile's
face and document are a markdown card about the thread; descending into it
opens the email itself, as HEY's own HTML, through the node's content door.

Nothing here writes to your mail. There is no delete, no archive, no reply.

## Setup

Install the HEY CLI and sign in, both as yourself:

```
curl -fsSL https://hey.com/install-cli | bash    # or: mise use -g github:basecamp/hey-cli
hey auth login
```

Then declare the plugin. There is nothing to configure — the CLI holds the
account and its credentials, and the plugin never asks for either:

```yaml
plugins:
  - kind: hey
    label: hey
```

Two optional keys:

| key | default | meaning |
|---|---|---|
| `binary` | `hey` on PATH | the CLI to run |
| `refresh` | `1m` | how often one collection is re-walked |

## The CLI contract

Built against **hey 1.4.1**. Two commands, and nothing else:

```
hey box view <box> --json --all      one collection's threads
hey thread read <topic-id> --html    one thread as an HTML5 document
```

`<box>` is one of the CLI's own named box selectors, which `resolveBox` in
hey-cli answers without a listing call: `imbox`, `laterbox` (Reply Later) and
`asidebox` (Set Aside).

`box view` answers the CLI's response envelope. The plugin reads exactly these
fields and ignores the rest:

```json
{"ok": true,
 "data": {"name": "Imbox",
          "next_page": "",
          "postings": [{"id": 900,
                        "topic_id": 101,
                        "kind": "topic",
                        "name": "Lunch plans",
                        "summary": "Are you free friday?",
                        "seen": false,
                        "created_at": "2026-01-05T14:03:00Z",
                        "creator": {"name": "Alice",
                                    "email_address": "alice@example.com"}}]}}
```

- `topic_id` is the thread — the CLI's own addition beside HEY's fields, and
  what `thread read` takes. A row that answers zero (a **bundle**: one
  sender's unseen threads grouped) opens no thread and is skipped.
- `id` is the box item, which changes when a thread moves between boxes. The
  plugin's key is `thread:<topic_id>`, so a thread keeps its tile and its
  placement when it moves.
- `name` is the subject, `summary` the preview.
- `next_page` present means the read was capped. The plugin then treats what
  came back as "what was seen", never as the collection's whole membership,
  and nothing is ever reported gone on the strength of it.

`thread read --html` answers no envelope: a bare HTML5 document, one
`<article data-entry-id=… data-body-state=…>` per entry, oldest first, each
holding the message exactly as HEY served it. The plugin serves those bytes
through the content door untouched; the node sandboxes them.

A refusal prints on **stderr**, not stdout, and stdout stays empty. With
`--json` the error envelope (`error`, `code`, `hint`) is there, after the
keyring warning; with `--html` there is no envelope at all, only
`Error: <reason>`. The plugin reads both, and drops `warning:` lines: a note
about the host is never the reason a command refused.

Exit status is the whole error vocabulary (`hey help exit-codes`): 2 not
found, 3 auth required, 4 forbidden, 5 rate limited, 6 network, 7 server or
local, 1 and 8 usage. 5, 6 and 7 become `Unavailable` — "not right now", and
the node serves what it has, stamped stale. The rest are verdicts and surface,
so "not signed in" reaches the user instead of an empty grid.

Every run carries `HEY_NONINTERACTIVE=1` and no stdin: a prompt with nothing
to read it would hang the sweep, and signing in is the user's own gesture.

`heycli/testdata/fake-hey` is this contract, executable. If a CLI release
changes any shape above, that script and its test are where it shows.

## State

`state_dir` holds one file, `mail.json`: the threads the plugin has seen and
each collection's membership, so a restart answers instantly and does not
re-run the CLI inside the refresh window. It is disposable — delete it any
time; the next sweep rewarms it. No credential is ever written there.

Email bodies are not cached. A listing is small; a mailbox's bodies are not.
