// Package wire serves the Apron protocol for a Backend: the program that
// decides who everyone is, which rooms exist, and what was said. wire owns
// no chat state at all; it is the part of PROTOCOL.md that is the same for
// every server, and every server in this module runs on it: package apron,
// the reference server, is a Backend like any other, and so is a small
// gateway in front of another chat system, a bridge, or a bot server.
//
// What wire does on its own:
//
//   - WebSocket transport (§1), or any [Transport] that carries whole
//     frames, with a bounded write queue per connection that drops a client
//     too slow to keep up, write timeouts, and transport-level pings;
//     frames that go out together ([Conn.SendBatch]), and frames held until
//     the backend's changes are durable ([Conn.Fence]);
//   - in-process connections ([Server.Local]) for bots, bridges, and tests,
//     whose requests ([Local.Call]) and notifications ([Local.Notify])
//     take a network client's path;
//   - the server frame on connect (§3.1), and the liveness ping and pong (§1);
//   - the envelope: parse errors, invalid requests, request ids, and the
//     error codes (§1.1), with unknown methods answered error/unsupported and
//     unknown notifications ignored;
//   - auth as a barrier (§3.2.2): frames are processed in order, nothing but
//     auth and ping before a sign-in, and every notification a sign-in
//     causes is sent after its result;
//   - deduplication of retried requests by user_id and id (§1.2);
//   - the core checks on a new message (§3.5.1, §3.6), and on the params of
//     every typed request;
//   - typed frames ([User], [Message], [Room], [ServerInfo], ...), the
//     system identities ([Private], [RoomNotice], [ServerNotice]), log_ids
//     and message_ids as [LogID], and a [Clock] that issues them (§2.1).
//
// # Backends
//
// A [Backend] says who the server is (Hello) and who each connection is
// (Auth). Everything else it adds one capability at a time, by implementing
// an optional interface — [Messenger] (message), [Editor] (edit),
// [Profiler] (me), [Commander] (command), [Rooms] (rooms), [Historian]
// (history), [Statuser] (status) — each of which adds its requests, with
// their params checked, and its capability in the server frame. Hooks
// follow a connection's life ([Connecter], [Disconnecter]), bring a
// duplicate's result up to date ([Refresher]), and decide the auth schemes
// ([SchemeChecker]).
//
// A [Method] registered with [Server.Handle] handles a request or client
// notification by name, raw: an extension's own method, such as the
// reference server's push_register and account methods, or one that takes
// the place of an interface's. The reference server implements the
// interfaces, and replies to each request itself ([Request.Reply]).
//
// # Requests and ordering
//
// Every handler's context carries its [Request] ([RequestFrom]): its params,
// with presence ([Request.Param]), and [Request.Reply], which sends the
// result now, followed by frames with nothing between them. A backend that
// keeps its state under a lock replies while it holds the lock, so that the
// result reflects every notification sent before it on the connection, and
// none after (§1); one that does not reply itself has its return value sent
// once the handler returns. Either way, the notifications a request queues
// to its own connection before its reply are sent before it: a Conn's queue
// is first in, first out.
//
// Sending never blocks, and calls nothing of the backend's but
// Broadcast's filter: [Conn.Send], [Conn.Notify], [Conn.SendBatch],
// [Server.Broadcast], and [Request.Reply] queue frames, and disconnect a
// client too slow to take them. So a backend sends while it holds the lock
// of the change a frame describes, which keeps every connection's frames in
// the order of the changes, such as messages in log_id order; one that
// sends after letting go of the lock lets two changes' frames cross.
//
// A handler's error that is an [*Error] is the error reply, as [CodeOf]
// tells; any other is error/internal_error, and logged. A handler that
// panics is logged, and answered error/internal_error unless it handles a
// notification; its connection goes on, as in net/http. The param
// accessors of [Fields] and [Request], such as [Fields.String], return an
// error that is always an *Error.
//
// # Hosts that keep less
//
// A host that keeps less than the protocol asks for gets helpers: [IDs]
// gives message_ids to messages that have none, derived from their own
// times, and [Page] answers history from a [Log]: any ordered slice of
// records, such as a host's recent-message buffer, and its bounds.
package wire
