package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileStore keeps variables in a JSON file. It is a simulation/test backend
// and never touches firmware.
type FileStore struct {
	mu   sync.Mutex
	path string
	vars map[string][]byte
}

type fileFormat struct {
	Format int               `json:"format"`
	Note   string            `json:"note,omitempty"`
	Vars   map[string][]byte `json:"vars"`
}

func OpenFile(path string) (*FileStore, error) {
	s := &FileStore{path: path, vars: map[string][]byte{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Format != 0 && f.Format != 1 {
		return nil, fmt.Errorf("%s: unsupported format %d", path, f.Format)
	}
	for k, v := range f.Vars {
		if len(v) > 0 {
			s.vars[k] = v
		}
	}
	return s, nil
}

func (s *FileStore) Describe() string { return "simulated variable file " + s.path }

func (s *FileStore) Get(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vars[name]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Set mirrors firmware: writing empty data removes the variable.
func (s *FileStore) Set(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) == 0 {
		if _, ok := s.vars[name]; !ok {
			return nil
		}
		delete(s.vars, name)
		return s.persistLocked()
	}
	s.vars[name] = append([]byte(nil), data...)
	return s.persistLocked()
}

func (s *FileStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vars[name]; !ok {
		return ErrNotFound
	}
	delete(s.vars, name)
	return s.persistLocked()
}

func (s *FileStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(fileFormat{
		Format: 1,
		Note:   "uefimgr simulated firmware variables",
		Vars:   s.vars,
	}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Names returns the stored variable names, sorted.
func (s *FileStore) Names() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.vars))
	for k := range s.vars {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
