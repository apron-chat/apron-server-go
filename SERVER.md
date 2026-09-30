# aprond

`cmd/aprond` serves the reference Apron backend: it implements protocol v7
([PROTOCOL.md](https://github.com/shazow/apron/blob/main/PROTOCOL.md)), every capability, private rooms,
roles, passkey and email sign-in, and liveness ping, but not the designs
under consideration in
[Appendix C](https://github.com/shazow/apron/blob/main/PROTOCOL.md#appendix-c--under-consideration):
WebRTC, multiplexing, and the actions embed.
State is kept in a store, by default a SQLite database in the user data
directory, so rooms, history, accounts, sessions, uploads, and push
registrations survive a restart; see [Storage](#storage).

This document describes how the server is configured and how it behaves where
the protocol leaves room for choice. To work on the server itself, see
[DEVELOPMENT.md](DEVELOPMENT.md).

## Configuration

```sh
go run ./cmd/aprond
```

Defaults:

- HTTP and WebSocket listener: `127.0.0.1:8080` (`--addr`)
- WebSocket endpoint: `/ws`; health endpoint: `/healthz`
- embed endpoints: `/write/<token>`, `/files/<embed_id>/<secret>`,
  `/streams/<embed_id>/<secret>`
- WebSocket origins: `localhost`, `127.0.0.1`, and `::1` during development
- capabilities: `history`, `edit`, `rooms`, `reactions`, `activity`,
  `embed:upload`, `embed:stream`, `command`; push kind `relay`
  (`server.push`); `server.ping`: 30 seconds
- `server.ext["apron-go"]`: frame, history, upload, avatar, stream, and
  member-listing limits
- seeded default room: `general` (title `General`)
- authentication (`server.auth`, in this order): WebAuthn passkeys, email
  codes when `--email.sender` is set (off by default), bearer-token resume,
  and `guest`; `server.signup` lists every one but `token`, which only
  resumes an account: a guest who registers a passkey or adds an address
  becomes an account, and an email code for a new address creates one
- passkey RP ID: `localhost`; frontend origins: `http://localhost:5173` and
  `http://localhost:8080`
- no `server.welcome` and no roles

Flags (`aprond --help` lists them all):

- `--config aprond.toml` loads settings from a TOML file; flags given on the
  command line take precedence. Top-level keys are flag names, and each
  group is a table: `[upload] max-mb = 5` sets `--upload.max-mb 5`. A list
  in the file replaces the flag's default. `--print-config` prints the
  current settings, with descriptions, in that form, so
  `aprond --print-config > aprond.toml` starts a config file. Unknown keys
  are an error.
- `--store sqlite:<path>` keeps state in a SQLite database, by default
  `aprond/aprond.db` in the user data directory (`$XDG_DATA_HOME`, usually
  `~/.local/share`, on Linux); `--store memory` keeps nothing across
  restarts.
- `--static-dir <directory>` serves a built frontend from the same listener:
  files, but never directory listings or names starting with `.`; a
  directory is served only through its `index.html`.
- `--origin <pattern>` (repeat for more) sets a deployment-specific origin
  allowlist; `--allow-any-origin` only when the deployment has its own
  cross-site protections. `--addr` changes the listener address;
  `--addr :8080` listens on every interface.
- `--public-url https://chat.example` sets the base of write, file, and
  stream URLs. Without it they use the host each WebSocket request arrived
  on (or `https://` and the first `--tls.domain`), which suits local
  development but lets a client choose the host in URLs others see; set it
  in any deployment.
- `--max-connections <n>` refuses WebSockets beyond `n` with an error without
  `id` (`retry_after`), then closes them. `--max-listener-connections <n>`
  caps concurrent TCP connections on the listener, HTTP included; further
  connections wait to be accepted.
- `--messages-per-minute <n>` limits each user's new messages, `room_set`
  requests, and `/avatar` commands together: a burst of `n`, refilled evenly
  over a minute. The excess gets `retry_after` with `data.retry_after` in
  seconds. Edits, reactions, and activity are not counted.
- `--max-listed-members <n>` (1000) bounds the `members` of one room in
  `room_list` and `room_update` `joined`, `0` for no bound; see
  [Rooms](#rooms-threads-and-membership).
- `--welcome <markdown>` sets `server.welcome`, which clients show on their
  sign-in screen, such as "Chat as a guest, or sign in with email to keep
  your name. Codes expire after 10 minutes."
- `--client-ip-header <header>`, behind a reverse proxy, names the header
  that carries the client's address, such as `X-Forwarded-For` (its last
  entry) or `X-Real-IP`, for the per-client limits of email sign-in; set it
  only when the proxy sets that header, since clients can send it too.
- `--role <role>=<user_id or email>` (repeat for more) grants a role, which
  is lowercased, to an account; see [Identity and profiles](#identity-and-profiles).
- `--email.sender` chooses how [email sign-in](#email-sign-in) codes are
  delivered: `none` (the default) turns email sign-in off; `smtp` sends them
  through `--email.smtp-addr host:port` from `--email.from`, with
  `--email.smtp-user` and `--email.smtp-password` when the relay needs them,
  and requires STARTTLS unless `--email.smtp-insecure` (on port 465 it
  speaks TLS from the start); `log` writes them to
  the server log, for development, where anyone who reads the log can sign
  in as anyone, so it is refused with `--public-url` or `--tls.domain`.
  `make dev-server` and `make run` pass `--email.sender log`.
  `--email.link-url` is the page a code's link opens (default
  `--public-url`; without either, emails carry only the code).
- `--webauthn.rp-id <domain>` (`localhost`) and `--webauthn.origin <origin>`
  (repeat for more; `http://localhost:5173` and `http://localhost:8080`)
  configure passkeys; `--webauthn.rp-id ''` disables them.
- `--upload.dir <directory>` holds uploaded files, by default
  `aprond/uploads` in the user data directory. At start the server removes
  files named `*.upload` there that the store does not refer to, and nothing
  else.
- `--upload.max-mb <n>` (20) bounds one upload, `--upload.max-message-mb <n>`
  (20) all of one message's uploads, and `--upload.max-storage-mb <n>`
  (1000) every hosted upload together, all in MiB. Past the storage bound the
  oldest message uploads are removed: their files are deleted and each
  affected message is republished without them. Avatars count toward the
  bound but are never removed.
- `--push.disable` removes push; `--push.allow-insecure` accepts `http` and
  internal push endpoints (development only).
- `--tls.domain <domain>` (repeat for more) serves HTTPS itself, with
  certificates from Let's Encrypt, on `--tls.addr` (`:443`) in place of
  `--addr`, and answers ACME challenges and redirects to HTTPS on
  `--tls.http-addr` (`:80`). Certificates are cached in `--tls.cache-dir`
  (`aprond/autocert` in the user cache directory); `--tls.email` is the
  contact address given to Let's Encrypt. Behind a reverse proxy that
  terminates TLS, leave it unset.
- `--debug-addr 127.0.0.1:6060` serves `net/http/pprof` and `expvar` under
  `/debug/` on a separate listener; keep it off public interfaces (the server
  warns at start when it is not bound to loopback).
  [`cmd/apron-hammer`](cmd/apron-hammer/README.md) load-tests the server and
  reads it to report heap and goroutines.

The listeners run together: SIGINT or SIGTERM, or any listener failing,
shuts them all down, telling open WebSockets to reconnect shortly.

## Storage

The server keeps its working state in memory and writes every change
through to a store. Each request's changes, and each timer's or upload's,
form one batch that the store applies atomically; a single writer applies
batches in order, outside the server's lock, so a slow disk delays
persistence rather than requests. At start the server rebuilds its state
from the store.

The store holds opaque entries by kind and ID (`internal/store`), so a
backend only keeps entries and applies batches:

- `sqlite:<path>` ([modernc.org/sqlite](https://modernc.org/sqlite), pure
  Go) keeps them in one table of a WAL-mode database file, created with
  mode `0600` because it holds passkey credentials and session hashes.
- `memory` keeps them in the process; a new server starts empty.

What survives a restart: rooms and threads with their complete logs, message
state and reactions, read cursors, accounts (passkey and email users) with
their credentials, addresses, profiles, memberships, and push registrations,
unexpired sessions, finished uploads and their files, and the `log_id`,
guest, account, and embed counters, so no `log_id` or `user_id` is reused.
What does not: connections, request deduplication, email sign-in codes, and
live streams. Guests exist only while connected, so at start every guest
left in the store is retired as if its last connection had just closed,
logging its leaves; a write that had not finished fails, and its message is
republished without the embed.

A store written by a protocol v6 server is migrated at the first start and
written back: a room's `intro_message` becomes its `description`, the text
of the intro snapshot each logged room record embedded, except that the
current record and the latest logged one take the message's current text,
as v6 showed it (none for a deleted message), and messages from `@room`, `@server`, and `@private` become
messages from `~room`, `~server`, and `~private`. A plain-text intro is
escaped as Markdown, since descriptions are Markdown by convention. The description is a copy: unlike the v6
intro, it stays when the message is later deleted, which only redacts the
message itself.

## Connections and liveness

The server answers the liveness ping `{"method":"ping"}` with
`{"method":"pong"}`, before authentication too; the exact bytes are answered
without parsing, and a `ping` notification with other spacing is answered as
well. It also pings at the WebSocket level every 30 seconds and closes a
connection that does not answer within ten. A connection that sent liveness
pings and then sent nothing for three ping intervals plus the timeout
(100 seconds) is closed: its page is frozen or gone.

## Log and history

Every change is a record in one append-only log with a single server-wide
`log_id` sequence (commit time in milliseconds, or the previous ID + 1) shared
by room records, message snapshots, reaction sets, and memberships across all
rooms. A message's `message_id` is its creation `log_id`, and a room created by
a client uses its creation `log_id` as its `room_id`. Room records and message
snapshots after the first for their key carry `prev_log_id`: an edit names the
previous snapshot and a room update the previous room record. A move snapshot
also carries `prev_room_id`, the source room, whose log holds the earlier
snapshots. Reaction sets and memberships carry neither. Nothing is compacted
or discarded, so each room's `history_log_id` is the `log_id` of its creation
record, `latest_log_id` is the newest record in that room's log, memberships
included, and the server never needs the full-member record the spec asks for
before discarding a prefix.

`history` returns a window of one room's log, `general`'s without `room_id`,
partitioned into `rooms`, `messages`, `reactions`, and `membership`; an empty
array is omitted. `limit` (default 100, clamped to 1000) counts records of
every kind, and `first_log_id`/`last_log_id` span all of them; an empty window
has neither. A page also ends, with `more: true`, once its records reach
4 MiB, keeping at least one record, so continuing from `first_log_id` or
`last_log_id` pages through large records as usual. Records are stored as the
JSON they are sent as (without escaping `<`, `>`, and `&`), and a history
reply is assembled from them without decoding them. Records keep the user
objects they were logged with. A window bounded to one `log_id` (`after` equal to
`before`) returns exactly that record from the room's log, so a client walks a
message's edits back through `prev_log_id`, asking the room in `prev_room_id`
after a move. Any user may page the history of any room they can see,
joined or not: every room but [private](#rooms-threads-and-membership) rooms
they are not in.

## Identity and profiles

`auth` with scheme `guest` assigns `guest_<n>` from a server-wide counter
(`guest_1`, `guest_2`, …) and honors an optional requested `name`. A
requested `user_id` is honored when it starts with a letter, uses only
`[A-Za-z0-9_.-]` (ending in a letter, digit, or `_`; at most 64 characters),
does not start with `guest_` or `user_` in any case, names no room (ignoring case), was never
assigned, ignoring case, and is not granted a role (`--role`); otherwise the
guest gets the next unused `guest_<n>`. So no user is ever given a
`user_id` starting with `~`, which are the system identities `~room` and
`~private` ([Commands](#commands)). The `guest_` namespace belongs to the
guest counter, and `user_` to that of email accounts: a request such as
`guest_7`, `GUEST_7`, `guest_07` or `guest_x` is refused rather than taking a
number out of sequence or impersonating a counter-assigned guest. Every guest
`auth` takes exactly one counter value unless its requested ID is honored, so
the latest guest number is roughly how many guests the process has admitted
(the counter is in memory and starts over with the process). No `user_id` is
ever reissued.

Accounts are users who signed in with a passkey or an email address; they
keep their profile, rooms, and push registrations across connections and
restarts. `--role` grants them roles, such as
`--role admin=ada --role moderator=bob@example.com`, by `user_id` or email
address. Roles are sent in current user objects (below), sorted, and are
shown beside names; `admin` and `moderator` may also remove people from
rooms. Role names are lowercased, so `--role Admin=ada` grants `admin`.
Guests hold no roles. A `user_id` granted a role but never used is reserved:
no guest or new account is given it, so nobody can claim it and then add a
passkey or address to inherit the role. So a grant by `user_id` applies to
accounts that already exist; grant a new admin by email address. Roles
follow the configuration: they change with a restart, or when a guest
becomes an account.

`me` merges into the caller's profile: a given field replaces its value, an
omitted field is unchanged, and an empty value removes it. `roles` cannot be
set and, like other unknown fields, is ignored. `name` is prepared
with the PRECIS Nickname profile (RFC 8266: compatibility characters are
mapped, runs of spaces folded, and the ends trimmed), after invisible
characters such as controls and bidirectional overrides are dropped, and is
capped at 64 characters; `avatar` must be an `https:` URL or a
`data:image/{png,jpeg,gif,webp};base64,` URL of at most 64 KiB; `ext`
(at most 16 KiB of JSON) replaces the profile extension object. The result's `you` and the `user`
notifications carry removed fields as their empty values (`""`, `{}`).
Current user objects (`you`, `new` in `user`, and `users` in `room_list` and
`room_update`) carry `avatar`, `ext`, and `roles`; recorded objects (`from` in messages
and reactions, `user` in memberships) carry only `user_id` and `name` as they
were when logged. Room `members` are bare `{user_id}` objects whose complete
objects are in the accompanying `users`.

`user` notifications carry profile and identity changes; joins and leaves
are memberships. A profile change sends
`user` with `you` to the user's other connections and with `new` to everyone
who shares a room with them. When a sign-in replaces a guest identity on a
connection, the guest is retired: a leave is logged in every room it had
joined, and then those who shared a room with it receive `user` with `new` and
`old`. A guest whose last connection closes is retired the same way, with a
logged leave for each room; its `user_id` is never reissued, so its records
stay consistent. Accounts are never retired.

## Rooms, threads, and membership

Every room is visible to every user, except private rooms (below). The
server does not push a room list at
sign-in: after `auth` the client lists its rooms with `room_list`, and later
changes arrive as `room_update`. `auth` is a barrier: each
connection's frames are processed one at a time, so requests sent right
behind `auth`, such as `room_list` and `history`, run as the new identity, and
are `denied` if the `auth` failed, or was a WebAuthn `begin` step or an email
code request on a connection not yet signed in.

A connection receives records only for the rooms its user has joined. A
thread is a room like any other: its messages, reactions, and memberships go
only to its members, while members of its parent room receive its room record
when it is created or edited, as `room_update` `updated`, unless the thread is
private.

A room created with `private: true` is private for good: its record carries
`private: true`, and it is visible only to its members. A user sees a room
when they are a member of every private room among the room and the rooms
it is a thread of: so a thread of a private room is visible only to that
room's members, members of the thread included, and a private thread of a
private room only to members of both. Someone can be added to a thread of a
private room only once they are in that room. Leaving or being removed from
a private room loses its threads at every depth: the user leaves those they
joined, which stops their deliveries, and their connections receive
`room_update` `left`, once, for the others they could see, never for a
private thread they were not in. To anyone else
a private room, its threads, and their messages are unknown: every request
naming them (`history`, `room_list`, `room_join`, `room_leave`, `room_set`,
`message`, `command`, `activity`, `reactions`, and a `reply_to` or
`read_message_id` naming a message in one) is `invalid_params`, as for an
unknown ID, and `room_list` never lists them. An author removed from a
private room can no longer edit or delete their messages there. Mentions in
a private room wake only its members.

A message cannot move to a room that some who can see its current room
cannot see (`denied`): a move snapshot is logged in and delivered to both
rooms and names both, so it would show them the other room's `room_id` and
the message. So a message moves from a public room only to a public room,
and from a private room or its threads to rooms visible to all their
members, such as its public threads or any public room. A move out to where
more people can see the message leaves out `prev_room_id`, so it does not
name the private room to them, but its reactions go with it, showing who in
the private room reacted. For the same reason a reply cannot quote a
message that some who see the reply cannot see (`denied`): `reply_to` would
name it.

Every membership change is a logged record in the room's log, delivered to
the room's members before and after the change (so to the joining or leaving
user too) and returned in `history`'s `membership` array:
`{"log_id", "room_id", "members": [{"user": {user_id, name}, "joined": true|false}]}`.
The server logs memberships for every user, guests included: a new guest's
join to `general` at `auth` (delivered to its connection before the `auth`
result), `room_join` and `room_leave` (for oneself or another user), the
creator's join when `room_set` creates a room, a `/kick` removal, a new email
account's join to `general`, and a guest's leaves when it is retired.

- `room_list` answers with `joined` (every joined room at any depth, never
  truncated) and `not_joined` (visible unjoined rooms: top-level ones, or
  with `parent_room_id` that room's threads, at most the 200 most recently
  active), each most recently active first. `filter` (`joined`,
  `not_joined`, or `all`, the default) leaves out the other array; an array
  it asks for is present even when empty. `parent_room_id` lists that room's
  threads; `room_id` lists one room and overrides `parent_room_id`. With
  `members: true` every listed room carries its `members`, and the
  result carries `users`, each listed member's current object once; without it,
  neither. `members` is complete unless the room has more than
  `--max-listed-members` (1000) members: then it lists that many, the most
  recently active (by their latest join or new message in the room), and the
  room carries `member_count`, the total. The same holds for `room_update`
  `joined`. With `latest_log_id`, only rooms whose `latest_log_id` is greater
  are listed, and a result with `joined` also carries `left` (present even
  when empty): `[{room_id}]` of the rooms among those listed the user left or
  was removed from since then. Such a room is also in `not_joined` when the
  filter asks for it. Unknown rooms and filters are `invalid_params`. After
  the result, the server sends the read cursors it keeps for the listed rooms
  as `activity`: every member's for a joined room, only the caller's own for
  another.
- `room_join` and `room_leave` take a `room_id` and return `{}`. Joining sends
  the membership, then `room_update` `joined` with the room record, its
  `members`, and `users` to the user's connections, then the result; joining
  a room already joined logs nothing and re-sends `room_update` `joined` to
  the calling connection only. Leaving sends the membership, then
  `room_update` `left`, then the result; leaving a room not joined changes
  nothing. Joining or leaving a room does not affect its threads, and no
  `~room` messages are posted for joins and leaves.
- With a `user_id` of another user, `room_join` adds that user and
  `room_leave` removes them: the membership goes to the room's members, the
  target's connections then get `room_update` `joined` or `left`, and the
  caller the result. Any member of a room may add someone to it, which is how
  people join a private room; only the room's creator and users with the
  `admin` or `moderator` role may remove someone (`denied` otherwise). The
  user must exist (a connected guest or an account), or it is
  `invalid_params`; adding a member or removing a non-member changes nothing.
- `room_set` without `room_id` creates a room (optional `parent_room_id`,
  `private`, `title`, `description`, `ext`) and joins only its creator,
  logging the room record and then the creator's membership; the creator's
  connections receive `room_update` `joined` (with `members` and `users`, and
  `latest_log_id` already the membership's), then the membership, then the
  result. A new thread that is not private goes to the parent's other members
  as `room_update` `updated`, without joining them. With `room_id` it
  replaces every client field except `parent_room_id` and `private`, which
  are fixed at creation, and omitted fields are cleared; the edit goes as
  `room_update` `updated` to the room's members, to the parent's members for
  a thread that is not private, and to the editor. Both return
  `{"room_id": ...}` after the `room_update`. Any authenticated user may
  create top-level rooms or threads (nested threads are allowed) and edit
  any room they can see, such as a bot keeping a thread's `description`
  current. `description` is a string of at most 16 KiB, Markdown by
  convention; the server never parses it. A thread saved without a title is
  titled from the first line of its description, or `Thread`.
- Posting does not require joining a room one can see: a poster who has not
  joined gets the result but not the broadcast.

On the requesting connection, every notification a request causes comes
before its result, and every result is queued while the state it describes
is locked, so a result reflects every notification before it: a `room_list`
sent after `room_leave` never lists the room.

## Messages

Message notifications and history `messages` are flat snapshots:
`{message_id, log_id, prev_log_id?, prev_room_id?, room_id, from, body?, reply_to?, deleted?, ext?}`.
`message` creates a message when `message_id` is absent and, when it is
present, replaces every client field (`room_id`, `body`, `reply_to`, `deleted`,
`ext`) with the submitted state. Without `room_id` the message goes to
`general`; an unknown `room_id` is `invalid_params`. A new message with no
`text` and no `embeds` is neither logged nor broadcast, and its result is
`{}`. The broadcast reaches the sender's connection before the result when
the sender has joined the room. `body.mentions` must be an array of at most 256 non-empty strings.
`from` is assigned from the authenticated connection and preserved across
edits; server fields in requests are ignored, and unknown top-level keys are
dropped (extension data belongs in `ext`, which is passed through unchanged).
Edits, deletion, and moves require the creating identity. A missing
`body.format` means `plain`. Posting to a room does not join it.

Deletion is a save with `deleted: true` and yields a tombstone without `body`.
The server then redacts the message: its earlier snapshots become tombstones
at their original `log_id`s, and the content of its hosted embeds is deleted.

`reply_to` is a bare `{"message_id": ...}` reference on input and in
snapshots. It may name a message in any room the sender can see, including a
tombstone, but not the message itself. A thread started from a message
carries that message in its first reply's `reply_to`, by the clients'
convention; the server keeps no other link between them.

A save with a different `room_id` moves the message. The destination must
exist. The move snapshot is logged in and delivered to both rooms, so it
appears in both rooms' history, and carries `prev_room_id` naming the source
room, where earlier snapshots stay. If the message has reactions, a
`reactions` record carrying every non-empty set is then logged in the
destination room, before the result.

## Embeds, uploads, and streams

Every embed gets a server-assigned `embed_id` (`embed_<n>`). A save keeps an
embed by sending it back with its `embed_id`; its `kind` and the fields the
server owns (an upload's `url` and `og`, a stream's `url` or `text`) are
restored from the server's records, while other fields come from the save.
An embed sent without `embed_id` is new, one left out is removed and its
hosted content deleted, and an unknown `embed_id` is `invalid_params`. `og`
sent by clients is dropped: the server describes only media it hosts.

New `upload` and `stream` embeds get a one-time write URL, listed in the
`message` result as `embeds: [{embed_id, kind, write_url}]`, which follows the
pending snapshot's broadcast. The sender PUTs
or POSTs the content there. A write URL expires after five minutes unused,
and a write that never starts or fails is finished by publishing the message
without the embed.

- **Uploads** (at most 20 MiB each, and 20 MiB for one message's uploads
  together; a message has at most 32 embeds). While pending the embed has no `url`. When
  the write finishes the server publishes a snapshot with `url` set to the
  hosted file and, for images and playable media, `og`: `image` (PNG, JPEG,
  GIF, and WebP with `width` and `height`; the sender's `og.image.alt` is kept),
  `video`, or `audio`, plus `title`. Files are served sandboxed
  (`Content-Security-Policy: sandbox`, `nosniff`); only images, media, and
  plain text are shown inline, and markup, script, stylesheet, and
  WebAssembly types are never served as such. An avatar must decode as the
  image type it declares.
- **Streams**: the broadcast embed carries a live `url`. Readers `GET` it for
  the kept text followed by more as it arrives; a reader that falls behind the
  kept window continues from it. The server keeps the trailing 64 KiB. The
  stream ends when the write body ends, after one hour, or at 16 MiB (the
  writer gets `413`); the server then publishes the kept text as `text` in
  place of `url`, and both URLs stop working. Saving the message without the
  embed ends the stream (the writer gets `410`).
- **Avatars** ([PROTOCOL.md §4.6.6](https://github.com/shazow/apron/blob/main/PROTOCOL.md#466-avatars)): the `/avatar` command with one `upload`
  embed returns a write URL. A PNG, JPEG, GIF, or WebP of at most 2 MiB
  becomes the sender's `avatar`, followed by a `user` notification; replacing
  or removing the avatar deletes the upload.

## Reactions

`reactions` sets the caller's complete emoji set on a message and returns `{}`
after the broadcast; the logged record is delivered as
`{log_id, message_id, room_id, reactions: [{from, emojis}]}`, with `room_id`
the message's current room. Duplicate emoji collapse, `[]`
clears, and a request that leaves the set unchanged logs nothing. Unknown
messages, non-string or empty entries, entries over 64 bytes, and more than 20
distinct emoji per user are `invalid_params`.

## Commands

`command` ([PROTOCOL.md §4.8](https://github.com/shazow/apron/blob/main/PROTOCOL.md#48-command)) takes the params of a new message, in
`general` without `room_id`, and is never logged, broadcast, or saved;
`message_id` and `deleted` are `invalid_params`, as are text that does not
start with `/` and unknown commands (`Unknown command /foo; try /help`).
Mentions in commands notify no one. A command's effects and `~private`
replies arrive before its result. Commands:

- `/help` sends the calling connection, and only that connection, a
  `~private` notice
  (`from: {user_id: "~private", name: "System message to you"}`, Markdown, no `message_id`
  or `log_id`, not logged) in the command's room, listing the commands the
  sender may use there, and returns `{}`.
- `/avatar` with exactly one `upload` embed returns
  `{"embeds": [{embed_id, kind, write_url}]}`; see Avatars above.
- `/kick @user [reason]` removes the one user named in `body.mentions` from
  the room. Only the room's creator and users with the `admin` or
  `moderator` role may kick (`denied` otherwise, so only they can kick in
  `general`). The removal is a logged leave membership with the
  target as `user`, delivered to the room's members, the target included;
  the target then receives `room_update` `left`, and the remaining members a
  logged `~room` message (`from: {user_id: "~room", name: <room title>}`),
  such as `@guest_3 was removed by @guest_1: spamming`. The reason is the first line
  of the rest of the text, at most 200 characters.

## Activity

`activity` relays `typing` (seconds; `0` stops) and `read_message_id` in a
room (`general` without `room_id`) to the room's members. A read cursor must
name an existing message and only moves forward; the server keeps each
user's latest cursor per room and sends it after `room_list` lists the room.
A frame that changes nothing relays nothing. `away` (boolean) marks the
sending connection as unattended; it is never delivered, and ends with
`away: false`, `typing`, `read_message_id`, a `message` or `command` from
that connection, or the connection closing.

## Push

`server.push` offers the `relay` kind. `push_register` takes
`{kind: "relay", url, token?}`; `url` (at most 2,048 bytes) must be `https`
(unless `--push.allow-insecure`) and identifies the registration, so
registering it again replaces it, even another user's: the URL names a
device, which may sign in as someone else; a user may hold ten. `push_unregister` removes the caller's
registration for a `url`. Registrations belong to the user, so they matter for
accounts; a guest's end with the guest.

A new message wakes the users listed in `body.mentions` and the author of the
message it replies to; an edit wakes only the users it adds to
`body.mentions`. Text is never parsed for mentions. A mention wakes a user in
any room they can see, so in a private room only its members; a reply wakes
its target's author only in a room they have joined. Either way, a user is woken only when every
connection of theirs is away or gone. The server POSTs the push payload (the message without `log_id`,
`format`, or `embeds`, text truncated to 1,000 characters) to each of their
endpoints with `token` as bearer. Deliveries run in the background, apart
from message delivery, in a lane per relay host: at most 8 at once to one
host and 32 in all, so a slow relay delays only pushes to itself. A user has
at most 20 deliveries waiting or running and the server 1,024; beyond them a
push is dropped. Deliveries connect only to public addresses: never loopback,
private, link-local, shared (CGNAT, `100.64.0.0/10`), benchmarking, reserved,
or IPv6 translation addresses. A relay answering `404` or `410` loses its
registration.

## Requests and errors

Frames must be I-JSON ([RFC 7493](https://www.rfc-editor.org/rfc/rfc7493)):
a frame repeating an object key or holding invalid UTF-8 is a parse error.
The server encodes JSON with `encoding/json/v2` and needs Go 1.27.

Request IDs deduplicate per user, across all of that user's connections: a
retry returns the original result without re-executing or rebroadcasting, a
concurrent duplicate waits for the original, and reuse with a different
method or params is `invalid_params`. The latest 1,024 IDs per user are kept;
failed requests are not cached, and neither are `history` and `room_list`,
which change nothing and may be large: a duplicate of one that has finished
runs again. Unknown requests return `unsupported`.

Errors not tied to a request omit `id`. On shutdown every connection
receives `retry_after` (`data.retry_after: 5`) before it closes; over
`--max-connections` a new connection receives the `server` frame and then
`retry_after` (`data.retry_after: 30`).

## Passkeys

Open the frontend at **http://localhost:5173** during development, or
**http://localhost:8080** when serving a static build. Use the profile panel's
**Add passkey** button to attach a discoverable, user-verified credential to your
current identity, retaining ownership of messages you already sent. **Sign in
with passkey** restores the identity selected in your browser's passkey picker.
You can add up to ten credentials to an identity. Guest chat remains available.

Use explicit settings for an HTTPS deployment (origins refer to the page running
the frontend, which may differ from the WebSocket server):

```sh
go run ./cmd/aprond --static-dir .apron-web/build \
  --origin https://chat.example.com \
  --webauthn.rp-id chat.example.com \
  --webauthn.origin https://chat.example.com
```

`--webauthn.rp-id ''` disables passkeys. `--webauthn.origin`, repeated, lists
exact origins, including ports. The RP ID must be a domain valid for the
frontend origin. Changing the RP ID creates a different credential scope.
`--allow-any-origin` does not relax WebAuthn origin validation. The default
`127.0.0.1` chat URL still supports guest chat; use `localhost` for passkeys.

The implementation uses [go-webauthn](https://github.com/go-webauthn/webauthn)
for registration and signature verification. Challenges are random, expire after
two minutes, and belong to one connection, action, RP ID, and origin. A finish
for the current challenge consumes it before verification, including failed
attempts; a new begin replaces the outstanding challenge. Authentication
requests are not cached for replay.
User presence and verification are required; login updates the credential's
signature counter and flags and rejects a clone warning.

Successful registration or login, like an [email sign-in](#email-sign-in),
returns an opaque bearer token for `scheme: "token"` on later connections
([PROTOCOL.md §3.2](https://github.com/shazow/apron/blob/main/PROTOCOL.md#32-authentication)).
Tokens last twelve hours from their latest use, are stored hashed on the
server, and are bound to the frontend origin. A token sign-in renews the
token and answers with the same token rather than a replacement, since
several tabs may share it; clients that keep the latest token keep it.
Sign-out drops the local token and reconnects. Already authenticated
connections and disconnected clients retain their server-side token until it
expires. An expired token requires another passkey or email sign-in.

Passkey users, their credentials, and unexpired tokens are kept in the store,
so they survive a restart with the default SQLite store; with `--store memory`
a restart invalidates them, including passkeys still present in your
authenticator. Credential removal and account recovery are future work.

### Example WebAuthn exchange

These examples define the Go server's bearer-token policy alongside the canonical
protocol exchange ([PROTOCOL.md §4.9](https://github.com/shazow/apron/blob/main/PROTOCOL.md#49-webauthn-authentication)). All steps use `auth` requests with fresh IDs
over the same WebSocket; no HTTP authentication endpoints are needed.

| `params.action` and `params.step` | Other parameters | Result |
| --- | --- | --- |
| `action: "register", step: "begin"` | None; current connection must be authenticated | `{challenge_id, public_key}` creation options |
| `action: "register", step: "finish"` | `challenge_id`, `credential`: browser credential JSON | `{you, token}` |
| `action: "login", step: "begin"` | None | `{challenge_id, public_key}` discoverable request options |
| `action: "login", step: "finish"` | `challenge_id`, `credential`: browser credential JSON | `{you, token}` |

Bearer resumption uses the separate `token` authentication scheme:
`{"scheme":"token","token":"..."}`. Dropping the token and reconnecting
signs out the current client; tokens remain valid for their configured lifetime
and are not revoked by disconnecting.

Every ceremony request includes `params.scheme: "webauthn"`. Binary values in options and
credentials use unpadded base64url, matching the browser's
`PublicKeyCredential.parseCreationOptionsFromJSON`,
`parseRequestOptionsFromJSON`, and `toJSON` APIs. Begin responses do not
authenticate the connection. Failed verification returns `denied` and preserves
the current identity. Notifications do not start or finish ceremonies. A client
should pause chat operations while switching identities and must start a new
ceremony after a disconnect.

Embedding applications opt in through `Config.WebAuthn`, using a validated
`webauthn.WebAuthn` instance, and `Config.EmailSender` for email sign-in.
With both nil only guest authentication is enabled.

## Email sign-in

With email sign-in ([PROTOCOL.md §4.10](https://github.com/shazow/apron/blob/main/PROTOCOL.md#410-email-authentication)),
`server.auth` and `server.signup` list `email`. An `auth` with
`scheme: "email"` and `email` asks for a code and returns `{}`, whether or
not the address has an account. It changes no authentication: on a
connection not yet signed in, requests pipelined behind it are `denied`, and
on one signed in they run as before. So does a denied code. A code is six
digits, valid for ten minutes, and consumed by its use; five wrong attempts
invalidate it. Codes are kept in memory only. Addresses are compared
lowercased; one with a display name, an address literal such as
`a@[10.0.0.5]`, or a domain without a dot such as `a@localhost` is
`invalid_params`, and so is a malformed `name` or `user_id`, which leaves
the code usable.

There are two kinds of code, and neither stands in for the other:

- **Sign-in codes**, asked for on a connection not signed in. A newer one
  replaces the address's earlier one. Presented on a connection not signed
  in, a known address signs in to its account; an address new to the server
  becomes a new account, which takes a requested `user_id` by the guests'
  rules or else `user_<n>`, honors a requested `name`, and joins `general`,
  delivered before the result. The result is `{you, token}`. Presented on a
  connection signed in, a sign-in code is `denied`: it changes nothing.
- **Add codes**, asked for on a connection signed in, add the address to that
  account (§4.10): a guest becomes an account and keeps its `user_id`, rooms,
  and messages. An account has one outstanding add code, replaced by its
  next request, and others' requests for the address do not touch it. It is
  accepted only on a connection signed in as that account, any of its
  connections, and denied anywhere else, so it never signs anyone in. It is
  sent only when the address could be added: no other account holds it,
  which is never moved, and the account has no address yet; otherwise the
  request still answers `{}` and the code, never sent, is `denied`. Its
  email has no link. The result is `{you}`, and a `token` too when the
  connection had none, as a guest's has not.

A code proves only that its presenter reads the address's mail, not that the
address belongs to whoever is signed in. Without this split, someone could
ask for a code for their own address, get a person who is signed in to
present it through a link, and then sign in to that person's account with
it. To sign in with another address, use a new connection (sign out in the
client).

Limits keep codes from being guessed or sent in floods. Each answers
`retry_after` (`-32002`, with `data.retry_after` in seconds; PROTOCOL.md
§1.1), whether or not the address has an account:

- Per client (the connection's IP address, or its IPv6 /64; with
  `--client-ip-header` behind a reverse proxy, the address the proxy
  reports): ten codes, then one every three minutes; ten wrong codes, then
  one every six minutes, across every address, which bounds untargeted
  guessing to about ten guesses an hour per client against a million codes.
- Per address, in any hour: six codes at least 30 seconds apart, and thirty
  wrong codes from anyone. Past thirty, codes for the address are refused,
  the right one included, until the oldest wrong code is an hour old.
- Per connection: five codes, then one every two minutes.
- At most sixteen deliveries at once.

The server tracks at most 10,000 addresses and 100,000 clients, forgetting
the least recently used past that, so a full table refuses no one. Two
risks remain. Guessers spread over many clients can still keep one address
from email sign-in, thirty wrong codes an hour, though they gain only thirty
guesses an hour against it; a passkey or a kept token still signs in. And a
flood over more than 10,000 addresses from many clients makes the server
forget older budgets.

Email accounts are kept like passkey users, and an account may have both: a
passkey registered on a signed-in connection is added to that account
(§4.9). The address is never sent to clients.

A sign-in code's email carries the code and, when `--email.link-url` (or
`--public-url`) is set, a link to that page with the address, the code, and, with
`--public-url`, this server's WebSocket URL in the fragment, form-encoded
and in that order:
`https://chat.example/#email=ada%40example.com&token=418092&server=wss%3A%2F%2Fchat.example%2Fws`.
A client reads it to sign in, on a new connection, to the server named.
The log sender, for development, writes codes and links to the server log
instead of sending them; use `smtp` in a deployment:

```sh
aprond --public-url https://chat.example.com \
  --email.sender smtp --email.from "Apron <chat@example.com>" \
  --email.smtp-addr smtp.example.com:587 \
  --email.smtp-user chat@example.com --email.smtp-password "$SMTP_PASSWORD"
```

The SMTP sender requires STARTTLS unless `--email.smtp-insecure` is set, for
a relay on the same host, or speaks TLS from the start to a relay on port
465, and bounds each delivery, connection included, to 30 seconds. Embedding applications provide any other delivery by
implementing `server.EmailSender`.

Invitation tokens that sign up several people
([PROTOCOL.md Appendix B](https://github.com/shazow/apron/blob/main/PROTOCOL.md#appendix-b--valid-scenarios-informative))
are not implemented: a token here always resumes one account.
