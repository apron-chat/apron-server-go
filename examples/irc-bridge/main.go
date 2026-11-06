// Command irc-bridge is an Apron server whose room is bridged to an IRC
// channel, both ways: IRC lines are posted by an "irc" bot, with the channel
// and the nick that sent each in the message's ext under irc-bridge
// (PROTOCOL.md §4.12), and messages people post are sent to the channel. The bridge keeps the
// log_id of the last message it relayed, so it relays what was posted while
// it was down once it is back.
//
//	go run ./examples/irc-bridge --irc-bridge.server irc.libera.chat:6697 \
//	  --irc-bridge.channel '#my-project' --irc-bridge.nick apron-bridge
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

func main() { aproncmd.Main(&Bridge{}) }

// Bridge relays between a room and an IRC channel.
type Bridge struct {
	Settings struct {
		Server    string `long:"server" description:"The IRC server, as host:port"`
		Plaintext bool   `long:"plaintext" description:"Connect without TLS"`
		Nick      string `long:"nick" default:"apron-bridge" description:"The bridge's nick"`
		Channel   string `long:"channel" description:"The IRC channel, such as #my-project"`
		Room      string `long:"room" default:"general" description:"The Apron room bridged to the channel"`
	}
	host  *apron.Host
	relay *apron.Bot

	mu   sync.Mutex
	conn *ircConn // nil while disconnected
}

func (*Bridge) Name() string { return "irc-bridge" }
func (b *Bridge) Flags() any { return &b.Settings }

func (b *Bridge) Init(r *apron.Registrar) error {
	if b.Settings.Server == "" || !strings.HasPrefix(b.Settings.Channel, "#") {
		return errors.New("set --irc-bridge.server and --irc-bridge.channel")
	}
	// Settings left at zero, as when the extension is used without
	// aproncmd's flags, take the flags' defaults.
	if b.Settings.Nick == "" {
		b.Settings.Nick = "apron-bridge"
	}
	if b.Settings.Room == "" {
		b.Settings.Room = "general"
	}
	b.host = r.Host
	var err error
	if b.relay, err = r.Bot("irc", apron.BotOptions{Name: "IRC"}); err != nil {
		return err
	}
	if err := b.relay.Join(r.Context(), b.Settings.Room); err != nil {
		return err
	}
	// Follow keeps the bridge's place in the log, so what was posted while
	// the bridge was down is relayed once it is back.
	r.Follow(b.outbound)
	r.Go(b.connectLoop)
	return nil
}

// outbound sends a message posted in the room to the channel. While the
// bridge is disconnected it fails, and Follow sends the event again later.
func (b *Bridge) outbound(_ context.Context, e apron.Event) error {
	m, ok := apron.DecodeMessage(e)
	if ok && m.New() && !m.Deleted && m.RoomID == b.Settings.Room && m.From.UserID != b.relay.UserID() && !strings.HasPrefix(m.From.UserID, "~") {
		b.mu.Lock()
		conn := b.conn
		b.mu.Unlock()
		if conn == nil {
			return errors.New("not connected to IRC")
		}
		name := m.From.Name
		if name == "" {
			name = m.From.UserID
		}
		for _, line := range strings.Split(strings.TrimSpace(m.Text), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				if err := conn.send("PRIVMSG", b.Settings.Channel, truncate("<"+name+"> "+line, 400)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// connectLoop keeps a connection to IRC, waiting longer after each failure.
func (b *Bridge) connectLoop(ctx context.Context) {
	for delay := time.Second; ctx.Err() == nil; delay = min(2*delay, 5*time.Minute) {
		err := b.session(ctx)
		b.host.Logger().Warn("IRC connection ended", "error", err, "retry_in", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
}

// session connects, joins the channel, and posts what the channel says
// until the connection fails.
func (b *Bridge) session(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var raw net.Conn
	var err error
	if b.Settings.Plaintext {
		raw, err = dialer.DialContext(ctx, "tcp", b.Settings.Server)
	} else {
		raw, err = (&tls.Dialer{NetDialer: dialer}).DialContext(ctx, "tcp", b.Settings.Server)
	}
	if err != nil {
		return err
	}
	conn := &ircConn{conn: raw}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	defer raw.Close()
	defer func() {
		b.mu.Lock()
		b.conn = nil
		b.mu.Unlock()
	}()

	nick := b.Settings.Nick
	conn.send("NICK", nick)
	conn.send("USER", nick, "0", "*", "Apron bridge")
	lines := bufio.NewScanner(raw)
	for lines.Scan() {
		prefix, command, params := parseLine(lines.Text())
		switch command {
		case "PING":
			conn.send("PONG", params...)
		case "433": // the nick is taken
			nick += "_"
			conn.send("NICK", nick)
		case "001": // welcome: registered
			conn.send("JOIN", b.Settings.Channel)
		case "JOIN":
			if strings.EqualFold(nickOf(prefix), nick) {
				b.mu.Lock()
				b.conn = conn
				b.mu.Unlock()
				b.host.Logger().Info("bridging", "channel", b.Settings.Channel, "room", b.Settings.Room)
			}
		case "PRIVMSG":
			if len(params) == 2 && strings.EqualFold(params[0], b.Settings.Channel) {
				from, text := nickOf(prefix), params[1]
				if action, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
					text = "_" + strings.TrimSuffix(action, "\x01") + "_"
				}
				_, err := b.relay.Post(ctx, b.Settings.Room, "**"+from+":** "+text, apron.Markdown(),
					apron.WithExt(map[string]any{"channel": b.Settings.Channel, "nick": from}))
				if err != nil {
					b.host.Logger().Error("posting from IRC", "error", err)
				}
			}
		}
	}
	return lines.Err()
}

// ircConn writes IRC lines, one at a time.
type ircConn struct {
	mu   sync.Mutex
	conn net.Conn
}

func (c *ircConn) send(command string, params ...string) error {
	line := command
	for i, param := range params {
		param = strings.NewReplacer("\r", " ", "\n", " ").Replace(param)
		if i == len(params)-1 && (param == "" || strings.ContainsAny(param, " :") || param[0] == ':') {
			param = ":" + param
		}
		line += " " + param
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.conn.Write([]byte(line + "\r\n"))
	return err
}

// parseLine splits an IRC line into its prefix, command, and params, the
// trailing one included.
func parseLine(line string) (prefix, command string, params []string) {
	if rest, ok := strings.CutPrefix(line, ":"); ok {
		prefix, line, _ = strings.Cut(rest, " ")
	}
	line, trailing, hasTrailing := strings.Cut(line, " :")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return prefix, "", nil
	}
	command, params = fields[0], fields[1:]
	if hasTrailing {
		params = append(params, trailing)
	}
	return prefix, command, params
}

func nickOf(prefix string) string {
	nick, _, _ := strings.Cut(prefix, "!")
	return nick
}

// truncate cuts text to at most n bytes, at a rune boundary.
func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !isRuneStart(text[n]) {
		n--
	}
	return text[:n] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
