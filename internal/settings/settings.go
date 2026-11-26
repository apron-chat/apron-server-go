// Package settings applies the defaults of an extension's settings: a
// struct of options tagged as github.com/jessevdk/go-flags reads them
// ("long", "default"), which aproncmd parses as flags.
package settings

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	flags "github.com/jessevdk/go-flags"
)

// parsed holds the settings that Parsed was told of, as keys.
var parsed sync.Map

// Parsed records that settings, a pointer to a struct, were parsed, as
// aproncmd parses flags and its config file: every option is set, to its
// default when it was not given, so that ApplyDefaults leaves them as they
// are, and an option given as zero stays zero. A process parses its
// settings once, so what it records is kept for as long as it runs.
func Parsed(settings any) {
	parsed.Store(settings, true)
}

// ApplyDefaults sets each option of settings, a pointer to a struct, that
// is zero and has a default tag to its default, as go-flags converts it,
// and leaves every other option as it is, as it leaves settings that were
// Parsed. Options are the fields with a long or short name, in settings or
// in the structs it nests, as go-flags finds them. An option whose zero is
// a value of its own is a pointer, which is zero while nil: a pointer to
// zero is kept.
func ApplyDefaults(settings any) error {
	target := reflect.ValueOf(settings)
	if target.Kind() != reflect.Pointer || target.IsNil() || target.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("settings are a %T, not a pointer to a struct", settings)
	}
	if _, ok := parsed.Load(settings); ok {
		return nil
	}
	// go-flags sets every option it is not given to its default, so it
	// parses no arguments into a zero copy, whose defaults are then
	// copied where settings has none of its own.
	defaults := reflect.New(target.Elem().Type())
	parser := flags.NewParser(defaults.Interface(), flags.None)
	if _, err := parser.ParseArgs([]string{}); err != nil {
		if flagErr, ok := errors.AsType[*flags.Error](err); !ok || flagErr.Type != flags.ErrRequired {
			return err
		}
	}
	copyDefaults(target.Elem(), defaults.Elem())
	return nil
}

// copyDefaults copies the options of from that have a default into to's
// that are zero.
func copyDefaults(to, from reflect.Value) {
	for i := range to.NumField() {
		field := to.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		switch {
		case field.Tag.Get("long") != "" || field.Tag.Get("short") != "":
			if _, has := field.Tag.Lookup("default"); has && to.Field(i).IsZero() {
				to.Field(i).Set(from.Field(i))
			}
		case field.Type.Kind() == reflect.Struct:
			copyDefaults(to.Field(i), from.Field(i))
		}
	}
}
