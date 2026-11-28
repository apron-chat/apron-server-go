package server_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/aprontest"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// Fields read an object's members in the order they arrived, raw, names
// with escapes unquoted, and encode as a map of them does.
func TestFields(t *testing.T) {
	raw := jsontext.Value(`{ "b" : [1, 2] , "a" : null , "c":{"d":"é"} }`)
	f, err := server.ParseFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Get("a"); !ok || string(v) != "null" {
		t.Errorf("a: %s, %v", v, ok)
	}
	if v, ok := f.Get("b"); !ok || string(v) != "[1, 2]" {
		t.Errorf("b: %s, %v", v, ok)
	}
	if _, ok := f.Get("z"); ok || f.Len() != 3 {
		t.Errorf("z present, or %d members", f.Len())
	}
	var names []string
	for name := range f.All() {
		names = append(names, name)
	}
	if len(names) != 3 || names[0] != "b" || names[1] != "a" || names[2] != "c" {
		t.Errorf("All: %q", names)
	}
	m := f.Map()
	m["b"][1] = 'x'
	if v, _ := f.Get("b"); string(v) != "[1, 2]" {
		t.Error("Map shares the values")
	}
	got, err := apron.Marshal(f)
	want, _ := apron.Marshal(map[string]jsontext.Value{"b": jsontext.Value("[1, 2]"), "a": jsontext.Value("null"), "c": jsontext.Value(`{"d":"é"}`)})
	if err != nil || string(got) != string(want) {
		t.Errorf("Marshal: %s, %v; want %s", got, err, want)
	}
	var decoded server.Fields
	if err := json.Unmarshal(raw, &decoded); err != nil || !maps.EqualFunc(decoded.Map(), f.Map(), func(a, b jsontext.Value) bool { return string(a) == string(b) }) {
		t.Errorf("Unmarshal: %v, %v", decoded.Map(), err)
	}
	for _, bad := range []string{`null`, `[]`, `"x"`, `{`} {
		if _, err := server.ParseFields(jsontext.Value(bad)); apron.CodeOf(err) != apron.CodeInvalidParams {
			t.Errorf("%s: %v", bad, err)
		}
	}
	var zero server.Fields
	if zero.Len() != 0 {
		t.Error("the zero Fields are not empty")
	}
}

// A request's Params are its params object as it was sent, {} without.
func TestRequestParams(t *testing.T) {
	s := wired(&backend{}, server.Config{})
	got := make(chan string, 2)
	s.Handle("ext:params", server.Handler{Serve: func(_ context.Context, _ *server.Conn, r *server.Request, _ server.Reply[any]) error {
		got <- string(r.Params())
		return nil
	}})
	c := aprontest.Connect(t, s)
	signIn(t, c)
	send(t, c, `{"method":"ext:params","id":"1","params":{ "b" : [1, 2], "a":"é" }}`)
	send(t, c, `{"method":"ext:params","id":"2"}`)
	for _, want := range []string{`{ "b" : [1, 2], "a":"é" }`, `{}`} {
		if params := <-got; params != want {
			t.Errorf("Params %s, want %s", params, want)
		}
	}
}
