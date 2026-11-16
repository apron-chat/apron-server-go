package aproncmd_test

import (
	"strings"
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
	if config.SignalUnknownPasskeys {
		t.Fatal("the passkey page signals unknown passkeys without --webauthn.signal-unknown")
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

func TestServerConfigNamesAnExternalIssuer(t *testing.T) {
	var options aproncmd.Options
	parser := flags.NewParser(&options, flags.None)
	if _, err := parser.ParseArgs([]string{"--external.issuer", "https://id.example", "--external.scope", "openid profile"}); err != nil {
		t.Fatal(err)
	}
	config, err := aproncmd.ServerConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if issuer := config.ExternalIssuer; issuer == nil || issuer.Issuer != "https://id.example" || issuer.Scope != "openid profile" {
		t.Fatalf("ExternalIssuer = %#v", config.ExternalIssuer)
	}

	// A client_id shared by every app is refused, saying why and what
	// instead.
	if _, err := parser.ParseArgs([]string{"--external.issuer", "https://id.example", "--external.client-id", "apron"}); err != nil {
		t.Fatal(err)
	}
	if _, err := aproncmd.ServerConfig(options); err == nil || !strings.Contains(err.Error(), "--external.client-id") || !strings.Contains(err.Error(), "client ID metadata document") {
		t.Fatalf("ServerConfig with --external.client-id = %v", err)
	}
}

func TestServerConfigSignalsUnknownPasskeysWhenAsked(t *testing.T) {
	var options aproncmd.Options
	parser := flags.NewParser(&options, flags.None)
	if _, err := parser.ParseArgs([]string{"--webauthn.signal-unknown"}); err != nil {
		t.Fatal(err)
	}
	config, err := aproncmd.ServerConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if !config.SignalUnknownPasskeys {
		t.Fatal("--webauthn.signal-unknown did not reach the server's config")
	}
}
