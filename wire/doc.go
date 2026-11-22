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
//   - in-process connections ([Server.Local]) for bots, bridges, and tests;
//   - the server frame on connect (§3.1), and the liveness ping and pong (§1);
//   - the envelope: parse errors, invalid requests, request ids, and the
//     error codes (§1.1), with unknown methods answered error/unsupported and
//     unknown notifications ignored;
//   - auth as a barrier (§3.2.2): frames are processed in order, nothing but
//     auth and ping before a sign-in, and every notification a sign-in
//     causes is sent after its result;
//   - deduplication of retried requests by user_id and id (§1.2);
//   - the params of every typed request, read and checked once (see
//     Validation), with the core rules of a new message (§3.5.1, §3.6);
//   - typed frames ([User], [Message], [Room] and [RoomView], [ServerInfo],
//     ...), which keep the members they have no field for (Unknown), with
//     ext as raw JSON ([Ext], [MergeExt]), and a message [Body] that
//     encodes as it was decoded, each field it left out left out and each
//     it sent empty sent empty (§3.5.1); typed notifications
//     ([Notification]: [Message], [UserUpdate], [StatusUpdate],
//     [RoomUpdate], [Reactions], [ServerInfo]), sent with [Conn.Notify] or
//     encoded once with [Encode]; the system identities ([Private],
//     [RoomNotice], [ServerNotice]); log_ids and message_ids as [LogID],
//     below 2^53, and a [Clock] that issues them (§2.1);
//   - user changes as patches ([User.Patch], [CurrentUser]), which carry
//     what changed and clear what was taken away (§3.3.1), published to the
//     user's connections and those who share a room ([Server.PublishUser]),
//     or each its own view of the user, such as the chosen status and the
//     shown one ([Server.PublishUserChange], §4.5.2).
//
// # Backends and handlers
//
// A [Backend] says who the server is (Hello) and who each connection is
// (Auth). Everything else is added one capability at a time, by setting a
// handler field of the [Server] — Messages ([MessageHandler], message),
// Edits ([EditHandler], edit), Profiles ([ProfileHandler], me), Commands
// ([CommandHandler], command), Rooms ([RoomHandler], rooms), History
// ([HistoryHandler], history), Status ([StatusHandler], status), Reactions
// ([ReactionHandler], reactions), Activity ([ActivityHandler], activity) —
// each of which adds its requests, with their params checked, and its
// capability in the server frame. One value may be every handler, or each
// may be its own, such as a wrapper that adds metrics to one of them; a
// server that refuses some room requests embeds [UnimplementedRooms]. Hook
// fields follow a connection's life (OnConnect, OnDisconnect), bring a
// duplicate's result up to date (RefreshDuplicate), decide the auth schemes
// (CheckScheme), and the origins that may open a WebSocket (CheckOrigin,
// [OriginPatterns], [AnyOrigin]). The Server takes its configuration as it
// serves its first connection, or opens its first Local one, in whatever
// order its fields were set.
//
// A [Handler] registered with [Server.Handle] handles a request by name,
// raw, and a [NotificationHandler] registered with
// [Server.HandleNotification] a client's notification: an extension's own
// method, such as the reference server's push_register and account
// methods, or one that takes the place of a handler field's. A Handler
// reads its params with the Request's accessors, such as
// [Request.String], or a [Decoder], which reads several and keeps the
// first error; one that takes a handler field's place reads them as the
// Server would with the field's parser, such as [ParseMessage], and passes
// them on to a typed handler with [NewReply].
//
// # Requests, replies, and ordering
//
// Every typed handler gets a [Reply] for its result: [Reply.Send] sends it
// now, followed by frames with nothing between them, and does what wire
// does with the method's result as it queues it, such as signing the
// connection in with an auth result. A handler sends exactly once, or
// returns an error; one whose result is [Empty] may return nil without
// sending, and has {} sent for it. A backend that keeps its state under a
// lock sends while it holds the lock, so that the result reflects every
// notification sent before it on the connection, and none after (§1), and
// so that a connection is signed in before any notification it sends after
// the auth result. The notifications a request queues to its own
// connection before its reply are sent before it: a Conn's queue is first
// in, first out.
//
// While Auth runs, the notifications sent to its connection are held, and
// sent after its result, or dropped when it fails (§3.2.2). Only those
// are: what is sent to the connection after a failed Auth has returned
// goes out, as a notice before sign-in may, so a backend stops its own
// senders for the connection, such as a goroutine it started during
// Auth, before it returns the error.
//
// [Reply.Request] is the request, raw: its params, with presence
// ([Request.Param]), and [Request.Reply], which sends any result without
// what Send does with the method's, as a [Handler] replies. A handler's
// context carries its Request too ([RequestFrom]), for code deep in its
// calls. A connection's frames are handled one at a time, in order, each
// to its end before the next: a handler that waits holds up only its own
// connection.
//
// # Sending never blocks
//
// Sending never blocks, and calls nothing of the backend's but
// Broadcast's and PublishUser's filters: [Conn.Send], [Conn.Notify],
// [Conn.SendBatch], [Server.Broadcast], [Server.PublishUser],
// [Server.PublishUserChange], [Reply.Send], and [Request.Reply] queue
// frames, and disconnect a client too slow to take them. So a backend sends while it holds the lock of the change a
// frame describes, which keeps every connection's frames in the order of
// the changes, such as messages in log_id order; one that sends after
// letting go of the lock lets two changes' frames cross.
//
// # Errors and panics
//
// A handler's error that is an [*Error] is the error reply, as [CodeOf]
// tells; any other is error/internal_error, and logged. An error returned
// after the reply was sent is logged, at debug level for an *Error. A
// handler that panics is logged, and answered error/internal_error unless
// it handles a notification; its connection goes on, as in net/http. The
// param accessors, the Decoder, and the parsers return an error that is
// always an *Error, error/invalid_params.
//
// # Validation
//
// wire checks what the protocol defines of every request: its syntax and
// shapes, the same for every server. The backend checks what depends on its
// state and policy. A request wire refuses never reaches the backend, and
// the backend counts on what wire checked; params a request has that wire
// does not know are left to the backend, which reads them raw
// ([Request.Param]) or ignores them.
//
//	What                         wire checks                              the backend checks
//	frames (§1, §1.1)            JSON object, method, request id a        —
//	                             string of at most MaxRequestIDBytes,
//	                             params an object, frame size (ReadLimit)
//	order (§3.2.2)               signed in before any request but auth    —
//	types                        each param it knows is of its type, not  —
//	                             null: such as a limit positive, status
//	                             idle false or seconds, emojis non-empty
//	                             strings
//	IDs (§2.4, §2.1, §2.2)       room_id and user_id syntax; log_ids and  that they exist, and that the
//	                             message_ids decimal strings below 2^53,  user may see them
//	                             a message_id not 0
//	enums                        room_list filter, body.format            status values (ServerInfo.Status),
//	                                                                      which emojis it takes
//	required fields              those the protocol requires, such as     —
//	                             reactions' message_id and emojis, and a
//	                             new message's body
//	field combinations           a deletion has a message_id, a command   —
//	                             no message_id or deleted, a status
//	                             room_id comes with mute
//	message body (§3.5)          text, format, mentions as user_ids,      limits: lengths and counts, and
//	                             embeds as objects with a kind            embeds it does not take
//	auth (§3.2)                  scheme present; name, user_id, agent,    whether the scheme is offered
//	                             token strings, ext an object             (CheckScheme), credentials, and
//	                                                                      user_id syntax, after the scheme
//	profiles, rooms (§3.3, §4.2) the types of the fields                  names, titles, avatars, policy
//	permissions, limits          —                                        all: who may do what, rates,
//	                                                                      sizes, the most history returns
//
// A scheme a server does not offer is unsupported whatever else the auth
// request holds (§3.2), so wire leaves a requested user_id to the backend,
// which checks it after the scheme. Activity is a notification: one whose
// params wire refuses is ignored, as nothing answers it (§1.1).
//
// # Local connections
//
// [Server.Local] opens an in-process connection, for a bot, a bridge, or a
// test: signed in from the start as [LocalOptions].User, it opens as a
// network connection does, with OnConnect, Hello, whose server frame is
// its first frame, and OnDisconnect when it ends, and its requests
// ([Local.Call]) and notifications ([Local.Notify]) take a network
// client's path, deduplication and all. The frames the backend sends it
// while OnConnect and Hello run go after the server frame, so a backend
// that keeps its connections under a lock adds a Local one in OnConnect,
// as it would sign a network one in; and as the Server calls those hooks,
// a backend does not open a Local connection while holding a lock they
// take.
//
// # Hosts that keep less
//
// A host that keeps less than the protocol asks for gets helpers: [IDs]
// gives message_ids to messages that have none, derived from their own
// times, and [Page] answers history from a [Log]: any ordered slice of
// records, such as a host's recent-message buffer, and its bounds.
//
// # Testing with wiretest
//
// Package wiretest tests a backend: it calls a typed handler directly with
// a Reply that records what it sends (wiretest.NewReply), and a Handler
// with a request on no connection (wiretest.NewRequest); and it tests a
// whole Server as a client sees it (wiretest.Connect, in process, and
// wiretest.Dial, over a WebSocket), with a Client that signs in, calls,
// and expects the notifications that came before a result
// (Client.CallFrames), or waits for every frame queued so far
// (Client.Sync).
package wire
