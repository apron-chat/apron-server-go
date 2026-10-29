# Browser interoperability tests

These tests exercise the SvelteKit client against the Go server through real
Chromium browser contexts. They cover cross-session broadcasts and history
recovery, edit/delete snapshot replay including a deleted-message tombstone,
thread creation and moves, safe rendering of untrusted markup, and phone viewport layout.
The desktop suite also reloads a browser while offline, restores connectivity,
verifies history replay without duplicates, and sends another message.
`reference.spec.ts` covers what the Go reference server adds: uploads hosted
with `og` previews, live streams written over HTTP, `iframe`/`html`/unknown
embeds, avatar uploads through the `/avatar` command and renames shown on
earlier messages, commands run from the composer with their `~private` replies
and errors shown only to you, leaving and rejoining rooms and threads through
`room_list`, the New divider from read cursors, `@user_id` and room
mentions, the server's title, avatar, and host in the sidebar head and its
description on the connect screen, and ❤ and ❤️ from two users counted as
one reaction. It creates streams, raw embeds, and the bare ❤ with a small protocol client
connected through the Vite proxy, so the URLs the server mints load
same-origin. Rooms follow protocol v9: a new guest has joined only `general`,
and a thread's members are those who joined it, so a second browser opens
another's thread from its card, which reads it without joining it
(`openThread` in `test-helpers.ts`); only joining, or replying, makes it live.
The WebAuthn suite uses Chromium's virtual authenticator against the real Go
verifier. It covers passkey registration, login, sign-out, session resumption,
message ownership, and recovery from an invalid signature. These tests use
`localhost` to match the default RP ID; other browser tests use `127.0.0.1`.

Install and run from the repository root:

```sh
cd tests/interop
npm ci
npx playwright install chromium
npm test
```

The config starts both services itself with `reuseExistingServer: false`:

* Go server: `127.0.0.1:8080`, started from the repository root with
  `--store memory --addr 127.0.0.1:8080 --push.allow-insecure
  --push.vapid-private-key <test key> --push.vapid-subject
  mailto:interop@example.com --title <title> --description <text> --avatar
  <data:image URL>`. The title, description, and avatar are in
  `server-look.ts`, for `reference.spec.ts`. The push flags are for `push.spec.ts` and
  change nothing for the other tests, which register no push endpoint. The
  key is the public test fixture in `push-test-vapid.ts`, never one for a real
  server; `--push.allow-insecure` lets the test's capture endpoint on
  `http://127.0.0.1` receive pushes, and aprond refuses it with `--public-url`
  or `--tls.domain`.
* Vite dev server: `127.0.0.1:5173`, started from `.apron-web`, a submodule of
  [apron-chat/apron-web](https://github.com/apron-chat/apron-web) pinned to the
  version under test, or from `APRON_WEB_DIR` when it is set

Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` when the browser is supplied by the
environment (for example, the NixOS VM). If it is unset, Playwright uses its
normal Chromium resolution.

## Web push and status

The `push` project runs `push.spec.ts` and `seen.spec.ts`. In `push.spec.ts`,
a passkey user turns on push in Preferences, registering the wake scopes the
web client notifies about by default that the server advertises (mentions,
replies, and private rooms), goes idle, and is mentioned, replied to, and
written to in a private room by a second client;
the test receives the pushes on a local capture endpoint, checks the VAPID
token against the test key and decrypts the `aes128gcm` payload with its own
subscription keys, and checks the service worker's notification. A second test
checks `status` as other clients see it: the derived online, idle, and
offline, and dnd and invisible set through the web client with `me`; and that
mutes stay private, come back to the user's connections as `status`
notifications, also after a reload, and are applied by the web client. The web
client sends `status` as a request, each answered `{}`; the test's own room
mutes are requests on the page's connection too, whose replies the page never
sees. The web client goes idle after five minutes without input, so both
tests in `push.spec.ts` move the page's clock (Playwright's `page.clock`) past
it, in steps shorter than the 30-second ping interval so the connection stays
up; the project allows three minutes a test. Run it alone with:

```sh
npx playwright test --project=push
```

Notifications need full Chromium in its new headless mode (`channel:
'chromium'`): the headless shell reports `Notification.permission` as denied
whatever is granted. `npx playwright install chromium` installs both builds;
with `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`, point it at full Chromium.

The status test sets a status through a control named for do not disturb in
the profile editor or Preferences, shown directly or behind one named Status.

`seen.spec.ts` checks `seen` (§4.5.2), when a user was last shown offline, as
the web client shows it in another browser: an offline member's row in the
member list (`room-member-list`, `li[data-user]` with `data-status`) and the
profile card it opens, which says "Last seen recently" within ten minutes
(`profile-seen`, a `<time>` whose tooltip then says "Last seen within the last
10 minutes" and whose `datetime` is checked against when it happened). A
browser that watched the change happen shows the later of the server's `seen`
and when it received the change (the web client's local floor), so the
server's own is read from a browser that opens afterwards, whose `room_list`
carries it, and the watcher's must be no earlier than that. The subject is
an account, made with a passkey on Chromium's virtual authenticator, since a
guest is retired when its connection ends and so is never shown offline after
it leaves; the watchers are guests. When the account's last tab closes it shows
offline with Last seen; back in a new tab, resuming its session, it shows
online without it; choosing Invisible in its Preferences shows it offline with
a new Last seen, which stays, with the row offline, while its tab closes and
another opens; and a browser that opens after the account left learns `seen`
from its `room_list`. Run it alone with `npx playwright test --project=push seen`.

The UI contract used by the tests is an accessible textbox named `Message`, a
`Send message` button, and message containers rendered as
`article[data-message-id]`. Mutation controls are named `Edit message`,
`Save changes`, and `Delete message`; the editor textbox is named `Edit
message`. A deleted container remains visible with the exact text `Message
deleted`.

## Shared session fixtures

`npm run test:wire` runs `../../testdata/apron/tests/fixtures/wire/session` against the actual
TypeScript `ChatClient` using Node.js WebSockets. It requires Node.js 24 and
Go; no browser, Vite process, or example backend is started. The runner builds
`wire-peer.go` using the existing Go module dependencies and starts it on a
random loopback port. HTTP control endpoints deliver fixture frames and capture
client requests; protocol traffic uses a real WebSocket.

The peer is a transport utility, not a protocol implementation or test oracle.
Expected state resides exclusively in JSON fixtures. The runner normalizes
client state and asserts it without querying UI elements or private fields.
Both envelope forms run for every scenario variant. See
[`tests/fixtures/wire/README.md`](https://github.com/shazow/apron/blob/main/tests/fixtures/wire/README.md) in shazow/apron (checked out at `testdata/apron`) for the portable format.

## Rendering benchmarks

`perf.spec.ts` times user journeys against the production build, served by the
Go server on `127.0.0.1:8090` (`make test-perf` from the repository root builds
it first). It seeds two rooms of 200 messages and switches between them, then
reports the time to the first painted frame and to the whole room with the CPU
throttled 4x, and Chrome's layout and style counts. The element count is the
same on every run, so `perf-ceilings.json` holds a ceiling for it: the test
fails when a change raises it. When a change lowers it, run with
`PERF_UPDATE=1` to write the new ceiling, and commit it. Set
`PERF_PROFILE=out.cpuprofile` to save a CPU profile of the measured switches.
