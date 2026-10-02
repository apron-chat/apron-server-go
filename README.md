# apron-server-go

**The reference server for [Apron](https://github.com/shazow/apron), a small,
open chat protocol over WebSockets.**

`aprond` is a single Go binary that implements every capability of Apron
protocol v7: rooms, threads, and private rooms, full history, edits and
replies, reactions, file uploads and live streams, commands, presence
status and mutes, Web Push and relay push notifications, roles, and passkey
and email sign-in. It keeps its state in SQLite, can serve a web client from the same
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

Email sign-in is off unless `--email.enable` is set. To try it locally, run
`go run ./cmd/aprond --email.enable --email.sender log`, which writes codes to
the server log instead of sending them.

Run `aprond --help` for every flag, or `aprond --print-config > aprond.toml`
to start a config file.

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
