// Package apron is a server for the Apron chat protocol, as a library: the
// reference implementation of every capability that
// https://github.com/shazow/apron/blob/main/PROTOCOL.md defines, which a
// program can embed and extend without forking it. Section references in the
// documentation, such as §4.1, are to that document.
//
// The module's packages each have one role:
//
//   - apron, this package, serves the protocol: rooms, history, sign-in,
//     uploads, push, and the extension API. It is not a program: it parses no
//     flags, opens no listener, and handles no signals.
//   - [github.com/apron-chat/apron-server-go/store] keeps a server's state
//     across restarts, in SQLite or in memory ([Config].Store).
//   - [github.com/apron-chat/apron-server-go/aprond] is the aprond command as
//     a library: flags, a config file, TLS, email delivery, and signals around
//     this package. Its Main runs a server with extensions added.
//   - cmd/aprond is the aprond binary, which calls aprond.Main.
//
// # Serving
//
// [Open] starts a [Server] from a [Config], [Server.Handler] serves its
// WebSocket endpoint (/ws) and HTTP routes on any listener or mux, and
// [Server.Shutdown] stops it, as the example for Open shows.
//
// # Extensions
//
// An [Extension] adds features to a server. Put it in [Config].Extensions,
// and its Init registers what it adds with its [Host]:
//
//   - commands ([Host.Command]) and request methods ([Host.Method]);
//   - a capability and its data in the server frame ([Host.Advertise]);
//   - HTTP routes ([Host.Handle]);
//   - filters that change or refuse messages before they are saved
//     ([Host.FilterMessage]);
//   - bots, in-process connections that speak the protocol ([Host.Session]),
//     which can stream live text ([Session.StreamAppend]);
//   - followers of the log, from now or replayed from a point
//     ([Host.OnEvent], [Host.OnEventFrom]), with [DecodeMessage] to read
//     messages;
//   - external sign-in, such as OAuth ([Host.ExternalSignIn]);
//   - values kept in the store ([Host.Store]) and goroutines that run until
//     Shutdown ([Host.Go]).
//
// Extension code never runs while the server holds its lock, so it may call
// any Host or Session method. What it sees of users and rooms are copies
// ([User], [Room]). [Host.Visible] tells whether someone may see a room, which
// an extension that shows messages checks.
//
// The examples directory of the module holds complete servers built with
// extensions, each with a test.
package apron
