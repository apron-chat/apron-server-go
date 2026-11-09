package aproncmd_test

import (
	"testing"

	"github.com/apron-chat/apron-server-go/aproncmd"
	flags "github.com/jessevdk/go-flags"
)

func TestServerConfigForCustomRunner(t *testing.T) {
	var options aproncmd.Options
	parser := flags.NewParser(&options, flags.None)
	_, err := parser.ParseArgs([]string{
		"--public-url", "https://chat.example.test",
		"--origin", "https://chat.example.test",
		"--webauthn.rp-id", "chat.example.test",
		"--webauthn.origin", "https://chat.example.test",
		"--disable-guests", "--push.disable",
		"--upload.max-mb", "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := aproncmd.ServerConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if config.PublicURL != options.PublicURL || !config.DisableGuests || !config.DisablePush {
		t.Fatalf("custom runner lost URL or admission options")
	}
	if len(config.OriginPatterns) != 1 || config.OriginPatterns[0] != options.PublicURL {
		t.Fatalf("unexpected origins: %v", config.OriginPatterns)
	}
	if config.WebAuthn == nil || config.MaxUploadBytes != 3<<20 {
		t.Fatal("custom runner lost passkey or upload options")
	}
	if config.Store != nil {
		t.Fatal("configuration conversion must not open a store")
	}
}

func TestServerConfigPreservesValidation(t *testing.T) {
	var options aproncmd.Options
	options.PublicURL = "https://chat.example.test"
	options.Push.AllowInsecure = true
	if _, err := aproncmd.ServerConfig(options); err == nil {
		t.Fatal("custom runners must retain insecure-push validation")
	}
}
