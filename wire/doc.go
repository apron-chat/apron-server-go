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
//   - replies answered later, by a goroutine of the backend's, while the
//     connection goes on to its next frame ([Reply.Later]);
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
// what Send does with the method's, as a [Handler] replies. Code deep in
// a handler's calls that needs the request gets it, or the Reply, passed
// down: a handler's context carries no Request.
//
// A connection's frames are handled one at a time, in order, each to its
// end before the next: a handler that waits holds up its own connection,
// and only it. One whose answer takes long, such as a gateway's that waits
// for a slow upstream, answers later instead: it calls [Reply.Later] and
// returns, and the connection goes on to its next frame, while a goroutine
// of the backend's sends the reply once it has it, with [Reply.Send] or
// [Reply.Fail]. Its handler's context is the connection's (a Local call's
// own), canceled when the connection ends, not when the handler returns,
// and the work that answers uses it; a panic in that work is the backend's, as the goroutine
// is. A duplicate of the request (§1.2) waits for its reply without holding
// up its own connection either, and gets its outcome. When the connection
// ends first, the request fails, so that its duplicates are not left
// waiting, and is not kept, so that a retry runs again; Send and Fail then
// return ErrClosed. Auth is a barrier, finished before the frames after it
// (§3.2.2), and is never answered later. A [Handler] answers later with the
// Reply of its request ([NewReply]), and [Local.Call] waits for a reply
// answered later, while the next Call on the connection does not.
//
// A reply answered later comes when the backend sends it, maybe after the
// replies to requests sent after it: a server may reply in any order, and
// a client that needs one request applied before another waits for the
// first reply (§1). What does not change is the rest of §1's order: the
// result still reflects every notification sent before it, and the
// notifications the request causes on its connection still come before
// it, which the backend sees to as it does for a result sent at once, by
// queuing them first. Where that takes more than a lock, see Ordering for
// asynchronous hosts.
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
// # Ordering for asynchronous hosts
//
// wire keeps a connection's frames in the order they are queued, and no
// more: §1's order of a connection's frames, a result after the
// notifications of its request's effects and every notification in the
// order of the changes, holds when the backend queues each frame in that
// order, as one that sends while it holds the lock of each change does. A
// host that applies changes asynchronously, such as another chat system
// that hands each to a queue or a goroutine of its own and delivers its
// effects later, has no such lock: when its handler is done, the effects
// of the request may not be queued yet, and a result sent then overtakes
// them.
//
// Such a backend orders them itself, with a barrier: a marker it sends to
// the connection through the host's own queue, the way the request's
// effects go, behind them, whose arrival tells that everything ahead of it
// was delivered. The handler sends the result once its barrier came
// through:
//
//	host.Apply(change)
//	arrived := make(chan struct{})
//	host.Deliver(member, barrier{arrived}) // behind the change's effects
//	select {
//	case <-arrived:
//	case <-ctx.Done():
//		return ctx.Err()
//	}
//	return reply.Send(result)
//
// Or, rather than wait, it answers later, and the goroutine that delivers
// the host's queue to the connection sends the result as it reaches the
// barrier, right after the effects it delivered, with nothing to wait for
// and nothing to hold up the connection:
//
//	if err := reply.Later(); err != nil {
//		return err
//	}
//	host.Apply(change)
//	host.Deliver(member, barrier{func() { reply.Send(resultOf(change)) }})
//	return nil
//
// wire has no barrier of its own: only the host knows its queues, and when
// a change's effects are all in them. Nor is [Conn.Fence] one: it holds
// the frames queued after it until a condition holds, which suits a change
// made durable in the background, but a result held behind a fence still
// goes before the notifications queued after it, which are the effects a
// barrier waits for.
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
// as it would sign a network one in. As Server.Local calls those hooks in
// its caller's goroutine, and returns OnConnect's error, a backend does
// not open a Local connection while holding a lock they take: that waits
// forever, which wire logs once it has waited ten seconds.
//
// # Hosts that keep less
//
// A host that keeps less than the protocol asks for gets helpers: [IDs]
// gives message_ids to messages that have none, derived from their own
// times, and [Page] answers history from a [Log]: any ordered slice of
// records, such as a host's recent-message buffer, and its bounds.
//
// # Commands
//
// Package commands (wire/commands) is the command capability's parts
// (§4.1), for any backend: commands registered by name with the forms
// /help shows, and a Router, a [CommandHandler], that parses the command
// line, refuses what a form requires and the sender may not use, runs the
// command, and answers /help, with the same list as data for apron's
// ext:command_list. Package apron runs its commands on one too.
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
