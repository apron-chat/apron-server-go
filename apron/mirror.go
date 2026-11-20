package apron

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The mirror's side of trusted federation (federation.go): it reads an
// owner's rooms through the owner's link for this server, writes copies of
// their records into mirror rooms, as the server, and forwards its users'
// writes in a mirror room to the owner, answering them once the owner's
// record has come back.

// mirrorRoom is a mirror room, or a thread of one, here: the owner's
// address and room, the room here it is a thread of, if any, where the
// owner's log was when it was made (no older record is copied), and whether
// the owner stopped sharing it.
type mirrorRoom struct {
	Server    string `json:"server"`
	OwnerRoom string `json:"owner_room"`
	Parent    string `json:"parent,omitzero"`
	Start     int64  `json:"start,omitzero"`
	Unshared  bool   `json:"unshared,omitzero"`
}

// mirror reads one owner's rooms, on one connection.
type mirror struct {
	f      *federation
	server string

	// local maps the owner's rooms and threads to the mirror rooms here;
	// copies and originals map the owner's message_ids to the copies here
	// and back; users maps owner user_ids to the users here that stand for
	// them; hidden holds the owner's messages whose copies an admin here
	// hid. All are guarded by s.mu, as copies are written under it.
	local     map[string]string
	copies    map[string]string
	originals map[string]string
	users     map[string]string
	hidden    map[string]bool

	mu     sync.Mutex
	token  string
	cursor int64
	// conn is the connection once it caught up; ready is closed when conn
	// is set. applied is the last notification applied from conn, and
	// appliedCh closed when it moves.
	conn      *remoteConn
	ready     chan struct{}
	applied   int64
	appliedCh chan struct{}
	inFlight  int
	// want asks the run loop to connect; wakeID and wakeSecret name the
	// mirror's wake URL and its bearer token.
	want               chan struct{}
	wakeID, wakeSecret string
	// memberships are this server's users' joins and leaves of mirror
	// rooms that the owner has not answered, which go to it in order, kept
	// in the KV under "memberships <server>"; guarded by s.mu. sendDue
	// asks sendMemberships to send them.
	memberships []membershipChange
	sendDue     chan struct{}
}

// membershipChange is a user's join or leave of an owner's room. ID makes
// its request id, the same when it is sent again.
type membershipChange struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name,omitzero"`
	OwnerRoom string `json:"room"`
	Joined    bool   `json:"joined"`
}

// mirrorTimeout bounds how long a forwarded write waits for the owner.
const mirrorTimeout = 10 * time.Second

// mirrorCheckEvery is how often a mirror that sleeps reads its owner's log,
// in case a wake push was lost.
const mirrorCheckEvery = 5 * time.Minute

func (f *federation) newMirror(server string) *mirror {
	return &mirror{
		f: f, server: server, local: make(map[string]string), copies: make(map[string]string),
		originals: make(map[string]string), users: make(map[string]string), hidden: make(map[string]bool),
		ready: make(chan struct{}), appliedCh: make(chan struct{}), want: make(chan struct{}, 1),
		sendDue: make(chan struct{}, 1),
	}
}

// mirrorLocked returns the mirror of an owner, made and started if there
// is none. It is called holding f.mu.
func (f *federation) mirrorLocked(server string) (*mirror, bool) {
	if ms := f.mirrors[server]; ms != nil {
		return ms, false
	}
	ms := f.newMirror(server)
	f.mirrors[server] = ms
	return ms, true
}

// initMirrors restores the mirror rooms and starts reading their owners.
func (f *federation) initMirrors(r *Registrar) error {
	var rooms map[string]mirrorRoom
	var tokens map[string]string
	if _, err := f.kv.Get("mirror-rooms", &rooms); err != nil {
		return err
	}
	if _, err := f.kv.Get("mirror-tokens", &tokens); err != nil {
		return err
	}
	for id, room := range rooms {
		f.rooms[id] = &room
		ms, _ := f.mirrorLocked(room.Server)
		ms.local[room.OwnerRoom] = id
	}
	s := f.s
	for server, ms := range f.mirrors {
		ms.token = tokens[server]
		_, _ = f.kv.Get("cursor "+server, &ms.cursor)
		var hidden []string
		_, _ = f.kv.Get("hidden "+server, &hidden)
		_, _ = f.kv.Get("memberships "+server, &ms.memberships)
		for _, id := range hidden {
			ms.hidden[id] = true
		}
		for _, key := range f.kv.Keys("user " + server + " ") {
			var ownerID string
			if found, _ := f.kv.Get(key, &ownerID); found {
				ms.users[ownerID] = strings.TrimPrefix(key, "user "+server+" ")
			}
		}
		var wake struct{ ID, Secret string }
		if found, _ := f.kv.Get("wake "+server, &wake); found {
			ms.wakeID, ms.wakeSecret = wake.ID, wake.Secret
		}
	}
	// The copies' originals are kept apart from their ext, which a
	// tombstone drops; remote users name their users on the owner.
	originals := make(map[string]string)
	for _, key := range f.kv.Keys("copy ") {
		var ownerID string
		if found, _ := f.kv.Get(key, &ownerID); found {
			originals[strings.TrimPrefix(key, "copy ")] = ownerID
		}
	}
	s.mu.RLock()
	for id, ownerID := range originals {
		if m := s.messages[id]; m != nil && f.rooms[m.roomID] != nil {
			if ms := f.mirrors[f.rooms[m.roomID].Server]; ms != nil {
				ms.copies[ownerID], ms.originals[id] = id, ownerID
			}
		}
	}
	for id, u := range s.users {
		if u.shadow == nil {
			continue
		}
		for server, ms := range f.mirrors {
			if u.shadow.server == server {
				ms.users[u.shadow.remoteID] = id
			} else if ownerID, err := shadowID(u.shadow.server, u.shadow.remoteID); err == nil {
				ms.users[ownerID] = id
			}
		}
	}
	s.mu.RUnlock()
	for _, ms := range f.mirrors {
		ms.start(r.Host)
	}
	return nil
}

func (ms *mirror) start(h *Host) {
	h.Go(ms.run)
	h.Go(ms.sendMemberships)
	ms.wakeUp()
	ms.sendMembershipsNow()
}

// saveRooms keeps the mirror rooms and the tokens.
func (f *federation) saveRooms() error {
	f.mu.Lock()
	rooms := make(map[string]mirrorRoom, len(f.rooms))
	for id, room := range f.rooms {
		rooms[id] = *room
	}
	tokens := make(map[string]string, len(f.mirrors))
	for server, ms := range f.mirrors {
		ms.mu.Lock()
		tokens[server] = ms.token
		ms.mu.Unlock()
	}
	f.mu.Unlock()
	return errors.Join(f.kv.Put("mirror-rooms", rooms), f.kv.Put("mirror-tokens", tokens))
}

// ---- /remote connect and unshare ----

// connect mirrors the room of an invite: it signs in to the owner as the
// owner's link for this server, reads the room's record, and makes a mirror
// room here like it, joining the admin to it.
func (f *federation) connect(ctx context.Context, call *CommandCall, invite string) error {
	server, ownerRoom, token, err := parseInvite(invite)
	switch {
	case err != nil:
		return InvalidParams("Usage: /remote connect <invite>: " + err.Error())
	case server == f.self:
		return InvalidParams("That invite is this server's own")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, you, err := f.dial(dialCtx, server, token, false)
	if err != nil {
		call.Notice(fmt.Sprintf("Could not sign in to %s: %v", server, err))
		return nil
	}
	defer conn.close()
	if bound, _ := normalizeAddress(you.Ext.Remote.Server); bound != f.self {
		call.Notice(fmt.Sprintf("That invite is for %s, not for this server (%s)", cmp.Or(you.Ext.Remote.Server, "no server"), f.self))
		return nil
	}
	raw, err := conn.call(dialCtx, "connect-"+randomID(), "room_list", map[string]any{"room_id": ownerRoom, "members": true})
	if err != nil {
		call.Notice(fmt.Sprintf("%s did not list room %s: %v", server, ownerRoom, err))
		return nil
	}
	var listed struct {
		Joined []struct {
			RoomID      string           `json:"room_id"`
			Title       string           `json:"title"`
			Description string           `json:"description"`
			Private     bool             `json:"private"`
			Latest      string           `json:"latest_log_id"`
			Members     []map[string]any `json:"members"`
		} `json:"joined"`
		Users []map[string]any `json:"users"`
	}
	if json.Unmarshal(raw, &listed) != nil || len(listed.Joined) == 0 || listed.Joined[0].RoomID != ownerRoom {
		call.Notice(fmt.Sprintf("The link has not joined room %s of %s", ownerRoom, server))
		return nil
	}
	owner := listed.Joined[0]
	start, _ := strconv.ParseInt(owner.Latest, 10, 64)
	title := cmp.Or(owner.Title, ownerRoom)
	// The room's threads that the link has joined get mirror threads now:
	// their records are older than the mirror room's start.
	threads, threadUsers, err := listJoined(dialCtx, conn)
	if err != nil {
		call.Notice(fmt.Sprintf("%s did not list the link's rooms: %v", server, err))
		return nil
	}

	s := f.s
	s.mu.Lock()
	fields := map[string]any{"title": title, "ext": extObject{remoteExt: encodeJSON(map[string]any{"server": server, "room_id": ownerRoom})}}
	if owner.Description != "" {
		fields["description"] = owner.Description
	}
	// The mirror room is private exactly when the owner's room is, from
	// now on.
	r := s.commitRoomLocked("", nil, owner.Private, fields)
	r.creator = call.User.ID
	s.touchRoom(r)
	f.mu.Lock()
	ms, made := f.mirrorLocked(server)
	f.rooms[r.id] = &mirrorRoom{Server: server, OwnerRoom: ownerRoom, Start: start}
	f.mu.Unlock()
	ms.local[ownerRoom] = r.id
	if admin := s.users[call.User.ID]; admin != nil {
		s.joinLocked(admin, r)
	}
	// The owner's members join it as remote users, once: later joins and
	// leaves come as records. A listing the owner cut short only adds.
	ms.seedMembersLocked(r, owner.Members, usersByID(listed.Users), false)
	ms.threadsLocked(threads, threadUsers)
	s.unlock()

	ms.mu.Lock()
	ms.token = token
	if ms.cursor == 0 || made {
		ms.cursor = max(ms.cursor, start)
	}
	cursor := ms.cursor
	ms.mu.Unlock()
	if err := errors.Join(f.saveRooms(), f.kv.Put("cursor "+server, cursor)); err != nil {
		return err
	}
	if made {
		ms.start(f.h)
	} else {
		ms.wakeUp()
	}
	authority, _ := remoteAuthority(server)
	call.Notice(fmt.Sprintf("Mirroring %s from %s in room `%s`.", title, authority, r.id))
	return nil
}

// unmirror makes the link leave the owner's room, and stops the mirror
// room, which stays, with its copies, and takes no more writes.
func (f *federation) unmirror(ctx context.Context, call *CommandCall, room *mirrorRoom) error {
	if room.Parent != "" {
		return InvalidParams("Run /remote unshare in the mirror room, not in a thread of it")
	}
	f.mu.Lock()
	ms := f.mirrors[room.Server]
	f.mu.Unlock()
	if _, _, err := ms.call(ctx, "unshare-"+randomID(), "room_leave", map[string]any{"room_id": room.OwnerRoom}); err != nil {
		call.Notice(fmt.Sprintf("%s did not take the link out of the room (%v); it is no longer mirrored here all the same.", room.Server, err))
	}
	f.markUnshared(room.Server, room.OwnerRoom)
	call.Notice("This room is no longer mirrored: it keeps its copies, and takes no more writes.")
	return nil
}

// markUnshared stops a mirror room, and its threads, whose owner no longer
// shares it.
func (f *federation) markUnshared(server, ownerRoom string) {
	f.mu.Lock()
	var top string
	for id, room := range f.rooms {
		if room.Server == server && room.OwnerRoom == ownerRoom && room.Parent == "" {
			top = id
		}
	}
	for changed := top != ""; changed; {
		changed = false
		for id, room := range f.rooms {
			if !room.Unshared && room.Server == server && (id == top || room.Parent != "" && f.rooms[room.Parent] != nil && f.rooms[room.Parent].Unshared) {
				room.Unshared, changed = true, true
			}
		}
	}
	f.mu.Unlock()
	if top != "" {
		f.h.logger.Info("a mirror room is no longer shared", "room", top, "server", server)
		_ = f.saveRooms()
	}
}

// ---- Reading ----

// youResult is the `you` of a link's sign-in.
type youResult struct {
	Ext struct {
		Remote struct {
			Server string `json:"server"`
		} `json:"remote"`
	} `json:"ext"`
}

// dial connects to an owner and signs in with a token of its link for this
// server.
func (f *federation) dial(ctx context.Context, server, token string, notes bool) (*remoteConn, youResult, error) {
	var you youResult
	conn, err := dialRemote(ctx, server, notes)
	if err != nil {
		return nil, you, err
	}
	var frame struct {
		Ping float64 `json:"ping"`
	}
	select {
	case raw := <-conn.server:
		_ = json.Unmarshal(raw, &frame)
	case <-conn.done:
		return nil, you, cmp.Or(conn.err, errRemoteDisconnected)
	case <-ctx.Done():
		conn.close()
		return nil, you, ctx.Err()
	}
	if frame.Ping > 0 {
		go conn.ping(time.Duration(frame.Ping * float64(time.Second)))
	}
	raw, err := conn.call(ctx, "auth-"+randomID(), "auth", map[string]any{"scheme": "token", "token": token, "agent": "apron-go/10 mirror"})
	if err != nil {
		conn.close()
		return nil, you, err
	}
	var result struct {
		You youResult `json:"you"`
	}
	_ = json.Unmarshal(raw, &result)
	return conn, result.You, nil
}

// active reports whether the mirror has a room that is still shared.
func (ms *mirror) active() bool {
	f := ms.f
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, room := range f.rooms {
		if room.Server == ms.server && !room.Unshared {
			return true
		}
	}
	return false
}

// wakeUp asks the run loop to connect and read.
func (ms *mirror) wakeUp() {
	select {
	case ms.want <- struct{}{}:
	default:
	}
}

// run keeps the mirror's connection while it is wanted: always without a
// linger, else after a wake push, a forwarded write, or the periodic check,
// until it has been idle for the linger time.
func (ms *mirror) run(ctx context.Context) {
	f := ms.f
	logger := f.h.logger.With("owner", ms.server)
	backoff := time.Second
	for ctx.Err() == nil {
		select {
		case <-ms.want:
		case <-time.After(mirrorCheckEvery):
		case <-ctx.Done():
			return
		}
		if !ms.active() {
			continue
		}
		err := ms.session(ctx)
		if e, ok := errors.AsType[*remoteError](err); ok && e.Code == remoteCodeDenied {
			logger.Error("the owner refused the link's token; mirroring stops until an admin connects again", "error", err)
			continue
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("the connection to the owner ended", "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(2*backoff, 5*time.Minute)
			ms.wakeUp()
			continue
		}
		backoff = time.Second
		if f.linger == 0 {
			ms.wakeUp()
		}
	}
}

// session connects, catches up from the cursor, and applies what arrives,
// until the connection ends or, with a linger, it has been idle for it.
func (ms *mirror) session(ctx context.Context) error {
	f := ms.f
	ms.mu.Lock()
	token, cursor := ms.token, ms.cursor
	ms.mu.Unlock()
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := f.dial(dialCtx, ms.server, token, true)
	cancel()
	if err != nil {
		return err
	}
	// The rooms the link has left since, such as while the mirror slept,
	// are no longer shared: the link's history has none of their records.
	// The threads it has joined that have no mirror thread get one.
	if listed, users, err := listJoined(ctx, conn); err == nil {
		joined := make(map[string]bool)
		for _, room := range listed {
			joined[room.RoomID] = true
		}
		var gone []string
		f.mu.Lock()
		for _, room := range f.rooms {
			if room.Server == ms.server && room.Parent == "" && !room.Unshared && !joined[room.OwnerRoom] {
				gone = append(gone, room.OwnerRoom)
			}
		}
		f.mu.Unlock()
		for _, ownerRoom := range gone {
			f.markUnshared(ms.server, ownerRoom)
		}
		f.s.mu.Lock()
		made := ms.threadsLocked(listed, users)
		f.s.unlock()
		if made {
			_ = f.saveRooms()
		}
	}
	defer conn.close()
	stop := context.AfterFunc(ctx, conn.close)
	defer stop()
	if f.linger > 0 {
		if err := ms.registerWake(ctx, conn); err != nil {
			f.h.logger.Warn("the owner did not take the wake push", "owner", ms.server, "error", err)
		}
	}
	// Catch up: the log of the link's rooms after the cursor, while the
	// live records queue (§4.3.5).
	for after := cursor + 1; ; {
		raw, err := conn.call(ctx, "read-"+randomID(), "history", map[string]any{"after": strconv.FormatInt(after, 10), "limit": 500})
		if err != nil {
			return err
		}
		var page struct {
			Rooms       []jsontext.Value `json:"rooms"`
			Messages    []jsontext.Value `json:"messages"`
			Reactions   []jsontext.Value `json:"reactions"`
			Memberships []jsontext.Value `json:"memberships"`
			LastLogID   string           `json:"last_log_id"`
			More        bool             `json:"more"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		var records []mirrorRecord
		for kind, list := range map[string][]jsontext.Value{"room": page.Rooms, "message": page.Messages, "reactions": page.Reactions, "membership": page.Memberships} {
			for _, raw := range list {
				records = append(records, mirrorRecord{kind: kind, raw: raw, logID: logIDOf(raw)})
			}
		}
		slices.SortStableFunc(records, func(a, b mirrorRecord) int { return cmp.Compare(a.logID, b.logID) })
		for _, record := range records {
			ms.apply(record)
		}
		last, _ := strconv.ParseInt(page.LastLogID, 10, 64)
		if last > 0 {
			ms.advance(last)
		}
		if !page.More || last == 0 {
			break
		}
		after = last + 1
	}
	ms.mu.Lock()
	ms.conn, ms.applied = conn, 0
	close(ms.ready)
	ms.mu.Unlock()
	f.h.logger.Info("mirror connected", "owner", ms.server)
	ms.sendMembershipsNow()
	defer func() {
		ms.mu.Lock()
		ms.conn, ms.ready = nil, make(chan struct{})
		close(ms.appliedCh)
		ms.appliedCh = make(chan struct{})
		ms.mu.Unlock()
	}()
	var idle <-chan time.Time
	for {
		if f.linger > 0 && idle == nil {
			idle = time.After(f.linger)
		}
		select {
		case note := <-conn.notes:
			ms.applyNote(note)
			ms.mu.Lock()
			ms.applied = note.Seq
			close(ms.appliedCh)
			ms.appliedCh = make(chan struct{})
			ms.mu.Unlock()
			idle = nil
		case <-ms.want:
			idle = nil
		case <-idle:
			ms.mu.Lock()
			busy := ms.inFlight > 0
			ms.mu.Unlock()
			if !busy {
				f.h.logger.Info("mirror closed the idle connection", "owner", ms.server)
				return nil
			}
			idle = nil
		case <-conn.done:
			return cmp.Or(conn.err, errRemoteDisconnected)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// registerWake registers the mirror's wake push with the owner: a relay to
// /remote/wake/<id> here, for every record of the link's rooms.
func (ms *mirror) registerWake(ctx context.Context, conn *remoteConn) error {
	f := ms.f
	ms.mu.Lock()
	if ms.wakeID == "" {
		ms.wakeID, ms.wakeSecret = randomID(), randomID()
		_ = f.kv.Put("wake "+ms.server, struct{ ID, Secret string }{ms.wakeID, ms.wakeSecret})
	}
	id, secret := ms.wakeID, ms.wakeSecret
	ms.mu.Unlock()
	_, err := conn.call(ctx, "wake-"+randomID(), "push_register", map[string]any{
		"kind": "relay", "url": f.wakeBase + "/remote/wake/" + id, "token": secret, "wake": []string{"ext:remote"},
	})
	return err
}

// wakeHandler takes an owner's wake push.
func (f *federation) wakeHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	var target *mirror
	for _, ms := range f.mirrors {
		ms.mu.Lock()
		if ms.wakeID != "" && ms.wakeID == id && r.Header.Get("Authorization") == "Bearer "+ms.wakeSecret {
			target = ms
		}
		ms.mu.Unlock()
	}
	f.mu.Unlock()
	if target == nil {
		http.NotFound(w, r)
		return
	}
	target.wakeUp()
	w.WriteHeader(http.StatusNoContent)
}

// advance moves the cursor, and keeps it.
func (ms *mirror) advance(logID int64) {
	ms.mu.Lock()
	moved := logID > ms.cursor
	if moved {
		ms.cursor = logID
	}
	ms.mu.Unlock()
	if moved {
		_ = ms.f.kv.Put("cursor "+ms.server, logID)
	}
}

func logIDOf(raw jsontext.Value) int64 {
	var at struct {
		LogID string `json:"log_id"`
	}
	_ = json.Unmarshal(raw, &at)
	n, _ := strconv.ParseInt(at.LogID, 10, 64)
	return n
}

// mirrorRecord is a record of the owner's log.
type mirrorRecord struct {
	kind  string
	raw   jsontext.Value
	logID int64
	// users are the user objects that came with a room record, by user_id.
	users map[string]map[string]any
}

// applyNote applies a notification of the link's connection.
func (ms *mirror) applyNote(note remoteFrame) {
	switch note.Method {
	case "message", "reactions":
		ms.apply(mirrorRecord{kind: note.Method, raw: note.Params, logID: logIDOf(note.Params)})
	case "room_update":
		var update struct {
			Joined      []jsontext.Value `json:"joined"`
			Updated     []jsontext.Value `json:"updated"`
			Memberships []jsontext.Value `json:"memberships"`
			Users       []map[string]any `json:"users"`
			Left        []struct {
				RoomID string `json:"room_id"`
			} `json:"left"`
		}
		_ = json.Unmarshal(note.Params, &update)
		users := usersByID(update.Users)
		for _, raw := range append(update.Joined, update.Updated...) {
			ms.apply(mirrorRecord{kind: "room", raw: raw, logID: logIDOf(raw), users: users})
		}
		for _, raw := range update.Memberships {
			ms.apply(mirrorRecord{kind: "membership", raw: raw, logID: logIDOf(raw)})
		}
		for _, left := range update.Left {
			ms.f.s.mu.RLock()
			_, mirrored := ms.local[left.RoomID]
			ms.f.s.mu.RUnlock()
			if mirrored {
				ms.f.markUnshared(ms.server, left.RoomID)
			}
		}
	case "user":
		var note2 struct {
			New map[string]any `json:"new"`
		}
		if json.Unmarshal(note.Params, &note2) == nil && note2.New != nil {
			s := ms.f.s
			s.mu.Lock()
			ms.userLocked(note2.New)
			s.unlock()
		}
	}
}

// apply writes the copy of one record of the owner's log, and moves the
// cursor past it. A thread's record is applied whatever its log_id, since
// the link can be joined to a thread made before.
func (ms *mirror) apply(record mirrorRecord) {
	ms.mu.Lock()
	cursor := ms.cursor
	ms.mu.Unlock()
	if record.logID <= cursor && record.kind != "room" {
		return
	}
	s := ms.f.s
	value := decodeObject(record.raw)
	if value != nil {
		s.mu.Lock()
		switch record.kind {
		case "room":
			ms.roomLocked(value, record.logID, record.users)
		case "message":
			ms.messageLocked(value, record.logID)
		case "reactions":
			ms.reactionsLocked(value)
		case "membership":
			ms.membershipLocked(value, record.logID)
		}
		s.unlock()
	}
	if record.logID > cursor {
		ms.advance(record.logID)
	}
}

// roomFor is the mirror room or thread here of an owner's room, if it is
// still shared, and whether a record at logID is after its start.
func (ms *mirror) roomFor(ownerRoom string, logID int64) (*roomState, bool) {
	f, s := ms.f, ms.f.s
	id := ms.local[ownerRoom]
	f.mu.Lock()
	room := f.rooms[id]
	f.mu.Unlock()
	if room == nil || room.Unshared || logID <= room.Start {
		return nil, false
	}
	return s.rooms[id], s.rooms[id] != nil
}

// roomLocked makes the thread here of a thread of a mirrored room, the
// first time, and keeps its title and description.
func (ms *mirror) roomLocked(record map[string]any, logID int64, users map[string]map[string]any) {
	f, s := ms.f, ms.f.s
	ownerRoom, _ := record["room_id"].(string)
	parent, _ := record["parent_room_id"].(string)
	fields := map[string]any{}
	for _, key := range []string{"title", "description"} {
		if value, ok := record[key].(string); ok && value != "" {
			fields[key] = value
		}
	}
	if local := s.rooms[ms.local[ownerRoom]]; local != nil {
		if local.parent == nil {
			// A mirror room keeps its own title and description, and its
			// private: when the owner's room becomes private, a public
			// mirror room stops mirroring, which would show it to all.
			if record["private"] == true && !local.private {
				go ms.f.markUnshared(ms.server, ownerRoom) // it takes s.mu, held here
			}
			return
		}
		// A thread's record with members is the link's join: its first
		// members, also when an updated record made the thread before it.
		if members := asMaps(record["members"]); len(members) > 0 {
			ms.seedMembersLocked(local, members, users, true)
		}
		current := maps.Clone(local.record)
		if current["title"] == fields["title"] && current["description"] == fields["description"] {
			return
		}
		fields["ext"] = local.record["ext"]
		s.commitRoomLocked(local.id, local.parent, false, fields)
		s.touchRoom(local)
		s.announceRoomLocked(local, nil)
		return
	}
	if parent == "" {
		return
	}
	if _, ok := ms.roomFor(parent, logID); ok {
		if r := ms.threadLocked(ownerRoom, parent, fields); r != nil {
			ms.seedMembersLocked(r, asMaps(record["members"]), users, true)
			go f.saveRooms() // it takes s.mu, held here
		}
	}
}

// threadLocked makes the mirror thread of an owner's thread, under the
// mirror room or thread of its parent, with the title and description in
// fields. It starts where the mirror room it is in does. It reports whether
// it made one: not when the parent is not mirrored, or no longer shared.
func (ms *mirror) threadLocked(ownerRoom, parent string, fields map[string]any) *roomState {
	f, s := ms.f, ms.f.s
	localParent := s.rooms[ms.local[parent]]
	f.mu.Lock()
	parentRoom := f.rooms[ms.local[parent]]
	f.mu.Unlock()
	if localParent == nil || parentRoom == nil || parentRoom.Unshared {
		return nil
	}
	fields["ext"] = extObject{remoteExt: encodeJSON(map[string]any{"server": ms.server, "room_id": ownerRoom})}
	// Not private, so whoever sees the mirror room sees its threads.
	r := s.commitRoomLocked("", localParent, false, fields)
	s.touchRoom(r)
	ms.local[ownerRoom] = r.id
	f.mu.Lock()
	f.rooms[r.id] = &mirrorRoom{Server: ms.server, OwnerRoom: ownerRoom, Parent: localParent.id, Start: parentRoom.Start}
	f.mu.Unlock()
	s.announceRoomLocked(r, nil)
	return r
}

// listedRoom is a room record of the owner's room_list.
type listedRoom struct {
	RoomID       string           `json:"room_id"`
	ParentRoomID string           `json:"parent_room_id"`
	Title        string           `json:"title"`
	Description  string           `json:"description"`
	Members      []map[string]any `json:"members"`
}

// listJoined lists the rooms and threads that the link has joined.
// listJoined lists the rooms and threads that the link has joined, with
// their members, and the members' user objects by user_id.
func listJoined(ctx context.Context, conn *remoteConn) ([]listedRoom, map[string]map[string]any, error) {
	raw, err := conn.call(ctx, "rooms-"+randomID(), "room_list", map[string]any{"filter": "joined", "members": true})
	if err != nil {
		return nil, nil, err
	}
	var listed struct {
		Joined []listedRoom     `json:"joined"`
		Users  []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, nil, err
	}
	return listed.Joined, usersByID(listed.Users), nil
}

// usersByID indexes user objects by their user_id.
func usersByID(objects []map[string]any) map[string]map[string]any {
	users := make(map[string]map[string]any, len(objects))
	for _, u := range objects {
		if id, _ := u["user_id"].(string); id != "" {
			users[id] = u
		}
	}
	return users
}

// asMaps is the objects of a JSON array.
func asMaps(value any) []map[string]any {
	var objects []map[string]any
	for _, item := range asList(value) {
		if object, ok := item.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}

// seedMembersLocked joins the first members of a new mirror room or
// thread, from the members of the owner's room record, with their user
// objects where they came: the owner's and other servers' users as remote
// users, and, with own, this server's users whose remote users they are.
// Links are no members here. A member without its user object is known
// by its user_id's form.
func (ms *mirror) seedMembersLocked(r *roomState, members []map[string]any, users map[string]map[string]any, own bool) {
	s := ms.f.s
	for _, member := range members {
		id, _ := member["user_id"].(string)
		object := users[id]
		if object == nil {
			object = member
		}
		if isLink(object) {
			continue
		}
		switch u := ms.userLocked(object); {
		case u == nil || s.users[u.id] != u:
			// Unknown, or a guest of this server's that left.
		case u.shadow != nil || own:
			s.joinLocked(u, r)
		}
	}
}

// threadsLocked makes the mirror threads of the owner's threads that the
// link has joined, as listed, that have none: threads that existed when
// their room was connected, whose records are older than its start. It
// reports whether it made one.
func (ms *mirror) threadsLocked(joined []listedRoom, users map[string]map[string]any) bool {
	made := false
	for changed := true; changed; {
		changed = false
		for _, room := range joined {
			if room.ParentRoomID == "" || ms.local[room.RoomID] != "" {
				continue
			}
			fields := map[string]any{}
			for key, value := range map[string]string{"title": room.Title, "description": room.Description} {
				if value != "" {
					fields[key] = value
				}
			}
			if r := ms.threadLocked(room.RoomID, room.ParentRoomID, fields); r != nil {
				ms.seedMembersLocked(r, room.Members, users, true)
				changed, made = true, true
			}
		}
	}
	return made
}

// userLocked returns the user here that stands for a user of the owner's
// records, given as its recorded or current user object: this server's own
// user, for its users' remote users on the owner, and a remote user for the
// others, of the server its ext.remote names, or of the owner. It keeps the
// remote user's name. nil for a system identity.
func (ms *mirror) userLocked(object map[string]any) *userState {
	s := ms.f.s
	ownerID, _ := object["user_id"].(string)
	if ownerID == "" || strings.HasPrefix(ownerID, "~") {
		return nil
	}
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext[remoteExt].(map[string]any)
	server, _ := remote["server"].(string)
	remoteID, _ := remote["user_id"].(string)
	if server == "" || remoteID == "" {
		// Without remote, by its user_id's form (ext:remote): this
		// server's user, another server's user seen before, or the
		// owner's own user. No remote user is made for an ID with an @.
		switch local, own := strings.CutSuffix(ownerID, "@"+ms.f.selfAuthority); {
		case own && local != "":
			server, remoteID = ms.f.self, local
		case strings.Contains(ownerID, "@"):
			if u := s.users[ms.users[ownerID]]; u != nil && u.shadow != nil {
				return ms.renamedLocked(u, object)
			}
			return nil
		default:
			server, remoteID = ms.server, ownerID
		}
	}
	if normalized, err := normalizeAddress(server); err == nil && normalized == ms.f.self && remoteID != "" {
		ms.users[ownerID] = remoteID
		if u := s.users[remoteID]; u != nil {
			return u
		}
		// A guest that left: its copy keeps its user_id.
		return &userState{id: remoteID, fromValue: map[string]any{"user_id": remoteID}}
	} else if err == nil {
		server = normalized
	}
	u, err := s.remoteUserLocked(server, remoteID)
	if err != nil {
		return nil
	}
	ms.users[ownerID] = u.id
	return ms.renamedLocked(u, object)
}

// renamedLocked keeps the name of a user object of the owner's on the
// remote user that stands for it.
func (ms *mirror) renamedLocked(u *userState, object map[string]any) *userState {
	s := ms.f.s
	if name, ok := object["name"].(string); ok && normalizeName(name) != u.name && u.shadow != nil {
		u.name = normalizeName(name)
		u.fromValue = nil
		s.touchUser(u.id)
		s.notifyProfileLocked(u, nil, nil)
	}
	return u
}

// localUserID is the user_id here of a user_id of the owner's, for
// mentions: one seen before, else by its form (ext:remote): one that ends
// in @ and this server's host is that user here, one with another @ is a
// third server's user as it is, and any other is the owner's own user's
// remote user.
func (ms *mirror) localUserID(ownerID string) string {
	if id := ms.users[ownerID]; id != "" {
		return id
	}
	if local, ok := strings.CutSuffix(ownerID, "@"+ms.f.selfAuthority); ok && local != "" {
		return local
	}
	if strings.Contains(ownerID, "@") {
		return ownerID
	}
	id, err := shadowID(ms.server, ownerID)
	if err != nil {
		return ownerID
	}
	return id
}

// messageLocked writes a message snapshot of the owner's as a new snapshot
// of its copy here: in the mirror room or thread of the owner's room, from
// the user here that stands for its author, with its reply, mentions, and
// @ids mapped, and ext.remote naming the original.
func (ms *mirror) messageLocked(record map[string]any, logID int64) {
	s := ms.f.s
	ownerID, _ := record["message_id"].(string)
	ownerRoom, _ := record["room_id"].(string)
	from, _ := record["from"].(map[string]any)
	deleted := record["deleted"] == true
	if ownerID == "" || ms.hidden[ownerID] {
		return
	}
	localID := ms.copies[ownerID]
	m := s.messages[localID]
	r, mirrored := ms.roomFor(ownerRoom, logID)
	switch {
	case !mirrored && m == nil:
		return
	case !mirrored:
		// Moved out of the mirrored rooms: the copy is deleted.
		r, deleted = s.rooms[m.roomID], true
	case m == nil && deleted:
		return
	}
	author := ms.userLocked(from)
	if author == nil {
		return
	}
	if author.removed[r.id] {
		return // a remote user an admin here removed from the room
	}
	logIDHere := s.nextIDLocked()
	var previous map[string]any
	if m == nil {
		localID = formatID(logIDHere)
		m = &messageState{id: localID, from: author.from(), owner: author.id, reactions: make(map[string]reactionSet)}
		s.messages[localID] = m
		ms.copies[ownerID], ms.originals[localID] = localID, ownerID
		if err := ms.f.kv.setLocked("copy "+localID, encodeJSON(ownerID)); err != nil {
			ms.f.h.logger.Warn("a copy's original was not kept", "message_id", localID, "error", err)
		}
	} else {
		previous = m.snapshot()
	}
	snapshot := map[string]any{"message_id": localID, "log_id": formatID(logIDHere), "room_id": r.id, "from": m.from}
	if deleted {
		snapshot["deleted"] = true
	} else {
		body, _ := record["body"].(map[string]any)
		mapped := make(map[string]string)
		for _, value := range asList(body["mentions"]) {
			if id, ok := value.(string); ok {
				mapped[id] = ms.localUserID(id)
			}
		}
		copied := remoteBody(body, mapped)
		copied["text"] = withServer(copied["text"].(string), copied["format"] == "markdown", ms.server)
		snapshot["body"] = copied
		if ref, ok := record["reply_to"].(map[string]any); ok {
			if target := ms.copies[fmt.Sprint(ref["message_id"])]; target != "" && target != localID {
				snapshot["reply_to"] = map[string]any{"message_id": target}
			}
		}
		ext := maps.Clone(extOf(record["ext"]))
		if ext == nil {
			ext = make(extObject)
		}
		delete(ext, editorsKey)
		ext[remoteExt] = encodeJSON(map[string]any{"server": ms.server, "room_id": ownerRoom, "message_id": ownerID})
		snapshot["ext"] = ext
	}
	source := m.roomID
	moved := s.commitSnapshotLocked(m, snapshot, logIDHere)
	if deleted {
		s.redactLocked(m)
	}
	if moved && len(m.reactions) > 0 {
		s.commitReactionsLocked(m, m.reactionElements())
	}
	var movedFrom *roomState
	if moved {
		movedFrom = s.rooms[source]
	}
	s.messageUnreadLocked(m, previous, movedFrom)
	if previous == nil {
		s.wakeNewLocked(m, snapshot)
	} else {
		s.wakeLocked(m, snapshot, previous)
	}
}

// reactionsLocked replaces the reaction sets of a copy with the owner's.
func (ms *mirror) reactionsLocked(record map[string]any) {
	s := ms.f.s
	m := s.messages[ms.copies[fmt.Sprint(record["message_id"])]]
	if m == nil || len(m.records) == 0 || m.snapshot()["deleted"] == true {
		return
	}
	var elements []any
	for _, value := range asList(record["reactions"]) {
		set, _ := value.(map[string]any)
		from, _ := set["from"].(map[string]any)
		u := ms.userLocked(from)
		if u == nil || u.removed[m.roomID] {
			continue
		}
		var emojis []string
		for _, e := range asList(set["emojis"]) {
			if emoji, ok := e.(string); ok {
				emojis = append(emojis, emoji)
			}
		}
		if len(emojis) == 0 {
			delete(m.reactions, u.id)
		} else {
			m.reactions[u.id] = reactionSet{from: u.from(), emojis: emojis}
		}
		elements = append(elements, map[string]any{"from": cloneObject(u.from()), "emojis": slices.Clone(emojis)})
	}
	if len(elements) > 0 {
		s.touchMessage(m)
		s.commitReactionsLocked(m, elements)
	}
}

// ---- Forwarding ----

// mirroredRoomLocked returns the mirror of a room here, if it is a mirror
// room or a thread of one, and the room's mirrorRoom.
func (f *federation) mirroredRoomLocked(roomID string) (*mirror, *mirrorRoom) {
	f.mu.Lock()
	defer f.mu.Unlock()
	room := f.rooms[roomID]
	if room == nil {
		return nil, nil
	}
	copied := *room
	return f.mirrors[room.Server], &copied
}

// forwardLocked returns the operation that forwards a user's request in a
// mirror room to its owner, or nil for a request that is not one. A delete
// of another's copy by an admin or moderator hides it here instead. An
// ext:remote hook in processFrame, called holding s.mu.
func (f *federation) forwardLocked(user *userState, req request) operation {
	if user.shadow != nil || user.link != nil {
		return nil
	}
	s := f.s
	var target struct {
		RoomID       *string `json:"room_id"`
		ParentRoomID *string `json:"parent_room_id"`
		MessageID    *string `json:"message_id"`
		Deleted      bool    `json:"deleted"`
	}
	if json.Unmarshal(encodeJSON(req.params), &target) != nil {
		return nil
	}
	var rooms []string
	var m *messageState
	switch req.method {
	case "message":
		rooms = append(rooms, cmp.Or(ptrValue(target.RoomID), defaultRoomID))
		if target.MessageID != nil {
			if m = s.messages[*target.MessageID]; m != nil {
				rooms = append(rooms, m.roomID)
			}
		}
	case "reactions":
		if target.MessageID != nil {
			if m = s.messages[*target.MessageID]; m != nil {
				rooms = append(rooms, m.roomID)
			}
		}
	case "room_set":
		switch {
		case target.RoomID != nil:
			if _, room := f.mirroredRoomLocked(*target.RoomID); room == nil || room.Parent == "" {
				return nil // a mirror room's own title is this server's
			}
			rooms = append(rooms, *target.RoomID)
		case target.ParentRoomID != nil:
			rooms = append(rooms, *target.ParentRoomID)
		}
	default:
		return nil
	}
	var ms *mirror
	local, mixed := false, false
	for _, id := range rooms {
		switch mirror, _ := f.mirroredRoomLocked(id); {
		case mirror == nil:
			local = true
		case ms != nil && ms != mirror:
			mixed = true
		default:
			ms = mirror
		}
	}
	mixed = mixed || ms != nil && local
	switch {
	case ms == nil:
		return nil
	case mixed:
		return func(*Server, *client, request) (any, bool, *Error) {
			return nil, false, &Error{Code: CodeDenied, Message: "A message moves only within the rooms one server hosts"}
		}
	case req.method == "message" && target.Deleted && m != nil && m.owner != user.id && (user.hasRole(RoleAdmin) || user.hasRole(RoleModerator)):
		return func(s *Server, c *client, req request) (any, bool, *Error) { return f.hide(ms, m.id) }
	}
	return func(s *Server, c *client, req request) (any, bool, *Error) { return f.forward(c, req, ms) }
}

func ptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// hide deletes a copy here only, and leaves its later records out.
func (f *federation) hide(ms *mirror, localID string) (any, bool, *Error) {
	s := f.s
	s.mu.Lock()
	m := s.messages[localID]
	ownerID := ms.originals[localID]
	if m == nil || ownerID == "" {
		s.unlock()
		return nil, false, invalidParams("Unknown message %q", localID)
	}
	ms.hidden[ownerID] = true
	hidden := slices.Collect(maps.Keys(ms.hidden))
	logID := s.nextIDLocked()
	snapshot := map[string]any{"message_id": m.id, "log_id": formatID(logID), "room_id": m.roomID, "from": m.from, "deleted": true}
	previous := m.snapshot()
	s.commitSnapshotLocked(m, snapshot, logID)
	s.redactLocked(m)
	s.messageUnreadLocked(m, previous, nil)
	s.unlock()
	_ = f.kv.Put("hidden "+ms.server, hidden)
	return map[string]any{"message_id": localID}, false, nil
}

// ownerUser is a user's user_id on the owner, from the result of the `me`
// that the mirror sends before the user's first write there.
func (ms *mirror) ownerUser(ctx context.Context, userID, name string) (string, *Error) {
	f := ms.f
	key := "user " + ms.server + " " + userID
	var ownerID string
	if found, _ := f.kv.Get(key, &ownerID); found {
		return ownerID, nil
	}
	return ms.sendProfile(ctx, userID, name)
}

// sendProfile sends a user's name to the owner, as the user's remote user
// there, and keeps the user's user_id on the owner.
func (ms *mirror) sendProfile(ctx context.Context, userID, name string) (string, *Error) {
	f := ms.f
	raw, _, err := ms.call(ctx, userID+":me-"+randomID(), "me", map[string]any{"name": name, "ext": map[string]any{"remote": map[string]any{"user_id": userID}}})
	if err != nil {
		return "", err
	}
	var result struct {
		You struct {
			UserID string `json:"user_id"`
		} `json:"you"`
	}
	if json.Unmarshal(raw, &result) != nil || result.You.UserID == "" {
		return "", internalError()
	}
	_ = f.kv.Put("user "+ms.server+" "+userID, result.You.UserID)
	f.s.mu.Lock()
	ms.users[result.You.UserID] = userID
	f.s.unlock()
	return result.You.UserID, nil
}

// call sends a request to the owner, connecting first, and returns its
// result with the number of the last notification before it. Errors are
// the owner's, or retry_after when the owner cannot be reached.
func (ms *mirror) call(ctx context.Context, id, method string, params map[string]any) (jsontext.Value, int64, *Error) {
	id = ownerRequestID(id)
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	unreachable := &Error{Code: CodeRetryAfter, Message: "The room's owner cannot be reached; try again later", Data: map[string]any{"retry_after": 5}}
	ms.mu.Lock()
	ms.inFlight++
	ready := ms.ready
	ms.mu.Unlock()
	defer func() {
		ms.mu.Lock()
		ms.inFlight--
		ms.mu.Unlock()
	}()
	ms.wakeUp()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, 0, unreachable
	}
	ms.mu.Lock()
	conn := ms.conn
	ms.mu.Unlock()
	if conn == nil {
		return nil, 0, unreachable
	}
	raw, seq, err := conn.callSeq(ctx, id, method, params)
	if e, ok := errors.AsType[*remoteError](err); ok {
		var data any
		if len(e.Data) > 0 {
			_ = json.Unmarshal(e.Data, &data)
		}
		return nil, seq, &Error{Code: e.Code, Message: e.Message, Data: data}
	}
	if err != nil {
		return nil, 0, unreachable
	}
	return raw, seq, nil
}

// maxOwnerRequestIDBytes bounds the request ids that a mirror sends.
const maxOwnerRequestIDBytes = 128

// ownerRequestID keeps a request id within maxOwnerRequestIDBytes, as the
// recipe has it: a longer one, such as <user_id>:<id> of a long id, is sent
// as a hash of it, which a resend repeats.
func ownerRequestID(id string) string {
	if len(id) <= maxOwnerRequestIDBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h:" + hex.EncodeToString(sum[:])
}

// waitApplied waits until the notifications up to seq are applied.
func (ms *mirror) waitApplied(ctx context.Context, seq int64) {
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	for {
		ms.mu.Lock()
		applied, ch := ms.applied, ms.appliedCh
		ms.mu.Unlock()
		if applied >= seq {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// forward sends a user's request in a mirror room to its owner, with the
// IDs mapped to the owner's, as the user's remote user there, and answers
// with the copy that the owner's record made here.
func (f *federation) forward(c *client, req request, ms *mirror) (any, bool, *Error) {
	s := f.s
	ctx := s.requestContext(c)
	s.mu.RLock()
	userID, name := c.user.id, c.user.name
	s.mu.RUnlock()
	if _, err := ms.ownerUser(ctx, userID, name); err != nil {
		return nil, false, err
	}
	s.mu.RLock()
	params, mapErr := ms.ownerParamsLocked(req, userID)
	s.mu.RUnlock()
	if mapErr != nil {
		return nil, false, mapErr
	}
	raw, seq, err := ms.call(ctx, userID+":"+req.id, req.method, params)
	if err != nil {
		if data, _ := err.Data.(map[string]any); data != nil {
			if ext, _ := data["ext"].(map[string]any); ext != nil && ext["remote"] == "not_joined" {
				f.linkLeft(req)
			}
		}
		return nil, false, err
	}
	ms.waitApplied(ctx, seq)
	// The owner took the request: the user is answered with its copy,
	// waiting for the owner's record, which comes before the result, but
	// may be read later, such as after a new connection. An error now would
	// make the user send it again.
	result := decodeObject(raw)
	var key string
	var copies map[string]string
	switch req.method {
	case "message":
		key, copies = fmt.Sprint(result["message_id"]), ms.copies
	case "room_set":
		key, copies = fmt.Sprint(result["room_id"]), ms.local
	default:
		return map[string]any{}, false, nil
	}
	deadline := time.Now().Add(mirrorCopyWait)
	for {
		ms.mu.Lock()
		changed := ms.appliedCh
		ms.mu.Unlock()
		s.mu.RLock()
		local := copies[key]
		s.mu.RUnlock()
		switch {
		case local != "" && req.method == "message":
			return map[string]any{"message_id": local}, false, nil
		case local != "":
			return map[string]any{"room_id": local}, false, nil
		case time.Now().After(deadline):
			// Safe now: the user's resend has the same request id, which
			// the owner answers as a duplicate.
			f.h.logger.Warn("the owner took a request whose record has not come", "owner", ms.server, "method", req.method, "id", key)
			return nil, false, &Error{Code: CodeRetryAfter, Message: "The room's owner took this; it has not arrived here yet", Data: map[string]any{"retry_after": 5}}
		}
		ms.wakeUp()
		select {
		case <-changed:
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, false, connectionClosing()
		}
	}
}

// linkLeft stops the mirror room of a request that the owner refused with
// not_joined: the link is no longer in the owner's room. The request's room
// is its room_id or parent_room_id, or its message's room; a thread's
// mirror room is the one it is a thread of.
func (f *federation) linkLeft(req request) {
	var target struct {
		RoomID       string `json:"room_id"`
		ParentRoomID string `json:"parent_room_id"`
		MessageID    string `json:"message_id"`
	}
	_ = json.Unmarshal(encodeJSON(req.params), &target)
	roomID := cmp.Or(target.RoomID, target.ParentRoomID)
	if roomID == "" {
		f.s.mu.RLock()
		if m := f.s.messages[target.MessageID]; m != nil {
			roomID = m.roomID
		}
		f.s.mu.RUnlock()
	}
	f.mu.Lock()
	room := f.rooms[roomID]
	for room != nil && room.Parent != "" {
		room = f.rooms[room.Parent]
	}
	var server, ownerRoom string
	if room != nil {
		server, ownerRoom = room.Server, room.OwnerRoom
	}
	f.mu.Unlock()
	if room != nil {
		f.markUnshared(server, ownerRoom)
	}
}

// mirrorCopyWait bounds how long an accepted request waits for its copy.
const mirrorCopyWait = time.Minute

// ownerParamsLocked maps a request in a mirror room to the owner's IDs, as
// the user's remote user there.
func (ms *mirror) ownerParamsLocked(req request, userID string) (map[string]any, *Error) {
	f := ms.f
	params := make(map[string]any, len(req.params))
	for key, value := range req.params {
		var decoded any
		_ = json.Unmarshal(value, &decoded)
		params[key] = decoded
	}
	ownerRoom := func(local string) (string, *Error) {
		f.mu.Lock()
		room := f.rooms[local]
		f.mu.Unlock()
		switch {
		case room == nil:
			return "", invalidParams("Unknown room %q", local)
		case room.Unshared:
			return "", &Error{Code: CodeDenied, Message: "This room is no longer shared; it takes no more writes"}
		}
		return room.OwnerRoom, nil
	}
	ownerMessage := func(local string) (string, *Error) {
		if id := ms.originals[local]; id != "" {
			return id, nil
		}
		return "", invalidParams("Unknown message %q", local)
	}
	for _, key := range []string{"room_id", "parent_room_id"} {
		if local, ok := params[key].(string); ok {
			id, err := ownerRoom(local)
			if err != nil {
				return nil, err
			}
			params[key] = id
		}
	}
	if req.method == "message" {
		if _, has := params["room_id"]; !has {
			id, err := ownerRoom(defaultRoomID)
			if err != nil {
				return nil, err
			}
			params["room_id"] = id
		}
	}
	if local, ok := params["message_id"].(string); ok {
		id, err := ownerMessage(local)
		if err != nil {
			return nil, err
		}
		params["message_id"] = id
	}
	if ref, ok := params["reply_to"].(map[string]any); ok {
		delete(params, "reply_to")
		if id := ms.originals[fmt.Sprint(ref["message_id"])]; id != "" {
			params["reply_to"] = map[string]any{"message_id": id}
		}
	}
	if body, ok := params["body"].(map[string]any); ok {
		mapped := make(map[string]string)
		for _, value := range asList(body["mentions"]) {
			if id, ok := value.(string); ok {
				mapped[id] = ms.ownerUserIDLocked(id)
			}
		}
		params["body"] = remoteBody(body, mapped)
	}
	ext, _ := params["ext"].(map[string]any)
	if ext == nil {
		ext = make(map[string]any)
	}
	ext[remoteExt] = map[string]any{"user_id": userID}
	params["ext"] = ext
	return params, nil
}

// ownerUserIDLocked is a user's user_id on the owner, for mentions: this
// server's user's remote user there, or the owner's own user, or a third
// server's user as the owner knows it.
func (ms *mirror) ownerUserIDLocked(localID string) string {
	f, s := ms.f, ms.f.s
	u := s.users[localID]
	if u != nil && u.shadow != nil {
		if u.shadow.server == ms.server {
			return u.shadow.remoteID
		}
		if id, err := shadowID(u.shadow.server, u.shadow.remoteID); err == nil {
			return id
		}
	}
	for ownerID, local := range ms.users {
		if local == localID {
			return ownerID
		}
	}
	return localID + "@" + f.selfAuthority
}

// profileChanged sends a local user's new name to the owners that the user
// wrote to. An ext:remote hook in updateProfile, called holding s.mu.
func (f *federation) profileChanged(userID string) {
	f.mu.Lock()
	mirrors := slices.Collect(maps.Values(f.mirrors))
	f.mu.Unlock()
	name := ""
	if u := f.s.users[userID]; u != nil {
		name = u.name
	}
	for _, ms := range mirrors {
		// Not Host.Go, which takes s.mu, held here.
		go func(ctx context.Context) {
			var ownerID string
			if found, _ := f.kv.Get("user "+ms.server+" "+userID, &ownerID); found {
				if _, err := ms.sendProfile(ctx, userID, name); err != nil {
					f.h.logger.Warn("sending a profile to the owner failed", "owner", ms.server, "user_id", userID, "error", err)
				}
			}
		}(f.s.extContext)
	}
}

// ---- Memberships ----

// isLink reports a user object of a link: ext.remote names a server and no
// user.
func isLink(object map[string]any) bool {
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext[remoteExt].(map[string]any)
	server, _ := remote["server"].(string)
	user, _ := remote["user_id"].(string)
	return server != "" && user == ""
}

// ownUser reports a user object of this server's user's remote user on the
// owner, and returns that user's user_id here.
func (ms *mirror) ownUser(object map[string]any) (string, bool) {
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext[remoteExt].(map[string]any)
	server, _ := remote["server"].(string)
	user, _ := remote["user_id"].(string)
	normalized, err := normalizeAddress(server)
	return user, err == nil && normalized == ms.f.self && user != ""
}

// boundHere reports a user object of a link bound to this server.
func (ms *mirror) boundHere(object map[string]any) bool {
	ext, _ := object["ext"].(map[string]any)
	remote, _ := ext[remoteExt].(map[string]any)
	server, _ := remote["server"].(string)
	normalized, err := normalizeAddress(server)
	return err == nil && normalized == ms.f.self
}

// membershipLocked copies a membership record of the owner's: remote users
// join and leave the mirror room as they did the owner's. Links are no
// members here, and this server's own users' remote users stand for its
// users, who are members already. A leave of this server's link stops the
// mirror room.
func (ms *mirror) membershipLocked(record map[string]any, logID int64) {
	s := ms.f.s
	ownerRoom, _ := record["room_id"].(string)
	r, ok := ms.roomFor(ownerRoom, logID)
	if !ok {
		return
	}
	for _, value := range asList(record["members"]) {
		entry, _ := value.(map[string]any)
		object, _ := entry["user"].(map[string]any)
		if isLink(object) {
			// This server's link left: the room is no longer shared.
			if entry["joined"] == false && ms.boundHere(object) {
				go ms.f.markUnshared(ms.server, ownerRoom) // it takes s.mu, held here
			}
			continue
		}
		if _, own := ms.ownUser(object); own {
			continue
		}
		u := ms.userLocked(object)
		switch {
		case u == nil || u.shadow == nil:
		case entry["joined"] == true && !u.removed[r.id]:
			s.joinLocked(u, r)
		case entry["joined"] == false:
			s.leaveLocked(u, r)
		}
	}
}

// membershipChangedLocked queues a local user's join or leave of a mirror
// room, or a thread of one, for the owner. An ext:remote hook in
// logMembershipLocked, called holding s.mu.
func (f *federation) membershipChangedLocked(u *userState, r *roomState, joined bool) {
	if u.link != nil && !joined {
		f.linkLeftLocked(u, r)
		return
	}
	if u.shadow != nil || u.link != nil {
		return
	}
	f.mu.Lock()
	room := f.rooms[r.id]
	var ms *mirror
	if room != nil && !room.Unshared {
		ms = f.mirrors[room.Server]
	}
	f.mu.Unlock()
	if ms != nil {
		ms.queueMembershipLocked(membershipChange{ID: randomID(), UserID: u.id, Name: u.name, OwnerRoom: room.OwnerRoom, Joined: joined})
	}
}

// queueMembershipLocked queues a join or leave for the owner, in place of
// one of the same user and room that has not gone yet, and keeps the
// queue. Called holding s.mu.
func (ms *mirror) queueMembershipLocked(change membershipChange) {
	ms.memberships = slices.DeleteFunc(ms.memberships, func(queued membershipChange) bool {
		return queued.UserID == change.UserID && queued.OwnerRoom == change.OwnerRoom
	})
	ms.memberships = append(ms.memberships, change)
	ms.saveMembershipsLocked()
	ms.sendMembershipsNow()
}

func (ms *mirror) saveMembershipsLocked() {
	if err := ms.f.kv.setLocked("memberships "+ms.server, encodeJSON(ms.memberships)); err != nil {
		ms.f.h.logger.Warn("the joins and leaves for the owner were not kept", "owner", ms.server, "error", err)
	}
}

// sendMembershipsNow asks sendMemberships to send the queue.
func (ms *mirror) sendMembershipsNow() {
	select {
	case ms.sendDue <- struct{}{}:
	default:
	}
}

// sendMemberships sends the queued joins and leaves to the owner, in order,
// as room_join and room_leave of the users' remote users, when asked: on a
// change, and on each connection. One that the owner did not answer stays
// queued, with those after it, until the next connection or membershipRetry.
func (ms *mirror) sendMemberships(ctx context.Context) {
	s := ms.f.s
	for {
		select {
		case <-ms.sendDue:
		case <-ctx.Done():
			return
		}
		for {
			s.mu.RLock()
			if len(ms.memberships) == 0 {
				s.mu.RUnlock()
				break
			}
			change := ms.memberships[0]
			s.mu.RUnlock()
			if !ms.sendMembership(ctx, change) {
				// Again on the next connection, or after a while, such as
				// after the owner's retry_after.
				time.AfterFunc(membershipRetry, ms.sendMembershipsNow)
				break
			}
			s.mu.Lock()
			if i := slices.Index(ms.memberships, change); i >= 0 {
				ms.memberships = slices.Delete(ms.memberships, i, i+1)
				ms.saveMembershipsLocked()
			}
			s.unlock()
		}
	}
}

// membershipRetry is when a join or leave that got no answer is sent again,
// if no connection came first.
const membershipRetry = 30 * time.Second

// sendMembership sends one join or leave, and reports whether the owner
// answered it: a refusal is an answer, and the change is dropped.
func (ms *mirror) sendMembership(ctx context.Context, change membershipChange) bool {
	logger := ms.f.h.logger.With("owner", ms.server, "user_id", change.UserID)
	method := "room_leave"
	if change.Joined {
		method = "room_join"
		if _, err := ms.ownerUser(ctx, change.UserID, change.Name); err != nil {
			logger.Warn("a join did not reach the owner", "error", err)
			return err.Code != CodeRetryAfter
		}
	} else if found, _ := ms.f.kv.Get("user "+ms.server+" "+change.UserID, new(string)); !found {
		return true // never on the owner
	}
	params := map[string]any{"room_id": change.OwnerRoom, "ext": map[string]any{"remote": map[string]any{"user_id": change.UserID}}}
	if _, _, err := ms.call(ctx, change.UserID+":"+method+"-"+change.ID, method, params); err != nil {
		logger.Warn("a membership did not reach the owner", "method", method, "error", err)
		return err.Code != CodeRetryAfter
	}
	return true
}
