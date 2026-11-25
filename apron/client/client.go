// Package client is a network client of any Apron server (PROTOCOL.md):
// [Dial] opens a WebSocket to the server's address, reads its server frame
// (§3.1), and signs in (§3.2), and the [Conn] it returns makes requests,
// typed ([Conn.Post], [Conn.History], [Conn.Me], [Conn.Command]) or raw
// ([Conn.Call], [Conn.Notify]), and hands every frame the server sends but
// the replies, such as new messages, to its OnFrame. It imports only
// package apron, so that a program that talks to a server imports none of
// one.
//
// A program that posts one message, such as a notice from CI:
//
//	conn, err := client.Dial(ctx, "wss://chat.example/ws", apron.AuthParams{Scheme: "token", Token: token})
//	if err != nil {
//		return err
//	}
//	defer conn.Close()
//	_, err = conn.Post(ctx, apron.MessageParams{RoomID: "general", Body: &apron.Body{Text: "Deployed"}})
//
// A Conn is one connection, and it is not opened again: when it ends, Done
// is closed, and its calls fail with an error that wraps apron.ErrClosed.
// Reconnecting is its caller's job, such as a bot's (package apron/bot),
// which opens a Conn with Dial each time its connection ends:
//
//	echo, err := bot.Start(func(ctx context.Context, onFrame func(apron.Frame)) (bot.Conn, error) {
//		return client.Dial(ctx, url, auth, client.OnFrame(onFrame))
//	}, bot.Options{OnMessage: ...})
//
// What a Conn does for its caller:
//
//   - request ids (§1.1) of its own, unique across connections, so that a
//     server that deduplicates by user_id and id (§1.2) never takes a new
//     request for an earlier connection's; Call does not retry;
//   - the replies: each goes straight to its Call, which never waits behind
//     the frames OnFrame has yet to take, so OnFrame may make calls and
//     wait for them;
//   - who it is (User): the auth result's you, kept up to date with the
//     user notifications about itself (§3.3.3) and me results;
//   - the liveness pings a server asks for (server.ping, §1): a Conn sends
//     {"method":"ping"} at that interval, and ends the connection when a
//     ping goes a whole interval without hearing anything back.
package client

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/apron-chat/apron-server-go/apron"
)

// Errors that end a connection, which its Err then wraps.
var (
	// ErrFellBehind ends a connection whose OnFrame did not keep up: more
	// than 1024 frames waited for it.
	ErrFellBehind = errors.New("client: the connection's frame handler fell behind")
	// ErrUnanswered ends a connection whose server did not answer a ping
	// (§1) within the ping interval, nor sent anything else meanwhile.
	ErrUnanswered = errors.New("client: the server did not answer a ping")
)

const (
	// queue bounds the frames waiting for OnFrame.
	queue = 1024
	// readLimit bounds a frame from the server: well above the 256 KiB a
	// request may be (§1), for results such as a page of history.
	readLimit = 32 << 20
	// writeTimeout bounds writing one frame.
	writeTimeout = 10 * time.Second
)

// An Option configures Dial.
type Option func(*options)

type options struct {
	onFrame func(apron.Frame)
	header  http.Header
}

// OnFrame has f get each frame the server sends the connection but the
// replies to Call, which go to Call: in order, on a goroutine of the
// connection's own, from the server frame on: notifications, such as new
// messages (§3.5), and errors about the connection as a whole (§1.1), but
// not the pongs to the Conn's own pings. f may be called before Dial
// returns. A nil f is no option.
//
// f may make calls on the connection and wait for their replies, which
// never wait behind the frames f has yet to take. Those wait for f to
// return meanwhile, which keeps them in order; when more than 1024 do, the
// connection ends with ErrFellBehind. The frames that arrived before the
// connection ended are handed to f after it has ended, unless Close ended
// it.
func OnFrame(f func(apron.Frame)) Option {
	return func(o *options) { o.onFrame = f }
}

// Header adds header to the HTTP request that opens the WebSocket, such
// as an Origin that the server checks, or a cookie.
func Header(header http.Header) Option {
	return func(o *options) { o.header = header }
}

// Conn is a connection to an Apron server, signed in. Its methods are safe
// for concurrent use.
type Conn struct {
	ws     *websocket.Conn
	prefix string
	nextID atomic.Uint64
	// frames are the frames waiting for OnFrame, or nil without one.
	frames chan apron.Frame
	// discard is set once the frames still waiting for OnFrame are not to
	// be handed to it.
	discard atomic.Bool
	// heard is set whenever a frame arrives, for the liveness pings.
	heard atomic.Bool
	// greeted is closed once the first server frame arrives.
	greeted chan struct{}
	// done is closed once the connection ends, with err set.
	done chan struct{}

	mu     sync.Mutex
	hello  apron.ServerInfo
	user   apron.User
	calls  map[string]*pending
	failed *apron.Error // the latest error about the connection as a whole
	err    error        // why the connection ended, wrapping apron.ErrClosed
	reason error        // why it ended, as it was told
}

// pending is a request waiting for its reply.
type pending struct {
	method string
	reply  chan reply
}

type reply struct {
	result jsontext.Value
	err    *apron.Error
}

// Dial connects to the Apron server at url, its address, such as
// wss://chat.example/ws, and signs in with auth (§3.2), such as
// {Scheme: "token", Token: ...} or {Scheme: "guest", Name: "CI"}; it
// returns the connection once the auth result has signed it in. ctx bounds
// connecting and signing in, not the connection.
//
// An error reply to auth, such as error/denied for a token that was
// revoked, is returned as an *apron.Error, after which a client does not
// sign in again on its own (§1.1); a bot's Dialer that stops on it returns
// it wrapped with apron.ErrClosed. Dial's other errors never wrap
// apron.ErrClosed, even when the server closed the connection, such as
// one at capacity (error/retry_after, which is returned): a bot opens it
// again later.
func Dial(ctx context.Context, url string, auth apron.AuthParams, opts ...Option) (*Conn, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: o.header})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(readLimit)
	c := &Conn{
		ws:      ws,
		prefix:  rand.Text()[:12] + "-",
		greeted: make(chan struct{}),
		done:    make(chan struct{}),
		calls:   map[string]*pending{},
	}
	if o.onFrame != nil {
		c.frames = make(chan apron.Frame, queue)
		go c.deliver(o.onFrame)
	}
	go c.read()

	// The server frame comes first (§3.1), and says how often to ping.
	select {
	case <-c.greeted:
	case <-c.done:
		return nil, c.dialError()
	case <-ctx.Done():
		c.abandon()
		return nil, ctx.Err()
	}
	go c.keepAlive()
	result, err := call[apron.AuthResult](ctx, c, "auth", auth)
	if err == nil && result.You == nil {
		err = errors.New("client: auth signed nobody in")
	}
	if err != nil {
		if errors.Is(err, apron.ErrClosed) {
			err = c.dialError()
		}
		c.abandon()
		return nil, err
	}
	return c, nil
}

// abandon ends a connection that Dial does not return, without waiting
// for the server.
func (c *Conn) abandon() {
	c.discard.Store(true)
	c.shut(apron.ErrClosed)
	c.ws.CloseNow()
}

// dialError is why a connection ended while Dial opened it, without
// apron.ErrClosed.
func (c *Conn) dialError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.reason.(*apron.Error); ok {
		return e
	}
	return fmt.Errorf("client: the connection ended while signing in: %v", c.reason)
}

// read reads frames until the connection ends.
func (c *Conn) read() {
	for {
		_, payload, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			if c.failed != nil {
				// The server said why, before it closed the connection.
				err = c.failed
			}
			c.mu.Unlock()
			c.shut(err)
			c.ws.CloseNow()
			return
		}
		c.heard.Store(true)
		c.receive(payload)
	}
}

// receive takes a frame from the server: a reply goes to its Call, and
// any other to OnFrame, once the Conn has noted what it says of the
// connection.
func (c *Conn) receive(payload []byte) {
	var f struct {
		ID     *string        `json:"id"`
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
		Result jsontext.Value `json:"result"`
		Error  *apron.Error   `json:"error"`
	}
	if json.Unmarshal(payload, &f) != nil {
		return
	}
	switch {
	case f.ID != nil && f.Method == "":
		c.mu.Lock()
		waiting := c.calls[*f.ID]
		delete(c.calls, *f.ID)
		if waiting != nil && f.Error == nil && (waiting.method == "auth" || waiting.method == "me") {
			// A complete you replaces the user (§3.3.1), before any
			// user notification after it is merged.
			var result struct {
				You *apron.CurrentUser `json:"you"`
			}
			if json.Unmarshal(f.Result, &result) == nil && result.You != nil {
				c.user = result.You.User
			}
		}
		c.mu.Unlock()
		if waiting != nil {
			waiting.reply <- reply{f.Result, f.Error}
		}
		return
	case f.Method == "pong":
		return
	case f.Method == "server":
		var info apron.ServerInfo
		if json.Unmarshal(f.Params, &info) == nil {
			// Each server frame replaces the one before (§3.1).
			c.mu.Lock()
			c.hello = info
			c.mu.Unlock()
			select {
			case <-c.greeted:
			default:
				close(c.greeted)
			}
		}
	case f.Method == "user":
		var update apron.UserUpdate
		if json.Unmarshal(f.Params, &update) == nil && update.You != nil {
			c.mu.Lock()
			c.user = merge(c.user, *update.You)
			c.mu.Unlock()
		}
	case f.Method == "" && f.ID == nil && f.Error != nil:
		c.mu.Lock()
		c.failed = f.Error
		c.mu.Unlock()
	}
	if c.frames == nil {
		return
	}
	select {
	case c.frames <- apron.Frame(payload):
	default:
		c.discard.Store(true)
		c.shut(ErrFellBehind)
		c.ws.CloseNow()
	}
}

// deliver hands the frames to OnFrame, in order, until the connection
// ends and those that arrived before have been handed too.
func (c *Conn) deliver(onFrame func(apron.Frame)) {
	for {
		var frame apron.Frame
		select {
		case frame = <-c.frames:
		case <-c.done:
			select {
			case frame = <-c.frames:
			default:
				return
			}
		}
		if c.discard.Load() {
			return
		}
		onFrame(frame)
	}
}

// keepAlive pings the server at the interval its server frame asks for
// (§1), and ends the connection when a ping goes a whole interval without
// anything heard back.
func (c *Conn) keepAlive() {
	pinged := false
	for {
		interval := time.Duration(c.Hello().Ping) * time.Second
		if interval <= 0 {
			return
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-c.done:
			timer.Stop()
			return
		}
		if heard := c.heard.Swap(false); pinged && !heard {
			c.shut(ErrUnanswered)
			c.ws.CloseNow()
			return
		}
		if c.Notify(context.Background(), "ping", nil) != nil {
			return
		}
		pinged = true
	}
}

// shut ends the connection for reason, unless it has ended, and reports
// whether it did.
func (c *Conn) shut(reason error) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false
	}
	c.reason, c.err = reason, apron.ErrClosed
	if reason != apron.ErrClosed {
		c.err = fmt.Errorf("%w: %w", apron.ErrClosed, reason)
	}
	c.mu.Unlock()
	close(c.done)
	return true
}

// merge merges a current user object into the kept one (§3.3.1): each
// field it carries replaces the kept one, a cleared field is cleared, and
// the others stay; its ext merges one level deep (§4.13). An object of
// another user_id is a new identity (§3.3.3), which replaces the kept one.
func merge(kept apron.User, u apron.CurrentUser) apron.User {
	if u.UserID != kept.UserID {
		return u.User
	}
	for _, f := range []struct {
		name    string
		kept    *string
		carried string
	}{
		{"name", &kept.Name, u.Name},
		{"avatar", &kept.Avatar, u.Avatar},
		{"status", &kept.Status, u.Status},
		{"seen", &kept.Seen, u.Seen},
	} {
		if f.carried != "" || slices.Contains(u.Cleared, f.name) {
			*f.kept = f.carried
		}
	}
	if len(u.Roles) > 0 || slices.Contains(u.Cleared, "roles") {
		kept.Roles = slices.Clone(u.Roles)
	}
	if len(u.Ext) > 0 {
		kept.Ext = apron.MergeExt(kept.Ext, u.Ext)
	}
	if len(u.Unknown) > 0 {
		kept.Unknown = mergeUnknown(kept.Unknown, u.Unknown)
	}
	return kept
}

// mergeUnknown merges the members of a current object that package apron
// has no field for into the kept ones, as merge does a field.
func mergeUnknown(kept, carried jsontext.Value) jsontext.Value {
	var members, write map[string]jsontext.Value
	if json.Unmarshal(carried, &write) != nil {
		return kept
	}
	if json.Unmarshal(kept, &members) != nil || members == nil {
		members = map[string]jsontext.Value{}
	}
	for name, value := range write {
		switch string(value) {
		case `""`, `[]`, `{}`:
			delete(members, name)
		default:
			members[name] = value
		}
	}
	if len(members) == 0 {
		return nil
	}
	merged, err := apron.Marshal(members)
	if err != nil {
		return kept
	}
	return merged
}

// Call sends a request with params, any value that encodes as a JSON
// object, or nil for none, and returns its result, or its error reply as
// an *apron.Error. Once the connection has ended, its error wraps
// apron.ErrClosed, and tells why it ended. When ctx is done before the
// reply, it returns ctx's error; the server may still carry out the
// request.
func (c *Conn) Call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	id := c.prefix + strconv.FormatUint(c.nextID.Add(1), 10)
	frame := map[string]any{"method": method, "id": id}
	if params != nil {
		frame["params"] = params
	}
	payload, err := apron.Marshal(frame)
	if err != nil {
		return nil, err
	}
	waiting := &pending{method: method, reply: make(chan reply, 1)}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.calls[id] = waiting
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
	}()
	if err := c.write(ctx, payload); err != nil {
		return nil, err
	}
	select {
	case r := <-waiting.reply:
		return r.result, r.unwrap()
	case <-c.done:
		// A reply that arrived just before the end is still the reply.
		select {
		case r := <-waiting.reply:
			return r.result, r.unwrap()
		default:
			return nil, c.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r reply) unwrap() error {
	if r.err != nil {
		return r.err
	}
	return nil
}

// Notify sends a notification, such as activity (§4.6), which gets no
// reply.
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	frame := map[string]any{"method": method}
	if params != nil {
		frame["params"] = params
	}
	payload, err := apron.Marshal(frame)
	if err != nil {
		return err
	}
	return c.write(ctx, payload)
}

// write sends a frame. A write is not abandoned when ctx is done, which
// would end the connection, but it is bounded.
func (c *Conn) write(ctx context.Context, payload []byte) error {
	if err := c.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		if ended := c.Err(); ended != nil {
			return ended
		}
		return err
	}
	return nil
}

// call sends a request on c, and decodes its result.
func call[T any](ctx context.Context, c *Conn, method string, params any) (T, error) {
	var result T
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("client: %s result: %w", method, err)
	}
	return result, nil
}

// Post posts a message (§3.5.1), and returns its result: the new
// message's message_id, unless the body was empty. Its notification, which
// the server sends the connection before the result, goes to OnFrame.
func (c *Conn) Post(ctx context.Context, p apron.MessageParams) (apron.MessageResult, error) {
	return call[apron.MessageResult](ctx, c, "message", p)
}

// History reads a page of a room's log, or of every room the connection
// receives (§4.3).
func (c *Conn) History(ctx context.Context, p apron.HistoryParams) (apron.HistoryResult, error) {
	return call[apron.HistoryResult](ctx, c, "history", p)
}

// Me changes the user's profile (§3.3.2), and returns the result's you:
// the complete object, with the fields it carries empty in Cleared, which
// User is from then on.
func (c *Conn) Me(ctx context.Context, p apron.MeParams) (apron.CurrentUser, error) {
	result, err := call[struct {
		You apron.CurrentUser `json:"you"`
	}](ctx, c, "me", p)
	return result.You, err
}

// Command runs a command (§4.1), whose body.text is the command line, such
// as "/nick Ada". Its replies, notices, go to OnFrame, before its result.
func (c *Conn) Command(ctx context.Context, p apron.CommandParams) (apron.CommandResult, error) {
	return call[apron.CommandResult](ctx, c, "command", p)
}

// Hello is the server frame (§3.1): the latest one the server sent, each
// of which replaces the one before. Its Apron is the server's protocol
// version, which may differ from apron.Version.
func (c *Conn) Hello() apron.ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// User is who the connection is signed in as: the auth result's you, as
// the user notifications to the connection (§3.3.3) and me results have
// changed it since. Once the connection has ended, it is who it was.
func (c *Conn) User() apron.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.user
}

// Done is closed once the connection has ended, with Err set.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns nil while the connection is open, and once it has ended, an
// error that wraps apron.ErrClosed, and why the server ended it, if it
// did: such as its error about the connection (§1.1), an *apron.Error,
// ErrFellBehind, or ErrUnanswered.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection, with the WebSocket's closing handshake, and
// OnFrame gets no more frames. It returns nil, or, when the connection had
// ended already, why, as Err does.
func (c *Conn) Close() error {
	c.discard.Store(true)
	ended := c.Err()
	c.shut(apron.ErrClosed)
	c.ws.Close(websocket.StatusNormalClosure, "")
	return ended
}
