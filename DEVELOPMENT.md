# Development

The SvelteKit client and Go server implement the protocol independently. The
server keeps its state in a SQLite database by default (`--store memory`
forgets it on restart); see [SERVER.md](SERVER.md#storage).

## Layout

- The repository root is the Go module and the reference server, implementing
  every capability in [PROTOCOL.md](https://github.com/shazow/apron/blob/main/PROTOCOL.md);
  `cmd/aprond` is the executable and `internal` contains implementation
  packages. See [SERVER.md](SERVER.md).
- [apron-chat/apron-web](https://github.com/apron-chat/apron-web): the
  SvelteKit and TypeScript web client, deployed at `https://web.apron.chat`.
  `make install` clones it into `.apron-web/` (ignored), where the browser
  tests, `make dev-web`, and `make run` use it; replace that directory with a
  symlink to work on your own checkout.
- `tests/interop`: Playwright tests against real clients and the Go backend.
- `testdata/apron`: a submodule of [shazow/apron](https://github.com/shazow/apron),
  pinned to the protocol version this server implements. Its
  `tests/fixtures/wire` holds portable JSON replay and session scenarios with
  expected protocol state; see its README for adapter requirements.
  `make install` initializes it (or run `git submodule update --init`); to
  follow protocol changes, check out a newer commit there and commit the
  submodule.

The public demo backend at `wss://server.apron.chat/`, a TypeScript Worker and
SQLite Durable Object, lives in
[apron-chat/apron-server-cloudflare](https://github.com/apron-chat/apron-server-cloudflare).

Each implementation owns its manifest, lockfile, and unit tests. Add other
clients or servers as sibling directories. Extract shared libraries only when
there is another consumer.

## Run locally

With Nix and [devenv](https://devenv.sh/getting-started/) 2.3+, from the
repository root:

```sh
devenv shell -- make install
devenv up
```

The locked environment provides Node.js 24, npm, Go 1.27, Make, a C compiler
for Go race tests, and Chromium on Linux. `devenv up` starts the Go backend and
Vite frontend; Ctrl-C stops both. Use `devenv shell` for an interactive shell
with the same tools. Dependency installation is explicit; rerun `make install`
after manifest or lockfile changes.
For background processes, use `devenv up --detach` and stop them with
`devenv down`.

Without Nix, install Node.js 24 LTS, npm, Go 1.27+, Make, Git, and a C
compiler. Run `make install`, then `make dev-server` and
`make dev-web` in separate terminals. All Make commands below work inside `devenv shell` or with those
tools installed directly.

Open `http://localhost:5173`. The development server proxies `/ws` to
`127.0.0.1:8080`. Open another browser tab to chat with a second client.

To use the Cloudflare demo backend locally, run it from
[apron-chat/apron-server-cloudflare](https://github.com/apron-chat/apron-server-cloudflare)
with `npx wrangler dev --port 8080` instead of `make dev-server`. The web
client, the Go server, and the worker speak protocol v7; the worker does so
within the demo's budgets and policies (only threads under `general` can be
created, guests only read until they sign in with a passkey, and guests'
memberships are not logged, so its `room_list` ignores `latest_log_id`). A
signed-in user can run `/invite-bot` for a bot token. The public demo has
persistent passkeys and rolling history; its passkey registration creates a new
identity instead of upgrading guest ownership as the Go example does.

The example starts with a guest identity. Use **Add passkey** in the profile
editor's Sign-in row to retain that identity and its message ownership, and
**Sign in with passkey** to return to it (or choose Passkey on the connect
screen). Passkey sessions resume after transport disconnects;
page reloads require signing in again. Guest reconnects receive a new identity.
Edit and delete permissions belong to the identity that created the message.
Use `localhost` for the default passkey configuration; see
[SERVER.md](SERVER.md#passkeys) for deployment settings.

The Go server also offers email sign-in, off unless `--email.enable` is set.
To try it without sending email, run
`go run ./cmd/aprond --email.enable --email.sender log`, which writes each
code, and a sign-in link when `--email.link-url` is set, to the server's log.
Adding an address while signed in as a guest keeps the guest's identity, as
adding a passkey does; see [SERVER.md](SERVER.md#email-sign-in). `--welcome` sets the text
the sign-in screen shows, and `--role admin=<user_id or email>` grants a
role, shown as a badge, that may remove people from any room.

## Threads

A thread is a room with a `parent_room_id` (PROTOCOL.md [§3.4](https://github.com/shazow/apron/blob/main/PROTOCOL.md#34-rooms), [§4.3.4](https://github.com/shazow/apron/blob/main/PROTOCOL.md#434-creating-and-editing)). The
client lists the rooms you have joined with `room_list`,
threads included, with their members, sent right behind `auth` without waiting
for its result ([§3.2](https://github.com/shazow/apron/blob/main/PROTOCOL.md#32-authentication)), and follows `room_update` from then on ([§4.3](https://github.com/shazow/apron/blob/main/PROTOCOL.md#43-rooms)). After
a dropped connection it resumes each room's history from where it stopped in
the same way, and a resumed passkey session lists only the rooms that changed
since. Member lists start from those listings and follow the `membership`
records of joins and leaves ([§4.3.2](https://github.com/shazow/apron/blob/main/PROTOCOL.md#432-membership)), which also show in the room's
timeline as quiet "Ada joined" lines, merged and netted out between messages so
guest churn stays quiet. The sidebar lists top-level rooms; the
open room's joined threads are listed under it, and every thread of the room,
joined or not, is shown as a card in its feed. Only joined threads deliver
live. Opening a thread you haven't joined reads it through `history` without
joining it (joining is a logged membership everyone sees): its header offers
Join, and replying joins it first. The cards of threads you haven't joined
refresh when the room is opened or its threads are listed, with `room_list`
and the room as `parent_room_id`. Start a thread from any message in a room (Start
thread in its toolbar; cap `rooms`): the client creates a room under the
current one with `room_set`, titled after the message's first line, whose
`description` carries the gist ([§3.4](https://github.com/shazow/apron/blob/main/PROTOCOL.md#34-rooms)), which joins you to it; by
convention the thread's first message replies to the message it started
from. The message stays where it is. A thread opens once its `room_update`
arrives. Drafts and reply targets are kept for each room, and a thread is a room
of its own. See [apron-web's README](https://github.com/apron-chat/apron-web#readme)
for how threads and their descriptions are shown.

Threads load their own history (`history` on the
thread's `room_id`) when opened, newest page first, so message counts in the
sidebar and on cards appear once a thread has loaded. The room settings
edit a room's title and description (`/topic` sets the description); the save
is a `room_set` request with the room's `room_id` that resubmits the other
fields and `ext` unchanged. Any
authenticated user may create threads and edit rooms on the Go example;
the Cloudflare demo allows creating threads but denies editing its permanent
`general` room, and a denied request is reported like any other error.

Your messages move through select mode (cap `edit`): shift-click one (or press
`x` on it, long-press it on touch, or use More → Select) to enter it,
shift-click another to fill the range, then pick a thread (or, from inside a
thread, the room) or start a new thread from the selection bar that takes the
composer's place. A move is a `message` save with the destination's `room_id`;
each message is a separate save. A new thread is created first, described by
the earliest picked message and titled after it, and the moves go out once the
server has named it; the thread then opens. Other moves leave the pane where it
is. Messages the server denies stay selected and the bar reports how many didn't
move. Escape leaves select mode. Moved messages keep their reply references and
reactions.

Who a message mentions is its `body.mentions` ([PROTOCOL.md §3.5](https://github.com/shazow/apron/blob/main/PROTOCOL.md#35-messages)); in the text a
mention follows the `@user_id` convention ([Appendix A.3](https://github.com/shazow/apron/blob/main/PROTOCOL.md#a3-prefixes-in-text)): a
known user renders as a chip with their current name, a room as a link, and
unknown IDs as written, never inside code. A message whose `body.mentions` lists
you tints its row and pulses once when it arrives (or an edit adds you), and
shows an `@` badge on a room you aren't reading (a thread's mentions badge its
parent room) or a rust jump bar when it landed above the fold; text that merely
contains your ID does not. Messages from history, including a thread's
history loaded when it is opened, never ping. Typing `@` in the composer lists
the room's members, or its recent senders on a server without `room_list`; a
picked person, or a finished `@name` or `@user_id` that names exactly one of
them, becomes a chip showing their name that is sent as `@user_id` and listed
in `body.mentions`. Message headers show each sender as their name with the
muted `@user_id` beside it, always when another user known to the client shows
under the same name ([§3.3](https://github.com/shazow/apron/blob/main/PROTOCOL.md#33-identity)). A sender's name and avatar come from the latest
profile the server sent for them, else from the message itself.

With cap `command` ([PROTOCOL.md §4.1](https://github.com/shazow/apron/blob/main/PROTOCOL.md#41-command)), composer text starting with one `/` is
a command: the composer tags it, and Run sends it as a `command` request
(`/nick`, `/join`, `/leave` and `/topic` map to `me`, `room_join`, `room_leave`
and `room_set` with `description`), while `//` posts a message starting with `/`. Replies arrive as
`~private` notices without a `message_id`, shown only to you for the session
with a dashed outline; a failed command's error shows the same way. The Go
server offers `/help`, `/avatar` with an attached image, and `/kick @user
[reason]` for a room's creator, admins, and moderators.

Use a message's Reply action to reference it in a new message. `reply_to` may
name a message in any room, so a reply in a thread can quote a message in the
room and the other way round. The composer keeps the reply target with the
destination's draft. A reply shows the quoted message above its body; clicking
the quote opens the room or thread the original lives in (loading a thread's
history first), scrolls to it and highlights it briefly. Edits preserve
references; More → Remove reply removes one. Deleted targets display as
“Message deleted”; targets the client has never seen display as “Message
unavailable”.

With cap `reactions`, a message's React action opens a small palette of emoji
(👍 ❤️ 😂 🎉 😮 😢 👀 ✅); its More emoji button opens the full emoji picker
([emoji-mart](https://github.com/missive/emoji-mart)) for any other. The
composer's emoji button opens the same picker and inserts the emoji at the
caret. The picker and its data are bundled and load on first open; they make
no requests beyond the app's own origin. Reactions show as chips under the message with a
count, highlighted when one of them is yours; a chip's tooltip lists who
reacted, and clicking it adds or removes your reaction. Each change sends your
complete emoji set for that message with `reactions`. Tombstones hide their
reactions.

A new guest on the Go server has joined `general`; other rooms are joined
from Browse rooms, and threads from More threads…, a thread's Join, or by
replying in it. A thread's members are only those who joined it (its creator
first), independent of its parent room. Empty threads remain available;
deleting or moving the message they were started from does not remove them.
Private rooms and private threads are visible only to their members, who add
others with `room_join` and a `user_id`. With the Go server the client also sends attachments and
voice clips, shows live streams, sets avatars, marks where you stopped reading,
and browses, joins, and leaves rooms; see
[apron-web's README](https://github.com/apron-chat/apron-web#readme). The interop suite's
`reference.spec.ts` exercises these against the Go server.

## Build and serve

```sh
make run
```

Open `http://localhost:8080`. The Go process serves the static SvelteKit build
and WebSocket endpoint from the same origin; no Node.js process is needed.
`make run` builds the frontend, builds the Go executable, then starts it.
Use `make serve` to run the existing build. Re-run `make run` after source changes;
use the two development processes above for frontend hot reload.
See [SERVER.md](SERVER.md) for server flags and origin configuration.

## Validate

Run the full validation suite, including builds and browser tests:

```sh
devenv test
```

Run individual checks inside `devenv shell`:

```sh
make check
make test
```

`make test` runs the Go tests and the session fixtures over real loopback
WebSockets against the web client's `ChatClient`. Run the latter alone with
`make test-wire`; it needs Node.js and Go, but no browser or running dev server.

The devenv environment supplies Chromium on Linux. Outside that environment,
or on macOS, install Chromium once for browser tests:

```sh
cd tests/interop
npx playwright install chromium
```

Then, from the repository root:

```sh
make test-interop
```

The browser tests start their own server and frontend. Stop existing processes
on ports 8080 and 5173 before running them. On Linux, Playwright may also need
system libraries (`npx playwright install --with-deps chromium` on supported
distributions). For a custom browser installation, set:

```sh
PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/path/to/chromium make test-interop
```

The `push` project (`push.spec.ts`) needs full Chromium, not the headless
shell, which `npx playwright install chromium` also installs; with a custom
browser, point the variable at full Chromium. It skips itself, listing the
reason at the end of the run, against an apron-web without web push. To run
against another apron-web checkout, such as a pull request's worktree, set
`APRON_WEB_DIR`:

```sh
APRON_WEB_DIR=../apron-web-pr make test-interop
```

See [`tests/interop/README.md`](tests/interop/README.md) for what each suite
covers and the flags the server is started with.

No root JavaScript workspace or Go workspace is required.

## Environment updates

`devenv.lock` pins Nix inputs. Run `devenv update`, validate with `devenv test`,
and commit the lockfile when updating the toolchain. Keep Node and Go versions
aligned with `.node-version`, `go.mod`, and CI. Machine-specific overrides go
in ignored `devenv.local.nix` or `devenv.local.yaml` files.
