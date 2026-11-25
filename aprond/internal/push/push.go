// Package push keeps users' push registrations (PROTOCOL.md §4.9): each
// endpoint's kind, url, token or keys, push_id, and wake scopes, the
// sign-in that registered it, and when it was last registered, which it
// expires a while after. A Table finds the registrations by user, tracks
// the ones changed, and keeps them in the store. Each registration also
// keeps what its deliveries left: the unread count its endpoint last
// accepted, and the push in flight, so a badge push goes out only when the
// count differs; and a Day counts the pushes a user's endpoints accepted
// on one UTC day. A Table also schedules each user's badge pushes, after
// BadgeDelay, which it hands back to the server to send.
//
// Delivery itself is webpush's, and who is woken, and the unread counts,
// the server's. A Table is not safe for concurrent use: the server calls
// it holding its lock.
package push

import (
	"cmp"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/apron-chat/apron-server-go/aprond/internal/webpush"
	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

const (
	// EntryKind is the kind of the store entries that hold registrations,
	// named by Key, or by the url alone before registrations belonged to
	// their user.
	EntryKind = "push"
	// MaxPerUser bounds one user's registrations.
	MaxPerUser = 10
	// Expiry is how long a registration lasts unless registered again;
	// clients register on each connection.
	Expiry = 30 * 24 * time.Hour
)

// Wake is a set of wake scopes (§4.9).
type Wake uint8

const (
	Mentions Wake = 1 << iota
	Replies
	Private
	Joined
	Badge
	// DefaultWake is the scopes of a registration without wake.
	DefaultWake = Mentions | Replies
	// Urgent are the scopes that push with Urgency high; joined is normal,
	// badge low.
	Urgent = Mentions | Replies | Private
)

// wakeNames lists the scopes this server implements, in the order
// server.push.wake advertises them.
var wakeNames = []struct {
	name  string
	scope Wake
}{{"mentions", Mentions}, {"replies", Replies}, {"private", Private}, {"joined", Joined}, {"badge", Badge}}

// Names lists the scopes in a set, in advertised order.
func (w Wake) Names() []string {
	names := []string{}
	for _, scope := range wakeNames {
		if w&scope.scope != 0 {
			names = append(names, scope.name)
		}
	}
	return names
}

// ParseWake reads a wake list, ignoring scopes this server does not
// implement (§4.9).
func ParseWake(names []string) Wake {
	var wake Wake
	for _, name := range names {
		for _, scope := range wakeNames {
			if scope.name == name {
				wake |= scope.scope
			}
		}
	}
	return wake
}

// Registration is one push endpoint of a user (§4.9), identified by the
// user and its url.
type Registration struct {
	// User is the user_id of the user it pushes to.
	User string
	Kind string
	URL  string
	// Token is a relay's bearer token.
	Token  string
	PushID string
	// Device is the key of the /invite-device token the registering
	// connection signed in with, zero for none, so replacing that token
	// removes only the registrations it made.
	Device [32]byte
	// Session is the key of the session token the registering connection
	// signed in or resumed with, zero for none, so ending that session
	// removes the registrations it made.
	Session [32]byte
	// Keys, for webpush and optionally relay, encrypt the payload.
	Keys *webpush.Keys
	Wake Wake
	// Renewed is when the client last registered it; it expires Expiry
	// later.
	Renewed time.Time

	// lastUnread is the unread count the endpoint last accepted, -1 when
	// none was since the server started. sent numbers the pushes handed to
	// the deliverer and accepted the latest of them the endpoint accepted,
	// so a push accepted late does not record an older count.
	// pendingUnread is the count of the latest push sent, while inFlight.
	lastUnread    int
	sent          uint64
	accepted      uint64
	pendingUnread int
	inFlight      bool
}

// New returns a registration as p has it, which no push was sent to yet.
func New(p Registration) *Registration {
	p.lastUnread, p.sent, p.accepted, p.pendingUnread, p.inFlight = -1, 0, 0, 0, false
	return &p
}

// Key identifies a registration: its user and url, which holds no space.
func Key(userID, url string) string { return userID + " " + url }

// Live reports whether the registration has not expired at now.
func (p *Registration) Live(now time.Time) bool {
	return now.Before(p.Renewed.Add(Expiry))
}

// TakesBadges reports whether the registration gets badge pushes: it has
// scope badge, which webpush ignores (§4.9).
func (p *Registration) TakesBadges() bool {
	return p.Wake&Badge != 0 && p.Kind != "webpush"
}

// Target is where the deliverer sends the registration's pushes.
func (p *Registration) Target() webpush.Target {
	return webpush.Target{UserID: p.User, Kind: p.Kind, URL: p.URL, Token: p.Token, Keys: p.Keys}
}

// Sending notes a push carrying the count unread handed to the deliverer,
// and returns its number, for Delivered.
func (p *Registration) Sending(unread int) uint64 {
	p.sent++
	p.pendingUnread, p.inFlight = unread, true
	return p.sent
}

// Delivered notes the outcome of push seq, which carried the count
// unread: accepted, when the endpoint accepted it, records the count as
// the endpoint's, unless it accepted a later push already.
func (p *Registration) Delivered(seq uint64, unread int, accepted bool) {
	if seq == p.sent {
		p.inFlight = false
	}
	if accepted && seq > p.accepted {
		p.accepted, p.lastUnread = seq, unread
	}
}

// WantsBadge reports whether a badge push of the count unread would tell
// the endpoint anything: it last accepted another count, and no push of
// unread is in flight.
func (p *Registration) WantsBadge(unread int) bool {
	return unread != p.lastUnread && !(p.inFlight && unread == p.pendingUnread)
}

// Day counts the pushes a user's endpoints accepted on one UTC day.
type Day struct {
	day   string
	count int
}

// Count is how many pushes were accepted on now's UTC day.
func (d *Day) Count(now time.Time) int {
	if day := now.UTC().Format(time.DateOnly); day != d.day {
		d.day, d.count = day, 0
	}
	return d.count
}

// Accepted counts a push accepted at now.
func (d *Day) Accepted(now time.Time) {
	d.Count(now)
	d.count++
}

// Table is the registrations, by user and url.
type Table struct {
	byKey  map[string]*Registration
	byUser map[string]map[string]*Registration
	// dirty are the keys added or removed since Entries last took them.
	dirty  map[string]struct{}
	badges badges
}

// NewTable returns an empty table, which calls due, from the timer's
// goroutine, when a badge push it scheduled (ScheduleBadge) is due.
func NewTable(due func(userID string, b *PendingBadge)) *Table {
	return &Table{
		byKey: make(map[string]*Registration), byUser: make(map[string]map[string]*Registration), dirty: make(map[string]struct{}),
		badges: badges{delay: BadgeDelay, pending: make(map[string]*PendingBadge), due: due},
	}
}

// Len counts the registrations.
func (t *Table) Len() int { return len(t.byKey) }

// Get returns userID's registration of url, or nil.
func (t *Table) Get(userID, url string) *Registration { return t.byUser[userID][url] }

// Count counts userID's registrations.
func (t *Table) Count(userID string) int { return len(t.byUser[userID]) }

// Of lists userID's registrations by url, so deliveries go out in a stable
// order.
func (t *Table) Of(userID string) []*Registration {
	registrations := t.byUser[userID]
	return slices.SortedFunc(maps.Values(registrations), func(a, b *Registration) int {
		return cmp.Compare(a.URL, b.URL)
	})
}

// Oldest returns userID's registration least recently registered, or nil.
func (t *Table) Oldest(userID string) *Registration {
	var oldest *Registration
	for _, p := range t.byUser[userID] {
		if oldest == nil || p.Renewed.Before(oldest.Renewed) {
			oldest = p
		}
	}
	return oldest
}

// Expired lists the registrations expired at now.
func (t *Table) Expired(now time.Time) []*Registration {
	var expired []*Registration
	for _, key := range slices.Sorted(maps.Keys(t.byKey)) {
		if p := t.byKey[key]; !p.Live(now) {
			expired = append(expired, p)
		}
	}
	return expired
}

// Add keeps p, replacing its user's registration of the same url.
func (t *Table) Add(p *Registration) {
	key := Key(p.User, p.URL)
	t.byKey[key] = p
	if t.byUser[p.User] == nil {
		t.byUser[p.User] = make(map[string]*Registration)
	}
	t.byUser[p.User][p.URL] = p
	t.dirty[key] = struct{}{}
}

// Remove forgets p, if it is still the one kept, and reports whether it
// was. A user left without registrations has no badge push pending.
func (t *Table) Remove(p *Registration) bool {
	key := Key(p.User, p.URL)
	if t.byKey[key] != p {
		return false
	}
	delete(t.byKey, key)
	delete(t.byUser[p.User], p.URL)
	if len(t.byUser[p.User]) == 0 {
		delete(t.byUser, p.User)
		t.cancelBadge(p.User)
	}
	t.dirty[key] = struct{}{}
	return true
}

// stored is a registration as the store keeps it.
type stored struct {
	User   string `json:"user"`
	Kind   string `json:"kind"`
	URL    string `json:"url,omitzero"`
	Token  string `json:"token,omitzero"`
	PushID string `json:"push_id,omitzero"`
	// Device is the hex SHA-256 of the /invite-device token the registering
	// connection signed in with.
	Device string `json:"device,omitzero"`
	// Session is the hex SHA-256 of the session token it signed in with.
	Session string `json:"session,omitzero"`
	// P256DH and Auth are the subscription's keys, in base64url.
	P256DH string `json:"p256dh,omitzero"`
	Auth   string `json:"auth,omitzero"`
	// Wake lists the registration's scopes.
	Wake    []string  `json:"wake"`
	Renewed time.Time `json:"renewed,omitzero"`
}

// Entries returns the store entries of the registrations added or removed
// since it was last called.
func (t *Table) Entries() []store.Entry {
	if len(t.dirty) == 0 {
		return nil
	}
	entries := make([]store.Entry, 0, len(t.dirty))
	for key := range t.dirty {
		entry := store.Entry{Kind: EntryKind, ID: key}
		if p := t.byKey[key]; p != nil {
			s := stored{
				User: p.User, Kind: p.Kind, URL: p.URL, Token: p.Token, PushID: p.PushID,
				Wake: p.Wake.Names(), Renewed: p.Renewed,
			}
			if p.Device != [32]byte{} {
				s.Device = hex.EncodeToString(p.Device[:])
			}
			if p.Session != [32]byte{} {
				s.Session = hex.EncodeToString(p.Session[:])
			}
			if p.Keys != nil {
				s.P256DH, s.Auth = webpush.EncodeBase64URL(p.Keys.P256DH), webpush.EncodeBase64URL(p.Keys.Auth)
			}
			value, err := wire.Marshal(s)
			if err != nil {
				slog.Error("cannot encode state for the store", "kind", EntryKind, "id", key, "error", err)
				continue
			}
			entry.Value = value
		}
		entries = append(entries, entry)
	}
	clear(t.dirty)
	return entries
}

// Snapshot returns the store entries of every registration, as Entries does
// those that changed.
func (t *Table) Snapshot() []store.Entry {
	for key := range t.byKey {
		t.dirty[key] = struct{}{}
	}
	return t.Entries()
}

// Restore takes the stored registrations that keep accepts, such as those
// of a user that returns and has not expired, and deletes the others from
// the store. A registration stored by its url alone is kept by Key, and
// the entry of its url stays.
func (t *Table) Restore(entries map[string]jsontext.Value, keep func(*Registration) bool) error {
	for id, raw := range entries {
		var s stored
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
		}
		p := New(Registration{
			User: s.User, Kind: s.Kind, URL: s.URL, Token: s.Token, PushID: s.PushID,
			Wake: ParseWake(s.Wake), Renewed: s.Renewed,
		})
		if s.P256DH != "" {
			keys, err := webpush.ParseKeys(s.P256DH, s.Auth)
			if err != nil {
				return fmt.Errorf("stored %s %q: %w", EntryKind, id, err)
			}
			p.Keys = &keys
		}
		if device, err := hex.DecodeString(s.Device); err == nil && len(device) == 32 {
			p.Device = [32]byte(device)
		}
		if session, err := hex.DecodeString(s.Session); err == nil && len(session) == 32 {
			p.Session = [32]byte(session)
		}
		if keep(p) {
			t.Add(p)
		} else {
			t.dirty[id] = struct{}{}
		}
	}
	return nil
}
