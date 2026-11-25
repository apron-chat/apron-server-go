// Package aprontest tests an extension of package apron as people use it:
// it serves a server with the extension in a test, and connects to it as
// clients do, as a guest, with an account, with roles, or not signed in
// yet, so that a test reads as what people do:
//
//	func TestWeather(t *testing.T) {
//		server := aprontest.Serve(t, aprond.DefaultConfig(), &weather.Extension{})
//		ada := server.Guest("Ada")
//		notices, err := ada.Command("general", "/weather Lisbon")
//		if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "Lisbon") {
//			t.Fatalf("/weather = %q, %v", notices, err)
//		}
//		if _, err := ada.Post("general", "@weather rain again?", aprond.Mention("weather")); err != nil {
//			t.Fatal(err)
//		}
//		reply := ada.Message(func(m aprond.Message) bool { return m.From.UserID == "weather" })
//		...
//	}
//
// A Server is served over HTTP by a test server: the extensions' routes
// are at its URL, and its WebSocket endpoint at WebSocketURL, which
// aprontest.Dial and conformance.Config.URL take too. A Client is a
// WebSocket connection to it, a aprontest.Client (Client.Wire), whose
// methods wait at most ten seconds, fail the test when the connection
// fails, and return the server's error replies as *aprond.Error, such as
// error/denied for a request an extension refused. Call them from the
// test's goroutine, as t.Fatal is.
//
// The methods take and return apron's types where package apron has one
// for what they carry, as an extension sees it: the post options of
// aprond.Session.Post (aprond.Mention, aprond.ReplyTo, aprond.Markdown,
// aprond.WithExt), aprond.Message, and aprond.Error. Frames that apron has no
// type of its own for, because extensions never handle them, are package
// wire's, as a client receives them: the server frame (wire.ServerInfo) and
// the result of auth (wire.AuthResult).
//
// # Members with roles
//
// Server.Member signs in an account that holds server roles, such as
// aprond.RoleModerator or an extension's own, as an invitation with those
// roles makes one (aprond.InvitationOptions), so that a test of what only
// some people may do needs no Config.Roles and no extension of its own.
//
// # Followers
//
// An extension's followers of the log (aprond.Registrar.OnEvent,
// OnEventFrom, and Follow) handle each event on a goroutine of their own,
// after the request that logged it was answered: a test that posts, then
// asks the extension about the post, such as a search index, first calls
// Server.Settle, which waits until every follower has handled every event
// logged so far, and those logged meanwhile, rather than polling or
// sleeping.
//
// # External sign-in
//
// An App is an app on the person's device signing in with the server's
// external sign-in (§4.12), which an extension offers
// (aprond.Registrar.ExternalSignIn): it opens the server's sign-in page, its
// browser follows the redirects to the extension's identity provider, such
// as a fake one the test serves, and back, presses the page's buttons, and
// redeems the code it returns with on a connection of its own.
//
// What a test does not do as a client, it does in process with the Server's
// methods, such as Invitations and Shutdown.
package aprondtest

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/aprond"
	"github.com/apron-chat/apron-server-go/aprond/internal/testhook"
)

// timeout bounds each wait of a Server's or a Client's methods.
const timeout = 10 * time.Second

// Server is a server under test, which Serve starts: an aprond.Server, and
// the test HTTP server that serves it.
type Server struct {
	*aprond.Server
	// URL is the base URL of the server's HTTP routes, the extensions'
	// included, such as http://127.0.0.1:41234.
	URL string

	t    testing.TB
	mail *Mailbox
	// extension is the name WithExt writes under for its clients.
	extension string
}

// Serve opens a server with config and extensions, after any config names,
// with its state in config.Store, or in memory when it is nil, and serves
// it on a test HTTP server at a loopback address. Both are shut down when
// the test ends, after its clients are closed. It fails the test when the
// server does not open, such as when an extension's Init fails.
func Serve(t testing.TB, config aprond.Config, extensions ...aprond.Extension) *Server {
	t.Helper()
	config.Extensions = append(slices.Clip(config.Extensions), extensions...)
	app, err := aprond.Open(config)
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
	s := &Server{Server: app, URL: httpServer.URL, t: t}
	s.mail, _ = config.EmailSender.(*Mailbox)
	if len(extensions) > 0 {
		s.extension = extensions[0].Name()
	} else if len(config.Extensions) > 0 {
		s.extension = config.Extensions[0].Name()
	}
	return s
}

// WebSocketURL is the address of the server's WebSocket endpoint, such as
// ws://127.0.0.1:41234/ws, for aprontest.Dial, or conformance.Config.URL,
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
	conn, err := aprontest.Dial(ctx, s.WebSocketURL())
	if err != nil {
		s.t.Fatalf("aprontest: connecting: %v", err)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	return &Client{Wire: conn, Extension: s.extension, t: s.t}
}

// SignIn connects a client and signs it in with the params of an auth
// request (§3.2), such as {"scheme": "token", "token": ...}, or fails the
// test.
func (s *Server) SignIn(params any) *Client {
	s.t.Helper()
	c := s.Dial()
	if err := c.signIn(params); err != nil {
		s.t.Fatalf("aprontest: signing in with %v: %v", params, err)
	}
	return c
}

// Guest connects a client and signs it in as a guest who asked for name,
// or fails the test, as on a server with Config.DisableGuests.
func (s *Server) Guest(name string) *Client {
	s.t.Helper()
	c := s.Dial()
	if err := c.SignInAsGuest(name); err != nil {
		s.t.Fatalf("aprontest: signing in as the guest %s: %v", name, err)
	}
	return c
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
	if err := c.AskForCode(address); err != nil {
		s.t.Fatalf("aprontest: asking for a code for %s: %v", address, err)
	}
	code := s.mail.Code(address, timeout)
	if code == "" {
		s.t.Fatalf("aprontest: no code was mailed to %s", address)
	}
	if err := c.SignInWithCode(code); err != nil {
		s.t.Fatalf("aprontest: signing in as %s: %v", address, err)
	}
	return c
}

// Member makes an account with the user_id userID, also its name, that
// holds roles, and connects a client signed in to it, or fails the test,
// as when userID is taken. The account is made in process, as an
// invitation with these roles makes one (aprond.InvitationOptions): it is a
// member of the default room, holds roles until RevokeEnrollment, and the
// invitation admits it to a server whose sign-in an extension restricts.
// Its identity is the email address userID@aprontest.invalid, with which
// Account signs it in again, on a server with a Mailbox. Its Token signs
// it in from any origin.
func (s *Server) Member(userID string, roles ...string) *Client {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), timeout)
	defer cancel()
	options := aprond.InvitationOptions{Name: userID, UserID: userID, Roles: roles}
	made, token, err := testhook.Member(s.Server, ctx, options, userID+"@aprontest.invalid")
	if err == nil && made != userID {
		err = errors.New("the user_id is taken; the account is " + made)
	}
	if err != nil {
		s.t.Fatalf("aprontest: making the member %s: %v", userID, err)
	}
	c := s.Dial()
	if err := c.SignInWithToken(token); err != nil {
		s.t.Fatalf("aprontest: signing in as the member %s: %v", userID, err)
	}
	return c
}

// Settle waits until each of the extensions' followers of the log
// (aprond.Registrar.OnEvent, OnEventFrom, and Follow) has handled every
// event logged before Settle returns: those logged as it is called, and
// those logged meanwhile, such as a bot's reply that a follower posted as
// it handled an event, until the log is quiet. It fails the test when they
// do not within ten seconds, as when a follower retries an event that
// fails. What a follower does on a goroutine of its own (aprond.Host.Go),
// such as a reply that it streams, Settle does not wait for: wait for its
// message with Client.Message.
func (s *Server) Settle() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), timeout)
	defer cancel()
	if err := testhook.Settle(s.Server, ctx); err != nil {
		s.t.Fatalf("aprontest: the followers did not settle: %v", err)
	}
}
