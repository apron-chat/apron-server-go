package client

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
)

// HoldBack has a Conn wait out the server's rate limits (§1.1), rather
// than return their errors: a request refused with error/retry_after is
// sent again, with the same id (§1.2), once its delay has passed, and
// until then the requests the same limit counts wait, rather than go out
// to be refused. Which requests those are, the refusal's ext:limits
// category says: the refused method for read, every request of the
// category for write (message, reactions, room_join, room_leave, and
// room_set) and upload (message and command requests with a new upload
// embed). Without a category, or with one that does not count the refused
// request, it is the refused method, as apron-web does.
//
// A call waits at most max in all, and as long as its context allows: one
// that would wait longer returns the retry_after error, the server's, or,
// for a request held back, one with the server's message and the time
// left, unsent. A max of 0 or less is no option.
//
// Without HoldBack, which is the default, a call returns every error reply
// as it arrives, so that an interactive client shows a refusal, and the
// time left, to the person who made the request. A program that works
// through many requests unattended, such as an export or a bot, holds
// back instead.
func HoldBack(max time.Duration) Option {
	return func(o *options) { o.holdBack = max }
}

// writes are the methods ext:limits' write category counts.
var writes = map[string]bool{"message": true, "reactions": true, "room_join": true, "room_leave": true, "room_set": true}

// limits are the holds of a Conn with HoldBack.
type limits struct {
	max   time.Duration
	mu    sync.Mutex
	holds map[string]hold // by the key that a request counts toward
}

// hold is a refusal's delay: when it ends, and what the server said.
type hold struct {
	until   time.Time
	message string
}

// keys are what a request counts toward: its method, its category of
// ext:limits if it has one, and upload if it carries a new upload embed.
func keys(method string, params jsontext.Value) []string {
	k := []string{"method " + method}
	if writes[method] {
		k = append(k, apron.LimitWrite)
	}
	if (method == "message" || method == "command") && newUpload(params) {
		k = append(k, apron.LimitUpload)
	}
	return k
}

// newUpload reports whether a message's or command's params carry a new
// upload embed: one of kind upload without an embed_id yet (§4.8.2).
func newUpload(params jsontext.Value) bool {
	// Most messages have no embeds: they are not decoded to tell, as a
	// member named embeds is written so, or with an escape.
	if !bytes.Contains(params, []byte(`"embeds"`)) && !bytes.Contains(params, []byte(`\u`)) {
		return false
	}
	var p struct {
		Body struct {
			Embeds []struct {
				Kind    string          `json:"kind"`
				EmbedID *jsontext.Value `json:"embed_id"`
			} `json:"embeds"`
		} `json:"body"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	for _, embed := range p.Body.Embeds {
		if embed.Kind == "upload" && embed.EmbedID == nil {
			return true
		}
	}
	return false
}

// held returns the latest hold on any of keys that has not ended.
func (l *limits) held(keys []string, now time.Time) (hold, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var latest hold
	for _, key := range keys {
		if h, ok := l.holds[key]; ok && h.until.After(now) && h.until.After(latest.until) {
			latest = h
		}
	}
	return latest, !latest.until.IsZero()
}

// refused holds back what a refusal of a request counts toward: the key
// its category names, if that counts the request, and else its method.
func (l *limits) refused(keys []string, category string, until time.Time, message string) {
	key := keys[0]
	for _, k := range keys[1:] {
		if k == category {
			key = k
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holds == nil {
		l.holds = map[string]hold{}
	}
	if h, ok := l.holds[key]; !ok || until.After(h.until) {
		l.holds[key] = hold{until, message}
	}
}

// send sends a request with HoldBack: it waits for the holds on what it
// counts toward, and sends it again after each refusal of retry_after,
// within the limits' max and ctx.
func (l *limits) send(ctx context.Context, method string, params jsontext.Value, once func() (jsontext.Value, error)) (jsontext.Value, error) {
	keys := keys(method, params)
	deadline := time.Now().Add(l.max)
	for {
		if h, ok := l.held(keys, time.Now()); ok {
			if h.until.After(deadline) {
				return nil, apron.RetryAfter(h.message, time.Until(h.until))
			}
			if err := sleep(ctx, time.Until(h.until)); err != nil {
				return nil, err
			}
			continue
		}
		result, err := once()
		delay, category, ok := apron.RetryDelay(err)
		if !ok {
			return result, err
		}
		until := time.Now().Add(delay)
		message := ""
		if e, ok := errors.AsType[*apron.Error](err); ok {
			message = e.Message
		}
		l.refused(keys, category, until, message)
		if until.After(deadline) {
			return nil, err
		}
	}
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
