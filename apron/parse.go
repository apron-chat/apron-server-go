package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// The parsers read the params of the methods of the Server's Config
// handlers, with what wire checks of each (see the package documentation's
// table), before a handler gets them. Each returns an error that is an
// *Error, error/invalid_params, and zero params with it.

// messageFields are the params of a message request or a command (§3.5),
// before they are told apart.
type messageFields struct {
	messageID LogID
	roomID    string
	deleted   bool
	body      *Body
	replyTo   *MessageRef
	ext       Ext
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
	if fields := d.Object("body", Optional); fields != nil {
		raw := r.fields["body"]
		body, err := parseBody(raw, fields)
		d.Fail(err)
		m.body = body
	}
	return parsed(m, d)
}

// parseBody reads a message body (§3.5), raw and by its fields: each field
// this package knows is checked, and the others are kept in Unknown.
func parseBody(raw jsontext.Value, fields Fields) (*Body, error) {
	body := &Body{}
	if raw, ok := fields["text"]; ok && (isNull(raw) || json.Unmarshal(raw, &body.Text) != nil) {
		return nil, InvalidParams("body.text must be a string")
	}
	if raw, ok := fields["format"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Format) != nil || (body.Format != Plain && body.Format != Markdown) {
			return nil, InvalidParams("body.format must be plain or markdown")
		}
	}
	if raw, ok := fields["mentions"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Mentions) != nil || slices.ContainsFunc(body.Mentions, func(id string) bool { return !ValidID(id) }) {
			return nil, InvalidParams("body.mentions must be an array of user_id strings")
		}
	}
	if raw, ok := fields["embeds"]; ok {
		if isNull(raw) || json.Unmarshal(raw, &body.Embeds) != nil {
			return nil, InvalidParams("body.embeds must be an array")
		}
		for i, embed := range body.Embeds {
			var object Fields
			if isNull(embed) || json.Unmarshal(embed, &object) != nil {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d] must be an object", i))
			}
			var kind string
			if raw, ok := object["kind"]; !ok || isNull(raw) || json.Unmarshal(raw, &kind) != nil || kind == "" {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d].kind must be a non-empty string", i))
			}
		}
	}
	// Every field it knows is well formed, so the body decodes, and keeps
	// the fields it does not know as they arrived.
	if err := json.Unmarshal(raw, body); err != nil {
		return nil, InvalidParams("body must be an object")
	}
	return body, nil
}

// ParseBody reads a message body (§3.5) as the message requests' parsers
// read one, such as parseMessage: text a string, format plain or
// markdown, mentions user_ids, and embeds objects with a kind, with the
// members this package does not know kept in Unknown. A backend checks
// with it a body that came from elsewhere, such as one its filter
// rewrote, as wire checks a client's. Its error is always an *Error,
// error/invalid_params.
func ParseBody(raw jsontext.Value) (*Body, error) {
	fields, _, err := Fields{"body": raw}.objectParam("body")
	if err != nil {
		return nil, err
	}
	return parseBody(raw, fields)
}

func (m messageFields) editParams() EditParams {
	return EditParams{MessageID: m.messageID, RoomID: m.roomID, Deleted: m.deleted, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}
}

// parseCommand reads a command's params (§4.1): a message's, but for
// message_id and deleted, and with a body.
func parseCommand(r *Request) (CommandParams, error) {
	for _, name := range []string{"message_id", "deleted"} {
		if _, ok := r.fields[name]; ok {
			return CommandParams{}, InvalidParams("A command has no " + name + "; send it as a message instead")
		}
	}
	m, err := parseMessageFields(r)
	if err != nil {
		return CommandParams{}, err
	}
	if m.body == nil {
		return CommandParams{}, InvalidParams("A command needs a body")
	}
	return CommandParams{RoomID: m.roomID, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, nil
}

// parseAuth reads auth's params (§3.2): scheme, a non-empty string, and the
// fields AuthParams has, each of its type. Whether the server offers the
// scheme is Config.CheckScheme's to decide, and the rest, such as whether
// user_id could be one (§2.4), the backend's, after it: a scheme the
// server does not offer is unsupported whatever else the request holds
// (§3.2).
func parseAuth(r *Request) (AuthParams, error) {
	d := r.Decoder()
	p := AuthParams{
		Scheme: d.String("scheme", Required),
		Name:   d.String("name", Optional),
		UserID: d.String("user_id", Optional),
		Agent:  d.String("agent", Optional),
		Token:  d.String("token", Optional),
		Ext:    d.Ext("ext", Optional),
	}
	if d.Err() == nil && p.Scheme == "" {
		d.Fail(Missing("scheme"))
	}
	return parsed(p, d)
}

// parseMe reads me's params (§3.3.2): name, avatar, and status are
// strings, and ext an object. What a server accepts of each, such as a
// name's length or a status value, is the backend's.
func parseMe(r *Request) (MeParams, error) {
	d := r.Decoder()
	p := MeParams{
		Name:   IfPresent(d, "name", d.String),
		Avatar: IfPresent(d, "avatar", d.String),
		Status: IfPresent(d, "status", d.String),
		Ext:    d.Ext("ext", Optional),
	}
	return parsed(p, d)
}

// parseRoomList reads room_list's params (§4.2.1): filter is joined,
// not_joined, or all, the default.
func parseRoomList(r *Request) (RoomListParams, error) {
	d := r.Decoder()
	p := RoomListParams{Filter: FilterAll}
	if filter := IfPresent(d, "filter", d.String); filter != nil {
		p.Filter = *filter
		if p.Filter != FilterAll && p.Filter != FilterJoined && p.Filter != FilterNotJoined {
			d.Fail(InvalidParams("filter must be joined, not_joined, or all"))
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
func parseRoomJoin(r *Request) (RoomJoinParams, error) {
	d := r.Decoder()
	p := RoomJoinParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// parseRoomLeave reads room_leave's params (§4.2.2), as parseRoomJoin does
// room_join's.
func parseRoomLeave(r *Request) (RoomLeaveParams, error) {
	d := r.Decoder()
	p := RoomLeaveParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// parseRoomSet reads room_set's params (§4.2.4).
func parseRoomSet(r *Request) (RoomSetParams, error) {
	d := r.Decoder()
	p := RoomSetParams{
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
func parseHistory(r *Request) (HistoryParams, error) {
	d := r.Decoder()
	p := HistoryParams{
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
func parseStatus(r *Request) (StatusParams, error) {
	d := r.Decoder()
	p := StatusParams{RoomID: d.ID("room_id", Optional)}
	scoped := d.Has("room_id")
	if raw, ok := r.fields["idle"]; ok && d.Err() == nil {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("false")):
			p.Idle = &IdleReport{Attended: true}
		case !isNull(raw) && json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			p.Idle = &IdleReport{Seconds: seconds}
		default:
			d.Fail(InvalidParams("idle must be false or a non-negative whole number of seconds"))
		}
	}
	if raw, ok := r.fields["mute"]; ok && d.Err() == nil {
		var mute Mute
		if mute.UnmarshalJSON(raw) != nil {
			d.Fail(InvalidParams("mute must be true, false, or a positive whole number of seconds"))
		}
		p.Mute = &mute
	}
	if scoped && p.Mute == nil {
		d.Fail(InvalidParams("room_id scopes a mute; send it with mute"))
	}
	return parsed(p, d)
}

// parseReact reads the params of reactions (§4.7): message_id, and emojis,
// an array of non-empty strings, empty to clear the set. Whether each is
// an emoji the server takes is the backend's.
func parseReact(r *Request) (ReactParams, error) {
	d := r.Decoder()
	p := ReactParams{MessageID: d.MessageID("message_id", Required)}
	raw, ok := r.fields["emojis"]
	switch {
	case d.Err() != nil:
	case !ok:
		d.Fail(Missing("emojis"))
	case isNull(raw) || json.Unmarshal(raw, &p.Emojis) != nil:
		d.Fail(InvalidParams("emojis must be an array of strings"))
	case slices.Contains(p.Emojis, ""):
		d.Fail(InvalidParams("emojis must be non-empty strings"))
	case p.Emojis == nil:
		p.Emojis = []string{}
	}
	return parsed(p, d)
}

// parseActivity reads an activity notification's params (§4.6): typing is
// a non-negative number of seconds, and read_message_id a message_id.
func parseActivity(r *Request) (ActivityParams, error) {
	d := r.Decoder()
	p := ActivityParams{RoomID: d.ID("room_id", Optional)}
	if raw, ok := r.fields["typing"]; ok && d.Err() == nil {
		var typing int
		if isNull(raw) || json.Unmarshal(raw, &typing) != nil || typing < 0 {
			d.Fail(InvalidParams("typing must be a non-negative integer"))
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
	case *Reply[CurrentUser]:
		if r.Method == "me" {
			p.check = func(you *CurrentUser) (any, error) { return meResult{You: *you}, nil }
			p.apply = func(you *CurrentUser) { r.c.SetUser(you.User) }
		}
	case *Reply[RoomListResult]:
		if r.Method == "room_list" {
			list, ok := params.(RoomListParams)
			if !ok {
				list, _ = parseRoomList(r)
			}
			p.check = func(result *RoomListResult) (any, error) { return normalizeRoomList(list, *result), nil }
		}
	case *Reply[AuthResult]:
		if r.Method == "auth" {
			p.check = checkAuth
			p.apply = func(result *AuthResult) {
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
func checkAuth(result *AuthResult) (any, error) {
	if result.You != nil && !ValidID(result.You.UserID) {
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
	You CurrentUser `json:"you"`
}
