// Command moderation is an Apron server for an open community: an extension
// keeps new accounts and guests from posting links, masks words from a
// list, and gives moderators a /slowmode command that limits how often each
// member may post in a room.
//
//	go run ./examples/moderation --role moderator=ada \
//	  --moderation.word darn --moderation.link-age 24h
package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Moderation{}) }

// Moderation filters messages and keeps each room's slow mode.
type Moderation struct {
	Settings struct {
		Words   []string      `long:"word" description:"A word to mask in messages; repeat for more"`
		LinkAge time.Duration `long:"link-age" default:"24h" description:"How old an account must be to post links; guests never may"`
	}
	host  *apron.Host
	words *regexp.Regexp

	mu sync.Mutex
	// posted is when each member last posted in each room, for slow mode.
	posted map[string]time.Time
}

func (*Moderation) Name() string { return "moderation" }
func (m *Moderation) Flags() any { return &m.Settings }

var linkPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S`)

func (m *Moderation) Init(h *apron.Host) error {
	m.host = h
	m.posted = make(map[string]time.Time)
	if len(m.Settings.Words) > 0 {
		quoted := make([]string, len(m.Settings.Words))
		for i, word := range m.Settings.Words {
			quoted[i] = regexp.QuoteMeta(word)
		}
		m.words = regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
	}
	h.FilterMessage(m.filter)
	h.Command(apron.Command{
		Name: "slowmode", Usage: "/slowmode <seconds>|off", Help: "limit how often each member may post here",
		Available: func(u apron.User, _ apron.Room) bool { return u.HasRole("moderator") || u.HasRole("admin") },
		Run:       m.slowmode,
	})
	return nil
}

// filter refuses links from new accounts, keeps slow mode, and masks
// words. Bots are trusted; edits are not slowed down.
func (m *Moderation) filter(_ context.Context, d *apron.Draft) error {
	if d.From.Bot {
		return nil
	}
	if linkPattern.MatchString(d.Text()) && !m.mayLink(d.From) {
		return apron.Denied(fmt.Sprintf("New accounts can post links once they are %s old", m.Settings.LinkAge))
	}
	if d.MessageID == "" {
		if wait := m.slowed(d.RoomID, d.From.ID); wait > 0 {
			return apron.RetryAfter("Slow mode is on in this room", wait)
		}
	}
	if m.words != nil {
		d.Body["text"] = m.words.ReplaceAllStringFunc(d.Text(), func(word string) string {
			return word[:1] + strings.Repeat("*", len([]rune(word))-1)
		})
	}
	return nil
}

func (m *Moderation) mayLink(u apron.User) bool {
	switch {
	case u.HasRole("moderator"), u.HasRole("admin"):
		return true
	case !u.Account:
		return false
	case u.Created.IsZero():
		return true // made before the server kept the time: not new
	}
	return time.Since(u.Created) >= m.Settings.LinkAge
}

// slowed returns how long the user must wait to post in the room, and
// otherwise notes the post.
func (m *Moderation) slowed(roomID, userID string) time.Duration {
	var seconds int
	if found, _ := m.host.Store().Get("slowmode/"+roomID, &seconds); !found || seconds <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := roomID + "\x00" + userID
	if wait := time.Until(m.posted[key].Add(time.Duration(seconds) * time.Second)); wait > 0 {
		return wait
	}
	m.posted[key] = time.Now()
	return 0
}

func (m *Moderation) slowmode(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
	key := "slowmode/" + call.Room.ID
	if call.Args == "off" || call.Args == "0" {
		m.host.Store().Delete(key)
		return nil, m.host.RoomNotice(call.Room.ID, fmt.Sprintf("@%s turned slow mode off", call.User.ID))
	}
	seconds, err := strconv.Atoi(call.Args)
	if err != nil || seconds < 0 || seconds > 3600 {
		return nil, apron.InvalidParams("Usage: /slowmode <seconds>|off, at most 3600 seconds")
	}
	if err := m.host.Store().Put(key, seconds); err != nil {
		return nil, err
	}
	return nil, m.host.RoomNotice(call.Room.ID, fmt.Sprintf("@%s turned slow mode on: one message every %d seconds", call.User.ID, seconds))
}
