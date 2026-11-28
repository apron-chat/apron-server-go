package aproncmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	flags "github.com/jessevdk/go-flags"

	"github.com/apron-chat/apron-server-go/aprond"
)

func parse(t *testing.T, args ...string) (*Options, *flags.Parser) {
	t.Helper()
	var options Options
	parser := flags.NewParser(&options, flags.Default&^flags.PrintErrors)
	if _, err := parser.ParseArgs(args); err != nil {
		t.Fatal(err)
	}
	return &options, parser
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aprond.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigFileSetsOptionsTheCommandLineDoesNot(t *testing.T) {
	path := writeConfig(t, `
addr = "0.0.0.0:9000"
public-url = "https://chat.example"
messages-per-minute = 30

[webauthn]
rp-id = "chat.example"
origin = ["https://chat.example"]
guest-first = true

[upload]
max-mb = 5
`)
	options, parser := parse(t, "--addr", "127.0.0.1:9999", "--upload.max-mb", "7")
	if err := loadConfig(parser, options, path); err != nil {
		t.Fatal(err)
	}
	if options.Addr != "127.0.0.1:9999" || options.Upload.MaxMB != 7 {
		t.Fatalf("command line overridden: addr %q, max-mb %d", options.Addr, options.Upload.MaxMB)
	}
	if options.PublicURL != "https://chat.example" || options.MessagesPerMinute != 30 || options.WebAuthn.RPID != "chat.example" || !options.WebAuthn.GuestFirst {
		t.Fatalf("file settings not applied: %+v", options)
	}
	if !reflect.DeepEqual(options.WebAuthn.Origins, []string{"https://chat.example"}) {
		t.Fatalf("a list replaces its default: %v", options.WebAuthn.Origins)
	}
	if options.Upload.MaxMessageMB != 20 {
		t.Fatalf("unset default changed: %d", options.Upload.MaxMessageMB)
	}
}

func TestConfigFileRejectsUnknownSettings(t *testing.T) {
	for _, text := range []string{`adress = ":80"`, "[upload]\nmax = 1", `config = "other.toml"`, `max-connections = "many"`} {
		options, parser := parse(t)
		if err := loadConfig(parser, options, writeConfig(t, text)); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestPrintedConfigLoadsBack(t *testing.T) {
	options, parser := parse(t, "--origin", "https://a.example", "--origin", "https://b.example", "--push.disable", "--tls.domain", "chat.example", "--webauthn.guest-first")
	var printed bytes.Buffer
	if err := printConfig(&printed, parser); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "[upload]") || strings.Contains(printed.String(), "print-config") {
		t.Fatalf("printed config:\n%s", printed.String())
	}
	loaded, fresh := parse(t)
	if err := loadConfig(fresh, loaded, writeConfig(t, printed.String())); err != nil {
		t.Fatalf("%v in:\n%s", err, printed.String())
	}
	if !reflect.DeepEqual(loaded, options) {
		t.Fatalf("round trip:\n%+v\nwant\n%+v", loaded, options)
	}
}

// The flags reach the server's configuration: each case parses its
// arguments and reads the fields they set.
func TestServerConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		got  func(aprond.Config) any
		want any
	}{
		{"tls domain is the public URL", []string{"--tls.domain", "chat.example", "--webauthn.rp-id", ""},
			func(c aprond.Config) any { return []any{c.PublicURL, c.WebAuthn == nil} }, []any{"https://chat.example", true}},
		{"VAPID", []string{"--push.vapid-private-key", "key", "--push.vapid-subject", "mailto:ops@chat.example"},
			func(c aprond.Config) any { return []string{c.VAPIDPrivateKey, c.VAPIDSubject} }, []string{"key", "mailto:ops@chat.example"}},
		{"insecure push", []string{"--push.allow-insecure"}, func(c aprond.Config) any { return c.AllowInsecurePush }, true},
		{"welcome", []string{"--welcome", "Sign in with **email**."}, func(c aprond.Config) any { return c.Welcome }, "Sign in with **email**."},
		{"roles", []string{"--role", "admin=ada", "--role", "Moderator = bob", "--role", "admin=carol"},
			func(c aprond.Config) any { return c.Roles }, map[string][]string{"admin": {"ada", "carol"}, "moderator": {"bob"}}},
		{"identity", []string{"--title", " Example chat ", "--description", "Talk about *examples*.", "--avatar", "https://chat.example/icon.png"},
			func(c aprond.Config) any { return []string{c.Title, c.Description, c.Avatar} },
			[]string{"Example chat", "Talk about *examples*.", "https://chat.example/icon.png"}},
		{"live text defaults", nil,
			func(c aprond.Config) any {
				return []any{c.LiveTextMaxDuration, c.LiveTextMaxBytes, c.TerminalKeepBytes, c.AppendMaxBytes, c.TerminalViewerURL}
			},
			[]any{time.Hour, int64(16 << 20), 64 << 10, 64 << 10, "https://terminal.apron.chat/"}},
		{"live text set", []string{"--live.max-minutes", "2", "--live.max-mb", "1", "--live.terminal-keep-kb", "8", "--live.append-max-kb", "256"},
			func(c aprond.Config) any {
				return []any{c.LiveTextMaxDuration, c.LiveTextMaxBytes, c.TerminalKeepBytes, c.AppendMaxBytes}
			},
			[]any{2 * time.Minute, int64(1 << 20), 8 << 10, 256 << 10}},
		{"terminal viewer", []string{"--live.terminal-viewer", "https://terminal.example/view"},
			func(c aprond.Config) any { return c.TerminalViewerURL }, "https://terminal.example/view"},
		{"no terminal viewer", []string{"--live.terminal-viewer", ""}, func(c aprond.Config) any { return c.TerminalViewerURL }, ""},
		{"external issuer", []string{"--external.issuer", "https://id.example", "--external.scope", "openid profile"},
			func(c aprond.Config) any {
				if c.ExternalIssuer == nil {
					return nil
				}
				return []string{c.ExternalIssuer.Issuer, c.ExternalIssuer.Scope}
			},
			[]string{"https://id.example", "openid profile"}},
		{"signal unknown passkeys", []string{"--webauthn.signal-unknown"}, func(c aprond.Config) any { return c.SignalUnknownPasskeys }, true},
		{"guest-first passkeys", []string{"--webauthn.guest-first"}, func(c aprond.Config) any { return c.GuestFirstPasskeys }, true},
	} {
		options, _ := parse(t, test.args...)
		config, err := serverConfig(*options)
		if err != nil {
			t.Errorf("%s: %q: %v", test.name, test.args, err)
		} else if got := test.got(config); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: %q gives %#v, want %#v", test.name, test.args, got, test.want)
		}
	}
}

// Flags that would make a broken or unsafe server are refused.
func TestServerConfigRefuses(t *testing.T) {
	for _, args := range [][]string{
		{"--push.vapid-subject", "ops@chat.example"},
		{"--push.vapid-subject", "http://chat.example"},
		// A public server refuses insecure push, as it lets clients reach
		// internal addresses.
		{"--push.allow-insecure", "--public-url", "https://chat.example"},
		{"--push.allow-insecure", "--tls.domain", "chat.example"},
		{"--role", "admin"}, {"--role", "=ada"}, {"--role", "admin="},
		// An append is part of a message's text, which a request carries.
		{"--live.append-max-kb", "257"},
		{"--live.terminal-viewer", "terminal.apron.chat"},
		{"--live.terminal-viewer", "javascript:alert(1)"},
		{"--live.terminal-viewer", "https://terminal.example/#stream="},
		{"--live.terminal-viewer", "https://user@terminal.example/"},
		// The log sender is for development: a public server refuses it.
		{"--email.enable", "--email.sender", "log", "--public-url", "https://chat.example"},
		{"--email.enable", "--email.sender", "log", "--tls.domain", "chat.example"},
		// smtp, the default sender, needs its server and sender address.
		{"--email.enable", "--email.from", "chat@example.com"},
		{"--email.enable", "--email.smtp-addr", "smtp.example:587"},
	} {
		options, _ := parse(t, args...)
		if _, err := serverConfig(*options); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestEmailSenders(t *testing.T) {
	// Email sign-in is off unless --email.enable is set, whatever the sender.
	for _, args := range [][]string{
		nil,
		{"--email.sender", "log"},
		{"--email.sender", "smtp", "--email.smtp-addr", "smtp.example:587", "--email.from", "chat@example.com"},
	} {
		options, _ := parse(t, args...)
		config, err := serverConfig(*options)
		if err != nil {
			t.Fatal(err)
		}
		if config.EmailSender != nil {
			t.Fatalf("email sender %T without --email.enable: %v", config.EmailSender, args)
		}
	}
	options, _ := parse(t, "--email.enable", "--email.sender", "log")
	if config, err := serverConfig(*options); err != nil || config.EmailSender != (aprond.LogEmailSender{}) || config.EmailLinkURL != "" {
		t.Fatalf("log sender %T, link %q, error %v", config.EmailSender, config.EmailLinkURL, err)
	}
	// smtp is the default sender.
	options, _ = parse(t, "--email.enable", "--email.smtp-addr", "smtp.example:587", "--email.from", "Apron <chat@example.com>", "--public-url", "https://chat.example/")
	config, err := serverConfig(*options)
	if err != nil {
		t.Fatal(err)
	}
	if sender, ok := config.EmailSender.(*smtpSender); !ok || sender.addr != "smtp.example:587" || sender.insecure || config.EmailLinkURL != "https://chat.example/" {
		t.Fatalf("smtp sender %#v, link %q", config.EmailSender, config.EmailLinkURL)
	}
}

// weatherExtension is a Configurable extension for the flag tests.
type weatherExtension struct {
	settings struct {
		Key   string   `long:"key" description:"The weather service's key"`
		Hosts []string `long:"host" default:"a.example" description:"Hosts to ask"`
		Days  int      `long:"days" default:"3" description:"Days to forecast; 0 is today only"`
		Hours *int     `long:"hours" default:"12" description:"Hours to forecast; 0 is none"`
	}
}

func (*weatherExtension) Name() string                 { return "weather" }
func (*weatherExtension) Init(*aprond.Registrar) error { return nil }
func (w *weatherExtension) Flags() any                 { return &w.settings }

func TestExtensionFlags(t *testing.T) {
	weather := &weatherExtension{}
	var options Options
	parser := flags.NewParser(&options, flags.Default&^flags.PrintErrors)
	targets, err := addExtensionFlags(parser, []aprond.Extension{weather})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseArgs([]string{"--weather.key", "k1"}); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "[weather]\nkey = \"from-file\"\nhost = [\"b.example\", \"c.example\"]\n")
	if err := loadConfig(parser, &options, path, targets...); err != nil {
		t.Fatal(err)
	}
	if weather.settings.Key != "k1" || !reflect.DeepEqual(weather.settings.Hosts, []string{"b.example", "c.example"}) {
		t.Fatalf("settings = %+v", weather.settings)
	}
	var printed bytes.Buffer
	if err := printConfig(&printed, parser); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "[weather]\n\n# The weather service's key\nkey = \"k1\"") {
		t.Fatalf("printed config:\n%s", printed.String())
	}
}

// aprond.Open gives an extension's settings their defaults however it is
// opened, and keeps what is set: a list the config file empties included.
func TestExtensionDefaultsWithoutMain(t *testing.T) {
	open := func(weather *weatherExtension) {
		t.Helper()
		config := aprond.DefaultConfig()
		config.Extensions = []aprond.Extension{weather}
		server, err := aprond.Open(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	weather := &weatherExtension{}
	weather.settings.Key = "k1"
	open(weather)
	if weather.settings.Key != "k1" || !reflect.DeepEqual(weather.settings.Hosts, []string{"a.example"}) {
		t.Fatalf("settings in Go = %+v", weather.settings)
	}

	emptied := &weatherExtension{}
	var options Options
	parser := flags.NewParser(&options, flags.Default&^flags.PrintErrors)
	targets, err := addExtensionFlags(parser, []aprond.Extension{emptied})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseArgs(nil); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(parser, &options, writeConfig(t, "[weather]\nhost = []\n"), targets...); err != nil {
		t.Fatal(err)
	}
	open(emptied)
	if emptied.settings.Hosts == nil || len(emptied.settings.Hosts) != 0 {
		t.Fatalf("hosts emptied in the config file = %#v", emptied.settings.Hosts)
	}

	// A setting with a default is its default when zero in Go, but for a
	// pointer to zero.
	zero := 0
	inGo := &weatherExtension{}
	inGo.settings.Hours = &zero
	open(inGo)
	if inGo.settings.Days != 3 || *inGo.settings.Hours != 0 {
		t.Fatalf("days %d, hours %d in Go", inGo.settings.Days, *inGo.settings.Hours)
	}

	bad := &badDefault{}
	config := aprond.DefaultConfig()
	config.Extensions = []aprond.Extension{bad}
	if _, err := aprond.Open(config); err == nil || !strings.Contains(err.Error(), "extension bad") {
		t.Fatalf("Open with a default that is not a duration = %v", err)
	}
}

// Main keeps a setting given as zero, on the command line or in the config
// file, and sets those it is not given to their defaults.
func TestExtensionZeroSettings(t *testing.T) {
	for _, test := range []struct {
		args        []string
		config      string
		days, hours int
	}{
		{days: 3, hours: 12},
		{args: []string{"--weather.days", "0", "--weather.hours", "0"}, days: 0, hours: 0},
		{config: "[weather]\ndays = 0\nhours = 0\n", days: 0, hours: 0},
		{args: []string{"--weather.days", "5"}, config: "[weather]\nhours = 1\n", days: 5, hours: 1},
	} {
		weather := &weatherExtension{}
		options := Options{}
		parser := flags.NewParser(&options, flags.Default&^flags.PrintErrors)
		targets, err := addExtensionFlags(parser, []aprond.Extension{weather})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parser.ParseArgs(test.args); err != nil {
			t.Fatal(err)
		}
		if test.config != "" {
			if err := loadConfig(parser, &options, writeConfig(t, test.config), targets...); err != nil {
				t.Fatal(err)
			}
		}
		markParsed(targets)
		config := aprond.DefaultConfig()
		config.Extensions = []aprond.Extension{weather}
		server, err := aprond.Open(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if weather.settings.Days != test.days || *weather.settings.Hours != test.hours {
			t.Errorf("%q and %q: days %d, hours %d, want %d and %d", test.args, test.config,
				weather.settings.Days, *weather.settings.Hours, test.days, test.hours)
		}
	}
}

// badDefault is an extension whose setting's default is not its type's.
type badDefault struct {
	settings struct {
		Every time.Duration `long:"every" default:"often" description:"How often"`
	}
}

func (*badDefault) Name() string                 { return "bad" }
func (*badDefault) Init(*aprond.Registrar) error { return nil }
func (b *badDefault) Flags() any                 { return &b.settings }
