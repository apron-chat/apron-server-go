// Package conformance checks a server against the core of PROTOCOL.md, as
// ordinary Go tests: a port of the protocol's own conformance suite,
// tests/conformance/conform.py in the protocol repository, for the rules
// that are a backend's to keep.
//
// Run runs each check as a subtest named by its conform.py ID, such as
// core.delivery.slow-reader, so that its results compare with the Python
// suite's against the same server:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, conformance.Config{Server: chat.Server})
//	}
//
// A check passes, fails, or skips, as the Python one passes, fails, or
// skips: a check of a capability or a sign-in scheme the server frame does
// not offer (§3.1) skips, as does one whose request the server's policy
// refused with error/retry_after, or error/denied where §1.1 allows it.
// Where the Python suite warns, of a SHOULD the server does not follow, the
// check skips with a message that starts with "WARN". Checks run in order,
// and later ones use what earlier ones learned, such as the default room
// (§3.6.3) and a message to reply to; when -run leaves out a check that
// learns something, it still runs, unreported.
//
// Every frame from the server is also checked as it arrives, as the Python
// suite does, and the last checks report what was seen: that every request
// gets one reply (§1.1), that IDs are well formed (§2), that each room's
// snapshots arrive in ascending log_id (§3.5.3), that listed room records
// carry their delivery fields (§4.3.3), and that seen follows the shown
// status (§4.5.2).
//
// # What is checked
//
// The checks of conform.py that are ported, by area:
//
//   - the server frame, and the envelope: unknown methods, notifications,
//     and keys, an unknown room, large frames, and ping (§1, §1.1, §3.1);
//   - sign-in: requests before auth, auth as a barrier, the scheme
//     checks, auth on every connection, and a made-up token (§3.2);
//   - deduplication of retries (§1.2), and me (§3.3.2);
//   - messages: the default room, the broadcast before the result, an
//     empty message, reply_to, and a save without edit (§3.5, §3.6);
//   - live delivery: a burst, and a reader that falls behind (§3.5.3);
//   - history: paging, every room at once, and bounds (§4.3);
//   - edit, rooms and membership, reactions, activity, status, command,
//     ext, and push_unregister (§4.1 to §4.7, §4.9.1, §4.13);
//   - the rules checked on every frame, but for the schema.
//
// Three more checks are this package's own, of rules wire leaves to the
// backend (see the Validation section of package wire) that conform.py
// does not check:
//
//   - auth.shape.unsupported-first: a scheme the server does not offer is
//     error/unsupported whatever else the request holds, such as a user_id
//     that is not valid (§3.2);
//   - core.message.empty-unknown-room: an empty message to a room that does
//     not exist is error/invalid_params, not dropped (§1.1, §3.5.1);
//   - history.retention.membership: a server that discards the start of a
//     room's log keeps its members in it, with a membership record, and its
//     effective lower bound never decreases (§4.3.3).
//
// Not ported: core.shape.schema, which needs a JSON Schema validator;
// embeds (§4.8), which need HTTP uploads and live reads; and the sign-in
// schemes that need a browser, a passkey, an email, or an authorization
// server (§4.10 to §4.12, ext:oauth). The Python suite covers those.
package conformance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/apron-chat/apron-server-go/wire"
)

// Config is the server to check, and how to sign in to it.
type Config struct {
	// Server is the server to check: each connection is to it, in
	// process, over a LineTransport (Server.ServeTransport) with no HTTP
	// request. Exactly one of Server and URL is set.
	Server *wire.Server
	// URL is the WebSocket address of a server to check over the network
	// instead, such as ws://127.0.0.1:8080/ws.
	URL string
	// Auth returns the params of an auth request (§3.2) that signs a new
	// connection in, with name as the requested name, such as
	// {"scheme": "token", "token": ..., "name": name}. When each sign-in
	// is a new user, the checks that need two users run; when every
	// sign-in is the same user, they skip. Nil signs in as a guest, with
	// {"scheme": "guest", "name": name}, on a server that offers guest
	// (§3.1); on one that does not, the checks that sign in skip.
	Auth func(name string) map[string]any
	// Skip names checks not to run, by ID, each with the reason, such as
	// a backend's known failure. A skipped check that learns something
	// for later checks still runs, unreported.
	Skip map[string]string
}

// Run runs the checks against the server of config, each as a subtest of
// t named by its ID, in order. The checks sign in many times, post
// hundreds of messages, and create rooms, so the server is one for tests.
func Run(t *testing.T, config Config) {
	t.Helper()
	if (config.Server == nil) == (config.URL == "") {
		t.Fatal("conformance: set one of Config.Server and Config.URL")
	}
	r := &run{config: config, order: map[orderKey]int64{}, violations: map[string]map[string]bool{}}
	for _, item := range checks {
		ran := false
		t.Run(item.id, func(t *testing.T) {
			ran = true
			r.check(t, item)
		})
		if !ran && item.setup {
			// Left out by -run, but later checks use what it learns.
			r.quietly(t, item)
		}
	}
}

// run is the state of one Run: what the checks learned along the way, and
// what the frames broke.
type run struct {
	config Config
	nextID atomic.Uint64

	// room is the default room, message a snapshot posted to it, and
	// newRoom a room that rooms.set.create made.
	room    string
	message obj
	newRoom string

	mu         sync.Mutex
	server     obj // the first server frame's params
	order      map[orderKey]int64
	violations map[string]map[string]bool
}

// orderKey is a connection's deliveries of one room.
type orderKey struct {
	c    *conn
	room string
}

// id returns a request id not used before in the run.
func (r *run) id() string { return fmt.Sprintf("conf%d", r.nextID.Add(1)) }

// info returns the server frame's params, or nil before one arrived.
func (r *run) info() obj {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.server
}

// has reports whether the server frame lists a capability.
func (r *run) has(capability string) bool {
	return hasString(r.info()["capabilities"], capability)
}

func (r *run) violate(key, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.violations[key] == nil {
		r.violations[key] = map[string]bool{}
	}
	r.violations[key][text] = true
}

// check runs one check as a subtest.
func (r *run) check(t *testing.T, item check) {
	c := &checker{t: t, r: r, ctx: t.Context()}
	defer c.closeAll()
	if reason, ok := r.config.Skip[item.id]; ok {
		if item.setup {
			r.quietly(t, item)
		}
		t.Skip(reason)
	}
	c.ready(item)
	item.fn(c)
}

// quietly runs a check unreported, on t, for what it learns.
func (r *run) quietly(t *testing.T, item check) {
	c := &checker{t: t, quiet: true, r: r, ctx: t.Context()}
	defer c.closeAll()
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(stopCheck); !ok {
				panic(p)
			}
		}
	}()
	c.ready(item)
	item.fn(c)
}

// check is one check: its ID and section, as conform.py has them, what it
// needs of the server frame, and whether later checks use what it learns.
type check struct {
	id, ref, title string
	needs          func(server obj) (ok bool, why string)
	setup          bool
	fn             func(*checker)
}

// checker is a running check: its subtest, or with quiet, the test it
// runs in unreported, and the connections it opened.
type checker struct {
	t     *testing.T
	quiet bool
	r     *run
	ctx   context.Context
	conns []*conn
}

// stopCheck ends a check run unreported, in place of t.FailNow or
// t.SkipNow.
type stopCheck struct{}

// ready skips the check unless it can run on this server: the first
// check always runs, and the others need a server frame, and what they
// need of it.
func (c *checker) ready(item check) {
	if item.id != checks[0].id && c.r.info() == nil {
		c.skip("no server frame")
	}
	if item.needs != nil {
		if ok, why := item.needs(c.r.info()); !ok {
			c.skip("%s", why)
		}
	}
}

func (c *checker) closeAll() {
	for _, conn := range c.conns {
		conn.close()
	}
	c.conns = nil
}

// fail fails the check, as must does: a rule the server breaks.
func (c *checker) fail(format string, args ...any) {
	if c.quiet {
		panic(stopCheck{})
	}
	c.t.Helper()
	c.t.Fatalf(format, args...)
}

// skip skips the check, with the reason.
func (c *checker) skip(format string, args ...any) {
	if c.quiet {
		panic(stopCheck{})
	}
	c.t.Helper()
	c.t.Skipf(format, args...)
}

// warn ends the check without failing it: a SHOULD the server does not
// follow, which the Python suite reports as WARN.
func (c *checker) warn(format string, args ...any) {
	if c.quiet {
		panic(stopCheck{})
	}
	c.t.Helper()
	c.t.Skipf("WARN: "+format, args...)
}

// note logs what a check found that is neither a pass nor a failure.
func (c *checker) note(format string, args ...any) {
	if !c.quiet {
		c.t.Helper()
		c.t.Logf(format, args...)
	}
}

// must fails the check unless ok.
func (c *checker) must(ok bool, format string, args ...any) {
	if !ok {
		c.t.Helper()
		c.fail(format, args...)
	}
}

// should warns unless ok.
func (c *checker) should(ok bool, format string, args ...any) {
	if !ok {
		c.t.Helper()
		c.warn(format, args...)
	}
}
