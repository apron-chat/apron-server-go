# Examples

## Servers

Most directories are an Apron server of its own: a `main` package that runs
[`aproncmd.Main`](../aproncmd) with an extension built on package
[`aprond`](../aprond), and a test that runs it. They take every `aprond` flag,
plus their own, named after the extension (`--sso.org`), which a config
file sets in a table of that name (`[sso]`). Run one with
`go run ./examples/<name> --help`.

| Example | What it shows |
|---------|---------------|
| [`sso`](sso) | External sign-in with GitHub for the members of one organization, with admins by login, behind the server's own OAuth sign-in page: `ExternalSignIn`, `Complete`, `Cancel`, `Handle`. |
| [`moderation`](moderation) | No links from new accounts or guests, masked words, and a moderators' `/slowmode` of two forms: `FilterMessage`, `OnEvent`, `Command` and `Form`, `Store`, `RoomNotice`. |
| [`assistant`](assistant) | A bot that answers its mentions, streaming the reply from a command such as a local model: `Bot`, `OnEvent`, `Visible`, `History`, `StreamAppend`, `Go`. |
| [`ops`](ops) | Signed GitHub webhooks posted to a room, and `/deploy` streaming a terminal for people with the `deployer` role: `Handle`, `Bot.Post`, `StreamTerminal`. |
| [`irc-bridge`](irc-bridge) | A room bridged to an IRC channel both ways, relaying what it missed while down: `Follow`, `WithExt`. |
| [`private`](private) | Only invited people may sign in, and nothing is visible to anyone else; admins manage the list with `/allow` and `/disallow`, which signs people out: `RestrictSignIn`, `Allowlist`, `ParsePrincipal`, `Store`. |
| [`search`](search) | Full-text search as a request and a command, limited to rooms the person may see, and a web archive of public rooms: `Method`, `Advertise`, `OnEventFrom`, `Visible`, `Public`. |

[`minimal`](minimal) is the other kind: a whole server in under two hundred
lines on package [`apron/server`](../apron/server), which speaks the
protocol for a program that keeps its own state, such as a gateway in front
of another chat system. Its commands are an
[`apron/server/commands`](../apron/server/commands) Router, which answers
`/help` and lists them, and its bot, which answers when someone mentions
`@echo`, an [`apron/bot`](../apron/bot) Bot. It takes only `--addr`. Its
tests use package [`aprontest`](../apron/aprontest), and run the protocol's
core conformance checks in process
([`apron/conformance`](../apron/conformance)); run the whole
suite against it with `conform.py ws://127.0.0.1:8080/ws`.

## Clients

The other examples are clients of any Apron server, over the network, on
package [`apron/client`](../apron/client): `client.Dial` signs in, and its
connection makes requests and hands on what the server sends. They sign in
with the token in `$APRON_TOKEN`, such as a bot account's, or else as a
guest, and take the server's address from `--url` or `$APRON_URL`. Each
has a test that runs it against a reference server
([`aprondtest`](../aprond/aprondtest)).

| Example | What it shows |
|---------|---------------|
| [`notify`](notify) | Post one message to a room and exit, such as a notice from CI, its text from the arguments or stdin: `Dial`, `Post`. |
| [`tail`](tail) | A room's recent history, then its new messages as they come, without gaps or repeats, until interrupted: `OnFrame`, `History`, `Done`. |
| [`remote-bot`](remote-bot) | An echo bot on package [`apron/bot`](../apron/bot), whose Dialer is `client.Dial`, which the bot calls again whenever the server ends its connection. |
