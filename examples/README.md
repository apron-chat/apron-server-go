# Example servers

Each directory is an Apron server of its own: a `main` package that runs
[`aproncmd.Main`](../aproncmd) with an extension built on package
[`apron`](../apron), and a test that runs it. They take every `aprond` flag,
plus their own, named after the extension (`--sso.org`), which a config
file sets in a table of that name (`[sso]`). Run one with
`go run ./examples/<name> --help`.

| Example | What it shows |
|---------|---------------|
| [`sso`](sso) | External sign-in with GitHub for the members of one organization, with admins by login, behind the server's own OAuth sign-in page: `ExternalSignIn`, `Complete`, `Cancel`, `Handle`. |
| [`moderation`](moderation) | No links from new accounts or guests, masked words, and a moderators' `/slowmode`: `FilterMessage`, `OnEvent`, `Command`, `Store`, `RoomNotice`. |
| [`assistant`](assistant) | A bot that answers its mentions, streaming the reply from a command such as a local model: `Bot`, `OnEvent`, `Visible`, `History`, `StreamAppend`, `Go`. |
| [`ops`](ops) | Signed GitHub webhooks posted to a room, and `/deploy` streaming a terminal for people with the `deployer` role: `Handle`, `Bot.Post`, `StreamTerminal`. |
| [`irc-bridge`](irc-bridge) | A room bridged to an IRC channel both ways, relaying what it missed while down: `Follow`, `WithExt`. |
| [`private`](private) | Only invited people may sign in, and nothing is visible to anyone else; admins manage the list with `/allow` and `/disallow`, which signs people out: `RestrictSignIn`, `Allowlist`, `ParsePrincipal`, `Store`. |
| [`search`](search) | Full-text search as a request and a command, limited to rooms the person may see, and a web archive of public rooms: `Method`, `Advertise`, `OnEventFrom`, `Visible`, `Public`. |

[`minimal`](minimal) is the other kind: a whole server in about a hundred
lines on package [`wire`](../wire), which speaks the protocol for a program
that keeps its own state, such as a gateway in front of another chat system.
Its commands are a [`wire/commands`](../wire/commands) Router, which answers
`/help` and lists them. It takes only `--addr`, and its test uses package
[`wiretest`](../wire/wiretest). Run the conformance checks against it with
`conform.py ws://127.0.0.1:8080/ws`.
