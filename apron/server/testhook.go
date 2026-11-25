package server

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/apron-chat/apron-server-go/apron/internal/testhook"
)

func init() {
	testhook.NewRequest = func(method string, params jsontext.Value, rec testhook.Recorder) (any, error) {
		return detachedRequest(method, params, rec)
	}
	testhook.InitReply = func(reply, r any) { reply.(initReply).init(r.(*Request)) }
	testhook.DedupUsage = func(s any) (users, requests, bytes int) {
		return s.(*Server).dedup.usage()
	}
	testhook.RecorderOf = func(r any) testhook.Recorder {
		if r, ok := r.(*Request); ok {
			return r.rec
		}
		return nil
	}
}

// detachedRequest returns a request of method on no connection, with
// params, an object, whose reply goes to rec: for package wiretest, and
// this package's tests.
func detachedRequest(method string, params jsontext.Value, rec testhook.Recorder) (*Request, error) {
	if len(params) == 0 {
		params = jsontext.Value("{}")
	}
	var fields Fields
	if err := json.Unmarshal(params, &fields); err != nil || fields == nil {
		return nil, errors.New("wire: params must be a JSON object")
	}
	return &Request{Method: method, params: params, fields: fields, hasID: true, rec: rec}, nil
}
