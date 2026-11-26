// Package proposals keeps the pending sign-ins that finish out of band
// (PROTOCOL.md §4.11), such as by email: each proposal's short code, which
// works on its own connection or with its state (§4.11.2), its link token,
// which works on any connection that may approve it (§4.11.3), and the
// failed attempts that cancel it (§4.11.1).
//
// A proposal holds neither its connection nor an account, so that a closed
// connection, and a retired guest, are not kept for it: the connection
// holds its latest proposal instead, and learns from Live that it was
// dropped. Proposals are kept in memory only.
//
// A Table is not safe for concurrent use: the server calls it holding its
// lock.
package proposals

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
	"time"

	"github.com/apron-chat/apron-server-go/internal/authn"
)

const (
	// Lifetime is how long a proposal and its tokens stay valid.
	Lifetime = 10 * time.Minute
	// MaxAttempts wrong tokens, on any connection, cancel a proposal.
	MaxAttempts = 5
	// HandleBytes starts each link token with a handle naming its
	// proposal, so a wrong secret behind it counts against that proposal
	// from any connection.
	HandleBytes = 10
	// maxLinks bounds the outstanding link tokens.
	maxLinks = 10_000
	// maxStates bounds the outstanding proposals that carry a state.
	maxStates = 10_000
)

// Proposal is a pending sign-in: to sign in, or to add an identity to the
// proposing account. It outlives its connection's disconnect, until it
// expires, is approved, or is dropped.
type Proposal struct {
	// Scheme proves its identity, and Proof is what approving it proves.
	Scheme authn.Scheme
	Proof  *authn.Proof
	// Add is set for an addition to the account whose user_id is
	// AccountID. User_ids are never reissued, so only a connection signed
	// in to that account has it.
	Add       bool
	AccountID string
	// Code is the short code to type.
	Code string
	// State is the client's, returned in the link, and which lets its code
	// approve it on another connection.
	State   string
	Expires time.Time
	// Name and UserID are what the proposing connection requested for a
	// new account.
	Name, UserID string
	// Server is the WebSocket URL a link names.
	Server string

	// handle and secret make up the link token; both are empty when no
	// link was made.
	handle, secret string
	attempts       int
	// dropped reports that the proposal was forgotten.
	dropped bool
}

// Live reports whether the proposal was neither dropped nor expired at now.
func (p *Proposal) Live(now time.Time) bool {
	return !p.dropped && now.Before(p.Expires)
}

// Table is the outstanding proposals' link tokens, by handle, and the
// proposals that carry a state, by stateKey: the latest proposal with each
// state.
type Table struct {
	links  map[string]*Proposal
	states map[string]*Proposal
}

// NewTable returns an empty table.
func NewTable() *Table {
	return &Table{links: make(map[string]*Proposal), states: make(map[string]*Proposal)}
}

// stateKey indexes a proposal's state, hashed so that finding it takes no
// time that depends on the state.
func stateKey(state string) string {
	sum := sha256.Sum256([]byte(state))
	return string(sum[:])
}

// Room reports whether the table has room for another proposal with a
// link, if link, and with a state, if state, forgetting expired proposals
// in a full table first.
func (t *Table) Room(link, state bool, now time.Time) bool {
	return (!link || t.prune(t.links, maxLinks, now)) && (!state || t.prune(t.states, maxStates, now))
}

// prune forgets expired proposals in a full table, and reports whether it
// has room now.
func (t *Table) prune(table map[string]*Proposal, limit int, now time.Time) bool {
	if len(table) < limit {
		return true
	}
	for _, p := range table {
		if !now.Before(p.Expires) {
			t.Drop(p)
		}
	}
	return len(table) < limit
}

// Offer makes p, whose Code is set, approvable: with its state, if it has
// one, as the latest proposal with that state, and with a link token, if
// link, which it returns.
func (t *Table) Offer(p *Proposal, link bool) (token string) {
	if p.State != "" {
		// The latest proposal with a state is the one it names; an earlier
		// one, of another connection, keeps its code there and its link.
		t.states[stateKey(p.State)] = p
	}
	if !link {
		return ""
	}
	p.handle, p.secret = t.newHandle(), rand.Text()
	t.links[p.handle] = p
	return p.handle + p.secret
}

// newHandle returns a link handle no outstanding link has.
func (t *Table) newHandle() string {
	for {
		if handle := rand.Text()[:HandleBytes]; t.links[handle] == nil {
			return handle
		}
	}
}

// Drop forgets a proposal: its link, and its state.
func (t *Table) Drop(p *Proposal) {
	p.dropped = true
	if p.handle != "" && t.links[p.handle] == p {
		delete(t.links, p.handle)
	}
	if p.State != "" {
		if key := stateKey(p.State); t.states[key] == p {
			delete(t.states, key)
		}
	}
}

// Names reports whether token is one that Approve would check against a
// proposal: own's, if any, the one its link handle names, or the one
// state names. A wrong token then counts as a failed attempt under any
// scheme; any other token is no temporary token.
func (t *Table) Names(own *Proposal, token, state string) bool {
	return own != nil || len(token) > HandleBytes && t.links[token[:HandleBytes]] != nil ||
		state != "" && t.states[stateKey(state)] != nil
}

// Approve returns the live proposal that token approves: own, a live
// proposal of the connection presenting it, by its code; any, by its code
// with its state (§4.11.2), which the state proves the proposer's client
// presents; or any, by its link token. Otherwise it returns nil, and
// counts a failed attempt against the proposal that token names by its
// link handle, else against the one state names, else against own,
// whichever connection presents it (§4.11.1): MaxAttempts of them drop it.
// Expired proposals are dropped.
func (t *Table) Approve(own *Proposal, token, state string, now time.Time) *Proposal {
	if own != nil && own.Code != "" && subtle.ConstantTimeCompare([]byte(token), []byte(own.Code)) == 1 {
		return own
	}
	target := own
	if p := t.stated(state, now); p != nil {
		if p.Code != "" && subtle.ConstantTimeCompare([]byte(token), []byte(p.Code)) == 1 {
			return p
		}
		target = p
	}
	if len(token) > HandleBytes {
		if p := t.links[token[:HandleBytes]]; p != nil {
			if !now.Before(p.Expires) {
				t.Drop(p)
				return nil
			}
			if subtle.ConstantTimeCompare([]byte(token[HandleBytes:]), []byte(p.secret)) == 1 {
				return p
			}
			target = p
		}
	}
	if target != nil {
		if target.attempts++; target.attempts >= MaxAttempts {
			t.Drop(target)
		}
	}
	return nil
}

// stated returns the live proposal whose state is state, if any, and drops
// it if it expired.
func (t *Table) stated(state string, now time.Time) *Proposal {
	if state == "" {
		return nil
	}
	p := t.states[stateKey(state)]
	if p == nil || subtle.ConstantTimeCompare([]byte(state), []byte(p.State)) != 1 {
		return nil
	}
	if !now.Before(p.Expires) {
		t.Drop(p)
		return nil
	}
	return p
}

// Links lists the proposals that have link tokens, in no order.
func (t *Table) Links() []*Proposal { return values(t.links) }

// States lists the proposals that a state names, in no order.
func (t *Table) States() []*Proposal { return values(t.states) }

func values(table map[string]*Proposal) []*Proposal {
	list := make([]*Proposal, 0, len(table))
	for _, p := range table {
		list = append(list, p)
	}
	return list
}

// Code returns a code to type: six digits.
func Code() string {
	number, _ := rand.Int(rand.Reader, big.NewInt(1_000_000)) // never fails (crypto/rand)
	return fmt.Sprintf("%06d", number)
}
