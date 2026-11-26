# apron-server-go

**The reference server for [Apron](https://github.com/shazow/apron), a small,
open chat protocol over WebSockets.**

`aprond` is a single Go binary that implements every capability of Apron
protocol v10: rooms, threads, and private rooms, full history, edits and
replies, reactions and custom emoji, file uploads, live terminal output and
text streamed into messages,
commands, presence status and mutes, Web Push and relay push notifications, roles, and passkey,
email, and external (OAuth) sign-in. It keeps its state in SQLite, can serve a web client from the same
port, and can fetch its own TLS certificates.

## Quick start

```sh
go run ./cmd/aprond
```

The server listens on `127.0.0.1:8080` with a WebSocket endpoint at `/ws`.
Point the [Apron web client](https://github.com/apron-chat/apron-web) at it, or
build both and serve them together:

```sh
make install   # fetch the web client and its dependencies
make run       # build both, then serve them on http://localhost:8080
```

A public deployment with HTTPS, passkeys, and email sign-in looks like:

```sh
go install github.com/apron-chat/apron-server-go/cmd/aprond@latest
aprond --static-dir .apron-web/build \
  --tls.domain chat.example.com \
  --public-url https://chat.example.com \
  --origin https://chat.example.com \
  --webauthn.rp-id chat.example.com \
  --webauthn.origin https://chat.example.com \
  --email.enable --email.from chat@example.com \
  --email.smtp-addr smtp.example.com:587 \
  --role admin=you@example.com
```

To let a client on another domain, such as `https://web.apron.chat`, use
passkeys too, add its origin with `--origin` and `--webauthn.origin`; see
[Passkeys](SERVER.md#passkeys). Clients that cannot use the RP ID at all,
such as apps on other domains and terminals, sign in with a passkey on the
server's own [passkey page](SERVER.md#the-passkey-page), which aprond offers
while its `--public-url` is one of the `--webauthn.origin`s, as above.

Email sign-in is off unless `--email.enable` is set. To try it locally, run
`go run ./cmd/aprond --email.enable --email.sender log`, which writes codes to
the server log instead of sending them.

External sign-in, such as with GitHub or an OpenID Connect provider, is an
extension's: aprond is the OAuth authorization server apps sign in with,
and hands its page to the extension; see the [sso example](examples/sso)
and [External sign-in](SERVER.md#external-sign-in).

Run `aprond --help` for every flag, or `aprond --print-config > aprond.toml`
to start a config file.

## Your own server

The server is also a Go library. Package `aprond` serves the protocol, and
extensions add commands, request methods, bots, message filters, sign-in,
and HTTP routes to it without a fork. `aproncmd.Main` runs it with every
flag of `aprond`:

```go
package main

import (
	"context"

	"github.com/apron-chat/apron-server-go/aproncmd"
	"github.com/apron-chat/apron-server-go/aprond"
)

type Hello struct{}

func (Hello) Name() string { return "hello" }

func (Hello) Init(r *aprond.Registrar) error {
	r.Command(aprond.Command{
		Name: "hello", Help: "say hello",
		Run: func(_ context.Context, call *aprond.CommandCall) (map[string]any, error) {
			call.Notice("Hello, " + call.User.Name + "!")
			return nil, nil
		},
	})
	return nil
}

func main() { aproncmd.Main(Hello{}) }
```

A program that already keeps its own chat state, such as a gateway in front
of another chat system, uses package [`apron/server`](apron/server) instead:
it speaks the protocol, whose vocabulary is package [`apron`](apron), and
asks the program for every answer, as
[`examples/minimal`](examples/minimal) shows in under two hundred lines.

[`examples`](examples) has complete servers: GitHub single sign-on,
moderation, an AI assistant, an ops room with deploys, an IRC bridge, and
search with a public archive. A program that talks to any Apron server,
such as a script, a CLI, or a bot on a server it does not run, uses package
[`apron/client`](apron/client), as the client examples do: a notifier for
CI, a tail of a room, and a bot over the network. See the
[package documentation](https://pkg.go.dev/github.com/apron-chat/apron-server-go/aprond)
and [SERVER.md](SERVER.md#extensions).

## Documentation

- [SERVER.md](SERVER.md): configuration, storage, and how the server behaves
  where the protocol leaves room for choice.
- [DEVELOPMENT.md](DEVELOPMENT.md): running locally, the repository layout,
  and the test suites.
- [PROTOCOL.md](https://github.com/shazow/apron/blob/main/PROTOCOL.md): the
  Apron protocol specification.
- [`cmd/apron-hammer`](cmd/apron-hammer/README.md): a load-testing tool.

## Related projects

- [shazow/apron](https://github.com/shazow/apron): the protocol and shared
  test fixtures.
- [apron-chat/apron-web](https://github.com/apron-chat/apron-web): the web
  client.
- [apron-chat/apron-server-cloudflare](https://github.com/apron-chat/apron-server-cloudflare):
  the Cloudflare Workers backend behind the public demo.

## License

[MIT](LICENSE)
