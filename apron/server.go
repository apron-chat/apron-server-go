package apron

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/time/rate"

	"github.com/apron-chat/apron-server-go/store"
)

const (
	defaultReadLimit         int64 = 256 << 10
	defaultOutgoingQueue           = 128
	defaultHistoryPageSize         = 100
	maxHistoryPageSize             = 1000
	defaultPingInterval            = 30 * time.Second
	defaultPingTimeout             = 10 * time.Second
	defaultWriteTimeout            = 10 * time.Second
	defaultMaxUploadBytes    int64 = 20 << 20
	defaultMaxAvatarBytes    int64 = 2 << 20
	defaultMaxMessageUploads int64 = 20 << 20
	defaultMaxUploadStorage  int64 = 1000 << 20
	defaultMaxEmojiBytes     int64 = 256 << 10
	// defaultMaxEmojiStorage holds the most custom emoji at the default
	// size cap: 250 MiB.
	defaultMaxEmojiStorage = maxCustomEmoji * defaultMaxEmojiBytes
	// uploadReadTimeout bounds how long one upload body may take to arrive.
	uploadReadTimeout = 15 * time.Minute
	// maxHistoryReplyBytes bounds the records in one history page; a page
	// always holds at least one record.
	maxHistoryReplyBytes             = 4 << 20
	defaultUploadStartTimeout        = 5 * time.Minute
	defaultLiveTextMaxBytes    int64 = 16 << 20
	defaultLiveTextMaxDuration       = time.Hour
	defaultTerminalKeepBytes         = 64 << 10
	defaultAppendMaxBytes            = 64 << 10
	defaultMaxListedMembers          = 1000
	// retryAfterSeconds is the delay suggested by connection-level
	// retry_after errors (capacity and shutdown).
	retryAfterSeconds = 30
	// maxRequestIDBytes bounds a request id, which deduplication keeps
	// (§1.2) for the latest maxDedupEntries requests of each user.
	maxRequestIDBytes = 256
)

// DefaultTerminalViewerURL is where a browser that opens a terminal's url is
// sent by default (Config.TerminalViewerURL): Apron's stand-alone terminal
// viewer.
const DefaultTerminalViewerURL = "https://terminal.apron.chat/"

// Config configures a Server: who may connect and sign in, its limits, what
// it tells clients about itself, where it keeps state, and its extensions.
// Start from DefaultConfig. Open uses the default for any limit, size,
// timeout, or interval left at zero.
type Config struct {
	// WebAuthn enables passkeys with an explicitly configured RP and frontend origins.
	WebAuthn *webauthn.WebAuthn
	// OriginPatterns is passed to the WebSocket origin checker. Empty uses the
	// local development origins for localhost, 127.0.0.1, and ::1.
	OriginPatterns []string
	// AllowAnyOrigin disables origin checking. Use only for a trusted deployment.
	AllowAnyOrigin bool
	// StaticDir serves a built frontend from the HTTP root when non-empty.
	StaticDir string
	// ReadLimit bounds one frame a client sends, in bytes.
	ReadLimit int64

	// OutgoingQueue bounds the frames waiting to be written to one
	// connection; a client that falls further behind is disconnected.
	OutgoingQueue int
	// HistoryPageSize is the records a history page holds when its request
	// names no limit (§4.3).
	HistoryPageSize int
	// PingInterval is how often the server pings each connection, and the
	// interval it asks clients to ping at (§1). A connection that does not
	// answer within PingTimeout, or a frame that takes longer than
	// WriteTimeout to write, closes it.
	PingInterval time.Duration
	PingTimeout  time.Duration
	WriteTimeout time.Duration

	// PublicURL is the external base URL (scheme and host, such as
	// https://chat.example) of write, file, and live text URLs. Empty derives it
	// from each WebSocket request's Host header.
	PublicURL string
	// MaxUploadBytes bounds one upload; MaxAvatarBytes bounds a /avatar upload.
	MaxUploadBytes int64
	MaxAvatarBytes int64
	// MaxMessageUploadBytes bounds the uploads hosted for one message.
	MaxMessageUploadBytes int64
	// MaxUploadStorageBytes bounds all hosted uploads but custom emoji;
	// beyond it the oldest message uploads are removed from their messages.
	// Avatars count toward it but are never removed.
	MaxUploadStorageBytes int64
	// MaxEmojiBytes bounds one custom emoji's image (ext:emoji), and
	// MaxEmojiStorageBytes all of them together: an image that would pass
	// it is refused, and nothing is removed to make room.
	MaxEmojiBytes        int64
	MaxEmojiStorageBytes int64
	// UploadDir holds hosted upload content. Empty uses a temporary directory
	// removed on Shutdown. At start, files named *.upload in UploadDir that
	// no stored embed holds are removed.
	UploadDir string
	// UploadStartTimeout is how long an unused write URL stays valid, that
	// of a /embed-terminal or /embed-append command included.
	UploadStartTimeout time.Duration
	// Live text (§4.8.3) ends when its writer has sent LiveTextMaxBytes,
	// after LiveTextMaxDuration, or when its writer disconnects, keeping what
	// arrived. A terminal keeps its last TerminalKeepBytes (§4.8.6); an
	// append keeps its first AppendMaxBytes of edited text (§4.8.5), at
	// most ReadLimit with its message's opening text, so that the finished
	// body.text fits in a request.
	LiveTextMaxBytes    int64
	LiveTextMaxDuration time.Duration
	TerminalKeepBytes   int
	AppendMaxBytes      int
	// TerminalViewerURL is a page that shows a terminal's live text, such as
	// https://terminal.apron.chat: a browser that opens a terminal's url as
	// a page is sent there, with the url and title in the fragment, which
	// never reaches the viewer's host (§4.8.5). Empty serves the text as
	// plain text to browsers too.
	TerminalViewerURL string
	// Store keeps the server's state across restarts; nil keeps it in memory
	// for this Server only. The server closes it on Shutdown.
	Store store.Store
	// DisablePush omits server.push and push registration.
	DisablePush bool
	// AllowInsecurePush accepts http push endpoints and internal addresses.
	// Use only for development and tests.
	AllowInsecurePush bool
	// VAPIDPrivateKey is the server's VAPID key for webpush (§4.9), the
	// P-256 scalar in base64url. Empty uses the key kept in Store,
	// generated at the first start.
	VAPIDPrivateKey string
	// VAPIDSubject is the contact push services may use, a mailto: or
	// https: URL, sent in VAPID tokens. Empty uses PublicURL, if any.
	VAPIDSubject string
	// MaxConnections bounds concurrent WebSockets, not extensions' bots; 0
	// is unlimited.
	MaxConnections int
	// MessagesPerMinute bounds each user's new messages; 0 is unlimited.
	MessagesPerMinute int
	// MaxListedMembers bounds a room's members in room_list and room_update
	// joined; a larger room lists its most recently active members and
	// member_count (§4.2.1). 0 is unlimited.
	MaxListedMembers int
	// MaxPrivateRooms bounds the top-level private rooms (§4.2.4) one user
	// is in at once, whether they created them or were added: past it,
	// creating one or being added to one is denied, so nobody can be pulled
	// into private rooms without end, as on a public deployment. It is
	// advertised as ext:settings private_rooms. 0, the default, is unlimited.
	MaxPrivateRooms int
	// Welcome is server.welcome (§3.2): Markdown shown on the sign-in
	// screen. Empty omits it.
	Welcome string
	// Extensions add features to the server (Extension), in order. Open
	// calls their Init after restoring the state, and fails if one fails.
	Extensions []Extension
	// Logger receives the server's and the extensions' logs; nil uses
	// slog.Default().
	Logger *slog.Logger
	// DisableGuests stops offering guest sign-in (§3.2), so only accounts
	// sign in: by passkey, email, or an extension's external sign-in.
	DisableGuests bool
	// Title, Description, and Avatar show people which server this is
	// (§3.1): a plain display name, CommonMark saying in a few words what
	// the server is about, and a square icon URL that follows the rules
	// for user avatars (§4.8.6). Empty omits each. Admins may override each
	// with /server, which keeps its value in the store; clearing it there
	// falls back to these. Without either, the description is the default
	// room's.
	Title       string
	Description string
	Avatar      string
	// Roles grants server roles (§3.3), such as "admin", to accounts: each
	// role lists the user_ids or email addresses that hold it. Roles are
	// shown beside names; "admin" and "moderator" may also remove others
	// from rooms (§4.2.2), and "admin" may replace and remove any custom
	// emoji (ext:emoji) and set the server's title, description, and avatar
	// with /server.
	Roles map[string][]string
	// EmailSender enables email sign-in (§4.11) and delivers its codes; nil
	// disables it. LogEmailSender logs codes, for development.
	EmailSender EmailSender
	// ClientIPHeader names the request header a reverse proxy puts the
	// client's address in, such as X-Forwarded-For (its last entry is
	// used) or X-Real-IP, for per-client rate limits. Empty uses the
	// connection's own address. Set it only behind a proxy that sets it.
	ClientIPHeader string
	// EmailLinkURL is the page the links in sign-in emails open, such as
	// https://chat.example/, with a token, the proposal's state, and this
	// server's WebSocket URL in its fragment (§4.11.2). Empty sends codes
	// without a link.
	EmailLinkURL string
}

// DefaultConfig is the configuration aprond starts from: the local
// development origins, and the default limits, sizes, and timeouts.
func DefaultConfig() Config {
	return Config{
		OriginPatterns:        []string{"http://localhost:*", "http://127.0.0.1:*", "http://[[]::1]:*"},
		ReadLimit:             defaultReadLimit,
		OutgoingQueue:         defaultOutgoingQueue,
		HistoryPageSize:       defaultHistoryPageSize,
		PingInterval:          defaultPingInterval,
		PingTimeout:           defaultPingTimeout,
		WriteTimeout:          defaultWriteTimeout,
		MaxUploadBytes:        defaultMaxUploadBytes,
		MaxAvatarBytes:        defaultMaxAvatarBytes,
		MaxMessageUploadBytes: defaultMaxMessageUploads,
		MaxUploadStorageBytes: defaultMaxUploadStorage,
		MaxEmojiBytes:         defaultMaxEmojiBytes,
		MaxEmojiStorageBytes:  defaultMaxEmojiStorage,
		UploadStartTimeout:    defaultUploadStartTimeout,
		LiveTextMaxBytes:      defaultLiveTextMaxBytes,
		LiveTextMaxDuration:   defaultLiveTextMaxDuration,
		TerminalKeepBytes:     defaultTerminalKeepBytes,
		TerminalViewerURL:     DefaultTerminalViewerURL,
		AppendMaxBytes:        defaultAppendMaxBytes,
		MaxListedMembers:      defaultMaxListedMembers,
	}
}

func (c Config) withDefaults() Config {
	defaults := DefaultConfig()
	if c.OriginPatterns == nil && !c.AllowAnyOrigin {
		c.OriginPatterns = defaults.OriginPatterns
	}
	if c.ReadLimit <= 0 {
		c.ReadLimit = defaults.ReadLimit
	}
	if c.OutgoingQueue <= 0 {
		c.OutgoingQueue = defaults.OutgoingQueue
	}
	if c.HistoryPageSize <= 0 {
		c.HistoryPageSize = defaults.HistoryPageSize
	}
	if c.PingInterval <= 0 {
		c.PingInterval = defaults.PingInterval
	}
	if c.PingTimeout <= 0 {
		c.PingTimeout = defaults.PingTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaults.WriteTimeout
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = defaults.MaxUploadBytes
	}
	if c.MaxAvatarBytes <= 0 {
		c.MaxAvatarBytes = defaults.MaxAvatarBytes
	}
	if c.MaxMessageUploadBytes <= 0 {
		c.MaxMessageUploadBytes = defaults.MaxMessageUploadBytes
	}
	if c.MaxUploadStorageBytes <= 0 {
		c.MaxUploadStorageBytes = defaults.MaxUploadStorageBytes
	}
	if c.MaxEmojiBytes <= 0 {
		c.MaxEmojiBytes = defaults.MaxEmojiBytes
	}
	if c.MaxEmojiStorageBytes <= 0 {
		c.MaxEmojiStorageBytes = defaults.MaxEmojiStorageBytes
	}
	if c.UploadStartTimeout <= 0 {
		c.UploadStartTimeout = defaults.UploadStartTimeout
	}
	if c.LiveTextMaxBytes <= 0 {
		c.LiveTextMaxBytes = defaults.LiveTextMaxBytes
	}
	if c.LiveTextMaxDuration <= 0 {
		c.LiveTextMaxDuration = defaults.LiveTextMaxDuration
	}
	if c.TerminalKeepBytes <= 0 {
		c.TerminalKeepBytes = defaults.TerminalKeepBytes
	}
	if c.AppendMaxBytes <= 0 {
		c.AppendMaxBytes = defaults.AppendMaxBytes
	}
	// The finished text is the body.text of a message, which fits in a
	// request.
	c.AppendMaxBytes = min(c.AppendMaxBytes, int(c.ReadLimit))
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	return c
}

// recordKind partitions the server's single append-only log (PROTOCOL.md §2).
type recordKind int

const (
	kindRoom recordKind = iota
	kindMessage
	kindReactions
	kindMembership
)

// logRecord is one committed change: raw is the complete wire object
// (including log_id) as it was at commit time, kept as JSON, which history
// and broadcasts send as is and which costs far less memory than decoded
// maps. Only redaction (§4.4) rewrites a record; raw is replaced, never
// modified, so a reader may hold it after releasing s.mu. A record is
// referenced from the log of every room it belongs to, so a move snapshot
// appears in both the source and destination room logs (§4.3.1).
type logRecord struct {
	id   int64
	kind recordKind
	raw  jsontext.Value
	// rooms are the rooms whose logs hold the record.
	rooms []string
	// message is the message_id of a message snapshot.
	message string
}

// newLogRecord encodes value. Values hold only JSON-decoded data and
// server-built strings, maps, and slices, so encoding cannot fail.
func newLogRecord(id int64, kind recordKind, value map[string]any) *logRecord {
	return &logRecord{id: id, kind: kind, raw: encodeJSON(value)}
}

// jsonOptions encode deterministically, sorting object keys, and keep
// encoding/json's null for nil slices and maps. encoding/json/v2 never
// escapes <, >, and &, which only matter for JSON embedded in HTML and would
// make stored text up to six times larger than it arrived.
var jsonOptions = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
)

// encodeJSON encodes a value with jsonOptions, or returns nil if it cannot.
func encodeJSON(value any) []byte {
	payload, err := json.Marshal(value, jsonOptions)
	if err != nil {
		return nil
	}
	return payload
}

// value decodes the record. Its ext keeps its values as raw JSON, so
// re-encoding the record keeps them exactly.
func (r *logRecord) value() map[string]any {
	return decodeObject(r.raw)
}

// rewrite replaces the record with edit applied to its decoded value.
func (r *logRecord) rewrite(edit func(value map[string]any)) {
	value := r.value()
	edit(value)
	r.raw = encodeJSON(value)
}

// pingFrame is the exact client liveness ping (§1); pongFrame answers it.
var (
	pingFrame = []byte(`{"method":"ping"}`)
	pongFrame = jsontext.Value(`{"method":"pong"}`)
)

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

// rawResponse renders a result reply around a result that is already JSON,
// as response would with the decoded result.
func rawResponse(id string, result []byte) jsontext.Value {
	payload := make([]byte, 0, len(id)+len(result)+24)
	payload = append(payload, `{"id":`...)
	payload = strconv.AppendQuote(payload, id)
	payload = append(payload, `,"result":`...)
	payload = append(payload, result...)
	return append(payload, '}')
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// transport carries a connection's frames: a WebSocket, or an in-process
// Session.
type transport interface {
	// write sends one frame.
	write(ctx context.Context, payload []byte) error
	// ping checks that the peer is still there.
	ping(ctx context.Context) error
	// close ends the transport without a handshake.
	close()
}

// wsTransport is a WebSocket connection.
type wsTransport struct{ ws *websocket.Conn }

func (t wsTransport) write(ctx context.Context, payload []byte) error {
	return t.ws.Write(ctx, websocket.MessageText, payload)
}

func (t wsTransport) ping(ctx context.Context) error { return t.ws.Ping(ctx) }

func (t wsTransport) close() { _ = t.ws.CloseNow() }

type client struct {
	server *Server
	conn   transport
	// request is the context of the request being processed, which the
	// extension code it runs gets: canceled when the connection closes. It
	// is set by whoever processes the connection's frames, one at a time.
	request context.Context
	out     chan outboundBatch
	done    chan struct{}
	stop    sync.Once

	// user is the connection's identity, nil before authentication. Guarded
	// by server.mu.
	user *userState
	// baseURL prefixes the write, file, and live text URLs this connection mints.
	baseURL  string
	origin   string
	ceremony *passkeyCeremony
	// token is the key of the session token the connection last signed in
	// or resumed with, which a later sign-in forgets. device is the key of
	// the /invite-device token it signed in with, zero for none; a later
	// session token leaves it, so replacing the device token still closes
	// the connection.
	token  [32]byte
	device [32]byte
	// emailSends limits the email proposals made on the connection,
	// proposal is its latest one, which may have been dropped since, and
	// clientKey names its client address for the limits of all its
	// connections (email.go). Guarded by server.mu.
	emailSends *rate.Limiter
	proposal   *emailProposal
	clientKey  string
	// idle reports that nobody is attending the connection (§4.5): false
	// until the connection says otherwise. Guarded by server.mu.
	idle bool
	// closing is set once the final batch is queued; later frames are dropped.
	closing atomic.Bool
	// pinged is set by the first liveness ping (§1); lastFrame is when the
	// latest frame arrived, in Unix nanoseconds.
	pinged    atomic.Bool
	lastFrame atomic.Int64
}

// outboundBatch keeps a sequence of protocol frames together in the writer's
// queue while each frame is still written as its own WebSocket message, such
// as a room_list result and the read cursors that follow it, without
// consuming one queue slot per frame or interleaving another broadcast between
// them. A batch with closeReason is the connection's last: the writer closes
// the connection after writing it.
type outboundBatch struct {
	frames      [][]byte
	closeReason string
}

// Server is an Apron server, which Open starts. It serves the protocol
// through Handler, and is safe for concurrent use.
type Server struct {
	config Config

	mu sync.RWMutex
	// lastID is the single log_id sequence shared by every record kind and room.
	lastID   int64
	rooms    map[string]*roomState
	messages map[string]*messageState
	clients  map[*client]struct{}
	// users holds every live identity: connected guests and passkey users.
	users map[string]*userState
	// usedIDs holds every user_id ever assigned, lowercased, so none is
	// reissued (§3.3).
	usedIDs map[string]bool
	// grantedIDs holds the user_ids Config.Roles names, lowercased, which
	// are never assigned to a new identity unless already used.
	grantedIDs  map[string]bool
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
	// uploadsArriving counts the bytes written by uploads whose bodies are
	// arriving, or not yet recorded (arrivingWriter), which
	// MaxUploadStorageBytes also bounds. It is atomic, as bodies arrive
	// without s.mu.
	uploadsArriving atomic.Int64
	// uploadDir holds upload content; tempUploadDir is removed on Shutdown.
	uploadDir     string
	tempUploadDir bool
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
	// store; storeWrites queues batches for the store writer.
	dirty       dirtySet
	storeWrites chan []store.Entry
	storeDone   chan struct{}
	storeClosed bool
	storedMeta  storedMeta
	pushes      map[string]*pushRegistration
	closed      bool
	passkeys    map[string]*passkeyUser
	credentials map[string]*passkeyUser
	sessions    map[[32]byte]session
	devices     map[[32]byte]deviceToken
	// emails maps sign-in addresses to their accounts; email holds the
	// outstanding codes and budgets, in memory only.
	emails map[string]*userState
	email  emailState
	// mailing tracks email deliveries in progress, which Shutdown awaits.
	mailing sync.WaitGroup

	ops  map[string]operation
	push *pushDeliverer
	// vapid is the webpush key (§4.9); vapidStored reports that it is the
	// one kept in the store.
	vapid       *vapidKey
	vapidStored bool
	// badgeDelay is how long badge pushes wait to coalesce the changes to
	// one user's unread count; badges counts the waits in progress.
	badgeDelay time.Duration
	badges     sync.WaitGroup
	// stopped ends the background sweep of expired push registrations.
	stopped     chan struct{}
	connections sync.WaitGroup

	// commands are the core commands, then the extensions' (§4.1).
	commands []serverCommand
	// The extensions' state (extension.go): the capabilities and ext they
	// advertise, their HTTP routes, message filters, and kept
	// values by entry kind and key. Registration takes s.mu.
	advertised map[string]jsontext.Value
	routes     []route
	filters    []extensionFilter
	extData    map[string]map[string]jsontext.Value
	// records lists every logged record in log_id order, for Event
	// followers; recordsChanged is closed and replaced when one is added.
	records        []*logRecord
	recordsChanged chan struct{}
	// extContext is canceled by Shutdown, which waits for extensionWork:
	// Host.Go goroutines and event followers.
	extContext    context.Context
	extCancel     context.CancelFunc
	extensionWork sync.WaitGroup
	// external is the external sign-in provider (§4.11.5), if any, and
	// identities the accounts it made, by identity.
	external   *externalSignIn
	identities map[string]*userState
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
		embeds:      make(map[string]*embedState),
		writes:      make(map[string]*embedState),
		uploads:     list.New(),
		emoji:       make(map[string]*customEmoji),
		pushes:      make(map[string]*pushRegistration),
		passkeys:    make(map[string]*passkeyUser),
		credentials: make(map[string]*passkeyUser),
		sessions:    make(map[[32]byte]session),
		devices:     make(map[[32]byte]deviceToken),
		emails:      make(map[string]*userState),
		email:       newEmailState(),
		dirty:       newDirtySet(),
		storeWrites: make(chan []store.Entry, storeQueue),
		storeDone:   make(chan struct{}),
		badgeDelay:  badgeDelay,
		stopped:     make(chan struct{}),

		commands:       coreCommands(),
		advertised:     make(map[string]jsontext.Value),
		extData:        make(map[string]map[string]jsontext.Value),
		recordsChanged: make(chan struct{}),
		identities:     make(map[string]*userState),
		logger:         config.Logger,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	s.extContext, s.extCancel = context.WithCancel(context.Background())
	for _, holders := range config.Roles {
		for _, holder := range holders {
			s.grantedIDs[strings.ToLower(holder)] = true
		}
	}
	s.ops = s.operations()
	s.push = newPushDeliverer(config.AllowInsecurePush)
	if config.VAPIDPrivateKey != "" {
		key, err := parseVAPIDKey(config.VAPIDPrivateKey)
		if err != nil {
			return fail(err)
		}
		s.vapid = key
	}
	s.push.subject = config.VAPIDSubject
	if s.push.subject == "" && strings.HasPrefix(config.PublicURL, "https://") {
		s.push.subject = config.PublicURL
	}
	s.openUploadDir()
	go s.writeStore()
	s.mu.Lock()
	files, err := s.restoreLocked()
	if err != nil {
		s.storeClosed = true
		close(s.storeWrites)
		s.mu.Unlock()
		<-s.storeDone
		return fail(fmt.Errorf("restoring the stored state: %w", err))
	}
	s.push.vapid = s.vapid
	if s.rooms[defaultRoomID] == nil {
		// The seeded default room has a logged creation record like any
		// other room, so its history_log_id is never null.
		s.commitRoomLocked(defaultRoomID, nil, false, map[string]any{"title": "General"})
	}
	s.removeStaleUploads(files)
	s.unlock()
	go s.sweepPushes()
	err = s.initExtensions(config.Extensions)
	if err == nil {
		s.handler, err = s.buildHandler(s.routes)
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
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/.well-known/webauthn", s.handleRelatedOrigins)
	mux.HandleFunc(writePath, s.handleWrite)
	mux.HandleFunc(filePath, s.handleFile)
	mux.HandleFunc(livePath, s.handleLive)
	mux.HandleFunc(emojiPath, s.handleEmoji)
	mux.HandleFunc(externalPath, s.handleExternal)
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
		for c := range s.clients {
			c.closeWithError(&Error{Code: CodeRetryAfter, Message: "Server is shutting down; reconnect shortly", Data: map[string]any{"retry_after": 5}})
		}
		for _, e := range s.embeds {
			e.endLive()
		}
		// Pending badge pushes would only be dropped once they fire, and
		// the ends of timed mutes and coalesced statuses would find the
		// server closed: none is kept waiting past it.
		for _, u := range s.users {
			s.stopTimersLocked(u)
		}
		if s.tempUploadDir {
			_ = os.RemoveAll(s.uploadDir)
		}
	}
	s.unlock()

	done := make(chan struct{})
	go func() {
		s.connections.Wait()
		s.badges.Wait()
		s.push.wait()
		s.mailing.Wait()
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

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.unlock()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	s.connections.Add(1)
	s.unlock()
	defer s.connections.Done()

	options := &websocket.AcceptOptions{}
	if s.config.AllowAnyOrigin {
		options.InsecureSkipVerify = true
	} else {
		options.OriginPatterns = s.config.OriginPatterns
	}
	ws, err := websocket.Accept(w, r, options)
	if err != nil {
		return
	}
	ws.SetReadLimit(s.config.ReadLimit)

	c := &client{
		server:    s,
		conn:      wsTransport{ws},
		out:       make(chan outboundBatch, s.config.OutgoingQueue),
		done:      make(chan struct{}),
		origin:    r.Header.Get("Origin"),
		clientKey: clientKey(s.clientIP(r)),
		baseURL:   s.baseURL(r),
	}
	c.lastFrame.Store(time.Now().UnixNano())
	go c.writeLoop()
	go c.pingLoop()
	s.mu.Lock()
	if s.closed {
		s.unlock()
		c.stopConnection()
		return
	}
	if s.config.MaxConnections > 0 && len(s.clients)-s.sessionCount >= s.config.MaxConnections {
		// An error about the connection as a whole omits id (§1.1).
		c.enqueue(s.serverFrameLocked(c))
		c.closeWithError(&Error{Code: CodeRetryAfter, Message: "Server at capacity; try again shortly", Data: map[string]any{"retry_after": retryAfterSeconds}})
		s.unlock()
		<-c.done
		return
	}
	s.clients[c] = struct{}{}
	s.seenBaseURL = c.baseURL
	// The server announcement is queued before the reader starts accepting auth.
	c.enqueue(s.serverFrameLocked(c))
	s.unlock()

	defer func() {
		c.stopConnection()
		s.mu.Lock()
		delete(s.clients, c)
		s.detachLocked(c)
		s.unlock()
	}()

	// Frames are processed in order, each to completion before the next,
	// which makes auth a barrier (§3.2), by a worker of the connection's
	// own: the reader goes on reading meanwhile, which keeps the WebSocket's
	// pings answered while an extension's callback runs. The worker's
	// requests get a context that the connection's end cancels.
	ctx, cancel := context.WithCancel(r.Context())
	frames := make(chan []byte, inboundQueue)
	worked := make(chan struct{})
	go func() {
		defer close(worked)
		c.request = ctx
		for payload := range frames {
			if ctx.Err() == nil {
				s.processFrame(c, payload)
			}
		}
	}()
	defer func() {
		cancel()
		close(frames)
		<-worked
	}()
	for {
		messageType, payload, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			_ = ws.Close(websocket.StatusUnsupportedData, "text frames required")
			return
		}
		c.lastFrame.Store(time.Now().UnixNano())
		// The liveness ping has fixed bytes, answered without parsing or
		// waiting for the frames before it (§1).
		if bytes.Equal(payload, pingFrame) {
			c.pong()
			continue
		}
		select {
		case frames <- payload:
		case <-c.done:
			return
		}
	}
}

// inboundQueue bounds the frames read from a connection and waiting for its
// worker; a client that sends more waits for them.
const inboundQueue = 64

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
func (s *Server) serverFrameLocked(c *client) jsontext.Value {
	return notification("server", s.serverParams(c.baseURL, s.revealedLocked(c)))
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
// whose URLs start with baseURL, whole when revealed. Callers hold s.mu.
func (s *Server) serverParams(baseURL string, revealed bool) map[string]any {
	var authSchemes []string
	if s.config.WebAuthn != nil {
		authSchemes = append(authSchemes, "webauthn")
	}
	if s.config.EmailSender != nil {
		authSchemes = append(authSchemes, "email")
	}
	if s.external != nil {
		authSchemes = append(authSchemes, "external")
	}
	if len(authSchemes) > 0 {
		authSchemes = append(authSchemes, "token")
	}
	if s.guestsAllowedLocked() {
		authSchemes = append(authSchemes, "guest")
	}
	// signup (§3.2) lists the schemes that can create an account: every
	// one but token, which only resumes one. A passkey registered by a
	// guest makes the guest an account, and email and guest start one.
	signup := slices.DeleteFunc(slices.Clone(authSchemes), func(scheme string) bool { return scheme == "token" })
	params := map[string]any{
		"apron":        10,
		"agent":        "apron-go/10",
		"capabilities": []string{"history", "edit", "rooms", "reactions", "activity", "embed:upload", "embed:append", "embed:terminal", "command", "status", "ext"},
		"auth":         authSchemes,
		"ping":         max(1, int(s.config.PingInterval/time.Second)),
		// The optional statuses accepted with `me` (§4.5); online and ""
		// always are, and are not listed.
		"status": optionalStatuses,
	}
	ext := make(map[string]any)
	if emoji := s.emojiExt(baseURL); emoji != nil && revealed {
		params["capabilities"] = append(params["capabilities"].([]string), "ext:emoji")
		ext["emoji"] = emoji
	}
	if !s.config.DisablePush {
		push := map[string]any{"relay": map[string]any{}, "wake": (urgentScopes | wakeJoined | wakeBadge).names()}
		if s.vapid != nil {
			push["webpush"] = map[string]any{"key": s.vapid.public}
		}
		params["push"] = push
	}
	if s.config.Welcome != "" {
		params["welcome"] = s.config.Welcome
	}
	for _, field := range profileFields {
		if value, source := s.profileValueLocked(field); value != "" && (revealed || source != fromRoom) {
			params[field] = value
		}
	}
	if s.config.MaxPrivateRooms > 0 {
		// ext:settings (apron's EXTENSIONS.md): read_cursors is absent,
		// so true, since the server keeps read_message_id (§4.6).
		params["capabilities"] = append(params["capabilities"].([]string), "ext:settings")
		ext["settings"] = map[string]any{"private_rooms": s.config.MaxPrivateRooms}
	}
	// Each extension that advertises (Host.Advertise) adds its capability,
	// ext:<name>, and its data under its name in ext (EXTENSIONS.md).
	for _, name := range slices.Sorted(maps.Keys(s.advertised)) {
		if !revealed {
			break
		}
		params["capabilities"] = append(params["capabilities"].([]string), "ext:"+name)
		if value := s.advertised[name]; len(value) > 0 {
			ext[name] = value
		}
	}
	if len(ext) > 0 {
		params["ext"] = ext
	}
	if !slices.Equal(signup, authSchemes) {
		params["signup"] = signup
	}
	return params
}

func (c *client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case batch := <-c.out:
			for _, payload := range batch.frames {
				select {
				case <-c.done:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), c.server.config.WriteTimeout)
				err := c.conn.write(ctx, payload)
				cancel()
				if err != nil {
					c.stopConnection()
					return
				}
			}
			if batch.closeReason != "" {
				// The error frame explains the close; a close handshake would
				// wait on a peer that may not be reading.
				c.stopConnection()
				return
			}
		}
	}
}

// pingLoop pings at the WebSocket level, which finds dead transports, and
// closes a connection whose client sent liveness pings (§1) and then fell
// silent for three intervals: its page is frozen or gone even if the socket
// is not. Three intervals leave room for background timer throttling.
func (c *client) pingLoop() {
	config := c.server.config
	ticker := time.NewTicker(config.PingInterval)
	defer ticker.Stop()
	silence := 3*config.PingInterval + config.PingTimeout
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if c.pinged.Load() && time.Since(time.Unix(0, c.lastFrame.Load())) > silence {
				c.stopConnection()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), config.PingTimeout)
			err := c.conn.ping(ctx)
			cancel()
			if err != nil {
				c.stopConnection()
				return
			}
		}
	}
}

// pong answers a liveness ping, before authentication too (§1).
func (c *client) pong() {
	c.pinged.Store(true)
	c.enqueue(pongFrame)
}

func (c *client) stopConnection() {
	c.stop.Do(func() {
		close(c.done)
		c.conn.close()
	})
}

// closeWithError sends an error about the connection as a whole, without id
// (PROTOCOL.md §1.1), after the frames already queued, then closes the
// connection.
func (c *client) closeWithError(e *Error) {
	payload := encodeJSON(errorResponse(nil, e))
	if payload == nil || !c.closing.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.out <- outboundBatch{frames: [][]byte{payload}, closeReason: e.Message}:
	default:
		c.stopConnection()
	}
}

func (c *client) enqueue(value any) bool {
	return c.enqueueBatch(value)
}

func (c *client) enqueueBatch(values ...any) bool {
	if len(values) == 0 {
		return true
	}
	if c.closing.Load() {
		return false
	}
	batch := outboundBatch{frames: make([][]byte, 0, len(values))}
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
		batch.frames = append(batch.frames, payload)
	}
	select {
	case <-c.done:
		return false
	case c.out <- batch:
		return true
	default:
		// A slow client cannot hold the room lock or the server's broadcast path.
		c.stopConnection()
		return false
	}
}

func (c *client) sendResult(req request, result any) {
	c.enqueue(response(req.id, result))
}

func (c *client) sendError(req request, err *Error) {
	c.enqueue(errorResponse(req.id, err))
}

// operation is a request handler. When it succeeds and has already queued its
// own reply, it reports replied. Handlers whose results describe state queue
// them while holding s.mu, so that a result reflects every notification sent
// before it on the connection, and after the notifications the request
// causes, which precede its result (§1).
type operation func(s *Server, c *client, req request) (result any, replied bool, err *Error)

func (s *Server) operations() map[string]operation {
	ops := map[string]operation{
		"me":         (*Server).updateProfile,
		"message":    (*Server).saveMessage,
		"command":    (*Server).command,
		"history":    (*Server).history,
		"room_set":   (*Server).setRoom,
		"room_list":  (*Server).listRooms,
		"room_join":  (*Server).joinRoom,
		"room_leave": (*Server).leaveRoom,
		"reactions":  (*Server).react,
		"activity":   (*Server).activity,
		"status":     (*Server).status,
	}
	if !s.config.DisablePush {
		ops["push_register"] = (*Server).registerPush
		ops["push_unregister"] = (*Server).unregisterPush
	}
	return ops
}

// readOnlyOps change nothing, so their results are not kept for
// deduplication (§1.2): a duplicate after the original finished runs again.
var readOnlyOps = map[string]bool{"history": true, "room_list": true}

func (s *Server) processFrame(c *client, payload []byte) {
	req, parseErr := parseRequest(payload)
	if parseErr != nil {
		if notificationOnly[req.method] {
			// The id of a notification method may be ignored, and the frame
			// handled as a notification (§1.1): it is never answered, even
			// with invalid params, and a ping is still a ping.
			if req.method == "ping" {
				c.pong()
			}
			return
		}
		if parseErr.Code == CodeInvalidParams && !req.hasID {
			return
		}
		if req.hasID {
			c.sendError(req, parseErr)
		} else {
			c.enqueue(errorResponse(nil, parseErr))
		}
		return
	}

	// parseRequest drops the id of a notification method, which a server
	// may ignore, handling the frame as a notification (§1.1): activity and
	// ping are never answered.
	if req.method == "ping" {
		// A ping is answered before sign-in too (§3.2), and one with other
		// spacing or keys is still a ping.
		c.pong()
		return
	}
	// Clients send every other method they send as a request, with an id.
	// A server may ignore a request method sent without one (§1.1), and
	// this one does, whatever its method and params, before sign-in or
	// after: guest auth and status alike.
	if !req.hasID && !notificationOnly[req.method] {
		return
	}
	// auth is a barrier (§3.2): the read loop processes one frame at a time,
	// so auth finishes before any later frame on the connection is read.
	// Requests pipelined behind a failed auth, or behind a WebAuthn begin
	// step, find no identity below and are denied.
	if req.method == "auth" {
		if _, err := s.authenticate(c, req); err != nil {
			c.sendError(req, err)
		}
		return
	}

	s.mu.Lock()
	user := c.user
	if user == nil {
		s.unlock()
		if req.hasID {
			c.sendError(req, &Error{Code: CodeDenied, Message: "Sign in before sending requests"})
		}
		return
	}
	op := s.ops[req.method]
	if op == nil {
		s.unlock()
		if req.hasID {
			c.sendError(req, &Error{Code: CodeUnsupported, Message: "Unsupported method " + strconv.Quote(req.method)})
		}
		return
	}
	// Requests deduplicate per user (§1.2): a duplicate waits for the original,
	// even one running on another connection, and replies with its outcome,
	// brought up to date with the current state.
	var entry *dedupEntry
	if req.hasID && len(req.id) > maxRequestIDBytes {
		// Request ids are any string (§2.4), but each is kept for
		// deduplication, so their size is bounded.
		s.unlock()
		c.sendError(req, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("A request id is at most %d bytes", maxRequestIDBytes)})
		return
	}
	if req.hasID {
		fingerprint := sha256.Sum256([]byte(requestFingerprint(req)))
		if prior := user.dedup.get(req.id); prior != nil {
			s.unlock()
			if prior.fingerprint != fingerprint {
				c.sendError(req, invalidParams("Request id %q was already used for another operation", req.id))
				return
			}
			<-prior.done
			if prior.err != nil {
				c.sendError(req, prior.err)
			} else {
				s.mu.Lock()
				result := s.currentResultLocked(user, req.method, prior.result)
				c.sendResult(req, result)
				s.unlock()
			}
			return
		}
		entry = &dedupEntry{fingerprint: fingerprint, done: make(chan struct{})}
		user.dedup.put(req.id, entry)
	}
	s.unlock()

	result, replied, err := op(s, c, req)
	if req.hasID && !replied {
		if err != nil {
			c.sendError(req, err)
		} else {
			c.sendResult(req, result)
		}
	}
	if entry != nil {
		s.mu.Lock()
		entry.result, entry.err = result, err
		if err != nil || readOnlyOps[req.method] {
			// Failed operations are not cached, and neither are reads, whose
			// results can be large and which are safe to run again: a retry
			// executes again. Concurrent duplicates still share the outcome.
			user.dedup.remove(req.id, entry)
		}
		close(entry.done)
		s.unlock()
	}
}

// currentResultLocked brings a duplicate's result up to date (§1.2): `me`
// answers with the current profile, and a message result lists only the
// write URLs that are still unused. Other results name what the request
// made, which does not change.
func (s *Server) currentResultLocked(u *userState, method string, result any) any {
	switch method {
	case "me":
		return map[string]any{"you": u.you()}
	case "message", "command":
		original, ok := result.(map[string]any)
		if !ok || original["embeds"] == nil {
			return result
		}
		current := maps.Clone(original)
		var writes []any
		for _, value := range asList(original["embeds"]) {
			written, _ := value.(map[string]any)
			id, _ := written["embed_id"].(string)
			if e := s.embeds[id]; e != nil && !e.started && !e.removed && s.writes[e.token] == e {
				writes = append(writes, written)
			}
		}
		if len(writes) == 0 {
			delete(current, "embeds")
		} else {
			current["embeds"] = writes
		}
		return current
	}
	return result
}

// nextIDLocked returns the next log_id in the server-wide sequence: the commit
// time in milliseconds, or the previous log_id + 1.
func (s *Server) nextIDLocked() int64 {
	s.lastID = max(time.Now().UnixMilli(), s.lastID+1)
	return s.lastID
}

// appendLocked adds a committed record to the log of each room it belongs to.
func (s *Server) appendLocked(record *logRecord, rooms ...*roomState) {
	// log_ids rise, so a record almost always goes last.
	i := len(s.records)
	if i > 0 && s.records[i-1].id > record.id {
		i, _ = slices.BinarySearchFunc(s.records, record.id, compareLogID)
	}
	s.records = slices.Insert(s.records, i, record)
	close(s.recordsChanged)
	s.recordsChanged = make(chan struct{})
	for _, r := range rooms {
		r.log = append(r.log, record)
		r.latestID = record.id
		record.rooms = append(record.rooms, r.id)
		s.touchRoom(r)
	}
	s.touchRecord(record)
}

var _ http.Handler = (*Server)(nil)
