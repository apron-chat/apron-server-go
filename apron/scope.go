package apron

import (
	"context"
	"errors"
	"maps"
	"slices"
)

// Room scopes. An account with a scope, such as a bot that should reach
// only the rooms it is assigned, sees only the rooms its scope lists, and
// their threads if it says so. Seeing is the server's one check of who may
// read and write a room (roomState.visibleTo): history, room_list, joins,
// posts, reactions, threads and their records, unread counts, push, live
// text, and Host.Visible all go through it, for a client's connection, a
// bot's in-process one, and one signed in with a service token alike. A
// scoped account makes no top-level room, and a thread only when its scope
// takes threads in.

// RoomScope limits an account to some rooms.
type RoomScope struct {
	// Rooms are the rooms the account may see, by room_id. A room made
	// later is outside it until it is added.
	Rooms []string
	// Threads takes in the threads of those rooms, at any depth, and lets
	// the account start one there. Otherwise a thread is outside the scope
	// unless it is listed itself.
	Threads bool
}

// roomScope is a RoomScope as the server checks it. Guarded by s.mu.
type roomScope struct {
	rooms   map[string]bool
	threads bool
}

type storedScope struct {
	Rooms   []string `json:"rooms"`
	Threads bool     `json:"threads,omitzero"`
}

func newRoomScope(scope *RoomScope) *roomScope {
	if scope == nil {
		return nil
	}
	sc := &roomScope{rooms: make(map[string]bool, len(scope.Rooms)), threads: scope.Threads}
	for _, id := range scope.Rooms {
		sc.rooms[id] = true
	}
	return sc
}

func (sc *roomScope) public() *RoomScope {
	if sc == nil {
		return nil
	}
	return &RoomScope{Rooms: slices.Sorted(maps.Keys(sc.rooms)), Threads: sc.threads}
}

func (sc *roomScope) stored() *storedScope {
	if sc == nil {
		return nil
	}
	return &storedScope{Rooms: slices.Sorted(maps.Keys(sc.rooms)), Threads: sc.threads}
}

func (sc *storedScope) restored() *roomScope {
	if sc == nil {
		return nil
	}
	return newRoomScope(&RoomScope{Rooms: sc.Rooms, Threads: sc.Threads})
}

// allows reports whether r is in the scope: listed, or a thread of a listed
// room when the scope takes threads in.
func (sc *roomScope) allows(r *roomState) bool {
	if sc == nil {
		return true
	}
	for room := r; room != nil; room = room.parent {
		if sc.rooms[room.id] {
			return true
		}
		if !sc.threads {
			return false
		}
	}
	return false
}

// SetScope limits an account to the rooms scope lists, or, with nil, lifts
// its limit, and returns once that is stored. The account leaves the rooms
// and threads it is in that the scope leaves out, and its writes into them
// end. It does not join the rooms the scope lists; a bot joins them with
// Bot.Join.
func (s *Server) SetScope(ctx context.Context, userID string, scope *RoomScope) error {
	s.mu.Lock()
	u := s.users[userID]
	if u == nil {
		s.unlock()
		return errors.New("apron: no such user")
	}
	s.setScopeLocked(u, newRoomScope(scope))
	f := s.newFenceLocked()
	s.unlock()
	return waitFence(ctx, f)
}

// SetScope limits an account to some rooms; see Server.SetScope.
func (h *Host) SetScope(ctx context.Context, userID string, scope *RoomScope) error {
	return h.s.SetScope(ctx, userID, scope)
}

// Scope returns the rooms an account is limited to, nil when it is not.
func (s *Server) Scope(userID string) *RoomScope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u := s.users[userID]; u != nil {
		return u.scope.public()
	}
	return nil
}

// Scope returns the rooms an account is limited to; see Server.Scope.
func (h *Host) Scope(userID string) *RoomScope { return h.s.Scope(userID) }

// setScopeLocked sets u's scope, and takes u out of what it leaves out.
func (s *Server) setScopeLocked(u *userState, scope *roomScope) {
	u.scope = scope
	s.touchUser(u.id)
	if scope == nil {
		return
	}
	// Threads go before the rooms they are threads of.
	rooms := slices.SortedFunc(maps.Values(u.joined), func(a, b *roomState) int {
		return depth(b) - depth(a)
	})
	for _, r := range rooms {
		if !r.visibleTo(u) {
			s.leaveLocked(u, r)
		}
	}
	for id := range u.pings {
		if r := s.rooms[id]; r == nil || !r.visibleTo(u) {
			delete(u.pings, id)
		}
	}
	s.unreadChangedLocked(u, "")
	s.endRefusedWritesLocked(u)
}

func depth(r *roomState) int {
	n := 0
	for room := r.parent; room != nil; room = room.parent {
		n++
	}
	return n
}

// mayCreateRoomLocked refuses a scoped account a top-level room, and a
// thread its scope does not take in.
func mayCreateRoomLocked(u *userState, parent *roomState) *Error {
	if u.scope == nil {
		return nil
	}
	if parent == nil || !u.scope.threads {
		return &Error{Code: CodeDenied, Message: "This account is limited to some rooms, and may not start one"}
	}
	return nil
}
