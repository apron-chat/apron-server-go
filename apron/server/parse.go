package server

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// The parsers read the params of the methods of the Server's Config
// handlers, with what wire checks of each (see the package documentation's
// table), before a handler gets them. Each returns an error that is an
// *Error, error/invalid_params, and zero params with it.

// messageFields are the params of a message request or a command (§3.5),
// before they are told apart.
type messageFields struct {
	messageID apron.LogID
	roomID    string
	deleted   bool
	body      *apron.Body
	replyTo   *apron.MessageRef
	ext       apron.Ext
}

// edit reports whether the request saves an existing message.
func (m messageFields) edit() bool { return m.messageID != 0 || m.deleted }

// parseMessageFields reads the params of a message or command (§3.5): what
// each field is, as the protocol defines it. Limits, such as on the count
// of mentions, are the backend's.
func parseMessageFields(r *Request) (messageFields, error) {
	d := r.Decoder()
	m := messageFields{
		roomID:    d.ID("room_id", Optional),
		messageID: d.MessageID("message_id", Optional),
		replyTo:   IfPresent(d, "reply_to", d.MessageRef),
		deleted:   d.Bool("deleted", Optional),
		ext:       d.Ext("ext", Optional),
	}
	if m.deleted {
		// A deletion leaves no body (§4.4): what it sends is ignored.
		return parsed(m, d)
	}
	if d.Object("body", Optional) != nil {
		body, err := apron.ParseBody(r.fields["body"])
		d.Fail(err)
		m.body = body
	}
	return parsed(m, d)
}

func (m messageFields) editParams() apron.EditParams {
	return apron.EditParams{MessageID: m.messageID, RoomID: m.roomID, Deleted: m.deleted, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}
}

// parseCommand reads a command's params (§4.1): a message's, but for
// message_id and deleted, and with a body.
func parseCommand(r *Request) (apron.CommandParams, error) {
	for _, name := range []string{"message_id", "deleted"} {
		if _, ok := r.fields[name]; ok {
			return apron.CommandParams{}, apron.InvalidParams("A command has no " + name + "; send it as a message instead")
		}
	}
	m, err := parseMessageFields(r)
	if err != nil {
		return apron.CommandParams{}, err
	}
	if m.body == nil {
		return apron.CommandParams{}, apron.InvalidParams("A command needs a body")
	}
	return apron.CommandParams{RoomID: m.roomID, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, nil
}

// parseAuth reads auth's params (§3.2): scheme, a non-empty string, and the
// fields AuthParams has, each of its type. Whether the server offers the
// scheme is Config.CheckScheme's to decide, and the rest, such as whether
// user_id could be one (§2.4), the backend's, after it: a scheme the
// server does not offer is unsupported whatever else the request holds
// (§3.2).
func parseAuth(r *Request) (apron.AuthParams, error) {
	d := r.Decoder()
	p := apron.AuthParams{
		Scheme: d.String("scheme", Required),
		Name:   d.String("name", Optional),
		UserID: d.String("user_id", Optional),
		Agent:  d.String("agent", Optional),
		Token:  d.String("token", Optional),
		Ext:    d.Ext("ext", Optional),
	}
	if d.Err() == nil && p.Scheme == "" {
		d.Fail(apron.Missing("scheme"))
	}
	return parsed(p, d)
}

// parseMe reads me's params (§3.3.2): name, avatar, and status are
// strings, and ext an object. What a server accepts of each, such as a
// name's length or a status value, is the backend's.
func parseMe(r *Request) (apron.MeParams, error) {
	d := r.Decoder()
	p := apron.MeParams{
		Name:   IfPresent(d, "name", d.String),
		Avatar: IfPresent(d, "avatar", d.String),
		Status: IfPresent(d, "status", d.String),
		Ext:    d.Ext("ext", Optional),
	}
	return parsed(p, d)
}

// parseRoomList reads room_list's params (§4.2.1): filter is joined,
// not_joined, or all, the default.
func parseRoomList(r *Request) (apron.RoomListParams, error) {
	d := r.Decoder()
	p := apron.RoomListParams{Filter: apron.RoomsAll}
	if filter := IfPresent(d, "filter", d.String); filter != nil {
		p.Filter = apron.RoomFilter(*filter)
		if p.Filter != apron.RoomsAll && p.Filter != apron.RoomsJoined && p.Filter != apron.RoomsNotJoined {
			d.Fail(apron.InvalidParams("filter must be joined, not_joined, or all"))
		}
	}
	p.RoomID = d.ID("room_id", Optional)
	p.ParentRoomID = d.ID("parent_room_id", Optional)
	p.UserID = d.ID("user_id", Optional)
	p.Members = d.Bool("members", Optional)
	p.LatestLogID = IfPresent(d, "latest_log_id", d.LogID)
	return parsed(p, d)
}

// parseRoomJoin reads room_join's params (§4.2.2): room_id, and user_id,
// for a join of another user.
func parseRoomJoin(r *Request) (apron.RoomJoinParams, error) {
	d := r.Decoder()
	p := apron.RoomJoinParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// parseRoomLeave reads room_leave's params (§4.2.2), as parseRoomJoin does
// room_join's.
func parseRoomLeave(r *Request) (apron.RoomLeaveParams, error) {
	d := r.Decoder()
	p := apron.RoomLeaveParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// parseRoomSet reads room_set's params (§4.2.4).
func parseRoomSet(r *Request) (apron.RoomSetParams, error) {
	d := r.Decoder()
	p := apron.RoomSetParams{
		RoomID:       d.ID("room_id", Optional),
		ParentRoomID: d.ID("parent_room_id", Optional),
		Title:        IfPresent(d, "title", d.String),
		Description:  IfPresent(d, "description", d.String),
		Private:      IfPresent(d, "private", d.Bool),
		Ext:          d.Ext("ext", Optional),
	}
	return parsed(p, d)
}

// parseHistory reads history's params (§4.3): after and before are
// log_ids, and limit is positive. A limit above the backend's own is the
// backend's to lower.
func parseHistory(r *Request) (apron.HistoryParams, error) {
	d := r.Decoder()
	p := apron.HistoryParams{
		RoomID: d.ID("room_id", Optional),
		After:  IfPresent(d, "after", d.LogID),
		Before: IfPresent(d, "before", d.LogID),
		Limit:  d.Count("limit", Optional),
	}
	return parsed(p, d)
}

// parseStatus reads a status request's params (§4.5.3): idle is false or
// a non-negative number of seconds, mute true, false, or a positive number
// of seconds, and room_id scopes only the mute, so it is sent with one.
func parseStatus(r *Request) (apron.StatusParams, error) {
	d := r.Decoder()
	p := apron.StatusParams{RoomID: d.ID("room_id", Optional)}
	scoped := d.Has("room_id")
	if raw, ok := r.fields["idle"]; ok && d.Err() == nil {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("false")):
			p.Idle = &apron.IdleReport{Attended: true}
		case !core.IsNull(raw) && json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			p.Idle = &apron.IdleReport{Seconds: seconds}
		default:
			d.Fail(apron.InvalidParams("idle must be false or a non-negative whole number of seconds"))
		}
	}
	if raw, ok := r.fields["mute"]; ok && d.Err() == nil {
		var mute apron.Mute
		if mute.UnmarshalJSON(raw) != nil {
			d.Fail(apron.InvalidParams("mute must be true, false, or a positive whole number of seconds"))
		}
		p.Mute = &mute
	}
	if scoped && p.Mute == nil {
		d.Fail(apron.InvalidParams("room_id scopes a mute; send it with mute"))
	}
	return parsed(p, d)
}

// parseReact reads the params of reactions (§4.7): message_id, and emojis,
// an array of non-empty strings, empty to clear the set. Whether each is
// an emoji the server takes is the backend's.
func parseReact(r *Request) (apron.ReactParams, error) {
	d := r.Decoder()
	p := apron.ReactParams{MessageID: d.MessageID("message_id", Required)}
	raw, ok := r.fields["emojis"]
	switch {
	case d.Err() != nil:
	case !ok:
		d.Fail(apron.Missing("emojis"))
	case core.IsNull(raw) || json.Unmarshal(raw, &p.Emojis) != nil:
		d.Fail(apron.InvalidParams("emojis must be an array of strings"))
	case slices.Contains(p.Emojis, ""):
		d.Fail(apron.InvalidParams("emojis must be non-empty strings"))
	case p.Emojis == nil:
		p.Emojis = []string{}
	}
	return parsed(p, d)
}

// parseActivity reads an activity notification's params (§4.6): typing is
// a non-negative number of seconds, and read_message_id a message_id.
func parseActivity(r *Request) (apron.ActivityParams, error) {
	d := r.Decoder()
	p := apron.ActivityParams{RoomID: d.ID("room_id", Optional)}
	if raw, ok := r.fields["typing"]; ok && d.Err() == nil {
		var typing int
		if core.IsNull(raw) || json.Unmarshal(raw, &typing) != nil || typing < 0 {
			d.Fail(apron.InvalidParams("typing must be a non-negative integer"))
		}
		p.Typing = &typing
	}
	p.ReadMessageID = IfPresent(d, "read_message_id", d.MessageID)
	return parsed(p, d)
}

// replyFor returns the Reply of r, for a result of T, with what Send does
// for r's method when T is its result type: the user of a me result
// becomes the connection's, a room_list result is normalized for its
// params, and an auth result signs the connection in. For any other method
// or type, Send sends the result as it is. params are r's, when the caller
// parsed them already.
func replyFor[T any](r *Request, params any) Reply[T] {
	reply := Reply[T]{r: r}
	if r == nil {
		return reply
	}
	switch p := any(&reply).(type) {
	case *Reply[apron.CurrentUser]:
		if r.Method == "me" {
			p.check = func(you *apron.CurrentUser) (any, error) { return meResult{You: *you}, nil }
			p.apply = func(you *apron.CurrentUser) { r.c.SetUser(you.User) }
		}
	case *Reply[apron.RoomListResult]:
		if r.Method == "room_list" {
			list, ok := params.(apron.RoomListParams)
			if !ok {
				list, _ = parseRoomList(r)
			}
			p.check = func(result *apron.RoomListResult) (any, error) { return normalizeRoomList(list, *result), nil }
		}
	case *Reply[apron.AuthResult]:
		if r.Method == "auth" {
			p.check = checkAuth
			p.apply = func(result *apron.AuthResult) {
				if result.You != nil {
					r.c.SetUser(result.You.User)
				}
			}
		}
	}
	return reply
}

// checkAuth checks an auth result: one that signs in has a valid user_id,
// and Extra names no field of its own.
func checkAuth(result *apron.AuthResult) (any, error) {
	if result.You != nil && !apron.ValidID(result.You.UserID) {
		return nil, fmt.Errorf("auth assigned an invalid user_id %q", result.You.UserID)
	}
	for _, name := range []string{"you", "token"} {
		if _, ok := result.Extra[name]; ok {
			return nil, fmt.Errorf("an auth result's Extra names its own field %s", name)
		}
	}
	return *result, nil
}

// meResult is the result of me (§3.3.2).
type meResult struct {
	You apron.CurrentUser `json:"you"`
}
