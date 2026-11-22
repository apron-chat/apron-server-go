package wire_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/apron-chat/apron-server-go/wire"
)

// The accessors' errors are plain errors, nil when there is none, and
// *Errors underneath when there is.
func TestParamErrors(t *testing.T) {
	read := func(f wire.Fields, name string) error {
		_, _, err := f.String(name)
		return err
	}
	if err := read(wire.Fields{}, "x"); err != nil {
		t.Fatalf("an absent field: %v", err)
	}
	err := read(wire.Fields{"x": []byte("1")}, "x")
	if wire.CodeOf(err) != wire.CodeInvalidParams {
		t.Fatalf("a number for a string: %v", err)
	}
	if _, ok := errors.AsType[*wire.Error](err); !ok {
		t.Fatalf("%T is not an *Error", err)
	}
	for err, want := range map[error]int{
		nil:                                    0,
		errors.New("x"):                        wire.CodeInternalError,
		wire.Denied("no"):                      wire.CodeDenied,
		fmt.Errorf("w: %w", wire.TooLarge("")): wire.CodeTooLarge,
	} {
		if got := wire.CodeOf(err); got != want {
			t.Errorf("CodeOf(%v) = %d, want %d", err, got, want)
		}
	}
}
