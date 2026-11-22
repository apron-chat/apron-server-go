package apron

import (
	"container/list"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/apron/internal/durable"
	"github.com/apron-chat/apron-server-go/apron/internal/kv"
	"github.com/apron-chat/apron-server-go/apron/internal/proposals"
	"github.com/apron-chat/apron-server-go/apron/internal/roomlog"
	"github.com/apron-chat/apron-server-go/apron/internal/sessions"
	"github.com/apron-chat/apron-server-go/apron/internal/uploads"
	"github.com/apron-chat/apron-server-go/apron/internal/webpush"
	"github.com/apron-chat/apron-server-go/internal/auth/external"
	"github.com/apron-chat/apron-server-go/internal/auth/passkey"
	"github.com/apron-chat/apron-server-go/internal/authn"
	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

// encodeJSON encodes a value as wire encodes frames (wire.Marshal), or
// returns nil if it cannot: deterministically, sorting object keys, with
// encoding/json's null for nil slices and maps, and without escaping <, >,
// and &, which only matter for JSON embedded in HTML and would make stored
// text up to six times larger than it arrived.
func encodeJSON(value any) []byte {
	payload, err := wire.Marshal(value)
	if err != nil {
		return nil
	}
	return payload
}

// notification renders a frame once, for sending to many connections.
func notification(method string, params any) jsontext.Value {
	return encodeJSON(map[string]any{"method": method, "params": params})
}

// rawNotification renders a frame around params that are already JSON, as
// notification would with the decoded params ("method" sorts first).
func rawNotification(method string, params jsontext.Value) jsontext.Value {
	payload := make([]byte, 0, len(method)+len(params)+24)
	payload = append(payload, `{"method":`...)
	payload = strconv.AppendQuote(payload, method)
	payload = append(payload, `,"params":`...)
	payload = append(payload, params...)
	return append(payload, '}')
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

type client struct {
	server *Server
	// conn is the connection as package wire serves it, and session the
	// Session it is, if any.
	conn    *wire.Conn
	session *Session
	// request is the context of the request being processed, which the
	// extension code it runs gets: canceled when the connection closes. It
	// is set by whoever processes the connection's frames, one at a time.
	request context.Context

	// user is the connection's identity, nil before authentication. Guarded
	// by server.mu.
	user *userState
	// baseURL prefixes the write, file, and live text URLs this connection mints.
	baseURL string
	origin  string
	// authStates are each sign-in scheme's state for the connection, such
	// as a pending passkey ceremony, which a sign-in to another account
	// clears. Only the connection's own auth requests use them, one at a
	// time.
	authStates map[string]*authn.State
	// proposalBudget limits the proposals made on the connection, of any
	// scheme (proposalsPerConnection). Guarded by server.mu.
	proposalBudget *rate.Limiter
	// token is the key of the session token the connection last signed in
	// or resumed with, which a later sign-in forgets. device is the key of
	// the /invite-device token it signed in with, zero for none; a later
	// session token leaves it, so replacing the device token still closes
	// the connection.
	token  [32]byte
	device [32]byte
	// credential names the credential the connection signed in with, as
	// scheme:id, such as a passkey's, or its session token was made with;
	// verified is its latest proof of its account with a credential, a
	// reauthentication (account.go). Guarded by server.mu.
	credential string
	verified   verification
	// service is the key of the service token the connection signed in
	// with, zero for none, and serviceManage whether it may manage its
	// account (service.go). Guarded by server.mu.
	service       [32]byte
	serviceManage bool
	// proposal is the connection's latest proposal (§4.11.1), which may
	// have been dropped since, and clientKey names its client address for
	// the limits of all its connections. Guarded by server.mu.
	proposal  *proposals.Proposal
	clientKey string
	// idle reports that nobody is attending the connection (§4.5): false
	// until the connection says otherwise. Guarded by server.mu.
	idle bool
}

// Server is an Apron server, which Open starts. It serves the protocol
// through Handler, and is safe for concurrent use.
type Server struct {
	config Config

	mu sync.RWMutex
	// log is the single append-only log of every room (roomlog), which
	// issues the log_id sequence.
	log      *roomlog.Log
	rooms    map[string]*roomState
	messages map[string]*messageState
	clients  map[*client]struct{}
	// users holds every live identity: connected guests and passkey users.
	users map[string]*userState
	// usedIDs holds every user_id ever assigned, lowercased, so none is
	// reissued (§3.3).
	usedIDs map[string]bool
	// grantedIDs holds the user_ids Config.Roles names, lowercased, which
	// are never assigned to a new identity unless already used; roleHolders
	// are the principals Config.Roles grants each role, lowercased.
	grantedIDs  map[string]bool
	roleHolders map[string][]Principal
	guestNumber uint64
	// accountNumber numbers the user_<n> of new email accounts.
	accountNumber uint64
	embedNumber   uint64
	embeds        map[string]*embedState
	// writes maps an unused or in-progress write token to its embed.
	writes map[string]*embedState
	// uploads lists finished uploads but custom emoji, oldest first, for
	// eviction beyond MaxUploadStorageBytes; uploadBytes is their total
	// size. Custom emoji have their own bound, MaxEmojiStorageBytes.
	uploads     *list.List
	uploadBytes int64
	// files holds upload content, and counts the bytes of bodies arriving,
	// which MaxUploadStorageBytes also bounds.
	files *uploads.Dir
	// uploadSeq orders finished uploads across restarts.
	uploadSeq int64
	// emoji is the custom emoji catalog by key (ext:emoji); emojiCatalog
	// is its JSON, served at emojiPath + emojiName.
	emoji        map[string]*customEmoji
	emojiCatalog []byte
	emojiName    string
	// profile is the server's title, description, and avatar as admins set
	// them with /server (§3.1), which override Config's.
	profile serverProfile

	// dirty is the state changed under mu since it was last written to the
	// store; store writes it, in batches, in the background (durable.go).
	dirty      dirtySet
	store      *durable.Writer
	storedMeta storedMeta
	pushes     map[string]*pushRegistration
	closed     bool
	sessions   *sessions.Table[*userState]
	devices    map[[32]byte]deviceToken
	// invitations are the outstanding invitations by the SHA-256 of their
	// tokens (invitations.go).
	invitations map[[32]byte]*invitation
	// services are the bots' service tokens by the SHA-256 of their tokens
	// (service.go).
	services map[[32]byte]*serviceToken
	// schemes are the sign-in schemes the server offers (auth.go), in the
	// order the server frame lists them; accounts are the accounts by the
	// identities that prove them; proposals are the outstanding proposals
	// of sign-ins that finish out of band (§4.11), in memory only.
	schemes   []authn.Scheme
	accounts  map[authn.Identity]*userState
	proposals *proposals.Table
	// externalBy names the extension that offers external sign-in.
	externalBy string
	// schemeWork tracks the schemes' work in progress, such as email
	// deliveries, which Shutdown awaits.
	schemeWork sync.WaitGroup

	ops map[string]operation
	// wire serves the protocol, with s as its Backend (backend.go).
	wire *wire.Server
	push *webpush.Deliverer
	// vapid is the webpush key (§4.9); vapidStored reports that it is the
	// one kept in the store.
	vapid       *webpush.VAPIDKey
	vapidStored bool
	// badgeDelay is how long badge pushes wait to coalesce the changes to
	// one user's unread count; badges counts the waits in progress.
	badgeDelay time.Duration
	badges     sync.WaitGroup
	// stopped ends the background sweep of expired push registrations.
	stopped chan struct{}

	// commands are the core commands, then the extensions' (§4.1).
	commands *serverCommands
	// The extensions' state (extension.go): the capabilities and ext they
	// advertise, their HTTP routes, message filters, and kept
	// values by entry kind and key. Registration takes s.mu.
	advertised map[string]jsontext.Value
	routes     []route
	filters    []extensionFilter
	extData    *kv.Values
	// extContext is canceled by Shutdown, which waits for extensionWork:
	// Host.Go goroutines and event followers.
	extContext    context.Context
	extCancel     context.CancelFunc
	extensionWork sync.WaitGroup
	// allowlist is who may sign in, once an extension restricts sign-in
	// (allowlist.go); nil lets in anyone.
	allowlist *allowlist
	logger    *slog.Logger
	// sessionCount counts the clients that are Sessions, which
	// MaxConnections does not bound.
	sessionCount int
	// seenBaseURL is the latest WebSocket connection's baseURL, for
	// Sessions without Config.PublicURL.
	seenBaseURL string
	// handler serves Handler, built once the extensions have added their
	// routes.
	handler http.Handler
}

// Open starts a server with the state in config.Store, or an empty memory
// store when it is nil, then runs each extension's Init in order. It fails
// when the configuration is invalid, the stored state cannot be read, or an
// extension fails to start. The server owns config.Store: it closes it on
// Shutdown, or when Open fails.
func Open(config Config) (*Server, error) {
	config = config.withDefaults()
	fail := func(err error) (*Server, error) {
		if config.Store != nil {
			if closeErr := config.Store.Close(); closeErr != nil {
				slog.Error("cannot close the store", "error", closeErr)
			}
		}
		return nil, err
	}
	if config.Avatar != "" && !validAvatar(config.Avatar) {
		return fail(fmt.Errorf("the server avatar must be an https: URL or a data:image URL of at most %d bytes", maxAvatarDataURLBytes))
	}
	if config.Store == nil {
		config.Store = store.NewMemory()
	}
	s := &Server{
		config:      config,
		rooms:       make(map[string]*roomState),
		messages:    make(map[string]*messageState),
		clients:     make(map[*client]struct{}),
		users:       make(map[string]*userState),
		usedIDs:     make(map[string]bool),
		grantedIDs:  make(map[string]bool),
		roleHolders: make(map[string][]Principal),
		embeds:      make(map[string]*embedState),
		writes:      make(map[string]*embedState),
		uploads:     list.New(),
		emoji:       make(map[string]*customEmoji),
		pushes:      make(map[string]*pushRegistration),
		sessions:    sessions.New[*userState](),
		devices:     make(map[[32]byte]deviceToken),
		invitations: make(map[[32]byte]*invitation),
		services:    make(map[[32]byte]*serviceToken),
		accounts:    make(map[authn.Identity]*userState),
		proposals:   proposals.NewTable(),
		dirty:       newDirtySet(),
		badgeDelay:  badgeDelay,
		stopped:     make(chan struct{}),

		commands:   coreCommands(),
		advertised: make(map[string]jsontext.Value),
		extData:    kv.New(),
		log:        roomlog.NewLog(),
		logger:     config.Logger,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	s.extContext, s.extCancel = context.WithCancel(context.Background())
	for role, holders := range config.Roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			return fail(errors.New("Config.Roles grants an empty role"))
		}
		for _, holder := range holders {
			principals, err := roleHolder(holder)
			if err != nil {
				return fail(fmt.Errorf("Config.Roles[%q]: %w", role, err))
			}
			for _, p := range principals {
				if p.kind == principalUser {
					s.grantedIDs[p.value] = true
				}
			}
			s.roleHolders[role] = append(s.roleHolders[role], principals...)
		}
	}
	s.ops = s.operations()
	s.newWire()
	s.push = webpush.NewDeliverer(config.AllowInsecurePush)
	if config.VAPIDPrivateKey != "" {
		key, err := webpush.ParseVAPIDKey(config.VAPIDPrivateKey)
		if err != nil {
			return fail(err)
		}
		s.vapid = key
	}
	s.push.Subject = config.VAPIDSubject
	if s.push.Subject == "" && strings.HasPrefix(config.PublicURL, "https://") {
		s.push.Subject = config.PublicURL
	}
	// The sign-in schemes get what they keep with the accounts once the
	// extensions have added theirs (startSignIn).
	if config.WebAuthn != nil {
		scheme := passkey.New(config.WebAuthn)
		scheme.SignalUnknown = config.SignalUnknownPasskeys
		scheme.GuestFirst = config.GuestFirstPasskeys
		if err := s.addSchemeLocked(scheme); err != nil {
			return fail(err)
		}
	}
	if config.EmailSender != nil {
		if err := s.addSchemeLocked(emailScheme(config.EmailSender, config.EmailLinkURL)); err != nil {
			return fail(err)
		}
	}
	if issuer := config.ExternalIssuer; issuer != nil {
		scheme, err := external.NewForeign(issuer.Issuer, issuer.Scope, issuer.HTTPClient)
		if err == nil {
			err = s.addSchemeLocked(scheme)
		}
		if err != nil {
			return fail(err)
		}
		s.externalBy = "Config.ExternalIssuer"
	}
	s.files = uploads.Open(config.UploadDir, config.MaxUploadStorageBytes)
	s.store = durable.Start(config.Store, config.OnStoreError, ErrServerClosed)
	s.mu.Lock()
	files, err := s.restoreLocked()
	if err != nil {
		s.store.Stop()
		s.mu.Unlock()
		return fail(fmt.Errorf("restoring the stored state: %w", err))
	}
	s.push.VAPID = s.vapid
	if s.rooms[defaultRoomID] == nil {
		// The seeded default room has a logged creation record like any
		// other room, so its history_log_id is never null.
		s.commitRoomLocked(defaultRoomID, nil, false, roomlog.RoomRecord{Title: "General"})
	}
	s.files.RemoveStale(files)
	s.unlock()
	go s.sweepPushes()
	err = s.initExtensions(config.Extensions)
	if err == nil {
		s.handler, err = s.buildHandler(s.routes)
	}
	if err == nil {
		err = s.startSignIn()
	}
	if err != nil {
		// Shutdown stops what the extensions started, and closes the store.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		return nil, err
	}
	return s, nil
}

// defaultRoomID is the seeded room that requests without room_id address
// (§3.5.1, §4.3) and that new guests join.
const defaultRoomID = "general"

// Handler returns the HTTP handler serving /ws, /healthz, the passkey related
// origins, the write, file, live text, and emoji catalog endpoints, the
// extensions' routes (Registrar.Handle), and StaticDir.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// buildHandler builds the server's routes with the extensions' routes,
// reporting a pattern that conflicts with another as an error.
func (s *Server) buildHandler(routes []route) (handler http.Handler, err error) {
	var pattern string
	defer func() {
		// http.ServeMux panics on a conflict.
		if recovered := recover(); recovered != nil {
			handler, err = nil, fmt.Errorf("route %q: %v", pattern, recovered)
		}
	}()
	mux := http.NewServeMux()
	mux.Handle("/ws", s.wire)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc(writePath, s.handleWrite)
	mux.HandleFunc(filePath, s.handleFile)
	mux.HandleFunc(livePath, s.handleLive)
	mux.HandleFunc(emojiPath, s.handleEmoji)
	for _, scheme := range s.schemes {
		if router, ok := scheme.(authn.Router); ok {
			router.Routes(mux)
		}
	}
	if s.config.StaticDir != "" {
		root := filepath.Clean(s.config.StaticDir)
		mux.Handle("/", http.FileServer(staticFS{http.Dir(root)}))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	for _, route := range routes {
		pattern = route.pattern
		mux.Handle(route.pattern, route.handler)
	}
	return mux, nil
}

// staticFS serves a built frontend without directory listings or dotfiles.
type staticFS struct{ root http.FileSystem }

func (f staticFS) Open(name string) (http.File, error) {
	for _, part := range strings.Split(path.Clean(name), "/") {
		if strings.HasPrefix(part, ".") {
			return nil, fs.ErrNotExist
		}
	}
	file, err := f.root.Open(name)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err == nil && info.IsDir() {
		// A directory is served only through its index.html.
		index, err := f.root.Open(strings.TrimSuffix(name, "/") + "/index.html")
		if err != nil {
			file.Close()
			return nil, fs.ErrNotExist
		}
		index.Close()
	}
	return file, nil
}

// ServeHTTP serves the Handler, so a Server is an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if s.StoreErr() != nil {
		// The server serves what it holds, but keeps none of it.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"store_failed"}`))
		return
	}
	if closed {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Shutdown tells active WebSockets to retry later, closes them, ends live
// text, and waits for the connection handlers to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stopped)
		s.extCancel()
		for _, e := range s.embeds {
			e.endLive()
		}
		for _, t := range s.services {
			if t.timer != nil {
				t.timer.Stop()
			}
		}
		// Pending badge pushes would only be dropped once they fire, and
		// the ends of timed mutes and coalesced statuses would find the
		// server closed: none is kept waiting past it.
		for _, u := range s.users {
			s.stopTimersLocked(u)
		}
		s.files.Close()
	}
	s.unlock()

	done := make(chan struct{})
	go func() {
		// Each connection is told to come back shortly, after what is
		// already queued for it.
		if s.wire != nil {
			_ = s.wire.Shutdown(ctx)
		}
		s.badges.Wait()
		s.push.Wait()
		s.schemeWork.Wait()
		// Extensions may keep values as they stop, so the store closes
		// after them.
		s.extensionWork.Wait()
		s.mu.Lock()
		s.closeStoreLocked()
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nativeOrigin reports whether origin is a native app's, such as
// android:apk-key-hash:…, that the RP accepts as a ceremony's origin: a
// trusted app connects with it (§4.10). OriginPatterns match a host, and
// such an origin has none, so they cannot accept it; a browser never sends
// one.
func (s *Server) nativeOrigin(origin string) bool {
	if s.config.WebAuthn == nil || !protocol.IsKnownOpaqueOrigin(origin) {
		return false
	}
	if parsed, err := url.Parse(origin); err != nil || parsed.Host != "" {
		return false
	}
	return slices.Contains(s.config.WebAuthn.Config.RPOrigins, origin)
}

// clientKey identifies a connection's client for rate limits: its IP, or
// the /64 of an IPv6 address, which one household or host usually has.
func clientKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// clientIP is the address of the client that opened a request: with
// Config.ClientIPHeader, the last entry across every line of that header,
// which the proxy in front added itself (a client can send earlier ones),
// without a port; else, or when that is not an IP address, the
// connection's own address.
func (s *Server) clientIP(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	header := s.config.ClientIPHeader
	if header == "" {
		return remote
	}
	values := strings.Split(strings.Join(r.Header.Values(header), ","), ",")
	entry := strings.TrimSpace(values[len(values)-1])
	if host, _, err := net.SplitHostPort(entry); err == nil {
		entry = host
	}
	entry = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
	if net.ParseIP(entry) == nil {
		return remote
	}
	return entry
}

// baseURL is the configured PublicURL or the scheme and host the WebSocket
// request arrived on.
func (s *Server) baseURL(r *http.Request) string {
	if s.config.PublicURL != "" {
		return s.config.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host}).String()
}

// serverFrameLocked is the `server` frame (PROTOCOL.md §3.1) for c.
func (s *Server) serverFrameLocked(c *client) wire.ServerInfo {
	return s.serverParams(c.baseURL, s.revealedLocked(c))
}

// revealedLocked reports whether c gets the whole server frame: once it is
// signed in, or before when anyone may sign in as a guest anyway. Before a
// sign-in that only some may make, the frame holds what signing in needs:
// not the default room's description, the custom emoji, or what extensions
// advertise.
func (s *Server) revealedLocked(c *client) bool {
	return c.user != nil || s.guestsAllowedLocked()
}

// serverParams is the `server` frame (PROTOCOL.md §3.1) for a connection
// whose URLs start with baseURL, whole when revealed; package wire adds the
// protocol version and the ping interval. Callers hold s.mu.
func (s *Server) serverParams(baseURL string, revealed bool) wire.ServerInfo {
	var authSchemes []string
	for _, scheme := range s.schemes {
		authSchemes = append(authSchemes, scheme.Name())
	}
	if len(authSchemes) > 0 {
		authSchemes = append(authSchemes, "token")
	}
	guests := s.guestsAllowedLocked()
	if guests {
		authSchemes = append(authSchemes, "guest")
	}
	// signup (§3.2) lists the schemes that can create an account: every
	// one but token, which only resumes one. Email, external sign-in, and
	// guest start one, and so does a passkey registration where guests are
	// allowed, on a connection that is not signed in (§4.10). Without
	// guests a registration makes an account only with an invitation, so
	// webauthn is no way to sign up. An invitation does not put it back,
	// since signup tells every client what it can do, and an invitation is
	// a secret one person was given; an invited client registers anyway,
	// as webauthn stays in auth. With GuestFirstPasskeys a registration
	// makes an account only of a signed-in guest, so signup lists guest but
	// not webauthn, which tells clients to sign in as a guest first.
	signup := slices.DeleteFunc(slices.Clone(authSchemes), func(scheme string) bool {
		return scheme == "token" || scheme == passkey.Name && (!guests || s.config.GuestFirstPasskeys)
	})
	info := wire.ServerInfo{
		Agent: "apron-go/10",
		// Package wire adds the capability of each handler and method the
		// server has, such as rooms, status, and ext:command_list
		// (methodCapabilities), so this lists only what no method stands
		// for: what the server keeps and does beyond its requests, and the
		// extensions that need no method, or that only some connections
		// are told of.
		Capabilities: slices.Clone(featureCapabilities),
		Auth:         authSchemes,
		// The optional statuses accepted with `me` (§4.5); online and ""
		// always are, and are not listed.
		Status: optionalStatuses,
	}
	if slices.Contains(authSchemes, external.Name) {
		// ext:oauth: external sign-in's OAuth extras (SERVER.md), the
		// server's own authorization server metadata and token endpoint,
		// or its protected resource metadata naming another issuer. A
		// client needs it to sign in, so it is listed before.
		info.Capabilities = append(info.Capabilities, "ext:oauth")
	}
	if revealed && slices.ContainsFunc(s.schemes, func(scheme authn.Scheme) bool {
		_, ok := scheme.(authn.Credentials)
		return ok
	}) {
		// ext:account (account.go): credentials, such as passkeys, and
		// other sessions.
		info.Capabilities = append(info.Capabilities, "ext:"+accountExt)
	}
	var ext wire.Ext
	if emoji := s.emojiExt(baseURL); emoji != nil && revealed {
		info.Capabilities = append(info.Capabilities, "ext:emoji")
		ext = ext.With("emoji", emoji)
	}
	if !s.config.DisablePush {
		push := map[string]any{"relay": map[string]any{}, "wake": (urgentScopes | wakeJoined | wakeBadge).names()}
		if s.vapid != nil {
			push["webpush"] = map[string]any{"key": s.vapid.Public}
		}
		info.Push = push
	}
	if s.config.Welcome != "" {
		info.Welcome = s.config.Welcome
	}
	for _, field := range profileFields {
		if value, source := s.profileValueLocked(field); value != "" && (revealed || source != fromRoom) {
			switch field {
			case "title":
				info.Title = value
			case "description":
				info.Description = value
			case "avatar":
				info.Avatar = value
			}
		}
	}
	if s.config.MaxPrivateRooms > 0 {
		// ext:settings (apron's EXTENSIONS.md): read_cursors is absent,
		// so true, since the server keeps read_message_id (§4.6).
		info.Capabilities = append(info.Capabilities, "ext:settings")
		ext = ext.With("settings", map[string]any{"private_rooms": s.config.MaxPrivateRooms})
	}
	// Each extension that advertises (Host.Advertise) adds its capability,
	// ext:<name>, and its data under its name in ext (EXTENSIONS.md).
	for _, name := range slices.Sorted(maps.Keys(s.advertised)) {
		if !revealed {
			break
		}
		info.Capabilities = append(info.Capabilities, "ext:"+name)
		if value := s.advertised[name]; len(value) > 0 {
			if ext == nil {
				ext = make(wire.Ext)
			}
			ext[name] = value
		}
	}
	info.Ext = ext
	if !slices.Equal(signup, authSchemes) {
		info.Signup = signup
	}
	return info
}

// stopConnection ends the connection at once.
func (c *client) stopConnection() { c.conn.Close() }

// closeWithError sends an error about the connection as a whole, without id
// (PROTOCOL.md §1.1), after the frames already queued, then closes the
// connection.
func (c *client) closeWithError(e *Error) { c.conn.CloseWithError(e) }

func (c *client) enqueue(value any) bool {
	return c.enqueueBatch(value)
}

// enqueueBatch keeps a sequence of protocol frames together in the writer's
// queue while each frame is still written as its own WebSocket message,
// such as a room_list result and the read cursors that follow it, without
// consuming one queue slot per frame or interleaving another broadcast
// between them. A slow client whose queue is full is disconnected: it
// cannot hold the room lock or the server's broadcast path.
func (c *client) enqueueBatch(values ...any) bool {
	if len(values) == 0 {
		return true
	}
	if c.conn.Closing() {
		return false
	}
	frames := make([]wire.Frame, 0, len(values))
	for _, value := range values {
		// A frame rendered once for many connections is shared as is; the
		// writer never modifies it.
		payload, rendered := value.(jsontext.Value)
		if !rendered {
			if payload = encodeJSON(value); payload == nil {
				c.stopConnection()
				return false
			}
		}
		frames = append(frames, wire.Frame(payload))
	}
	return c.conn.SendBatch(frames...)
}

// sendResult replies to req now (request.reply).
func (c *client) sendResult(req request, result any) { req.reply(result) }

// operation is a request handler. When it succeeds and has already queued its
// own reply, it reports replied. Handlers whose results describe state queue
// them while holding s.mu, so that a result reflects every notification sent
// before it on the connection, and after the notifications the request
// causes, which precede its result (§1).
type operation func(s *Server, c *client, req request) (result any, replied bool, err *Error)

func (s *Server) operations() map[string]operation {
	ops := map[string]operation{
		// ext:command_list (apron's extensions/ext_command_list.md).
		commandListMethod: (*Server).listCommands,
	}
	accountOps(ops)
	if !s.config.DisablePush {
		ops["push_register"] = (*Server).registerPush
		ops["push_unregister"] = (*Server).unregisterPush
	}
	return ops
}

// readOnlyOps change nothing, so their results are not kept for
// deduplication (§1.2): a duplicate after the original finished runs again.
var readOnlyOps = map[string]bool{commandListMethod: true}

// methodCapabilities are the capabilities of the server's own methods,
// which package wire lists in the server frame as it does its handlers'
// (wire.Handler.Capability). A method of a capability that only some
// connections are told of, such as ext:account's, has none here, and its
// capability is in serverParams.
var methodCapabilities = map[string]string{
	// ext:command_list (apron's extensions/ext_command_list.md) lists the
	// commands that `command` runs.
	commandListMethod: commandListMethod,
}

// featureCapabilities are the capabilities of what the server does beyond
// its requests, which every connection is told of: the embeds it makes
// (§4.8), the ext it keeps (§4.12), and ext:editors (apron's
// EXTENSIONS.md), which needs no method or data of its own.
var featureCapabilities = []string{"embed:upload", "embed:append", "embed:terminal", "ext", "ext:editors"}

var _ http.Handler = (*Server)(nil)
