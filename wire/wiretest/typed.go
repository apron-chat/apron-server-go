package wiretest

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/apron-chat/apron-server-go/wire"
)

// Call sends a request on c, as Client.Call does, and decodes its result
// into a T, such as a wire.HistoryResult. Its error is the error reply, as
// an *wire.Error, or the decoding's. me's result carries the user in you,
// which Client.Me returns.
func Call[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var result T
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("wiretest: %s result %s: %w", method, raw, err)
	}
	return result, nil
}

// Expect takes the first notification of T's method that the test has not
// taken, decoded, for which match returns true, or the first when match is
// nil, waiting for one, as Client.Expect does; the frames before it stay,
// in order. T is a notification type, such as wire.Message or
// wire.UserUpdate:
//
//	update, err := wiretest.Expect(ctx, c, func(u wire.UserUpdate) bool { return u.New != nil })
//
// A frame of T's method that does not decode as a T does not match.
func Expect[T wire.Notification](ctx context.Context, c *Client, match func(T) bool) (T, error) {
	var zero, got T
	method := zero.Method()
	_, err := c.take(ctx, func(f Frame) bool {
		var n T
		if f.Method != method || f.Decode(&n) != nil || match != nil && !match(n) {
			return false
		}
		got = n
		return true
	})
	if err != nil {
		return zero, err
	}
	return got, nil
}

// ExpectMessage takes the first message notification the test has not
// taken for which match returns true, or the first when match is nil,
// waiting for one: it is Expect for a wire.Message.
func (c *Client) ExpectMessage(ctx context.Context, match func(wire.Message) bool) (wire.Message, error) {
	return Expect(ctx, c, match)
}

// Post posts a message (§3.5.1), as bot.Conn.Post does, and returns its
// result: the new message's message_id, unless the body was empty. The
// message's notification, which comes before the result, stays for Next
// and Expect.
func (c *Client) Post(ctx context.Context, p wire.MessageParams) (wire.MessageResult, error) {
	return Call[wire.MessageResult](ctx, c, "message", p)
}

// Me changes the user's profile (§3.3.2), and returns the result's you:
// the complete object, with the fields it carries empty in Cleared.
func (c *Client) Me(ctx context.Context, p wire.MeParams) (wire.CurrentUser, error) {
	result, err := Call[struct {
		You wire.CurrentUser `json:"you"`
	}](ctx, c, "me", p)
	return result.You, err
}

// Command runs a command line, such as "/nick Ada", in a room, "" for the
// default room (§4.1), and returns the text of the notices sent to the
// connection ahead of its result, in order: the messages from wire.Private,
// such as what /help lists, which it takes. The other frames, such as the
// command's effects, stay for Next and Expect. Its error is the error
// reply, as an *wire.Error, which comes with the notices ahead of it.
//
// As CallFrames does, it takes the notices that arrived before the request
// was sent too; a test that wants only the command's takes those first.
// A test that needs the command's result, the embeds it made, calls
// Call[wire.CommandResult].
func (c *Client) Command(ctx context.Context, roomID, line string) (notices []string, err error) {
	p := wire.CommandParams{RoomID: roomID, Body: &wire.Body{Text: line}}
	_, frames, err := c.call(ctx, "command", p, func(f Frame) bool {
		var m wire.Message
		return f.Method == "message" && f.Decode(&m) == nil && m.From.UserID == wire.Private.UserID
	})
	for _, f := range frames {
		var m wire.Message
		if f.Decode(&m) == nil && m.Body != nil {
			notices = append(notices, m.Body.Text)
		}
	}
	return notices, err
}
