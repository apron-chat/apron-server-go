package apron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strconv"

	"github.com/apron-chat/apron-server-go/apron/internal/core"
)

// LogID is a log_id (§2.1): the position of one change in the server's log,
// the time of its commit in milliseconds since the Unix epoch. A message_id
// is the log_id of the message's creation (§2.2), so it is a LogID too.
//
// Its JSON is a decimal string, such as "1724803200042". The zero LogID is
// no log_id, and encodes as null: a field that is left out when it has
// none is tagged omitzero, and one the protocol requires, such as a
// history result's history_log_id, is null. A null decodes as 0. A LogID
// is decoded only from a string, as ParseLogID reads it.
type LogID int64

// MaxLogID is the greatest log_id (§2.1): log_ids are below 2^53, so that
// a client may read one as a number, as JavaScript does, without losing
// precision.
const MaxLogID LogID = 1<<53 - 1

// ParseLogID reads a log_id or message_id (§2.1): a decimal string of a
// non-negative number, without a sign or leading zeros, of at most
// MaxLogID. "0" is 0, which is a valid bound but no record's log_id. Its
// error is error/invalid_params, the error of a rejected value (§1.1).
func ParseLogID(s string) (LogID, error) {
	id, err := core.ParseLogID(s)
	switch err {
	case core.ErrNotLogID:
		return 0, InvalidParams("Not a log_id: " + strconv.Quote(s))
	case core.ErrLogIDRange:
		return 0, InvalidParams("A log_id is below 2^53: " + strconv.Quote(s))
	}
	return LogID(id), nil
}

// String returns the log_id as a decimal string, "0" for none.
func (id LogID) String() string { return strconv.FormatInt(int64(id), 10) }

// MarshalJSON writes the log_id as a decimal string, or null for 0. A
// negative LogID, or one past MaxLogID, is no log_id (§2.1), and is an
// encoding error rather than sent: a reply whose result has one is logged
// and answered error/internal_error, and a notification is not sent.
func (id LogID) MarshalJSON() ([]byte, error) {
	switch {
	case id == 0:
		return []byte("null"), nil
	case id < 0:
		return nil, errors.New("apron: a log_id is not negative: " + id.String())
	case id > MaxLogID:
		return nil, errors.New("apron: a log_id is below 2^53: " + id.String())
	}
	return strconv.AppendQuote(nil, id.String()), nil
}

// MarshalJSONTo writes the log_id as MarshalJSON does, without allocating:
// a message has several, and a page of history hundreds.
func (id LogID) MarshalJSONTo(enc *jsontext.Encoder) error {
	if id <= 0 || id > MaxLogID {
		if id == 0 {
			return enc.WriteToken(jsontext.Null)
		}
		_, err := id.MarshalJSON()
		return err
	}
	var buf [20]byte
	quoted := strconv.AppendInt(append(buf[:0], '"'), int64(id), 10)
	return enc.WriteValue(append(quoted, '"'))
}

// UnmarshalJSONFrom reads a log_id as UnmarshalJSON does, without
// allocating for one written plainly, as servers write them.
func (id *LogID) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	if raw.Kind() != '"' || bytes.IndexByte(raw, '\\') >= 0 {
		return id.UnmarshalJSON(raw)
	}
	parsed, err := core.ParseLogID([]byte(raw[1 : len(raw)-1]))
	if err != nil {
		// Worded as ParseLogID words it.
		_, err = ParseLogID(string(raw[1 : len(raw)-1]))
		return err
	}
	*id = LogID(parsed)
	return nil
}

// UnmarshalJSON reads a log_id from a decimal string, as ParseLogID does,
// or 0 from null.
func (id *LogID) UnmarshalJSON(data []byte) error {
	if core.IsNull(data) {
		*id = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return InvalidParams("A log_id is a decimal string")
	}
	parsed, err := ParseLogID(s)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
