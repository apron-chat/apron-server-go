package conformance

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/apron-chat/apron-server-go/wire/wiretest/internal/jsonschema"
)

// schemaDocument is schema/apron.schema.json of the protocol repository,
// copied, as the wire module cannot reach the protocol's files:
// TestSchemaCopy checks that it is the same as testdata/apron's, when this
// module is in the server's repository.
//
//go:embed apron.schema.json
var schemaDocument []byte

// frameSchema is schemaDocument compiled, which every frame from the
// server is checked against (core.shape.schema), as conform.py does.
var frameSchema = sync.OnceValue(func() *jsonschema.Schema {
	s, err := jsonschema.Compile(schemaDocument)
	if err != nil {
		panic("conformance: apron.schema.json: " + err.Error())
	}
	return s
})

// checkSchema checks a frame against the schema: as a ServerFrame, and a
// reply's result as its method's result (<Method>Result), when the schema
// defines one. method is the method of the request a reply answers, or "".
func checkSchema(r *run, frame obj, method string) {
	s := frameSchema()
	label, ok := frame["method"]
	if !ok {
		label = "reply"
	}
	for _, e := range s.Validate(frame, "ServerFrame") {
		r.violate("schema", fmt.Sprintf("%v: ServerFrame at %s: %s", label, orRoot(e.Path), e.Message))
	}
	result, ok := frame["result"]
	if _, isNotification := frame["method"]; isNotification || !ok {
		return
	}
	name := resultName(method)
	for _, e := range s.Validate(result, name) {
		r.violate("schema", fmt.Sprintf("%s result: %s at %s: %s", method, name, orRoot(e.Path), e.Message))
	}
}

// resultName is the name of a method's result in the schema, such as
// RoomListResult for room_list.
func resultName(method string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(method, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + strings.ToLower(part[1:]))
		}
	}
	return b.String() + "Result"
}

func orRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}
