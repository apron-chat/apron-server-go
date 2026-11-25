// Package messages holds the server's own rules for what a message may
// hold, beyond the protocol's: the limits on a body (PROTOCOL.md §3.5) and
// on a message's ext (§4.13), and a reaction set's (§4.7), whose emoji it
// normalizes. Package wire checks what the protocol defines, such as that
// mentions are user_ids and embeds objects with a kind, before a request
// reaches the server; these are the server's policy, checked after it.
//
// Its checks return the protocol error a client gets (§1.1): a count over
// a limit the server sets is denied, a value over a size limit too_large,
// and a malformed one invalid_params. What depends on the server's state,
// such as which custom emoji exist, is the server's to check.
package messages

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aprond/internal/emoji"
)

// The server's limits on a message.
const (
	// MaxMentions bounds body.mentions.
	MaxMentions = 256
	// MaxEmbeds bounds body.embeds.
	MaxEmbeds = 32
	// MaxExtBytes bounds a message's ext, merged (§4.13), which every
	// snapshot of the message carries.
	MaxExtBytes = 64 << 10
	// MaxEmojiBytes bounds each emoji of a reaction set, and a custom
	// emoji's key.
	MaxEmojiBytes = 64
	// MaxDistinctEmoji bounds the distinct emoji of a reaction set.
	MaxDistinctEmoji = 20
)

// CheckBody checks a body that package wire read, from a client's request
// or with apron.ParseBody, against the server's limits: the count of its
// mentions and of its embeds.
func CheckBody(body *apron.Body) *apron.Error {
	if len(body.Mentions) > MaxMentions {
		return denied("A message mentions at most %d users", MaxMentions)
	}
	if len(body.Embeds) > MaxEmbeds {
		return denied("A message holds at most %d embeds", MaxEmbeds)
	}
	return nil
}

// CheckRaw checks a body that did not come from a client's request, such
// as one an extension's filter rewrote: as package wire checks a client's
// (apron.ParseBody), then as CheckBody does.
func CheckRaw(raw jsontext.Value) *apron.Error {
	body, err := apron.ParseBody(raw)
	if err != nil {
		if e, ok := errors.AsType[*apron.Error](err); ok {
			return e
		}
		return apron.InvalidParams(err.Error())
	}
	return CheckBody(body)
}

// CheckExt checks a message's ext, merged, against MaxExtBytes: one over
// it is too_large, and the write that made it changes nothing (§4.13).
func CheckExt(ext apron.Ext) *apron.Error {
	if encoded, _ := apron.Marshal(ext); len(encoded) > MaxExtBytes {
		return &apron.Error{Code: apron.CodeTooLarge, Message: fmt.Sprintf("The message's ext would be over %d bytes", MaxExtBytes)}
	}
	return nil
}

// Emojis normalizes a reaction set (§4.7): each entry fully qualified,
// so that entries that differ only in U+FE0F are the same entry, and each
// kept once, in the order it first came. An entry that is empty once
// qualified is invalid_params, one over MaxEmojiBytes too_large, and a set
// of more than MaxDistinctEmoji denied.
func Emojis(values []string) ([]string, *apron.Error) {
	emojis := make([]string, 0, len(values))
	for _, entry := range values {
		if entry = emoji.Qualify(entry); entry == "" {
			return nil, apron.InvalidParams("emojis must be non-empty strings")
		}
		if len(entry) > MaxEmojiBytes {
			return nil, &apron.Error{Code: apron.CodeTooLarge, Message: fmt.Sprintf("An emoji is at most %d bytes", MaxEmojiBytes)}
		}
		if !slices.Contains(emojis, entry) {
			emojis = append(emojis, entry)
		}
	}
	if len(emojis) > MaxDistinctEmoji {
		return nil, denied("At most %d distinct emoji per message", MaxDistinctEmoji)
	}
	return emojis, nil
}

// denied is the error of a count over a limit the server sets (§1.1).
func denied(format string, limit int) *apron.Error {
	return &apron.Error{Code: apron.CodeDenied, Message: fmt.Sprintf(format, limit)}
}
