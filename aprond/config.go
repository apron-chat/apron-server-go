package aprond

import (
	"log/slog"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/apron-chat/apron-server-go/apron/server"
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
	maxRequestIDBytes = server.MaxRequestIDBytes
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
	// AddCredentialWithoutReauth lets a connection add a passkey to an
	// account that has one without a recent passkey sign-in on it. By
	// default it needs one, as removing a passkey does: otherwise a stolen
	// session token could add a passkey of its own, sign in with it, and
	// remove the owner's.
	AddCredentialWithoutReauth bool
	// SignalUnknownPasskeys has the passkey page say that no account has a
	// passkey, when none has nor may yet have it, and ask the browser to
	// hide it (signalUnknownCredential). Off, such a login gets the page's
	// usual error, the same as a known passkey that fails: the distinct
	// answer tells anyone who posts a made-up assertion whether a
	// credential ID is registered, which is worth it only for the signal.
	// A server knows only its own accounts, so turn it on only where it is
	// the sole user of its RP ID: no parent-domain RP ID shared with other
	// services, and no other deployment on the same RP ID with storage of
	// its own.
	//
	// FIXME: keep off until Apple fixes
	// https://bugs.webkit.org/show_bug.cgi?id=302624 and
	// https://bugs.webkit.org/show_bug.cgi?id=322267 (signalUnknownCredential
	// can delete other accounts' passkeys on iOS/macOS 26). Check again on
	// 2027-02-16. Turn on only where this server is the sole user of its RP
	// ID.
	SignalUnknownPasskeys bool
	// GuestFirstPasskeys has a passkey registration make an account only
	// of a signed-in guest, or with an invitation, and leaves webauthn out
	// of server.signup, so clients sign in as a guest first (§3.2). By
	// default, where guests are allowed, a registration on a connection
	// that is not signed in makes a new account itself, with the checks of
	// any new account: anyone may make one as a guest anyway, so the guest
	// step adds a round trip and no protection. Without guests
	// (DisableGuests, or sign-in an extension restricts), only an
	// invitation makes one either way.
	GuestFirstPasskeys bool
	// OnStoreError, if set, is called once, on a goroutine of its own, with
	// the first error the store returns. The server then
	// writes nothing more and fails Sync (see Server.Sync); the program
	// usually shuts it down.
	OnStoreError func(error)
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
	// role lists who holds it, as ParsePrincipal reads them, such as
	// email:ada@example.com or external:github:583231, or by a bare user_id
	// or email address, which names the account with that user_id, and with
	// that address when it is one. Open refuses a holder that is none of
	// these. Roles are
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
	// ExternalIssuer names another OAuth authorization server for external
	// sign-in (§4.12), such as an OpenID Connect provider, instead of an
	// extension's sign-in on the server's own page; nil for none. Then no
	// extension may offer external sign-in.
	ExternalIssuer *ExternalIssuer
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
