package wire

// Presence says whether a param a Decoder reads is required.
type Presence bool

const (
	// Optional is a param that may be left out: the Decoder reads its zero
	// value.
	Optional Presence = false
	// Required is a param that may not be left out: the Decoder's error is
	// then Missing.
	Required Presence = true
)

// Decoder reads params one at a time, with the rules of Fields' accessors,
// and keeps the first error, so that a handler reads every param it needs
// and checks once:
//
//	d := r.Decoder()
//	kind := d.String("kind", wire.Required)
//	token := wire.Opt(d, "token", d.String) // nil when left out
//	if err := d.Err(); err != nil {
//		return err
//	}
//
// The params are checked in the order they are read, and once the Decoder
// has an error every read returns a zero value. A check of the handler's
// own takes its place in that order with Fail. The Server's typed params
// are read with a Decoder (ParseMessage, ParseRoomList, ...).
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
	if e, ok := err.(*Error); ok && e == nil {
		return
	}
	if d.err == nil {
		d.err = err
	}
}

// Has reports whether the params have the field, null included.
func (d *Decoder) Has(name string) bool {
	_, ok := d.fields[name]
	return ok
}

// read reads a param with one of Fields' accessors.
func read[T any](d *Decoder, name string, p Presence, get func(string) (T, bool, error)) T {
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
		d.err = Missing(name)
		return zero
	}
	return value
}

// String reads a string param, as Fields.String does.
func (d *Decoder) String(name string, p Presence) string {
	return read(d, name, p, d.fields.String)
}

// ID reads a room_id or user_id param, as Fields.IDParam does.
func (d *Decoder) ID(name string, p Presence) string {
	return read(d, name, p, d.fields.IDParam)
}

// Bool reads a boolean param, as Fields.Bool does.
func (d *Decoder) Bool(name string, p Presence) bool {
	return read(d, name, p, d.fields.Bool)
}

// Object reads an object param, as Fields.Object does.
func (d *Decoder) Object(name string, p Presence) Fields {
	return read(d, name, p, d.fields.Object)
}

// Ext reads an ext param, as Fields.Ext does: nil when left out.
func (d *Decoder) Ext(name string, p Presence) Ext {
	return read(d, name, p, d.fields.Ext)
}

// LogID reads a log_id param, as Fields.LogID does.
func (d *Decoder) LogID(name string, p Presence) LogID {
	return read(d, name, p, d.fields.LogID)
}

// MessageID reads a message_id param, as Fields.MessageID does.
func (d *Decoder) MessageID(name string, p Presence) LogID {
	return read(d, name, p, d.fields.MessageID)
}

// Ref reads a reference to a message, as Fields.MessageRef does.
func (d *Decoder) Ref(name string, p Presence) Ref {
	id := read(d, name, p, d.fields.MessageRef)
	return Ref{MessageID: id}
}

// Count reads a positive integer param, as Fields.Count does.
func (d *Decoder) Count(name string, p Presence) int {
	return read(d, name, p, d.fields.Count)
}

// Opt reads an optional param with one of d's methods, such as d.String or
// d.Bool, and returns it, or nil when it is left out, or d has an error:
// for a param whose absence means something of its own, such as a profile
// field left as it is.
func Opt[T any](d *Decoder, name string, read func(name string, p Presence) T) *T {
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
