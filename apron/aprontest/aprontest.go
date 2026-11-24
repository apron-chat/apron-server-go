// Package aprontest tests an extension of package apron as people use it:
// it serves a server with the extension in a test, and connects to it as
// clients do, as a guest, with an account, or not signed in yet, so that a
// test reads as what people do:
//
//	func TestWeather(t *testing.T) {
//		server := aprontest.Serve(t, apron.DefaultConfig(), &weather.Extension{})
//		ada := server.Guest("Ada")
//		notices, err := ada.Command("general", "/weather Lisbon")
//		if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "Lisbon") {
//			t.Fatalf("/weather = %q, %v", notices, err)
//		}
//		if _, err := ada.Post("general", "rain again?"); err != nil {
//			t.Fatal(err)
//		}
//		reply := ada.Message(func(m apron.Message) bool { return m.From.UserID == "weather" })
//		...
//	}
//
// A Server is served over HTTP by a test server: the extensions' routes
// are at its URL, and its WebSocket endpoint at WebSocketURL, which
// wiretest.Dial and conformance.Config.URL take too. A Client is a
// WebSocket connection to it, a wiretest.Client (Client.Wire), whose
// methods wait at most ten seconds, fail the test when the connection
// fails, and return the server's error replies as *apron.Error, such as
// error/denied for a request an extension refused. Call them from the
// test's goroutine, as t.Fatal is.
//
// What a test does not do as a client, it does in process: a bot of a
// test extension of its own (apron.Registrar.Bot), whose user_id
// Config.Roles may grant a role, or the Server's methods, such as
// Invitations and Shutdown.
package aprontest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/wire"
	"github.com/apron-chat/apron-server-go/wire/wiretest"
)

// timeout bounds each wait of a Server's or a Client's methods.
const timeout = 10 * time.Second

// Server is a server under test, which Serve starts: an apron.Server, and
// the test HTTP server that serves it.
type Server struct {
	*apron.Server
	// URL is the base URL of the server's HTTP routes, the extensions'
	// included, such as http://127.0.0.1:41234.
	URL string

	t    testing.TB
	mail *Mailbox
}

// Serve opens a server with config and extensions, after any config names,
// with its state in config.Store, or in memory when it is nil, and serves
// it on a test HTTP server at a loopback address. Both are shut down when
// the test ends, after its clients are closed. It fails the test when the
// server does not open, such as when an extension's Init fails.
func Serve(t testing.TB, config apron.Config, extensions ...apron.Extension) *Server {
	t.Helper()
	config.Extensions = append(slices.Clip(config.Extensions), extensions...)
	app, err := apron.Open(config)
	if err != nil {
		t.Fatalf("aprontest: opening the server: %v", err)
	}
	httpServer := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("aprontest: shutting the server down: %v", err)
		}
		httpServer.Close()
	})
	mail, _ := config.EmailSender.(*Mailbox)
	return &Server{Server: app, URL: httpServer.URL, t: t, mail: mail}
}

// WebSocketURL is the address of the server's WebSocket endpoint, such as
// ws://127.0.0.1:41234/ws, for wiretest.Dial, or conformance.Config.URL,
// which checks the server and its extensions against the protocol's core.
func (s *Server) WebSocketURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

// Dial connects a client that has not signed in, which Client.Hello gives
// the server frame of, and which is closed when the test ends.
func (s *Server) Dial() *Client {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), timeout)
	defer cancel()
	conn, err := wiretest.Dial(ctx, s.WebSocketURL())
	if err != nil {
		s.t.Fatalf("aprontest: connecting: %v", err)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	return &Client{Wire: conn, t: s.t}
}

// SignIn connects a client and signs it in with the params of an auth
// request (§3.2), such as {"scheme": "token", "token": ...}, or fails the
// test.
func (s *Server) SignIn(params any) *Client {
	s.t.Helper()
	c := s.Dial()
	result, err := c.Auth(params)
	if err == nil && result.You == nil {
		err = errors.New("the auth result signed nobody in")
	}
	if err != nil {
		s.t.Fatalf("aprontest: signing in with %v: %v", params, err)
	}
	return c
}

// Guest connects a client and signs it in as a guest who asked for name,
// or fails the test, as on a server with Config.DisableGuests.
func (s *Server) Guest(name string) *Client {
	s.t.Helper()
	return s.SignIn(map[string]any{"scheme": "guest", "name": name})
}

// Account connects a client and signs it in with email (§4.11), as the
// account of address, which it makes if there is none, with the code the
// server sends to Config.EmailSender, which is a *Mailbox; or fails the
// test, as when the server does not let address sign in.
func (s *Server) Account(address string) *Client {
	s.t.Helper()
	if s.mail == nil {
		s.t.Fatal("aprontest: Account needs a *aprontest.Mailbox as Config.EmailSender")
	}
	c := s.Dial()
	if _, err := c.Auth(map[string]any{"scheme": "email", "email": address}); err != nil {
		s.t.Fatalf("aprontest: asking for a code for %s: %v", address, err)
	}
	code := s.mail.Code(address, timeout)
	if code == "" {
		s.t.Fatalf("aprontest: no code was mailed to %s", address)
	}
	if result, err := c.Auth(map[string]any{"scheme": "email", "token": code}); err != nil || result.You == nil {
		s.t.Fatalf("aprontest: signing in as %s: %v", address, err)
	}
	return c
}

// Client is a connection to a Server.
type Client struct {
	// Wire is the connection, for what the methods below do not do, such
	// as reading frames in the order they arrived (wiretest.Client.Next,
	// CallFrames, and Sync).
	Wire *wiretest.Client
	// UserID is the user_id the client last signed in as, and Token the
	// session token that sign-in gave, if any, which signs in again
	// (scheme token); both are "" before.
	UserID, Token string

	t testing.TB
}

// requests numbers the requests Command sends on any connection.
var requests atomic.Uint64

// deadline returns a context that ends after timeout, or with the test.
func (c *Client) deadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.t.Context(), timeout)
}

// refusal returns err if it is the server's error reply, and fails the
// test with it otherwise, as when the connection ended.
func (c *Client) refusal(what string, err error) error {
	c.t.Helper()
	if _, ok := errors.AsType[*apron.Error](err); ok {
		return err
	}
	c.t.Fatalf("aprontest: %s: %v", what, err)
	return err
}

// Hello is the server frame (§3.1) the client got as it connected.
func (c *Client) Hello() wire.ServerInfo { return c.Wire.Hello() }

// Auth sends an auth request (§3.2) with params, such as {"scheme":
// "guest"}, and returns its result, or the server's error reply. A result
// with You signed the client in, as UserID and Token then say; one
// without, such as the begin step of external sign-in, signed nobody in.
func (c *Client) Auth(params any) (wire.AuthResult, error) {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	result, err := c.Wire.Auth(ctx, params)
	if err != nil {
		return result, c.refusal("auth", err)
	}
	if result.You != nil {
		c.UserID, c.Token = result.You.UserID, result.Token
	}
	return result, nil
}

// Call sends a request of method with params, such as an extension's
// method, and decodes its result into result, unless nil, as
// encoding/json does, matching names ignoring case; or returns the
// server's error reply.
func (c *Client) Call(method string, params, result any) error {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	raw, err := c.Wire.Call(ctx, method, params)
	if err != nil {
		return c.refusal(method, err)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result, json.MatchCaseInsensitiveNames(true)); err != nil {
			c.t.Fatalf("aprontest: decoding the result of %s, %s: %v", method, raw, err)
		}
	}
	return nil
}

// Post posts a message of text to a room (§3.5.1), and returns its
// message_id, or the server's error reply, such as an extension's filter's
// refusal (apron.Registrar.FilterMessage).
func (c *Client) Post(roomID, text string) (messageID string, err error) {
	c.t.Helper()
	var result struct {
		MessageID string `json:"message_id"`
	}
	err = c.Call("message", map[string]any{"room_id": roomID, "body": map[string]any{"text": text}}, &result)
	return result.MessageID, err
}

// Command runs a command line, such as "/weather Lisbon", in a room
// (§4.1), and returns the text of the notices it sent the client, which
// arrive before its reply (apron.CommandCall.Notice), and its error reply,
// if any. It takes those notices and the reply, and leaves every other
// frame, such as the message snapshots that arrived meanwhile, for
// Message.
func (c *Client) Command(roomID, line string) (notices []string, err error) {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	id := "aprontest-" + strconv.FormatUint(requests.Add(1), 10)
	payload, err := wire.Marshal(map[string]any{
		"method": "command", "id": id,
		"params": map[string]any{"room_id": roomID, "body": map[string]any{"text": line}},
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.Wire.Send(ctx, payload); err != nil {
		c.t.Fatalf("aprontest: command %s: %v", line, err)
	}
	for {
		frame, err := c.Wire.Expect(ctx, func(f wiretest.Frame) bool {
			return f.Method == "" && f.ID == id || privateNotice(f) != nil
		})
		if err != nil {
			c.t.Fatalf("aprontest: command %s: %v", line, err)
		}
		if notice := privateNotice(frame); notice != nil {
			notices = append(notices, notice.Body.Text)
			continue
		}
		if frame.Error != nil {
			return notices, frame.Error
		}
		return notices, nil
	}
}

// privateNotice is the transient ~private notice that f carries, or nil.
func privateNotice(f wiretest.Frame) *wire.Message {
	if f.Method != "message" {
		return nil
	}
	var m wire.Message
	if f.Decode(&m) != nil || m.From.UserID != wire.Private.UserID || m.Body == nil {
		return nil
	}
	return &m
}

// Message waits for a message snapshot (§3.5) that match is true of, the
// first one the client got that a call of Message has not taken yet, takes
// it, and returns it. It fails the test when none comes. Snapshots are
// sent to the members of a room: a guest is a member of the default room,
// "general".
func (c *Client) Message(match func(apron.Message) bool) apron.Message {
	c.t.Helper()
	ctx, cancel := c.deadline()
	defer cancel()
	var found apron.Message
	_, err := c.Wire.Expect(ctx, func(f wiretest.Frame) bool {
		if f.Method != "message" {
			return false
		}
		m, ok := apron.DecodeMessage(apron.Event{Kind: apron.EventMessage, Record: jsontext.Value(f.Params)})
		if ok && match(m) {
			found = m
			return true
		}
		return false
	})
	if err != nil {
		c.t.Fatalf("aprontest: no message matched: %v", err)
	}
	return found
}

// Closed reports whether the server closes the connection within ten
// seconds, as it does when it signs the client's account out, taking
// every frame until then.
func (c *Client) Closed() bool {
	ctx, cancel := c.deadline()
	defer cancel()
	for {
		if _, err := c.Wire.Next(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

// Mailbox is an apron.EmailSender, for Config.EmailSender, that keeps the
// sign-in codes the server sends, with which Server.Account signs in, and
// Code reads. Its zero value is ready to use.
type Mailbox struct {
	mu      sync.Mutex
	sent    []apron.SignInEmail
	changed chan struct{}
}

// SendSignInCode keeps the message.
func (m *Mailbox) SendSignInCode(_ context.Context, message apron.SignInEmail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, message)
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
	return nil
}

// Code takes the code of the first message to address it has not taken,
// waiting for one at most wait, and returns it, or "" if none came, as
// for an address that may not sign in.
func (m *Mailbox) Code(address string, wait time.Duration) string {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		if i := slices.IndexFunc(m.sent, func(s apron.SignInEmail) bool { return strings.EqualFold(s.To, address) }); i >= 0 {
			code := m.sent[i].Code
			m.sent = slices.Delete(m.sent, i, i+1)
			m.mu.Unlock()
			return code
		}
		if m.changed == nil {
			m.changed = make(chan struct{})
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			return ""
		}
	}
}
