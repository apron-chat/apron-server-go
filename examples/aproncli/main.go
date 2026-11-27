// Command aproncli is a command-line client of any Apron server, on the
// client side of the protocol's library: packages apron, apron/client, and
// apron/archive. It posts, lists and joins rooms, reads and follows
// history, exports rooms as an archive (apron-web's ARCHIVE.md), and sends
// raw requests:
//
//	export APRON_URL=wss://chat.example/ws APRON_TOKEN=...
//	aproncli info
//	aproncli message --room general --format markdown 'Deployed *v1.2*'
//	aproncli history --room general --limit 50
//	aproncli tail --room general
//	aproncli export > chat.jsonl
//	aproncli export --resume chat.jsonl > chat-new.jsonl
//	aproncli call room_list '{"filter":"joined"}'
//
// It signs in with --token, or $APRON_TOKEN (§3.2.1), or, without one, as
// a guest named --guest; info also runs signed out. Run it with --help for
// its commands, and a command with --help for its flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
)

// agent is what aproncli tells servers it is (§3.2).
const agent = "aproncli"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:], env{os.Stdin, os.Stdout, os.Stderr, os.Getenv})
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case errors.Is(err, context.Canceled):
		os.Exit(130)
	default:
		fmt.Fprintf(os.Stderr, "aproncli: %v\n", err)
		os.Exit(1)
	}
}

// env is what a run reads and writes besides the network.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
}

// errUsage is a command line that is not one; its usage has been printed.
var errUsage = errors.New("usage")

// command is one of aproncli's commands.
type command struct {
	name, args, summary string
	run                 func(ctx context.Context, c *cli, args []string) error
}

// commands are aproncli's commands, which init fills in: their funcs
// read the list for their usage.
var commands []command

func init() {
	commands = []command{
		{"info", "[--json]", "show the server frame, and who you are", info},
		{"message", "[--room R] [--format markdown] [--reply-to ID] TEXT|-", "post a message; - reads it from stdin", message},
		{"rooms", "[--joined] [--json]", "list the rooms you can see", rooms},
		{"join", "ROOM", "join a room", join},
		{"leave", "ROOM", "leave a room", leave},
		{"history", "--room R [--limit N] [--json]", "print a room's latest messages", history},
		{"tail", "[--room R] [--limit N] [--json]", "print the latest messages, then new ones as they come", tail},
		{"export", "[--room R]... [--resume FILE] > FILE", "write rooms' history as an archive (ARCHIVE.md)", export},
		{"call", "METHOD [JSON|-]", "send a request, and print its result", call},
	}
}

// cli is a run's global flags and environment.
type cli struct {
	env
	url, token, guest string
	maxWait           time.Duration
}

// run runs the command line args.
func run(ctx context.Context, args []string, e env) error {
	c := &cli{env: e}
	flags := flag.NewFlagSet("aproncli", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	flags.StringVar(&c.url, "url", e.getenv("APRON_URL"), "the server's address, such as wss://chat.example/ws; $APRON_URL by default")
	flags.StringVar(&c.token, "token", "", "the token to sign in with; $APRON_TOKEN by default")
	flags.StringVar(&c.guest, "guest", "", "without a token, sign in as a guest with this name")
	flags.DurationVar(&c.maxWait, "max-wait", time.Minute, "how long a request waits out the server's rate limits (retry_after) before it fails; 0 fails at once")
	flags.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: aproncli [flags] COMMAND [ARGS]\n\nA command-line client of any Apron server.\n\nCommands:\n")
		for _, cmd := range commands {
			fmt.Fprintf(e.stderr, "  %-8s %s\n           %s\n", cmd.name, cmd.args, cmd.summary)
		}
		fmt.Fprintf(e.stderr, "\nFlags:\n")
		flags.PrintDefaults()
		fmt.Fprintf(e.stderr, "\nRun 'aproncli COMMAND --help' for a command's flags.\n")
	}
	if err := flags.Parse(args); err != nil {
		return usage(err)
	}
	if c.token == "" {
		c.token = e.getenv("APRON_TOKEN")
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return errUsage
	}
	name, rest := flags.Arg(0), flags.Args()[1:]
	if name == "help" {
		if len(rest) == 0 {
			flags.Usage()
			return nil
		}
		name, rest = rest[0], []string{"--help"}
	}
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd.run(ctx, c, rest)
		}
	}
	fmt.Fprintf(e.stderr, "aproncli: no command %q\n", name)
	flags.Usage()
	return errUsage
}

// usage returns a flag package's parse error as a run's: --help is no
// error, and the others have printed their usage.
func usage(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return flag.ErrHelp
	}
	return errUsage
}

// flags returns the flag set of a command, whose usage names its args.
func (c *cli) flags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	for _, cmd := range commands {
		if cmd.name == name {
			flags.Usage = func() {
				fmt.Fprintf(c.stderr, "Usage: aproncli %s %s\n\n%s.\n", cmd.name, cmd.args, strings.ToUpper(cmd.summary[:1])+cmd.summary[1:])
				if hasFlags(flags) {
					fmt.Fprintf(c.stderr, "\nFlags:\n")
					flags.PrintDefaults()
				}
			}
		}
	}
	return flags
}

func hasFlags(flags *flag.FlagSet) bool {
	has := false
	flags.VisitAll(func(*flag.Flag) { has = true })
	return has
}

// parse parses a command's args, and checks that it has from min to max
// arguments after its flags; max < 0 is any number.
func parse(flags *flag.FlagSet, args []string, min, max int) error {
	if err := flags.Parse(args); err != nil {
		return usage(err)
	}
	if n := flags.NArg(); n < min || max >= 0 && n > max {
		flags.Usage()
		return errUsage
	}
	return nil
}

// connect connects to the server, and signs in with the token, or as a
// guest, unless signIn is false and the command line names neither.
func (c *cli) connect(ctx context.Context, signIn bool, opts ...client.Option) (*client.Conn, error) {
	if c.url == "" {
		return nil, errors.New("no server: set --url or $APRON_URL")
	}
	if c.maxWait > 0 {
		opts = append(opts, client.HoldBack(c.maxWait))
	}
	conn, err := client.Connect(ctx, c.url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", c.url, err)
	}
	hello := conn.Hello()
	if hello.Apron != apron.Version {
		// A client warns, and goes on as well as it can (§3.1).
		fmt.Fprintf(c.stderr, "aproncli: the server speaks protocol version %d, and aproncli %d: some things may not work\n", hello.Apron, apron.Version)
	}
	var auth apron.AuthParams
	switch {
	case c.token != "":
		auth = apron.AuthParams{Scheme: "token", Token: c.token, Agent: agent}
	case c.guest != "":
		auth = apron.AuthParams{Scheme: "guest", Name: c.guest, Agent: agent}
	case !signIn:
		return conn, nil
	default:
		conn.Close()
		return nil, fmt.Errorf("not signed in: set --token or $APRON_TOKEN, or --guest NAME (the server offers %s)", schemes(hello))
	}
	if _, err := conn.Auth(ctx, auth); err != nil {
		conn.Close()
		switch apron.CodeOf(err) {
		case apron.CodeUnsupported:
			return nil, fmt.Errorf("the server does not offer sign-in with %s; it offers %s", auth.Scheme, schemes(hello))
		case apron.CodeDenied:
			return nil, fmt.Errorf("signing in with %s was refused: %s", auth.Scheme, said(err))
		}
		return nil, fmt.Errorf("signing in with %s: %w", auth.Scheme, err)
	}
	return conn, nil
}

// schemes lists the sign-in schemes that a server frame offers (§3.2).
func schemes(hello apron.ServerInfo) string {
	offered := slices.Clone(hello.Auth)
	for _, s := range hello.Signup {
		if !slices.Contains(hello.Auth, s) {
			offered = append(offered, s+" (to sign up)")
		}
	}
	if len(offered) == 0 {
		return "none"
	}
	return strings.Join(offered, ", ")
}

// said is what an error reply says, for people (§1.1), or the error.
func said(err error) string {
	if e, ok := errors.AsType[*apron.Error](err); ok {
		return e.Message
	}
	return err.Error()
}

// needs fails unless the server frame lists a capability.
func needs(conn *client.Conn, capability string) error {
	if !conn.Hello().Has(capability) {
		return fmt.Errorf("the server does not offer %s", capability)
	}
	return nil
}
