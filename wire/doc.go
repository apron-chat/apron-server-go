// Package wire serves the Apron protocol for a program that keeps its own
// chat state: a bridge, a gateway in front of another chat system, a coding
// harness, or a small bot server. The program is the authority on who
// everyone is, which rooms exist, and what was said; wire speaks the
// protocol for it.
//
// Package apron in this module is a complete server that owns its state.
// Package wire is the other way around: it owns no state at all, and asks a
// [Backend] for every answer. What it does on its own is the part of
// PROTOCOL.md that is the same for every server:
//
//   - WebSocket transport (§1), or any [Transport] that carries whole
//     frames, with a bounded write queue per connection that drops a client
//     too slow to keep up, write timeouts, and transport-level pings;
//     frames that go out together ([Conn.SendBatch]), and frames held until
//     the backend's changes are durable ([Conn.Fence]);
//   - the server frame on connect (§3.1), and the liveness ping and pong (§1);
//   - the envelope: parse errors, invalid requests, request ids, and the
//     error codes (§1.1), with unknown methods answered error/unsupported and
//     unknown notifications ignored;
//   - auth as a barrier (§3.2.2): frames are processed in order, nothing but
//     auth and ping before a sign-in, and every notification a sign-in
//     causes is sent after its result;
//   - deduplication of retried requests by user_id and id (§1.2);
//   - the core checks on a new message (§3.5.1, §3.6): a message with
//     message_id or deleted without capability edit is unsupported, an empty
//     one is not passed on, room_id and format are checked;
//   - typed frames for the core ([User], [Message], [ServerInfo]), the
//     system identities ([Private], [RoomNotice], [ServerNotice]), and a
//     [Clock] for log_ids (§2.1).
//
// A Backend implements three methods, which together are the core
// conformance checklist (§3.6): Hello, Auth, and Message. It adds the
// optional core request me, and capabilities, by implementing more
// interfaces ([Profiler], [Commander], [Editor]), or by registering request
// methods of its own with [Server.Handle]. It sends notifications with
// [Conn.Notify], or encodes a frame once with [Encode] and sends it to many
// connections with [Conn.Send].
//
// A request's notifications to its own connection are sent before its
// result when the backend queues them before it returns (§1): Conn's queue
// is first in, first out.
package wire
