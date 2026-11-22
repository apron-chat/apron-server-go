package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// The parsers read the params of the methods of the Server's handler
// fields, with what wire checks of each (see the package documentation's
// table): the Server calls them before a handler, and a Handler that takes
// a handler field's place calls them to read the same params the same way,
// then passes them on, with NewReply. Each returns an error that is an
// *Error, error/invalid_params, and zero params with it.

// messageFields are the params of a message request or a command (§3.5),
// before they are told apart.
type messageFields struct {
	messageID LogID
	roomID    string
	deleted   bool
	body      *Body
	replyTo   *Ref
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
		replyTo:   Opt(d, "reply_to", d.Ref),
		deleted:   d.Bool("deleted", Optional),
		ext:       d.Ext("ext", Optional),
	}
	if m.deleted {
		// A deletion leaves no body (§4.4): what it sends is ignored.
		return parsed(m, d)
	}
	if fields := d.Object("body", Optional); fields != nil {
		raw, _ := r.Param("body")
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
			var kind struct {
				Kind *string `json:"kind"`
			}
			if json.Unmarshal(embed, &kind) != nil || isNull(embed) || embed[0] != '{' {
				return nil, InvalidParams(fmt.Sprintf("body.embeds[%d] must be an object", i))
			}
			if kind.Kind == nil || *kind.Kind == "" {
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

// ParseMessage reads the params of a new message (§3.5.1): a message
// request without message_id or deleted, with a body, whose fields are
// well formed (§3.5).
func ParseMessage(r *Request) (MessageParams, error) {
	m, err := parseMessageFields(r)
	switch {
	case err != nil:
		return MessageParams{}, err
	case m.edit():
		return MessageParams{}, InvalidParams("A new message has no message_id or deleted")
	case m.body == nil:
		return MessageParams{}, Missing("body")
	}
	return MessageParams{RoomID: m.roomID, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}, nil
}

// ParseEdit reads the params of a save of an existing message (§4.4): a
// message request with message_id, which deleted, if set, makes a
// deletion.
func ParseEdit(r *Request) (EditParams, error) {
	m, err := parseMessageFields(r)
	switch {
	case err != nil:
		return EditParams{}, err
	case m.messageID == 0 && m.deleted:
		// Only a save of an existing message deletes it (§4.4).
		return EditParams{}, InvalidParams("Cannot create a deleted message")
	case m.messageID == 0:
		return EditParams{}, Missing("message_id")
	}
	return m.editParams(), nil
}

func (m messageFields) editParams() EditParams {
	return EditParams{MessageID: m.messageID, RoomID: m.roomID, Deleted: m.deleted, Body: m.body, ReplyTo: m.replyTo, Ext: m.ext}
}

// ParseCommand reads a command's params (§4.1): a message's, but for
// message_id and deleted, and with a body.
func ParseCommand(r *Request) (CommandParams, error) {
	for _, name := range []string{"message_id", "deleted"} {
		if r.Has(name) {
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

// ParseAuth reads auth's params (§3.2): scheme, a non-empty string, and the
// fields AuthParams has, each of its type. Whether the server offers the
// scheme is Server.CheckScheme's to decide, and the rest, such as whether
// user_id could be one (§2.4), the backend's, after it: a scheme the
// server does not offer is unsupported whatever else the request holds
// (§3.2).
func ParseAuth(r *Request) (AuthParams, error) {
	d := r.Decoder()
	p := AuthParams{
		Scheme: d.String("scheme", Required),
		Name:   d.String("name", Optional),
		UserID: d.String("user_id", Optional),
		Agent:  d.String("agent", Optional),
		Token:  d.String("token", Optional),
		Ext:    d.Ext("ext", Optional),
		Raw:    r.Params,
	}
	if d.Err() == nil && p.Scheme == "" {
		d.Fail(Missing("scheme"))
	}
	return parsed(p, d)
}

// ParseMe reads me's params (§3.3.2): name, avatar, and status are
// strings, and ext an object. What a server accepts of each, such as a
// name's length or a status value, is the backend's.
func ParseMe(r *Request) (MeParams, error) {
	d := r.Decoder()
	p := MeParams{
		Name:   Opt(d, "name", d.String),
		Avatar: Opt(d, "avatar", d.String),
		Status: Opt(d, "status", d.String),
		Ext:    d.Ext("ext", Optional),
	}
	return parsed(p, d)
}

// ParseRoomList reads room_list's params (§4.2.1): filter is joined,
// not_joined, or all, the default.
func ParseRoomList(r *Request) (RoomListParams, error) {
	d := r.Decoder()
	p := RoomListParams{Filter: FilterAll}
	if filter := Opt(d, "filter", d.String); filter != nil {
		p.Filter = *filter
		if p.Filter != FilterAll && p.Filter != FilterJoined && p.Filter != FilterNotJoined {
			d.Fail(InvalidParams("filter must be joined, not_joined, or all"))
		}
	}
	p.RoomID = d.ID("room_id", Optional)
	p.ParentRoomID = d.ID("parent_room_id", Optional)
	p.UserID = d.ID("user_id", Optional)
	p.Members = d.Bool("members", Optional)
	p.LatestLogID = Opt(d, "latest_log_id", d.LogID)
	return parsed(p, d)
}

// ParseRoomJoin reads room_join's params (§4.2.2): room_id, and user_id,
// for a join of another user.
func ParseRoomJoin(r *Request) (RoomJoinParams, error) {
	d := r.Decoder()
	p := RoomJoinParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// ParseRoomLeave reads room_leave's params (§4.2.2), as ParseRoomJoin does
// room_join's.
func ParseRoomLeave(r *Request) (RoomLeaveParams, error) {
	d := r.Decoder()
	p := RoomLeaveParams{RoomID: d.ID("room_id", Required), UserID: d.ID("user_id", Optional)}
	return parsed(p, d)
}

// ParseRoomSet reads room_set's params (§4.2.4).
func ParseRoomSet(r *Request) (RoomSetParams, error) {
	d := r.Decoder()
	p := RoomSetParams{
		RoomID:       d.ID("room_id", Optional),
		ParentRoomID: d.ID("parent_room_id", Optional),
		Title:        Opt(d, "title", d.String),
		Description:  Opt(d, "description", d.String),
		Private:      Opt(d, "private", d.Bool),
		Ext:          d.Ext("ext", Optional),
	}
	return parsed(p, d)
}

// ParseHistory reads history's params (§4.3): after and before are
// log_ids, and limit is positive. A limit above the backend's own is the
// backend's to lower.
func ParseHistory(r *Request) (HistoryParams, error) {
	d := r.Decoder()
	p := HistoryParams{
		RoomID: d.ID("room_id", Optional),
		After:  Opt(d, "after", d.LogID),
		Before: Opt(d, "before", d.LogID),
		Limit:  d.Count("limit", Optional),
	}
	return parsed(p, d)
}

// ParseStatus reads a status request's params (§4.5.3): idle is false or
// a non-negative number of seconds, mute true, false, or a positive number
// of seconds, and room_id scopes only the mute, so it is sent with one.
func ParseStatus(r *Request) (StatusParams, error) {
	d := r.Decoder()
	p := StatusParams{RoomID: d.ID("room_id", Optional)}
	scoped := d.Has("room_id")
	if raw, ok := r.Param("idle"); ok && d.Err() == nil {
		var seconds int64
		switch raw := bytes.TrimSpace(raw); {
		case bytes.Equal(raw, []byte("false")):
			p.Idle = &Idle{Attended: true}
		case !isNull(raw) && json.Unmarshal(raw, &seconds) == nil && seconds >= 0:
			p.Idle = &Idle{Seconds: seconds}
		default:
			d.Fail(InvalidParams("idle must be false or a non-negative whole number of seconds"))
		}
	}
	if raw, ok := r.Param("mute"); ok && d.Err() == nil {
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

// ParseReact reads the params of reactions (§4.7): message_id, and emojis,
// an array of non-empty strings, empty to clear the set. Whether each is
// an emoji the server takes is the backend's.
func ParseReact(r *Request) (ReactParams, error) {
	d := r.Decoder()
	p := ReactParams{MessageID: d.MessageID("message_id", Required)}
	raw, ok := r.Param("emojis")
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

// ParseActivity reads an activity notification's params (§4.6): typing is
// a non-negative number of seconds, and read_message_id a message_id.
func ParseActivity(r *Request) (ActivityParams, error) {
	d := r.Decoder()
	p := ActivityParams{RoomID: d.ID("room_id", Optional)}
	if raw, ok := r.Param("typing"); ok && d.Err() == nil {
		var typing int
		if isNull(raw) || json.Unmarshal(raw, &typing) != nil || typing < 0 {
			d.Fail(InvalidParams("typing must be a non-negative integer"))
		}
		p.Typing = &typing
	}
	p.ReadMessageID = Opt(d, "read_message_id", d.MessageID)
	return parsed(p, d)
}

// NewReply returns a Reply that answers r, for a Handler that reads a
// request with one of the parsers, such as ParseMessage, and passes it on
// to a typed handler, or for a test (package wiretest). Its Send does what
// the Server does with the result of r's method when T is that method's
// result type, as for its own handler fields: the user of a me result
// becomes the connection's, a room_list result is normalized for its
// params, and an auth result signs the connection in. For any other method
// or type, Send sends the result as Request.Reply does.
func NewReply[T any](r *Request) Reply[T] { return replyFor[T](r, nil) }

// replyFor returns the Reply of r, for a result of T, with what Send does
// for r's method; params are r's, when the caller parsed them already.
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
				list, _ = ParseRoomList(r)
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
