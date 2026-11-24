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
