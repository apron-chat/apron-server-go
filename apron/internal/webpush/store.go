package webpush

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/apron-chat/apron-server-go/store"
	"github.com/apron-chat/apron-server-go/wire"
)

// StoreKind and StoreID name the store entry that keeps the VAPID key a
// server made itself: its private scalar, as Encoded renders it, in a JSON
// string.
const StoreKind, StoreID = "meta", "vapid"

// StoredKey is the VAPID key a server keeps in its store, when it is given
// none: the one it made at its first start. It is not safe for concurrent
// use: the server calls it holding its lock.
type StoredKey struct {
	key *VAPIDKey
	// dirty reports a key made since Entries last took it.
	dirty bool
}

// Restore returns the key in the store's entry raw, or, when raw is nil, a
// new key, which Entries then stores.
func (k *StoredKey) Restore(raw jsontext.Value) (*VAPIDKey, error) {
	if raw == nil {
		key, err := NewVAPIDKey()
		if err != nil {
			return nil, err
		}
		k.key, k.dirty = key, true
		return key, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, fmt.Errorf("stored VAPID key: %w", err)
	}
	key, err := ParseVAPIDKey(encoded)
	if err != nil {
		return nil, fmt.Errorf("stored VAPID key: %w", err)
	}
	k.key = key
	return key, nil
}

// Entries returns the key's entry if the key was made since Entries last
// took it.
func (k *StoredKey) Entries() []store.Entry {
	if !k.dirty {
		return nil
	}
	k.dirty = false
	value, _ := wire.Marshal(k.key.Encoded())
	return []store.Entry{{Kind: StoreKind, ID: StoreID, Value: value}}
}

// Snapshot returns the key's entry, as Entries does when the key is new,
// and none when there is no key to store.
func (k *StoredKey) Snapshot() []store.Entry {
	k.dirty = k.key != nil
	return k.Entries()
}
