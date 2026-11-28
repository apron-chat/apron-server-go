package server

import "github.com/apron-chat/apron-server-go/apron"

// Requirement says whether a param a Decoder reads is required.
type Requirement bool

const (
	// Optional is a param that may be left out: the Decoder reads its zero
	// value.
	Optional Requirement = false
	// Required is a param that may not be left out: the Decoder's error is
	// then Missing.
	Required Requirement = true
)

// Decoder reads params one at a time, each with the protocol's rules for
// its kind (§1.1), and keeps the first error, so that a handler reads
// every param it needs and checks once:
//
//	d := r.Decoder()
//	kind := d.String("kind", server.Required)
//	token := server.IfPresent(d, "token", d.String) // nil when left out
//	if err := d.Err(); err != nil {
//		return err
//	}
//
// It is how params are read: the Server reads its handlers' typed params
// with one, and a Handler reads its own. A param of the wrong type, or a
// null where a value is needed, is invalid_params. The params are checked
// in the order they are read, and once the Decoder has an error every read
// returns a zero value. A check of the handler's own takes its place in
// that order with Fail. A param no method reads, such as an array of the
// backend's own, is in the raw Fields.
type Decoder struct {
	fields Fields
	err    error
}

// Decoder returns a Decoder of the fields.
func (f Fields) Decoder() *Decoder { return &Decoder{fields: f} }

// Decoder returns a Decoder of the request's params.
func (r *Request) Decoder() *Decoder { return r.fields.Decoder() }

// Err returns the first error of the reads, an *Error, or nil.
func (d *Decoder) Err() error { return d.err }

// Fail records err as the Decoder's error, unless it has one: a check of
// the handler's own, such as one value's range, in the order of the reads.
// A nil err records nothing, and neither does a nil *Error.
func (d *Decoder) Fail(err error) {
	if e, ok := err.(*apron.Error); ok && e == nil {
		return
	}
	if d.err == nil {
		d.err = err
	}
}

// Has reports whether the params have the field, null included.
func (d *Decoder) Has(name string) bool {
	_, ok := d.fields.Get(name)
	return ok
}

// read reads a param with one of Fields' readers.
func read[T any](d *Decoder, name string, p Requirement, get func(string) (T, bool, error)) T {
	var zero T
	if d.err != nil {
		return zero
	}
	value, present, err := get(name)
	switch {
	case err != nil:
		d.err = err
		return zero
	case !present && p == Required:
		d.err = apron.Missing(name)
		return zero
	}
	return value
}

// String reads a string param.
func (d *Decoder) String(name string, r Requirement) string {
	return read(d, name, r, d.fields.stringParam)
}

// ID reads a room_id or user_id param, which is not empty and has no
// whitespace or control characters (§2.4).
func (d *Decoder) ID(name string, r Requirement) string {
	return read(d, name, r, d.fields.idParam)
}

// Bool reads a boolean param.
func (d *Decoder) Bool(name string, r Requirement) bool {
	return read(d, name, r, d.fields.boolParam)
}

// Object reads an object param, with its values raw.
func (d *Decoder) Object(name string, r Requirement) Fields {
	return read(d, name, r, d.fields.objectParam)
}

// Ext reads an ext param (§4.13): an object whose values are any JSON,
// kept as they arrived, compacted; nil when left out.
func (d *Decoder) Ext(name string, r Requirement) apron.Ext {
	return read(d, name, r, d.fields.extParam)
}

// LogID reads a log_id param, such as a history bound (§2.1): a decimal
// string of a non-negative number below 2^53, as ParseLogID reads it.
func (d *Decoder) LogID(name string, r Requirement) apron.LogID {
	return read(d, name, r, d.fields.logIDParam)
}

// MessageID reads a message_id param (§2.2): a log_id that is not 0.
func (d *Decoder) MessageID(name string, r Requirement) apron.LogID {
	return read(d, name, r, d.fields.messageIDParam)
}

// MessageRef reads a reference to a message (§3.5.4), {"message_id": ...}.
func (d *Decoder) MessageRef(name string, r Requirement) apron.MessageRef {
	id := read(d, name, r, d.fields.messageRefParam)
	return apron.MessageRef{MessageID: id}
}

// Count reads a positive integer param, such as a limit.
func (d *Decoder) Count(name string, r Requirement) int {
	return read(d, name, r, d.fields.countParam)
}

// IfPresent reads an optional param with one of d's methods, such as
// d.String or d.Bool, and returns it, or nil when it is left out, or d has
// an error: for a param whose absence means something of its own, such as
// a profile field left as it is.
func IfPresent[T any](d *Decoder, name string, read func(name string, r Requirement) T) *T {
	value := read(name, Optional)
	if d.err != nil || !d.Has(name) {
		return nil
	}
	return &value
}

// parsed returns p, or, when d has an error, the zero P and the error.
func parsed[P any](p P, d *Decoder) (P, error) {
	if d.err != nil {
		var zero P
		return zero, d.err
	}
	return p, nil
}
