// Package profile keeps the server's profile as admins set it with
// /server (PROTOCOL.md §3.1, §4.1's suggested convention): a title, a
// description, and an avatar upload, each overriding the server's
// configured one while set. It tracks its changes, and keeps the profile
// in one store entry. Where a value falls back to when unset, and how the
// server frame shows it, are the server's.
//
// A Profile knows its avatar only as a value of the type I, such as the
// server's hosted embed. It is not safe for concurrent use: the server
// calls it holding its lock.
package profile

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

// StoreKind and StoreID name the store entry that keeps the profile.
const StoreKind, StoreID = "meta", "profile"

// Image is the type of a profile's avatar: a finished upload, which the
// zero I stands for the absence of.
type Image interface {
	comparable
	// ImageID names the upload in the store.
	ImageID() string
}

// Profile is what admins set with /server: empty values, and the zero
// avatar, are unset.
type Profile[I Image] struct {
	title       string
	description string
	avatar      I
	// dirty reports a change since Entries last took it, and kept that the
	// store holds an entry.
	dirty bool
	kept  bool
}

// Title is the title set, "" for none.
func (p *Profile[I]) Title() string { return p.title }

// Description is the description set, "" for none.
func (p *Profile[I]) Description() string { return p.description }

// Avatar is the avatar upload set, the zero I for none.
func (p *Profile[I]) Avatar() I { return p.avatar }

// SetTitle sets the title, "" to unset it.
func (p *Profile[I]) SetTitle(title string) { p.title, p.dirty = title, true }

// SetDescription sets the description, "" to unset it.
func (p *Profile[I]) SetDescription(description string) {
	p.description, p.dirty = description, true
}

// SetAvatar sets the avatar upload, the zero I to unset it.
func (p *Profile[I]) SetAvatar(avatar I) { p.avatar, p.dirty = avatar, true }

// stored is the profile as the store keeps it.
type stored struct {
	Title       string `json:"title,omitzero"`
	Description string `json:"description,omitzero"`
	AvatarEmbed string `json:"avatar_embed,omitzero"`
}

// Entries returns the profile's entry if it changed since Entries last
// took it.
func (p *Profile[I]) Entries() []store.Entry {
	if !p.dirty {
		return nil
	}
	p.dirty, p.kept = false, true
	s := stored{Title: p.title, Description: p.description}
	var none I
	if p.avatar != none {
		s.AvatarEmbed = p.avatar.ImageID()
	}
	value, _ := wire.Marshal(s)
	return []store.Entry{{Kind: StoreKind, ID: StoreID, Value: value}}
}

// Snapshot returns the profile's entry, as Entries does after a change,
// and none for a profile that was never set, which the store does not
// hold.
func (p *Profile[I]) Snapshot() []store.Entry {
	p.dirty = p.dirty || p.kept
	return p.Entries()
}

// Restore takes the profile from the store's entry raw, nil for none.
// avatar returns the upload a stored avatar names, and false when it is
// gone or no longer an avatar, which drops it from the profile, and from
// the store at the next Entries.
func (p *Profile[I]) Restore(raw jsontext.Value, avatar func(id string) (I, bool)) error {
	if raw == nil {
		return nil
	}
	var s stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("stored profile: %w", err)
	}
	p.title, p.description, p.kept = s.Title, s.Description, true
	if s.AvatarEmbed != "" {
		if image, ok := avatar(s.AvatarEmbed); ok {
			p.avatar = image
		} else {
			p.dirty = true
		}
	}
	return nil
}
