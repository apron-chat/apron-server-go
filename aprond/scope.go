package aprond

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/apron-chat/apron-server-go/aprond/internal/rooms"
)

// Room scopes. An account with a scope, such as a bot that should reach
// only the rooms it is assigned, sees only the rooms its scope lists, and
// their threads if it says so. Seeing is the server's one check of who may
// read and write a room (rooms.Room.VisibleTo): history, room_list, joins,
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

// roomScope is a RoomScope as the server checks it (rooms.Room.InScope).
// Guarded by s.mu.
type roomScope = rooms.Scope

func newRoomScope(scope *RoomScope) *roomScope {
	if scope == nil {
		return nil
	}
	return rooms.NewScope(scope.Rooms, scope.Threads)
}

// SetScope limits an account to the rooms scope lists, and returns once
// that is stored. The account leaves the rooms and threads it is in that
// the scope leaves out, and its writes into them end. It does not join the
// rooms the scope lists; a bot joins them with Bot.Join. The zero
// RoomScope lists no room; ClearScope lifts the limit.
func (s *Server) SetScope(ctx context.Context, userID string, scope RoomScope) error {
	return s.storeScope(ctx, userID, rooms.NewScope(scope.Rooms, scope.Threads))
}

// ClearScope lifts an account's limit to some rooms, and returns once that
// is stored.
func (s *Server) ClearScope(ctx context.Context, userID string) error {
	return s.storeScope(ctx, userID, nil)
}

func (s *Server) storeScope(ctx context.Context, userID string, scope *roomScope) error {
	s.mu.Lock()
	u := s.users.Get(userID)
	if u == nil {
		s.unlock()
		return errors.New("apron: no such user")
	}
	s.setScopeLocked(u, scope)
	f := s.newFenceLocked()
	s.unlock()
	return f.Wait(ctx)
}

// SetScope limits an account to some rooms; see Server.SetScope.
func (h *Host) SetScope(ctx context.Context, userID string, scope RoomScope) error {
	return h.s.SetScope(ctx, userID, scope)
}

// ClearScope lifts an account's limit; see Server.ClearScope.
func (h *Host) ClearScope(ctx context.Context, userID string) error {
	return h.s.ClearScope(ctx, userID)
}

// Scope returns the rooms an account is limited to, and whether it is
// limited: false for an account with no scope, or no such account.
func (s *Server) Scope(userID string) (RoomScope, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u := s.users.Get(userID); u != nil && u.Scope != nil {
		return RoomScope{Rooms: u.Scope.Rooms(), Threads: u.Scope.Threads()}, true
	}
	return RoomScope{}, false
}

// Scope returns the rooms an account is limited to; see Server.Scope.
func (h *Host) Scope(userID string) (RoomScope, bool) { return h.s.Scope(userID) }

// setScopeLocked sets u's scope, and takes u out of what it leaves out.
func (s *Server) setScopeLocked(u *userState, scope *roomScope) {
	u.Scope = scope
	s.users.Touch(u.id)
	if scope == nil {
		return
	}
	// Threads go before the rooms they are threads of.
	joined := slices.SortedFunc(maps.Values(u.joined), func(a, b *roomState) int {
		return b.Depth() - a.Depth()
	})
	for _, r := range joined {
		if !r.VisibleTo(u) {
			s.leaveLocked(u, r)
		}
	}
	for id := range u.Pings {
		if r := s.rooms.Get(id); r == nil || !r.VisibleTo(u) {
			delete(u.Pings, id)
		}
	}
	s.unreadChangedLocked(u, "")
	s.endRefusedWritesLocked(u)
}

// mayCreateRoomLocked refuses a scoped account a top-level room, and a
// thread its scope does not take in.
func mayCreateRoomLocked(u *userState, parent *roomState) *Error {
	if u.Scope == nil {
		return nil
	}
	if parent == nil || !u.Scope.Threads() {
		return &Error{Code: CodeDenied, Message: "This account is limited to some rooms, and may not start one"}
	}
	return nil
}
