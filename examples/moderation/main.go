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
	"unicode/utf8"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
	"github.com/apron-chat/apron-server-go/wire/commands"
)

func main() { aproncmd.Main(&Moderation{}) }

// Moderation filters messages and keeps each room's slow mode.
type Moderation struct {
	Settings struct {
		Words []string `long:"word" description:"A word to mask in messages; repeat for more"`
		// LinkAge is how old an account must be to post links; zero is 24
		// hours.
		LinkAge time.Duration `long:"link-age" default:"24h" description:"How old an account must be to post links; guests never may"`
	}
	host  *apron.Host
	words map[string]bool

	mu sync.Mutex
	// posted is when each member last posted in each room, for slow mode,
	// as the log says: a message the server refused is no post.
	posted map[string]time.Time
}

func (*Moderation) Name() string { return "moderation" }
func (m *Moderation) Flags() any { return &m.Settings }

var (
	linkPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S`)
	// wordPattern matches words in any script.
	wordPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)
)

func (m *Moderation) Init(r *apron.Registrar) error {
	m.host = r.Host
	m.posted = make(map[string]time.Time)
	if m.Settings.LinkAge == 0 {
		m.Settings.LinkAge = 24 * time.Hour
	}
	m.words = make(map[string]bool)
	for _, word := range m.Settings.Words {
		m.words[strings.ToLower(word)] = true
	}
	r.FilterMessage(m.filter)
	r.OnEvent(m.notePost)
	// /slowmode has two forms, which /help lists apart: /slowmode <seconds>
	// and /slowmode off. Moderators may use them in the room they run in.
	room := []string{commands.FieldRoom}
	moderator := apron.May(func(u apron.User, _ apron.Room) bool { return moderates(u) })
	r.Commands(commands.Command{
		Name: "slowmode",
		Forms: []commands.Form{
			{Hint: "<seconds>", Help: "limit how often each member may post here", Uses: room, Requires: room, Group: commands.GroupRole, May: moderator},
			{Name: "slowmode off", Help: "let members post as often as they like here", Uses: room, Requires: room, Group: commands.GroupRole, May: moderator},
		},
		Run: m.slowmode,
	})
	return nil
}

func moderates(u apron.User) bool {
	return u.HasRole(apron.RoleModerator) || u.HasRole(apron.RoleAdmin)
}

// notePost records when each member posted in each room, for slow mode.
func (m *Moderation) notePost(_ context.Context, e apron.Event) {
	if message, ok := apron.DecodeMessage(e); ok && message.New() {
		m.mu.Lock()
		m.posted[message.RoomID+"\x00"+message.From.UserID] = time.Now()
		m.mu.Unlock()
	}
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
	if d.MessageID == "" && !moderates(d.From) {
		if wait := m.slowed(d.RoomID, d.From.ID); wait > 0 {
			return apron.RetryAfter("Slow mode is on in this room", wait)
		}
	}
	if len(m.words) > 0 {
		d.SetText(wordPattern.ReplaceAllStringFunc(d.Text(), func(word string) string {
			if !m.words[strings.ToLower(word)] {
				return word
			}
			first, _ := utf8.DecodeRuneInString(word)
			return string(first) + strings.Repeat("*", utf8.RuneCountInString(word)-1)
		}))
	}
	return nil
}

func (m *Moderation) mayLink(u apron.User) bool {
	switch {
	case moderates(u):
		return true
	case !u.Account:
		return false
	case u.Created.IsZero():
		return true // made before the server kept the time: not new
	}
	return time.Since(u.Created) >= m.Settings.LinkAge
}

// slowed returns how long the user must wait to post in the room.
func (m *Moderation) slowed(roomID, userID string) time.Duration {
	var seconds int
	if found, _ := m.host.Store().Get("slowmode/"+roomID, &seconds); !found || seconds <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return max(0, time.Until(m.posted[roomID+"\x00"+userID].Add(time.Duration(seconds)*time.Second)))
}

func (m *Moderation) slowmode(_ context.Context, call *commands.Call) error {
	cc := apron.CallOf(call) // who ran it, and in which room
	key := "slowmode/" + cc.Room.ID
	seconds, err := strconv.Atoi(call.Rest())
	var notice string
	switch {
	case call.Form.Name == "slowmode off" || err == nil && seconds == 0:
		if err := m.host.Store().Delete(key); err != nil {
			return err
		}
		notice = fmt.Sprintf("@%s turned slow mode off", cc.User.ID)
	case err != nil || seconds < 0 || seconds > 3600:
		return call.UsageError("at most 3600 seconds")
	default:
		if err := m.host.Store().Put(key, seconds); err != nil {
			return err
		}
		notice = fmt.Sprintf("@%s turned slow mode on: one message every %d seconds", cc.User.ID, seconds)
	}
	if err := m.host.RoomNotice(cc.Room.ID, notice); err != nil {
		return err
	}
	return call.Done()
}
