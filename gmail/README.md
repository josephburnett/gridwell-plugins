# gridwell-plugin-gmail

A read-only projection of one Gmail account into Gridwell: two grids — the
inbox and the starred mail — with one tile per message. A tile's face and
document are a markdown card about the message; descending into it opens the
email itself, as the HTML the sender wrote, through the node's content door.

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
| `refresh` | no | `1m` | how often one grid is re-walked |
| `max_messages` | no | `500` | how many of the newest messages a grid holds |
| `endpoint` | no | Gmail's own | the Gmail API base URL |

`endpoint` is the address of the service this plugin reads — the same ordinary
knob the gitlab plugin's `url` is. Point it at a recorded Gmail to exercise the
plugin without an account; the credential is still required and still sent.

A missing or unreadable credential is refused at launch, with the reason and
the `-auth` command to fix it.

## The API contract

Built against `google.golang.org/api/gmail/v1`. Three calls, and nothing else:

```
GET users/me/messages?labelIds=…&maxResults=…&pageToken=…
GET users/me/messages/<id>?format=metadata&metadataHeaders=Subject,From,Date
GET users/me/messages/<id>?format=full
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
  said.
- **The body** is the first `text/html` part of the MIME tree, base64url
  decoded, with the charset its own `Content-Type` header declared. A message
  with no HTML part — many are still plain text — is escaped and wrapped in a
  minimal document. A message with no text part at all gets a page that says
  so.
- **Inline images do not load.** A `cid:` URL names an attachment, and this
  plugin serves no attachments: the image is broken and everything else reads.

Failures map onto the node's vocabulary: 401 and 403 (and a refresh Google
refuses) are `PermissionDenied` and surface, so "this token was revoked"
reaches you instead of an empty grid; 429, 5xx, a stall and a refused
connection are `Unavailable`, and the node serves what it has, stamped stale.

## Keys and the grid

A key is `msg:<gmail message id>` — the message, not the thread. It names the
same email for the life of the plugin, so a message starred out of the inbox
keeps its tile, its id and every link to it.

Tiles are hinted as a calendar: one row per day, newest at the top, the day's
messages left to right. A hint seeds a tile's first placement only; where you
put it afterwards is yours.

`max_messages` bounds each grid, because a mailbox has no end and a grid with
a hundred thousand tiles on it is not a place. It is not a truncation the
plugin hides: Gmail lists newest first, so a read that stops at the cap is
authoritative down to its oldest message and silent below it. Mail above that
line that has left the label loses its tile; mail below it keeps one until a
read reaches it.

## State

`state_dir` holds one file, `gmail.json`: the messages the plugin has seen,
the unread set, and each grid's membership, so a restart answers instantly
and does not call Gmail inside the refresh window. It is disposable — delete
it any time; the next walk rewarms it. **No credential is ever written
there.**

Every walk after the first is a delta: two id listings, then a metadata read
for only the ids the memory has never seen. Message bodies are not cached at
all. A listing is small; a mailbox's bodies are not.
