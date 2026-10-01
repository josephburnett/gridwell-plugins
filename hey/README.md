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
| `refresh` | `1m` | how often one collection is re-walked while the live feed is down |

A CLI that cannot be found refuses the plugin's handshake with that reason, so
its row shows it broken until the CLI is installed.

## The CLI contract

Built against **hey 1.4.1**. Three commands, and nothing else:

```
hey box view <box> --json --all                   one collection's threads
hey thread read <topic-id> --html                 one thread as an HTML5 document
hey watch --events added,updated,deleted,resync   the live feed of mail changes
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

`watch` answers no envelope: one JSON object per line, until interrupted.
The plugin reads exactly these fields:

```json
{"change": "added", "at": "2026-09-28T19:01:04.695Z",
 "box": {"id": 1, "kind": "imbox", "name": "Imbox"},
 "posting_id": 930, "thread_id": 103, "new": true,
 "posting": {"id": 930, "kind": "topic", "name": "Board games",
             "summary": "Thursday at mine?", "seen": true,
             "created_at": "2026-09-28T19:01:04.695683Z",
             "creator": {"name": "Erin", "email_address": "erin@example.com"}}}
{"change": "deleted", "at": "…", "box": {"id": 1, "kind": "imbox", "name": "Imbox"}, "posting_id": 930}
{"change": "resync", "at": "…", "box": {"id": 2, "kind": "laterbox", "name": "Reply Later"}}
{"change": "ready", "at": "…"}
{"change": "disconnected", "at": "…"}
```

- `added` and `updated` carry the posting — the same object a `box view` row
  is, minus `topic_id`: the thread is the line's `thread_id`. `seen` is absent
  while the thread is unseen.
- `deleted` names only the posting and its box. The plugin remembers which
  posting holds each thread in each box, so it knows which thread left.
- Every thread line names its box, so a change maps to its collection without
  a read. `box.kind` is the selector `box view` takes (`imbox`, `laterbox`,
  `asidebox`); every other box (the Feed, the Paper Trail, …) is ignored.
- `resync` means the box changed more than the feed could list: re-read it.
- `ready` comes once every box is caught up and the subscription is live, and
  again after every reconnect; `disconnected` when the connection drops.
  Neither names a box. Without `--since` the feed prints nothing about the
  past, so the first line of a healthy watch is `ready`.

`added`, `updated`, `deleted` and `ready` are as observed from hey 1.4.1;
`resync` and `disconnected` are as its own help documents them.
`heycli/testdata/watch.jsonl` holds one of each.

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

## Live changes

The plugin runs one `hey watch` for its whole life. Every line lands in
memory at once, and a line that changes a collection's listing tells the node
through the plugin's `Watch` stream, so new mail reaches the grid in seconds.
`resync` re-reads its box. `ready` re-reads all three: the feed says nothing
about the time before it was live, so the first `ready` and every one after a
reconnect is a catch-up.

A feed that ends is started again after 1s, doubling to 5m, and starting over
once one reaches `ready`. `disconnected` is not an error — the CLI reconnects
by itself — but one that has not said `ready` within 2m is restarted. A CLI
that refuses `watch` as usage (exit 1 or 8: it has no such command) is not
restarted; the log says so and the plugin keeps memory by walking instead.

While the feed is live, and a collection's catch-up walk has landed, nothing
walks that collection on a clock: the feed keeps it. While the feed is down —
not started, disconnected, restarting, or refused — `refresh` rules again:
each collection is re-walked when its last walk is older than that.

A read answers from memory at once whenever memory has a listing of the
collection, even while a walk it started runs behind it; it carries the last
failed walk's error until a walk lands, and the verdict the feed ended on
(not signed in, no CLI) until the feed is live again. Only a read with no listing to
answer — the first ever — waits for the walk, at most 2s.

`Watch` sends only `ContextChanged`, one per collection whose listing changed,
whether the feed or a walk changed it. It never sends `EntryRemoved`: the
listings are not authoritative, and a thread that leaves one box is usually
in another, so whether it is gone is `Probe`'s to say. A subscriber that falls
64 changes behind never holds the feed up: its changes are dropped, and when
it reads again it is told all three collections changed.

## State

`state_dir` holds one file, `mail.json`: the threads the plugin has seen and
each collection's membership, so a restart answers instantly. The feed's
first `ready` then walks every box behind that answer, because nothing says
what changed while no plugin was running. It is disposable — delete it any
time; the next sweep rewarms it. No credential is ever written there.

Email bodies are not cached. A listing is small; a mailbox's bodies are not.
