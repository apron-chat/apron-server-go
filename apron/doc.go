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
//   - [github.com/apron-chat/apron-server-go/aproncmd] is the aprond command as
//     a library: flags, a config file, TLS, email delivery, and signals around
//     this package. Its Main runs a server with extensions added.
//   - cmd/aprond is the aprond binary, which calls aproncmd.Main.
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
// and its Init registers what it adds with its [Registrar], which works only
// during Init:
//
//   - commands ([Registrar.Command]) and request methods
//     ([Registrar.Method]);
//   - HTTP routes ([Registrar.Handle]);
//   - filters that change or refuse messages before they are saved
//     ([Registrar.FilterMessage]);
//   - bots ([Registrar.Bot]): accounts the extension speaks through, whose
//     requests get the same checks as a client's, and which can stream live
//     text ([Bot.StreamAppend]);
//   - followers of the log ([Registrar.OnEvent], [Registrar.OnEventFrom],
//     and [Registrar.Follow], which keeps its place across restarts), with
//     [DecodeMessage] to read messages;
//   - external sign-in, such as OAuth ([Registrar.ExternalSignIn]).
//
// The Registrar's [Host] is the extension's handle on the server for as long
// as it runs: its capability in the server frame ([Host.Advertise]), values
// kept in the store ([Host.Store]), goroutines that run until Shutdown
// ([Host.Go]), and copies of users and rooms ([User], [Room]).
//
// Extension code never runs while the server holds its lock, so it may call
// any Host, Bot, or Session method. Values it hands the server, such as
// results, advertised data, and ext, are encoded when they are handed over,
// so one that is not JSON is the extension's error and never reaches a
// client. [Host.Visible] says whether a user may see a room, and
// [Host.Public] whether anyone may: an extension that shows messages checks
// them, so that it never shows a private room's.
//
// The examples directory of the module holds complete servers built with
// extensions, each with a test.
package apron
