// Command federation is an Apron server with a room federated with a room
// on another Apron server, both ways, through a link account there (the
// ext:remote link primitive, trusted federation v2): one bridge, in this
// server, holds the whole mapping.
//
// The other server's operator runs `/remote invite <this server's address>`
// in the room to share, which makes a link account bound to this server and
// replies with a link to it. Give the bridge that link:
//
//	go run ./examples/federation --addr 127.0.0.1:8081 --store memory \
//	  --federation.link 'https://link.apron.chat/#server=ws://127.0.0.1:8787/ws&room=<room_id>&token=<token>' \
//	  --federation.room ops --federation.self ws://127.0.0.1:8081/ws
//
// The bridge connects to the other server (B) as the link, with the token,
// and keeps the connection. What people post in the local room goes to B's
// room as their shadow users there (`alice@<this server's authority>`): the
// link sends ordinary requests with params.ext.remote.user_id, which B runs
// as that user's shadow. What B's users post comes here through this
// server's shadow users of them (apron.Host.Shadow), `bob@<B's authority>`.
// Each copy carries the original's IDs in ext.remote, which is all the
// mapping there is: the bridge rebuilds it from both rooms at each start,
// and keeps only its place in B's log.
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Federation{}) }

// Federation bridges a local room with a room on a remote Apron server.
type Federation struct {
	Settings struct {
		Link string `long:"link" description:"The link that /remote invite on the remote server replied with: https://link.apron.chat/#server=…&room=…&token=…"`
		Room string `long:"room" default:"ops" description:"The local room to federate; made, public, if there is none"`
		Self string `long:"self" description:"This server's address, as the remote server knows it, such as wss://a.example/ws"`
		Bot  string `long:"bot" default:"federation" description:"The bridge's bot's user_id"`
	}

	host *apron.Host
	bot  *apron.Bot
	kv   apron.KV
	log  logger

	// From the settings: the remote server's address, room, and the
	// link's token; the authorities of both servers (apron.RemoteAuthority).
	remote, remoteRoom, linkToken  string
	remoteAuthority, selfAuthority string

	mu sync.Mutex
	// pairs maps each message to its copy, both ways: a local message_id
	// to its copy's on the remote server, and a remote one to its copy's
	// here. Both kinds of message_id are decimal Unix milliseconds of
	// different servers, so the two directions are kept apart.
	localToRemote, remoteToLocal map[string]string
	// conn is the connection to the remote server once it caught up, nil
	// otherwise; connected is closed when it is set, and replaced when it
	// is cleared.
	conn      *remoteConn
	connected chan struct{}
	// linkID is the link's user_id on the remote server.
	linkID string
	// stopped is set once the remote server refused the link (denied):
	// the bridge stops. scanned is set once the bridge read the whole
	// remote room at this start, for the copies there; later connections
	// catch up from the checkpoint only.
	stopped, scanned bool
	// sentNames are the names the bridge last gave local users' shadows on
	// the remote server, and shadowNames those it gave remote users'
	// shadows here.
	sentNames   map[string]string
	shadowNames map[string]string
	// shadows are the remote users' shadows here, by their remote user_id,
	// that joined the room.
	shadows map[string]*apron.Bot
	// profiles carries the local users' profile changes to the remote
	// server.
	profiles chan profileChange
}

type profileChange struct{ userID, name string }

type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

func (*Federation) Name() string { return "federation" }
func (f *Federation) Flags() any { return &f.Settings }

// parseLink reads an Apron link (PROTOCOL.md Appendix A.4) for its server,
// room, and token.
func parseLink(link string) (server, room, token string, err error) {
	var query string
	switch {
	case strings.HasPrefix(link, "apron:?"):
		query = strings.TrimPrefix(link, "apron:?")
	default:
		_, query, _ = strings.Cut(link, "#")
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", "", "", fmt.Errorf("the link: %w", err)
	}
	server, room, token = values.Get("server"), values.Get("room"), values.Get("token")
	if server == "" || room == "" || token == "" {
		return "", "", "", errors.New("the link needs server, room, and token")
	}
	return server, room, token, nil
}

func (f *Federation) Init(r *apron.Registrar) error {
	s := &f.Settings
	if s.Link == "" || s.Self == "" {
		return errors.New("set --federation.link and --federation.self")
	}
	s.Room = cmp.Or(s.Room, "ops")
	s.Bot = cmp.Or(s.Bot, "federation")
	var err error
	if f.remote, f.remoteRoom, f.linkToken, err = parseLink(s.Link); err != nil {
		return err
	}
	if f.remoteAuthority, err = apron.RemoteAuthority(f.remote); err != nil {
		return err
	}
	if f.selfAuthority, err = apron.RemoteAuthority(s.Self); err != nil {
		return fmt.Errorf("--federation.self: %w", err)
	}
	if f.remoteAuthority == f.selfAuthority {
		return errors.New("the link is to this server")
	}
	f.host, f.kv, f.log = r.Host, r.Store(), r.Logger()
	f.localToRemote, f.remoteToLocal = make(map[string]string), make(map[string]string)
	f.sentNames, f.shadowNames = make(map[string]string), make(map[string]string)
	f.shadows = make(map[string]*apron.Bot)
	f.connected = make(chan struct{})
	f.profiles = make(chan profileChange, 256)

	if _, created, err := r.CreateRoom(s.Room, apron.RoomOptions{Title: s.Room}); err != nil {
		return err
	} else if created {
		f.log.Info("created the federated room", "room", s.Room)
	}
	if f.bot, err = r.Bot(s.Bot, apron.BotOptions{Name: "Federation"}); err != nil {
		return err
	}
	if err := f.bot.Join(r.Context(), s.Room); err != nil {
		return err
	}
	// Profile changes have no event: the bot hears them as `user`
	// notifications of the users it shares a room with (§3.3.3).
	if _, err := f.bot.Connect(apron.SessionOptions{OnFrame: f.onLocalFrame}); err != nil {
		return err
	}
	// Follow keeps the bridge's place in this server's log, so that what is
	// posted while the remote server is away goes there once it is back.
	r.Follow(f.outbound)
	r.Go(f.run)
	r.Go(f.sendProfiles)
	return nil
}

// ---- IDs ----

// sameServer reports whether an address is the server whose authority is
// given.
func sameServer(address, authority string) bool {
	a, err := apron.RemoteAuthority(address)
	return err == nil && a == authority
}

// remoteUserOf is the user_id on the remote server of a local user: the
// original of a shadow of the remote server's user, else the user's
// shadow there, `<user_id>@<this server's authority>`.
func (f *Federation) remoteUserOf(localID string) string {
	if strings.HasPrefix(localID, "~") {
		return localID
	}
	if u, ok := f.host.User(localID); ok && u.Remote != nil && sameServer(u.Remote.Server, f.remoteAuthority) {
		return u.Remote.UserID
	}
	return localID + "@" + f.selfAuthority
}

// localUserOf is the user_id here of a user of the remote server: the
// original of the remote server's shadow of a local user, else the user's
// shadow here.
func (f *Federation) localUserOf(remoteID string) string {
	if local, ok := strings.CutSuffix(remoteID, "@"+f.selfAuthority); ok && local != "" {
		return local
	}
	if strings.HasPrefix(remoteID, "~") {
		return remoteID
	}
	id, err := apron.ShadowID(f.remote, remoteID)
	if err != nil {
		return remoteID
	}
	return id
}

// pair records that a local message and a remote one are copies.
func (f *Federation) pair(localID, remoteID string) {
	if localID == "" || remoteID == "" {
		return
	}
	f.mu.Lock()
	f.localToRemote[localID] = remoteID
	f.remoteToLocal[remoteID] = localID
	f.mu.Unlock()
}

func (f *Federation) remoteOf(localID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localToRemote[localID]
}

func (f *Federation) localOf(remoteID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remoteToLocal[remoteID]
}

// remoteExt is a record's ext.remote (ext:remote).
type remoteExt struct {
	Server    string `json:"server"`
	UserID    string `json:"user_id"`
	RoomID    string `json:"room_id"`
	MessageID string `json:"message_id"`
}

// record is a message record, as both servers send it (§3.5).
type record struct {
	MessageID string `json:"message_id"`
	LogID     string `json:"log_id"`
	RoomID    string `json:"room_id"`
	From      struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
		Ext    struct {
			Remote *remoteExt `json:"remote"`
		} `json:"ext"`
	} `json:"from"`
	Body    map[string]any `json:"body"`
	ReplyTo *struct {
		MessageID string `json:"message_id"`
	} `json:"reply_to"`
	Deleted bool `json:"deleted"`
	Ext     struct {
		Remote *remoteExt `json:"remote"`
	} `json:"ext"`
}

// reactionsRecord is a reactions record (§4.7).
type reactionsRecord struct {
	LogID     string `json:"log_id"`
	MessageID string `json:"message_id"`
	RoomID    string `json:"room_id"`
	Reactions []struct {
		From struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
			Ext    struct {
				Remote *remoteExt `json:"remote"`
			} `json:"ext"`
		} `json:"from"`
		Emojis []string `json:"emojis"`
	} `json:"reactions"`
}

func parseLogID(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

// requestID names an operation by what it is, so that a retry of the same
// one, after a reconnect or a restart, is deduplicated (§1.2), and a
// changed one is a new operation.
func requestID(prefix string, params any) string {
	encoded, _ := json.Marshal(params, json.Deterministic(true))
	sum := sha256.Sum256(encoded)
	return prefix + "-" + hex.EncodeToString(sum[:8])
}

// ---- local → remote ----

// localAuthor reports whether records by userID go to the remote server:
// a local user's, not the bridge's bot's, a shadow's (the remote server's
// users and other servers' come from their own server), or a system
// identity's.
func (f *Federation) localAuthor(userID string) bool {
	if userID == "" || strings.HasPrefix(userID, "~") || userID == f.bot.UserID() {
		return false
	}
	u, ok := f.host.User(userID)
	return !ok || u.Remote == nil
}

// outbound sends a record of the local room to the remote server. It waits
// while the bridge is not connected, so that Follow keeps its place.
func (f *Federation) outbound(ctx context.Context, e apron.Event) error {
	if !slices.Contains(e.Rooms, f.Settings.Room) {
		return nil
	}
	switch e.Kind {
	case apron.EventMessage:
		var m record
		if err := json.Unmarshal(e.Record, &m); err != nil {
			return apron.SkipEvent(err)
		}
		return f.outboundMessage(ctx, e.LogID, m)
	case apron.EventReactions:
		var r reactionsRecord
		if err := json.Unmarshal(e.Record, &r); err != nil {
			return apron.SkipEvent(err)
		}
		return f.outboundReactions(ctx, e.LogID, r)
	}
	return nil
}

func (f *Federation) outboundMessage(ctx context.Context, logID int64, m record) error {
	if !f.localAuthor(m.From.UserID) {
		return nil
	}
	copyID := f.remoteOf(m.MessageID)
	act := map[string]any{"user_id": m.From.UserID}
	switch {
	case m.Deleted || m.RoomID != f.Settings.Room:
		// A deletion, or a move out of the room, deletes the copy.
		if copyID == "" {
			return nil
		}
		params := map[string]any{"message_id": copyID, "room_id": f.remoteRoom, "deleted": true, "ext": map[string]any{"remote": act}}
		_, err := f.callRemote(ctx, requestID("del-"+strconv.FormatInt(logID, 10), params), "message", params)
		return f.skipRefused(err, "deleting a copy", "message_id", m.MessageID)
	}
	if err := f.introduce(ctx, m.From.UserID, m.From.Name); err != nil {
		return err
	}
	act["room_id"], act["message_id"] = f.Settings.Room, m.MessageID
	// A save sends every client field again (§4.4).
	params := map[string]any{
		"room_id": f.remoteRoom,
		"body":    mapBody(m.Body, f.remoteUserOf),
		"ext":     map[string]any{"remote": act},
	}
	if m.ReplyTo != nil {
		if target := f.remoteOf(m.ReplyTo.MessageID); target != "" {
			params["reply_to"] = map[string]any{"message_id": target}
		}
	}
	if copyID != "" {
		params["message_id"] = copyID
	}
	raw, err := f.callRemote(ctx, requestID("msg-"+strconv.FormatInt(logID, 10), params), "message", params)
	if err != nil {
		return f.skipRefused(err, "copying a message", "message_id", m.MessageID)
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(raw, &result) == nil {
		f.pair(m.MessageID, result.MessageID)
	}
	return nil
}

func (f *Federation) outboundReactions(ctx context.Context, logID int64, r reactionsRecord) error {
	target := f.remoteOf(r.MessageID)
	if target == "" {
		return nil
	}
	for _, set := range r.Reactions {
		if !f.localAuthor(set.From.UserID) {
			continue
		}
		if err := f.introduce(ctx, set.From.UserID, set.From.Name); err != nil {
			return err
		}
		params := map[string]any{"message_id": target, "emojis": emojiSet(set.Emojis), "ext": map[string]any{"remote": map[string]any{"user_id": set.From.UserID}}}
		_, err := f.callRemote(ctx, requestID("react-"+strconv.FormatInt(logID, 10), params), "reactions", params)
		if err := f.skipRefused(err, "copying reactions", "message_id", r.MessageID); err != nil {
			return err
		}
	}
	return nil
}

// introduce gives a local user's shadow on the remote server the user's
// name, the first time the bridge copies something of theirs, and when the
// name changed.
func (f *Federation) introduce(ctx context.Context, userID, recorded string) error {
	name := recorded
	if u, ok := f.host.User(userID); ok {
		name = u.Name
	}
	f.mu.Lock()
	sent, known := f.sentNames[userID]
	f.mu.Unlock()
	if known && sent == name || !known && name == "" {
		f.mu.Lock()
		f.sentNames[userID] = name
		f.mu.Unlock()
		return nil
	}
	params := map[string]any{"name": name, "ext": map[string]any{"remote": map[string]any{"user_id": userID}}}
	// A name can come back, so each change is an operation of its own.
	_, err := f.callRemote(ctx, requestID("me-"+userID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), params), "me", params)
	if err := f.skipRefused(err, "copying a profile", "user_id", userID); err != nil {
		return err
	}
	f.mu.Lock()
	f.sentNames[userID] = name
	f.mu.Unlock()
	return nil
}

// onLocalFrame hears the bot's notifications, for local users' profile
// changes.
func (f *Federation) onLocalFrame(raw jsontext.Value) {
	var note struct {
		Method string `json:"method"`
		Params struct {
			New *struct {
				UserID string  `json:"user_id"`
				Name   *string `json:"name"`
			} `json:"new"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &note) != nil || note.Method != "user" || note.Params.New == nil || note.Params.New.Name == nil {
		return
	}
	select {
	case f.profiles <- profileChange{note.Params.New.UserID, *note.Params.New.Name}:
	default:
	}
}

// sendProfiles copies the name changes of local users whose shadows the
// bridge introduced to the remote server.
func (f *Federation) sendProfiles(ctx context.Context) {
	for {
		select {
		case change := <-f.profiles:
			f.mu.Lock()
			sent, known := f.sentNames[change.userID]
			f.mu.Unlock()
			if known && sent != change.name && f.localAuthor(change.userID) {
				if err := f.introduce(ctx, change.userID, change.name); err != nil {
					f.log.Warn("copying a profile failed", "user_id", change.userID, "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// callRemote sends a request to the remote server, as the link, once the
// bridge is connected and caught up, and again after a reconnect or a
// retry_after, until it has a reply.
func (f *Federation) callRemote(ctx context.Context, id, method string, params any) (jsontext.Value, error) {
	for {
		conn, err := f.waitConnected(ctx)
		if err != nil {
			return nil, err
		}
		result, err := conn.call(ctx, id, method, params)
		switch {
		case errors.Is(err, errDisconnected):
			continue
		case retryAfter(err) > 0:
			if err := sleep(ctx, retryAfter(err)); err != nil {
				return nil, err
			}
			continue
		}
		return result, err
	}
}

// skipRefused logs a request the remote server refused, which is not sent
// again, and returns any other error.
func (f *Federation) skipRefused(err error, what string, args ...any) error {
	if _, refused := errors.AsType[*rpcError](err); refused {
		f.log.Warn(what+" was refused", append(args, "error", err)...)
		return nil
	}
	return err
}

func (f *Federation) waitConnected(ctx context.Context) (*remoteConn, error) {
	for {
		f.mu.Lock()
		conn, connected := f.conn, f.connected
		f.mu.Unlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-connected:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- the connection ----

// errStopped ends the bridge: the remote server refused the link.
var errStopped = errors.New("the remote server refused the link")

// run keeps a connection to the remote server, waiting longer after each
// failure, until the remote server refuses the link (denied, §1.1).
func (f *Federation) run(ctx context.Context) {
	if err := f.rebuildLocal(ctx); err != nil {
		f.log.Error("reading the local room failed", "error", err)
	}
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := f.session(ctx)
		if errors.Is(err, errStopped) {
			f.log.Error("the bridge stopped: the remote server refused the link; get a new link with /remote invite", "error", err)
			f.mu.Lock()
			f.stopped = true
			f.mu.Unlock()
			return
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		f.log.Warn("the connection to the remote server ended", "error", err, "retry_in", delay)
		if sleep(ctx, delay) != nil {
			return
		}
		delay = min(2*delay, 5*time.Minute)
	}
}

// rebuildLocal reads the local room for the copies of remote messages,
// which name their originals in ext.remote.
func (f *Federation) rebuildLocal(ctx context.Context) error {
	after := int64(1)
	for {
		raw, err := f.bot.Call(ctx, "history", map[string]any{"room_id": f.Settings.Room, "after": strconv.FormatInt(after, 10), "limit": 500})
		if err != nil {
			return err
		}
		var page historyPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, raw := range page.Messages {
			var m record
			if json.Unmarshal(raw, &m) == nil && m.Ext.Remote != nil && sameServer(m.Ext.Remote.Server, f.remoteAuthority) {
				f.pair(m.MessageID, m.Ext.Remote.MessageID)
			}
		}
		if !page.More || page.LastLogID == "" {
			return nil
		}
		after = parseLogID(page.LastLogID) + 1
	}
}

// historyPage is a history result (§4.3).
type historyPage struct {
	Messages     []jsontext.Value `json:"messages"`
	Reactions    []jsontext.Value `json:"reactions"`
	LastLogID    string           `json:"last_log_id"`
	LatestLogID  string           `json:"latest_log_id"`
	HistoryLogID *string          `json:"history_log_id"`
	More         bool             `json:"more"`
}

// tokenState is the link's token, kept as the remote server rotates it
// (§3.2.1), with the token of the link it came from.
type tokenState struct {
	Link  string `json:"link"`
	Token string `json:"token"`
}

// cursorKey keeps the log_id of the last record of the remote room that the
// bridge handled.
func (f *Federation) cursorKey() string { return "cursor " + f.remote + " " + f.remoteRoom }

// session connects to the remote server, signs in as the link, catches up
// with history, and handles what the room delivers until the connection
// ends.
func (f *Federation) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := dialRemote(dialCtx, f.remote)
	cancel()
	if err != nil {
		return err
	}
	defer conn.close()
	stop := context.AfterFunc(ctx, conn.close)
	defer stop()

	// The server frame comes first (§3.1).
	var server struct {
		Capabilities []string `json:"capabilities"`
		Ping         float64  `json:"ping"`
	}
	select {
	case raw := <-conn.server:
		_ = json.Unmarshal(raw, &server)
	case <-conn.done:
		return conn.err
	case <-time.After(30 * time.Second):
		return errors.New("no server frame")
	}
	if !slices.Contains(server.Capabilities, "ext:remote") {
		f.log.Warn("the remote server does not advertise ext:remote; its link may not act as shadows", "server", f.remote)
	}
	if server.Ping > 0 {
		go conn.ping(time.Duration(server.Ping * float64(time.Second)))
	}

	var kept tokenState
	token := f.linkToken
	if found, _ := f.kv.Get("token", &kept); found && kept.Link == f.linkToken && kept.Token != "" {
		token = kept.Token
	}
	raw, err := conn.call(ctx, "auth-"+strconv.FormatInt(time.Now().UnixNano(), 36), "auth", map[string]any{"scheme": "token", "token": token, "agent": "apron-go-federation/0"})
	if e, ok := errors.AsType[*rpcError](err); ok && e.Code == codeDenied {
		return errors.Join(errStopped, err)
	} else if err != nil {
		return err
	}
	var auth struct {
		You struct {
			UserID string `json:"user_id"`
		} `json:"you"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil || auth.You.UserID == "" {
		return fmt.Errorf("auth result %s", raw)
	}
	if auth.Token != "" && auth.Token != token {
		if err := f.kv.Put("token", tokenState{Link: f.linkToken, Token: auth.Token}); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.linkID = auth.You.UserID
	f.mu.Unlock()
	f.log.Info("connected to the remote server", "server", f.remote, "room", f.remoteRoom, "link", auth.You.UserID)

	// Catch up: read the whole room for the copies there, which name their
	// originals in ext.remote, and copy what came after the checkpoint.
	// Live records queue meanwhile, and those already read are skipped.
	var cursor int64
	found, err := f.kv.Get(f.cursorKey(), &cursor)
	if err != nil {
		return err
	}
	f.mu.Lock()
	scanned := f.scanned
	f.mu.Unlock()
	after := int64(1)
	if scanned && found {
		after = cursor + 1
	}
	for {
		raw, err := conn.call(ctx, "history-"+strconv.FormatInt(after, 10)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), "history", map[string]any{"room_id": f.remoteRoom, "after": strconv.FormatInt(after, 10), "limit": 200})
		if err != nil {
			if e, ok := errors.AsType[*rpcError](err); ok && e.Code == codeDenied {
				return errors.Join(errStopped, err)
			}
			return err
		}
		var page historyPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		if !found {
			// The first time, the bridge starts at the room's latest record,
			// as Follow does: the room's past stays where it was.
			cursor, found = parseLogID(page.LatestLogID), true
			if err := f.kv.Put(f.cursorKey(), cursor); err != nil {
				return err
			}
		}
		for _, item := range mergeByLogID(page.Messages, page.Reactions) {
			if item.logID <= cursor {
				if item.kind == "message" {
					f.learn(item.raw)
				}
				continue
			}
			if err := f.inbound(ctx, item.kind, item.raw); err != nil {
				return err
			}
			cursor = item.logID
			if err := f.kv.Put(f.cursorKey(), cursor); err != nil {
				return err
			}
		}
		if !page.More || page.LastLogID == "" {
			break
		}
		after = parseLogID(page.LastLogID) + 1
	}

	f.mu.Lock()
	f.scanned = true
	f.conn = conn
	close(f.connected)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.conn = nil
		f.connected = make(chan struct{})
		f.mu.Unlock()
	}()

	for {
		select {
		case note := <-conn.notes:
			switch note.Method {
			case "message", "reactions":
				var at struct {
					LogID string `json:"log_id"`
				}
				_ = json.Unmarshal(note.Params, &at)
				logID := parseLogID(at.LogID)
				if logID == 0 || logID <= cursor {
					// A transient notice, or a record read already.
					if note.Method == "message" {
						f.learn(note.Params)
					}
					continue
				}
				if err := f.inbound(ctx, note.Method, note.Params); err != nil {
					return err
				}
				cursor = logID
				if err := f.kv.Put(f.cursorKey(), cursor); err != nil {
					return err
				}
			case "user":
				f.remoteProfile(ctx, note.Params)
			}
		case <-conn.done:
			return conn.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type logItem struct {
	logID int64
	kind  string
	raw   jsontext.Value
}

// mergeByLogID orders a history page's messages and reactions, each
// ascending, into one sequence by log_id.
func mergeByLogID(messages, reactions []jsontext.Value) []logItem {
	var items []logItem
	add := func(kind string, records []jsontext.Value) {
		for _, raw := range records {
			var at struct {
				LogID string `json:"log_id"`
			}
			if json.Unmarshal(raw, &at) == nil {
				items = append(items, logItem{parseLogID(at.LogID), kind, raw})
			}
		}
	}
	add("message", messages)
	add("reactions", reactions)
	slices.SortStableFunc(items, func(a, b logItem) int { return cmp.Compare(a.logID, b.logID) })
	return items
}

// ---- remote → local ----

// remoteAuthor reports whether records by a user of the remote server come
// here: not the link's, not the remote server's shadows of this server's
// users (their records are copies of this server's), and not a system
// identity's.
func (f *Federation) remoteAuthor(userID string, remote *remoteExt) bool {
	f.mu.Lock()
	link := f.linkID
	f.mu.Unlock()
	switch {
	case userID == "", userID == link, strings.HasPrefix(userID, "~"):
		return false
	case remote != nil && sameServer(remote.Server, f.selfAuthority):
		return false
	case strings.HasSuffix(userID, "@"+f.selfAuthority):
		return false
	}
	return true
}

// learn pairs a copy on the remote server of a local message with it.
func (f *Federation) learn(raw jsontext.Value) {
	var m record
	if json.Unmarshal(raw, &m) != nil || m.Ext.Remote == nil || m.Ext.Remote.MessageID == "" {
		return
	}
	if (m.Ext.Remote.RoomID == "" || m.Ext.Remote.RoomID == f.Settings.Room) && !f.remoteAuthor(m.From.UserID, m.From.Ext.Remote) {
		f.pair(m.Ext.Remote.MessageID, m.MessageID)
	}
}

// inbound copies a record of the remote room here. Only a closed server
// is an error: what this server refuses is logged and skipped.
func (f *Federation) inbound(ctx context.Context, kind string, raw jsontext.Value) error {
	var err error
	switch kind {
	case "message":
		f.learn(raw)
		var m record
		if json.Unmarshal(raw, &m) != nil || m.MessageID == "" {
			return nil
		}
		err = f.inboundMessage(ctx, m)
	case "reactions":
		var r reactionsRecord
		if json.Unmarshal(raw, &r) != nil {
			return nil
		}
		err = f.inboundReactions(ctx, r)
	}
	if errors.Is(err, apron.ErrServerClosed) || ctx.Err() != nil {
		return cmp.Or(ctx.Err(), err)
	}
	if err != nil {
		f.log.Warn("copying a record of the remote room failed", "kind", kind, "error", err)
	}
	return nil
}

func (f *Federation) inboundMessage(ctx context.Context, m record) error {
	if !f.remoteAuthor(m.From.UserID, m.From.Ext.Remote) {
		return nil
	}
	copyID := f.localOf(m.MessageID)
	if (m.Deleted || m.RoomID != f.remoteRoom) && copyID == "" {
		return nil
	}
	shadow, err := f.shadow(ctx, m.From.UserID, m.From.Name)
	if err != nil {
		return err
	}
	if m.Deleted || m.RoomID != f.remoteRoom {
		_, err := callLocal(ctx, shadow, "message", map[string]any{"message_id": copyID, "room_id": f.Settings.Room, "deleted": true})
		return err
	}
	params := map[string]any{
		"room_id": f.Settings.Room,
		"body":    mapBody(m.Body, f.localUserOf),
		"ext":     map[string]any{"remote": map[string]any{"server": f.remote, "user_id": m.From.UserID, "room_id": f.remoteRoom, "message_id": m.MessageID}},
	}
	if m.ReplyTo != nil {
		if target := f.localOf(m.ReplyTo.MessageID); target != "" {
			params["reply_to"] = map[string]any{"message_id": target}
		}
	}
	if copyID != "" {
		params["message_id"] = copyID
	}
	raw, err := callLocal(ctx, shadow, "message", params)
	if err != nil {
		return err
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal(raw, &result) == nil {
		f.pair(result.MessageID, m.MessageID)
	}
	return nil
}

func (f *Federation) inboundReactions(ctx context.Context, r reactionsRecord) error {
	target := f.localOf(r.MessageID)
	if target == "" || r.RoomID != f.remoteRoom {
		return nil
	}
	var errs []error
	for _, set := range r.Reactions {
		if !f.remoteAuthor(set.From.UserID, set.From.Ext.Remote) {
			continue
		}
		shadow, err := f.shadow(ctx, set.From.UserID, set.From.Name)
		if err == nil {
			_, err = callLocal(ctx, shadow, "reactions", map[string]any{"message_id": target, "emojis": emojiSet(set.Emojis)})
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// remoteProfile copies a remote user's name change to their shadow here,
// if the bridge made one.
func (f *Federation) remoteProfile(ctx context.Context, params jsontext.Value) {
	var note struct {
		New *struct {
			UserID string  `json:"user_id"`
			Name   *string `json:"name"`
			Ext    struct {
				Remote *remoteExt `json:"remote"`
			} `json:"ext"`
		} `json:"new"`
	}
	if json.Unmarshal(params, &note) != nil || note.New == nil || note.New.Name == nil || !f.remoteAuthor(note.New.UserID, note.New.Ext.Remote) {
		return
	}
	id, err := apron.ShadowID(f.remote, note.New.UserID)
	if err != nil {
		return
	}
	if _, ok := f.host.User(id); !ok {
		return
	}
	if _, err := f.shadow(ctx, note.New.UserID, *note.New.Name); err != nil {
		f.log.Warn("copying a profile failed", "user_id", note.New.UserID, "error", err)
	}
}

// shadow returns the shadow here of a remote user, a member of the room
// with the name given, if any.
func (f *Federation) shadow(ctx context.Context, remoteID, name string) (*apron.Bot, error) {
	f.mu.Lock()
	shadow := f.shadows[remoteID]
	f.mu.Unlock()
	if shadow == nil {
		var err error
		if shadow, err = f.host.Shadow(f.remote, remoteID); err != nil {
			return nil, err
		}
		// The bot adds it, as a member may add anyone to a room, a private
		// one too (§4.2.2).
		if _, err := callLocal(ctx, f.bot, "room_join", map[string]any{"room_id": f.Settings.Room, "user_id": shadow.UserID()}); err != nil {
			return nil, err
		}
		if u, ok := f.host.User(shadow.UserID()); ok {
			f.mu.Lock()
			f.shadowNames[shadow.UserID()] = u.Name
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.shadows[remoteID] = shadow
		f.mu.Unlock()
	}
	f.mu.Lock()
	current := f.shadowNames[shadow.UserID()]
	f.mu.Unlock()
	if name != "" && name != current {
		if _, err := callLocal(ctx, shadow, "me", map[string]any{"name": name}); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.shadowNames[shadow.UserID()] = name
		f.mu.Unlock()
	}
	return shadow, nil
}

// callLocal sends a request as a bot or shadow here, waiting out
// retry_after.
func callLocal(ctx context.Context, b *apron.Bot, method string, params any) (jsontext.Value, error) {
	for {
		result, err := b.Call(ctx, method, params)
		var e *apron.Error
		if errors.As(err, &e) && e.Code == apron.CodeRetryAfter {
			delay := time.Second
			if data, ok := e.Data.(map[string]any); ok {
				switch seconds := data["retry_after"].(type) {
				case float64:
					delay = time.Duration(seconds * float64(time.Second))
				case int64:
					delay = time.Duration(seconds) * time.Second
				}
			}
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return result, err
	}
}

// emojiSet is a reaction set as a request sends it: [] for none.
func emojiSet(emojis []string) []string {
	if emojis == nil {
		return []string{}
	}
	return emojis
}
