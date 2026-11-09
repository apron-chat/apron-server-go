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
//   - external sign-in, such as OAuth ([Registrar.ExternalSignIn]);
//   - an allowlist of who may sign in, with no guests, which signs out
//     whoever is taken off it ([Registrar.RestrictSignIn]).
//
// A scope ([Host.SetScope], [BotOptions].Scope) limits an account, such as
// a bot, to some rooms, on every path the server checks who sees a room;
// a bot's service tokens ([Bot.IssueToken]) sign in its clients outside
// the process, expire on time, and are revoked at once.
//
// Invitations ([Host.Invite], [Server.Invite]) each let one person make an
// account with a passkey, without a guest to start from: the account is
// made, admitted, and given what the invitation grants in one stored
// change, and [Host.Enrollment] returns the invitation's data with it.
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
// # Durability
//
// The server answers most requests once their changes are made in memory,
// and writes them to [Config].Store in the background, in order. [Server.Sync]
// and [Host.Sync] wait until every earlier change is stored; answers that
// must mean a change is stored, such as a new account's from an
// invitation, wait the same way. A store that fails stays failed:
// [Config].OnStoreError is told, nothing more is written, and Sync returns
// an error wrapping [ErrStoreFailed].
//
// The examples directory of the module holds complete servers built with
// extensions, each with a test.
package apron
