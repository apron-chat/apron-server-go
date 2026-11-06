# Example servers

Each directory is an Apron server of its own: a `main` package that runs
[`aproncmd.Main`](../aproncmd) with an extension built on package
[`apron`](../apron), and a test that runs it. They take every `aprond` flag,
plus their own, named after the extension (`--sso.org`), which a config
file sets in a table of that name (`[sso]`). Run one with
`go run ./examples/<name> --help`.

| Example | What it shows |
|---------|---------------|
| [`sso`](sso) | External sign-in with GitHub for the members of one organization, with admins by login: `ExternalSignIn`, `Complete`, `Handle`. |
| [`moderation`](moderation) | No links from new accounts or guests, masked words, and a moderators' `/slowmode`: `FilterMessage`, `OnEvent`, `Command`, `Store`, `RoomNotice`. |
| [`assistant`](assistant) | A bot that answers its mentions, streaming the reply from a command such as a local model: `Bot`, `OnEvent`, `Visible`, `History`, `StreamAppend`, `Go`. |
| [`ops`](ops) | Signed GitHub webhooks posted to a room, and `/deploy` streaming a terminal for people with the `deployer` role: `Handle`, `Bot.Post`, `StreamTerminal`. |
| [`irc-bridge`](irc-bridge) | A room bridged to an IRC channel both ways, relaying what it missed while down: `Follow`, `WithExt`. |
| [`search`](search) | Full-text search as a request and a command, limited to rooms the person may see, and a web archive of public rooms: `Method`, `Advertise`, `OnEventFrom`, `Visible`, `Public`. |
