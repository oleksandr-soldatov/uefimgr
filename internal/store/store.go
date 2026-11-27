// Package store abstracts the firmware variable namespace.
package store

import (
	"bytes"
	"errors"
	"sort"
	"sync"
)

var (
	// ErrNotFound means the variable does not exist in the namespace.
	ErrNotFound = errors.New("firmware variable not found")
	// ErrUnsupported means this platform/build cannot access variables.
	ErrUnsupported = errors.New("firmware variables are not supported on this platform")
	// ErrNotUEFI means the machine did not boot through UEFI firmware.
	ErrNotUEFI = errors.New("this system did not boot in UEFI mode, so there are no firmware boot entries to manage")
)

// Store is a named byte-variable namespace with UEFI semantics: a variable
// cannot be empty, so setting empty data deletes it.
type Store interface {
	Get(name string) ([]byte, error)
	Set(name string, data []byte) error
	// Delete returns ErrNotFound when the variable does not exist.
	Delete(name string) error
	Describe() string
}

// Lister is implemented by stores that can enumerate their variables.
type Lister interface {
	Names() ([]string, error)
}

// Change is one variable that differs between an Overlay and its base.
// A nil Old means the variable is created; a nil New means it is deleted.
type Change struct {
	Name string
	Old  []byte
	New  []byte
}

// Overlay buffers writes on top of a base store without touching it. It lets
// edits be staged, inspected and then applied in one go.
type Overlay struct {
	base Store

	mu   sync.Mutex
	sets map[string][]byte
	dels map[string]bool
}

func NewOverlay(base Store) *Overlay {
	return &Overlay{base: base, sets: map[string][]byte{}, dels: map[string]bool{}}
}

func (o *Overlay) Describe() string { return o.base.Describe() }

func (o *Overlay) Get(name string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.getLocked(name)
}

func (o *Overlay) getLocked(name string) ([]byte, error) {
	if o.dels[name] {
		return nil, ErrNotFound
	}
	if v, ok := o.sets[name]; ok {
		return append([]byte(nil), v...), nil
	}
	return o.base.Get(name)
}

func (o *Overlay) Set(name string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(data) == 0 {
		delete(o.sets, name)
		o.dels[name] = true
		return nil
	}
	delete(o.dels, name)
	o.sets[name] = append([]byte(nil), data...)
	return nil
}

func (o *Overlay) Delete(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.getLocked(name); err != nil {
		return err
	}
	delete(o.sets, name)
	o.dels[name] = true
	return nil
}

// Occupied reports whether name is taken in the base store or staged in the
// overlay. A variable that is only staged for deletion still counts, so a
// new variable never takes over the name of one that is about to go.
func (o *Overlay) Occupied(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.sets[name]; ok {
		return true
	}
	_, err := o.base.Get(name)
	return !errors.Is(err, ErrNotFound)
}

// Names lists the variables visible through the overlay. It needs a base
// that can enumerate.
func (o *Overlay) Names() ([]string, error) {
	l, ok := o.base.(Lister)
	if !ok {
		return nil, ErrUnsupported
	}
	baseNames, err := l.Names()
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := map[string]bool{}
	for _, n := range baseNames {
		if !o.dels[n] {
			seen[n] = true
		}
	}
	for n := range o.sets {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Changes reports how the overlay differs from its base, sorted by name.
// Staged writes that match what the base already holds are not changes.
func (o *Overlay) Changes() ([]Change, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Change
	for name, data := range o.sets {
		old, err := o.base.Get(name)
		switch {
		case errors.Is(err, ErrNotFound):
			out = append(out, Change{Name: name, New: append([]byte(nil), data...)})
		case err != nil:
			return nil, err
		case !bytes.Equal(old, data):
			out = append(out, Change{Name: name, Old: old, New: append([]byte(nil), data...)})
		}
	}
	for name := range o.dels {
		old, err := o.base.Get(name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Change{Name: name, Old: old})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Reset drops every staged write.
func (o *Overlay) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sets = map[string][]byte{}
	o.dels = map[string]bool{}
}
