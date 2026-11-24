package settings

import (
	"reflect"
	"testing"
	"time"
)

type example struct {
	Name    string        `long:"name" default:"assistant"`
	Context int           `long:"context" default:"20"`
	Timeout time.Duration `long:"timeout" default:"2m"`
	Hosts   []string      `long:"host" default:"a.example" default:"b.example"`
	Key     string        `long:"key" required:"true"`
	Plain   string        `long:"plain"`
	Group   struct {
		Limit int `long:"limit" default:"5"`
	} `group:"Limits" namespace:"limits"`
	untagged string
}

func TestApplyDefaults(t *testing.T) {
	var settings example
	settings.Context = 3
	settings.Hosts = []string{}
	settings.untagged = "kept"
	if err := ApplyDefaults(&settings); err != nil {
		t.Fatal(err)
	}
	want := example{Name: "assistant", Context: 3, Timeout: 2 * time.Minute, Hosts: []string{}, untagged: "kept"}
	want.Group.Limit = 5
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("settings = %+v, want %+v", settings, want)
	}

	var empty example
	if err := ApplyDefaults(&empty); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty.Hosts, []string{"a.example", "b.example"}) {
		t.Fatalf("Hosts = %q", empty.Hosts)
	}

	bad := struct {
		Timeout time.Duration `long:"timeout" default:"soon"`
	}{}
	if err := ApplyDefaults(&bad); err == nil {
		t.Fatal("applied a default that is not a duration")
	}
	if err := ApplyDefaults(example{}); err == nil {
		t.Fatal("applied defaults to a struct that is not a pointer")
	}
}

// A pointer option is zero while nil: a pointer to zero is a value of its
// own, which is kept.
func TestApplyDefaultsToPointers(t *testing.T) {
	type pointers struct {
		Limit *int           `long:"limit" default:"100"`
		Every *time.Duration `long:"every" default:"1m"`
	}
	zero := 0
	settings := pointers{Limit: &zero}
	if err := ApplyDefaults(&settings); err != nil {
		t.Fatal(err)
	}
	if settings.Limit != &zero || *settings.Limit != 0 || settings.Every == nil || *settings.Every != time.Minute {
		t.Fatalf("limit %v, every %v", settings.Limit, settings.Every)
	}
}

// Settings that were parsed keep their zeros.
func TestParsed(t *testing.T) {
	settings := example{Key: "k"}
	Parsed(&settings)
	if err := ApplyDefaults(&settings); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settings, example{Key: "k"}) {
		t.Fatalf("parsed settings = %+v", settings)
	}
	other := example{Key: "k"}
	if err := ApplyDefaults(&other); err != nil {
		t.Fatal(err)
	}
	if other.Context != 20 {
		t.Fatalf("settings not parsed: context %d", other.Context)
	}
}
