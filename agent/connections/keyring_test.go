package connections

import (
	"errors"
	"testing"

	"github.com/99designs/keyring"
)

// fakeKeyring implements keyring.Keyring. setErr, when non-nil, simulates a
// backend that "opens" but rejects writes — the exact Secret Service failure
// mode on a headless / locked-keyring Linux box.
type fakeKeyring struct {
	setErr error
	store  map[string]keyring.Item
}

func newFakeKeyring(setErr error) *fakeKeyring {
	return &fakeKeyring{setErr: setErr, store: map[string]keyring.Item{}}
}

func (f *fakeKeyring) Get(key string) (keyring.Item, error) {
	it, ok := f.store[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return it, nil
}
func (f *fakeKeyring) GetMetadata(key string) (keyring.Metadata, error) {
	return keyring.Metadata{}, nil
}
func (f *fakeKeyring) Set(item keyring.Item) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.store[item.Key] = item
	return nil
}
func (f *fakeKeyring) Remove(key string) error {
	delete(f.store, key)
	return nil
}
func (f *fakeKeyring) Keys() ([]string, error) {
	keys := make([]string, 0, len(f.store))
	for k := range f.store {
		keys = append(keys, k)
	}
	return keys, nil
}

func TestProbeKeyringWritable_Usable(t *testing.T) {
	kr := newFakeKeyring(nil)
	if err := probeKeyringWritable(kr); err != nil {
		t.Fatalf("probe on a writable backend should pass, got %v", err)
	}
	// The probe must clean up after itself (no sentinel left behind).
	if keys, _ := kr.Keys(); len(keys) != 0 {
		t.Errorf("probe left keys behind: %v", keys)
	}
}

func TestProbeKeyringWritable_Unusable(t *testing.T) {
	// Mirrors the headless Secret Service failure ("Object does not exist at path /").
	sentinel := errors.New("Object does not exist at path \"/\"")
	kr := newFakeKeyring(sentinel)
	if err := probeKeyringWritable(kr); !errors.Is(err, sentinel) {
		t.Fatalf("probe should surface the write error, got %v", err)
	}
}
