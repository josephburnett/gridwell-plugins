# gridwell-plugin-gmail

A read-only projection of one Gmail account into Gridwell: the inbox, the
starred mail, and all mail, their union, with one tile per message.
Descending into a tile opens the email itself, as the HTML the sender wrote,
through the node's content door.

Nothing here writes to your mail. There is no delete, no archive, no reply,
and the token the plugin holds carries the `gmail.readonly` scope and nothing
else.

## Setup

### 1. An OAuth client

There is no official Gmail CLI, so the plugin talks to the Gmail API and needs
an OAuth client of your own. Once, in the
[Google Cloud console](https://console.cloud.google.com/):

1. Create a project (or pick one).
2. **APIs & Services → Library → Gmail API → Enable.**
3. **APIs & Services → OAuth consent screen**: user type *External*, fill in
   the app name and your own email. Under **Audience** add your own Google
   account as a **test user** — a personal-use app stays in "Testing"
   forever, and only test users can authorize it.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID**,
   application type **Desktop app**.
5. Download the JSON. That is the `credentials` file. Nothing registers a
   redirect URI: a Desktop-app client may redirect to any loopback port,
   which is what the flow below uses.

Keep the file where you keep other secrets, mode 0600. It is never copied
anywhere and its contents never appear in `server.yaml`.

### 2. Authorize once

```
gridwell-plugin-gmail -auth \
  -credentials ~/.config/gridwell/gmail-client.json \
  -token       ~/.config/gridwell/gmail-token.json
```

It listens on `127.0.0.1`, prints a Google URL, and waits. Open the URL in a
browser signed in as the account you want Gridwell to read, approve the
read-only Gmail request, and the browser lands back on the listener. The
refresh token is written to `-token` as 0600.

A testing-mode app's refresh token expires after seven days. Publishing the
app (**OAuth consent screen → Publish app**) stops that; while it is in
testing, re-run `-auth` when the plugin starts reporting a refused token.

### 3. Declare the plugin

```yaml
plugins:
  - kind: gmail
    label: gmail
    config:
      credentials: /home/you/.config/gridwell/gmail-client.json
      token:       /home/you/.config/gridwell/gmail-token.json
```

Both values are **paths**. No secret is ever a config value, and neither file
belongs in the plugin's state directory: that directory is disposable, and a
deleted credential is not rewarmed by use.

| key | required | default | meaning |
|---|---|---|---|
| `credentials` | yes | — | the OAuth client JSON from the console |
| `token` | yes | — | the token file `-auth` wrote |
| `refresh` | no | `1m` | how often memory catches up with Gmail while a grid is shown |
| `max_messages` | no | `500` | how many of a label's newest messages one walk reads |
| `endpoint` | no | Gmail's own | the Gmail API base URL |

`endpoint` is the address of the service this plugin reads — the same ordinary
knob the gitlab plugin's `url` is. Point it at a recorded Gmail to exercise the
plugin without an account; the credential is still required and still sent.

A missing or unreadable credential is refused, with the reason and the
`-auth` command to fix it. So is a token Google refuses when the plugin
first starts: it asks Google for the account's profile, and a 401 or 403
leaves the plugin's row broken with that sentence until the fix lands, with
no restart. Google out of reach at that moment is not a refusal; the plugin
starts and its grids read as dark until Google answers. Once the plugin has
started it never asks again: a token revoked later is reported on the grids,
which keep what they showed (see State).

## The API contract

Built against `google.golang.org/api/gmail/v1`. Five calls, and nothing else:

```
GET users/me/messages?labelIds=…&maxResults=…&pageToken=…
GET users/me/messages/<id>?format=metadata&metadataHeaders=Subject,From,Date
GET users/me/messages/<id>?format=full
GET users/me/profile
GET users/me/history?startHistoryId=…&historyTypes=…&pageToken=…
```

`gmailapi/testdata/` holds the exact JSON each of those answers with, and the
tests serve it from an `httptest` server the real Gmail client is pointed at.
That is the contract: if Google changes a shape the plugin reads, it shows
there.

- **The listing** answers ids only, newest first, with a `nextPageToken` while
  there is more. The plugin pages to `max_messages` and no further; a read
  that stopped there is not a whole read.
- **Label ids are ANDed.** `INBOX` is the inbox; `INBOX,UNREAD` is its unread
  part. That second listing is the whole unread mechanism — one cheap call
  per walk, and no message ever needs its metadata re-read to stay honest
  about being read.
- **Metadata** is asked for three headers, not all of them. `internalDate` is
  the date a tile is placed by: epoch **milliseconds**, set by Gmail when it
  received the message, where a `Date:` header is whatever the sender's clock
  said. A metadata read is also how all mail asks whether Gmail still has a
  message (see Keys and the grid).
- **The body** is the first `text/html` part of the MIME tree, base64url
  decoded, with the charset its own `Content-Type` header declared. A message
  with no HTML part — many are still plain text — is escaped and wrapped in a
  minimal document. A message with no text part at all gets a page that says
  so.
- **The profile** answers the account's history id: read when the plugin
  starts, to check the token, and before each full walk, where the next
  catch-up starts.
- **History** is every change since a history id, paged. Only arrivals
  carrying `INBOX`, `STARRED` or `UNREAD`, changes to those labels, and
  deletions are read; each such message's metadata is fetched again and its
  current `labelIds` place it. A 404 means the id is too old.
- **Inline images do not load.** A `cid:` URL names an attachment, and this
  plugin serves no attachments: the image is broken and everything else reads.

401 and 403, and a refresh Google refuses, are a refused token; 429, 5xx, a
stall and a refused connection are Google out of reach. One message Gmail
will not answer for costs that message's tile, never the walk.

## Keys and the grid

Three grids, one (+) menu entry each: **inbox**, **starred**, and **all
mail**, their union. All mail holds each message once, as a url tile whose
page is the email. The inbox and the starred grid hold links to those tiles,
so a message that is in both is one tile, and a message starred out of the
inbox keeps it. All mail is not Gmail's All Mail label: it is what the two
labels hold, and a message that leaves both leaves it.

A key is `msg:<gmail message id>` — the message, not the thread. It names the
same email in every grid for the life of the plugin, so a row you placed in
the inbox before all mail existed keeps its id and becomes a link in place.

A tile is named by the message's subject, the same whatever its state. Its
state is one mark beside the name, only when there is something to notice:
`●` unread, else `★` starred (except in the starred grid, which already says
so). A read message carries nothing.

A tile's first placement is the shared calendar's cell for the date Gmail
received it (`memo/calendar`): a column per day, newest to the right, a row
per hour of the day, in the host's time zone. It depends on the message
alone, so mail that arrives late moves nothing else. Where you put a tile
afterwards is yours.

`max_messages` bounds each walk, not each grid: a mailbox has no end, and the
newest are what a person is looking at. Gmail lists newest first, so a walk
that stops at the cap is exact down to its oldest message and silent below
it. Mail above that line that has left the label leaves the grid; mail below
it stays, so a grid holds everything the plugin remembers for that label.

Whether a message has left a grid is answered for that grid. The inbox or the
starred grid, read to its end, lists every message it holds and says so, and
a message it does not list has left it; after a capped walk, or while the
plugin is behind Gmail, the grid cannot say. All mail never says so on its
own: a message it no longer lists is usually archived, so the plugin asks
Gmail, and only Gmail answering that it has no such message retires the
tile.

## State

`state_dir` holds one file, `gmail.json`: the messages the plugin has seen,
the unread set, each label's membership, the history id they are current to,
and when the last refresh and full walk landed. A restart answers at once,
does not call Gmail inside the refresh window, and catches up from the
history id after it. It is disposable — delete it any time; the next walk
rewarms it. A message leaves it once no label holds it and Gmail says it is
gone. **No credential is ever written there.**

A refresh reads Gmail's history since the id memory is current to and
applies it: a quiet mailbox costs one request. A full walk of both labels —
two id listings each, then a metadata read for only the ids the memory has
never seen — runs instead when there is no id yet, when Gmail answers that
the id is too old, and once a day as a consistency pass. Message bodies are
not cached at all. A listing is small; a mailbox's bodies are not.

Nothing runs for nobody. A refresh happens when a grid is read and memory is
older than `refresh`, or on the `refresh` clock while the node holds a
`Watch` stream showing one of the grids; with no grid shown, Gmail is asked
nothing. There is no push: Gmail's own (`users.watch`) publishes to a Google
Cloud Pub/Sub topic that must deliver to a public HTTPS endpoint, and a
personal node has neither.

A grid answers from memory at once whenever memory has an answer; only a
grid never read waits, briefly, on its first walk. When a refresh fails —
Google out of reach, or a token revoked since the plugin started — every
grid keeps what it showed and the plugin says why, which the node shows as
the plugin's health, until a refresh lands again.

The node hears about a change without asking: after each refresh the plugin
sends a `ContextChanged` on its `Watch` stream for every grid whose listing
changed, and nothing when none did. It never sends an `EntryChanged`: Gmail
does not change a message's content once it has it, so a message's page
never changes in place; read, starred and which label holds it are the
listing's.
