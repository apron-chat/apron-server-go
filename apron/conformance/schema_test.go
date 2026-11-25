package conformance

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The schema is the protocol's, as testdata/apron has it, when this module
// is in the server's repository; elsewhere there is nothing to compare.
func TestSchemaCopy(t *testing.T) {
	protocol, err := os.ReadFile("../../testdata/apron/schema/apron.schema.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no testdata/apron: the wire module is not in the server's repository")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(protocol, schemaDocument) {
		t.Fatal("apron.schema.json differs from testdata/apron/schema/apron.schema.json; copy it here")
	}
	frameSchema() // It compiles.
}

// A frame that breaks the schema, or whose result breaks its method's, is
// a violation of core.shape.schema; one that keeps it is not.
func TestCheckSchema(t *testing.T) {
	r := &run{violations: map[string]map[string]bool{}}
	checkSchema(r, obj{"method": "pong"}, "")
	checkSchema(r, obj{"id": "1", "result": obj{"message_id": "5"}}, "message")
	checkSchema(r, obj{"id": "2", "result": obj{}}, "x_unknown")
	if len(r.violations["schema"]) != 0 {
		t.Fatalf("valid frames: %v", r.violations)
	}
	checkSchema(r, obj{"method": "pong", "id": "1"}, "")
	checkSchema(r, obj{"id": "1", "result": obj{"message_id": 5.0}}, "message")
	var found []string
	for v := range r.violations["schema"] {
		found = append(found, v)
	}
	joined := strings.Join(found, "\n")
	if len(found) != 2 || !strings.Contains(joined, "pong: ServerFrame at /: ") ||
		!strings.Contains(joined, "message result: MessageResult at /message_id: ") {
		t.Fatalf("violations: %q", found)
	}
	for method, want := range map[string]string{"room_list": "RoomListResult", "me": "MeResult", "push_register": "PushRegisterResult", "": "Result"} {
		if got := resultName(method); got != want {
			t.Errorf("resultName(%q) = %q, want %q", method, got, want)
		}
	}
}
