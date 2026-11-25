// Package apron is the vocabulary of the Apron protocol (PROTOCOL.md):
// what travels over a connection, which a server and a client both use. It
// opens no connections and runs no goroutines. Its module, which depends
// only on the standard library and the WebSocket package, has these
// packages:
//
//   - apron, this one: the frames, params, results, and errors;
//   - [github.com/apron-chat/apron-server-go/apron/server]: the engine
//     that serves the protocol for a backend, the program that decides who
//     everyone is, which rooms exist, and what was said;
//   - [github.com/apron-chat/apron-server-go/apron/server/commands]: the
//     command capability (§4.1) for any backend of apron/server;
//   - [github.com/apron-chat/apron-server-go/apron/bot]: bots, on any
//     connection they are given, such as an in-process one of apron/server;
//   - [github.com/apron-chat/apron-server-go/apron/aprontest]: tests of a
//     backend of apron/server, a handler at a time or a whole server as a
//     client sees it;
//   - [github.com/apron-chat/apron-server-go/apron/conformance]: the
//     protocol's core conformance checks, as Go tests against any server.
//
// Package apron and package bot import nothing of apron/server, so that a
// client imports them without the engine.
//
// # The vocabulary
//
//   - typed frames ([User], [Message], [Room] and [RoomView], [ServerInfo],
//     ...), which keep the members they have no field for (Unknown), with
//     ext as raw JSON ([Ext], merged with [MergeExt]), and a message [Body]
//     that encodes as it was decoded, each field it left out left out and
//     each it sent empty sent empty (§3.5.1);
//   - the params and results of the methods, such as [MessageParams] and
//     [MessageResult], and [OK], the result {}; the params a client sends
//     encode as it sends them;
//   - typed notifications ([Notification]: [Message], [UserUpdate],
//     [StatusUpdate], [RoomUpdate], [Reactions], [Activity], [ServerInfo]),
//     encoded as a [Frame] with [Encode]; the system identities
//     ([PrivateNotice], [RoomNotice], [ServerNotice]), which send notices
//     ([Notice]);
//   - user objects that carry what they clear ([CurrentUser]), and the
//     patch of a change of a user ([User.Patch], [User.Complete], §3.3.1);
//   - log_ids and message_ids as [LogID], below 2^53 ([MaxLogID],
//     [ParseLogID], §2.1);
//   - the error reply, [Error], with its codes and constructors ([Denied],
//     [InvalidParams], [Missing], [Unsupported], [TooLarge], [RetryAfter]),
//     and [CodeOf], the code a client is told for any error; and
//     [ErrClosed], a call on a connection that has ended, on either side;
//   - the checks of what the protocol defines of a value: [ParseBody],
//     [ValidID], and room_list's filters ([RoomFilter]).
//
// [Marshal] encodes a value as every frame is encoded: deterministically,
// so that a program that keeps or compares what it sends has the frames'
// bytes.
package apron
