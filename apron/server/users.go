package server

import (
	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// ApplyMe returns old with what a me request changes (§3.3.2), merged as
// the protocol merges it: a name or avatar the request carries replaces
// old's, and "" clears it; its ext merges into old's (MergeExt). Status is
// left as it was: the chosen status is capability status's (§4.5.1),
// which others see as the shown status, so a backend with it applies it
// itself, and publishes the two views with PublishUser. The backend
// checks the result against its own policy, such as a name's length, or
// normalizes it, before it makes it the user's. The result shares nothing
// with old.
func ApplyMe(old apron.User, p apron.MeParams) apron.User {
	u := cloneUser(old)
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Avatar != nil {
		u.Avatar = *p.Avatar
	}
	if p.Ext != nil {
		u.Ext = core.CloneExt(apron.MergeExt(old.Ext, p.Ext))
	}
	return u
}

// UserChange is a change of a user (§3.3.3), which PublishUser announces,
// as its two audiences see it: the user's own connections, which are sent
// you, and everyone else, who are sent new. The two see different objects
// when the user's own see what the user chose, and others what the server
// shows of it, such as the chosen status and the shown one (§4.5.2); most
// changes set Old and New alone.
type UserChange struct {
	// Old and New are the user before and after the change as its own
	// connections see it: what the backend last made their identity and
	// told their clients, and what it makes them now.
	Old, New apron.User
	// OldShown and NewShown are the user before and after the change as
	// everyone else sees it, when NewShown has a user_id; without one,
	// others see Old and New. Their user_ids are Old's and New's, whatever
	// they hold. A change may be others' alone, such as a shown status
	// that follows the user's connections while the chosen one stays.
	OldShown, NewShown apron.User
	// Except, if set, is one of the user's own connections that is told
	// of the change otherwise, and is sent nothing, nor given an identity:
	// the one whose me or auth result carries the user, complete (§3.3.2),
	// and makes it the connection's as it is sent (see ProfileHandler).
	Except *Conn
}

// PublishUser announces a change of a user (§3.3.3), and reports whether
// there was one: whether either view of it changed anything, as User.Patch
// tells. Each audience is sent the patch of its own view, and only when
// that view changed:
//
//   - the user's own connections, those signed in as New's user_id or
//     Old's, but Except, get New as their identity (Conn.SetUser), and are
//     sent {"you": New.Patch(Old)};
//   - the other signed-in connections for which shares returns true, or
//     every one when shares is nil, are sent
//     {"new": NewShown.Patch(OldShown)}, or New's patch when NewShown is
//     unset, with the old user as old when the user_id changed, as when a
//     guest becomes an account.
//
// A backend tells others only of users who share a room with them
// (§3.3.3): shares says who does. A change of the user's own profile, as
// a rename, is the simplest:
//
//	srv.PublishUser(server.UserChange{Old: old, New: renamed}, nil)
//
// When nothing changed, PublishUser does nothing at all: it neither walks
// the connections nor calls SetUser, so a backend may call it on every
// change it might have made, and learn from the result whether it did.
// Patch covers every field of User, so nothing changed means New is Old;
// Old is what the backend last made the user's connections' identity and
// told their clients, and a connection whose identity is something else,
// such as one the backend set itself with Conn.SetUser, is the backend's
// to bring up to date. When only others' view changed, the user's own
// connections are neither sent anything nor given an identity.
//
// Like Broadcast, it never blocks, and is meant to be called while the
// backend holds the lock of the change, so that every connection gets a
// user's changes in the order they were made; shares is called in the
// caller's goroutine, so it may read what that lock guards.
func (s *Server) PublishUser(change UserChange, shares func(*Conn) bool) bool {
	old, cur := change.Old, change.New
	oldShown, curShown := old, cur
	if change.NewShown.UserID != "" {
		oldShown, curShown = change.OldShown, change.NewShown
		oldShown.UserID, curShown.UserID = old.UserID, cur.UserID
	}
	yours, youChanged := cur.Patch(old)
	theirs, othersChanged := curShown.Patch(oldShown)
	if !youChanged && !othersChanged {
		return false
	}
	var you, others apron.Frame
	var err error
	if youChanged {
		you, err = apron.Encode(apron.UserUpdate{You: &yours})
	}
	if othersChanged && err == nil {
		update := apron.UserUpdate{New: &theirs}
		if old.UserID != "" && old.UserID != cur.UserID {
			update.Old = &apron.CurrentUser{User: cloneUser(oldShown)}
		}
		others, err = apron.Encode(update)
	}
	if err != nil {
		s.logger().Error("apron: encoding a user notification", "user_id", cur.UserID, "error", err)
		you, others = nil, nil
	}
	w := newWaker()
	for _, c := range s.openConns() {
		switch id := c.UserID(); {
		case id == "", c == change.Except:
		case id == cur.UserID || id == old.UserID:
			if youChanged {
				c.SetUser(cur)
				if you != nil {
					w.add(c.sendDeferred(you))
				}
			}
		case others != nil && (shares == nil || shares(c)):
			w.add(c.sendDeferred(others))
		}
	}
	w.wake()
	return true
}
