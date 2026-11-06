package aproncmd_test

import (
	"context"
	"time"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/aproncmd"
)

// Uptime is an extension with a setting of its own.
type Uptime struct {
	Settings struct {
		Precision time.Duration `long:"precision" default:"1s" description:"How precisely /uptime answers"`
	}
	started time.Time
}

func (*Uptime) Name() string { return "uptime" }

// Flags makes Uptime an aproncmd.Configurable: aprond reads --uptime.precision,
// or precision in the config file's [uptime] table.
func (u *Uptime) Flags() any { return &u.Settings }

func (u *Uptime) Init(h *apron.Host) error {
	u.started = time.Now()
	h.Command(apron.Command{
		Name: "uptime", Help: "how long the server has run",
		Run: func(_ context.Context, call *apron.CommandCall) (map[string]any, error) {
			call.Notice("Up for " + time.Since(u.started).Round(u.Settings.Precision).String())
			return nil, nil
		},
	})
	return nil
}

// A server of your own is aprond with extensions: this is all of its main
// function.
func ExampleMain() {
	aproncmd.Main(&Uptime{})
}
