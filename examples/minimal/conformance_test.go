package main

import (
	"testing"

	"github.com/apron-chat/apron-server-go/wire/wiretest/conformance"
)

// TestConformance runs the protocol's core checks against the chat, in
// process: one room, guests, profiles, and /help.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Config{Server: NewChat().Server})
}
