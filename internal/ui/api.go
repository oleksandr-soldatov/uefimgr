// Package ui is the application's user interface: the page, the methods the
// page calls, and an HTTP transport for running it in a browser.
package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
	"github.com/oleksandr-soldatov/uefimgr/internal/target"
)

// API is what the page can ask for. Edits are staged in the session; only
// "apply" writes to the variable store.
type API struct {
	// Version and Simulated are shown in the page header.
	Version   string
	Simulated bool
	// OpenFolder shows a folder in the system file manager.
	OpenFolder func(path string) error

	mu      sync.Mutex
	session *manager.Session
	volumes esp.Provider
}

func NewAPI(session *manager.Session, volumes esp.Provider, version string) *API {
	return &API{Version: version, session: session, volumes: volumes}
}

// View is everything the page needs to draw itself.
type View struct {
	Version   string         `json:"version"`
	Elevated  bool           `json:"elevated"`
	Simulated bool           `json:"simulated"`
	BackupDir string         `json:"backupDir"`
	Current   *manager.State `json:"current"`
	Draft     *manager.State `json:"draft"`
	Plan      *manager.Plan  `json:"plan"`
}

type result map[string]any

// Call runs one method. params is a JSON object, or empty.
func (a *API) Call(method string, params json.RawMessage) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := methods[method]
	if !ok {
		return nil, fmt.Errorf("unknown method %q", method)
	}
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	return h(a, params)
}

var methods = map[string]func(*API, json.RawMessage) (any, error){
	"ping":          func(*API, json.RawMessage) (any, error) { return result{}, nil },
	"state":         (*API).state,
	"create":        (*API).create,
	"edit":          (*API).edit,
	"delete":        (*API).remove,
	"setActive":     (*API).setActive,
	"setOrder":      (*API).setOrder,
	"setNext":       (*API).setNext,
	"setTimeout":    (*API).setTimeout,
	"setFirmwareUI": (*API).setFirmwareUI,
	"discard":       (*API).discard,
	"apply":         (*API).apply,
	"targets":       (*API).targetOptions,
	"volumes":       (*API).listVolumes,
	"browse":        (*API).browse,
	"backups":       (*API).backups,
	"backupNow":     (*API).backupNow,
	"loadBackup":    (*API).loadBackup,
	"loadSnapshot":  (*API).loadSnapshot,
	"openBackups":   (*API).openBackups,
}

func decode(params json.RawMessage, v any) error {
	if err := json.Unmarshal(params, v); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

func (a *API) view() (*View, error) {
	cur, err := a.session.Current().State()
	if err != nil {
		return nil, err
	}
	draft, err := a.session.Draft().State()
	if err != nil {
		return nil, err
	}
	plan, err := a.session.Plan()
	if err != nil {
		return nil, err
	}
	return &View{
		Version:   a.Version,
		Elevated:  store.IsElevated(),
		Simulated: a.Simulated,
		BackupDir: a.session.BackupDir,
		Current:   cur,
		Draft:     draft,
		Plan:      plan,
	}, nil
}

// staged runs an edit against the draft and returns the refreshed view.
func (a *API) staged(edit func(*manager.Manager) error, extra result) (any, error) {
	if err := edit(a.session.Draft()); err != nil {
		return nil, err
	}
	v, err := a.view()
	if err != nil {
		return nil, err
	}
	out := result{"view": v}
	for k, val := range extra {
		out[k] = val
	}
	return out, nil
}

func (a *API) state(json.RawMessage) (any, error) {
	return a.staged(func(*manager.Manager) error { return nil }, nil)
}

type entryParams struct {
	ID          uint16 `json:"id"`
	Description string `json:"description"`
	// Partition is the key of one of the partitions "targets" offered.
	Partition string `json:"partition"`
	Path      string `json:"path"`
	Inactive  bool   `json:"inactive"`
	First     bool   `json:"first"`
}

func (a *API) targets() *target.Targets {
	return target.New(a.session.Draft(), a.volumes)
}

// targetOptions lists where the entry with the given id, or a new entry when
// there is no id, can be pointed at.
func (a *API) targetOptions(params json.RawMessage) (any, error) {
	var p struct {
		ID *uint16 `json:"id"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.targets().Options(p.ID)
}

func (a *API) create(params json.RawMessage) (any, error) {
	var p entryParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	dp, err := a.targets().Resolve(nil, p.Partition, p.Path)
	if err != nil {
		return nil, err
	}
	pos := -1
	if p.First {
		pos = 0
	}
	extra := result{}
	return a.staged(func(m *manager.Manager) error {
		id, err := m.Add(manager.AddOptions{
			Description: p.Description,
			DevicePath:  dp,
			Inactive:    p.Inactive,
			Position:    pos,
		})
		extra["id"], extra["name"] = id, manager.VarName(id)
		return err
	}, extra)
}

func (a *API) edit(params json.RawMessage) (any, error) {
	var p entryParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	dp, err := a.targets().Resolve(&p.ID, p.Partition, p.Path)
	if err != nil {
		return nil, err
	}
	opts := manager.EditOptions{Description: &p.Description, DevicePath: dp}
	return a.staged(func(m *manager.Manager) error { return m.Edit(p.ID, opts) }, nil)
}

func (a *API) remove(params json.RawMessage) (any, error) {
	var p struct {
		ID uint16 `json:"id"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.Remove(p.ID) }, nil)
}

func (a *API) setActive(params json.RawMessage) (any, error) {
	var p struct {
		ID     uint16 `json:"id"`
		Active bool   `json:"active"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.SetActive(p.ID, p.Active) }, nil)
}

func (a *API) setOrder(params json.RawMessage) (any, error) {
	var p struct {
		IDs []uint16 `json:"ids"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.SetOrder(p.IDs) }, nil)
}

func (a *API) setNext(params json.RawMessage) (any, error) {
	var p struct {
		ID *uint16 `json:"id"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.SetNext(p.ID) }, nil)
}

func (a *API) setTimeout(params json.RawMessage) (any, error) {
	var p struct {
		Seconds *uint16 `json:"seconds"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.SetTimeout(p.Seconds) }, nil)
}

func (a *API) setFirmwareUI(params json.RawMessage) (any, error) {
	var p struct {
		On bool `json:"on"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return a.staged(func(m *manager.Manager) error { return m.SetFirmwareUI(p.On) }, nil)
}

func (a *API) discard(json.RawMessage) (any, error) {
	a.session.Discard()
	return a.state(nil)
}

func (a *API) apply(params json.RawMessage) (any, error) {
	var p struct {
		Acknowledge bool `json:"acknowledge"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	res, applyErr := a.session.Apply(p.Acknowledge)
	if applyErr != nil {
		return nil, applyErr
	}
	v, err := a.view()
	if err != nil {
		return nil, err
	}
	return result{"view": v, "applied": res}, nil
}

func (a *API) listVolumes(json.RawMessage) (any, error) {
	vols, err := a.volumes.Volumes()
	if err != nil {
		// The page still works without discovery: entries can reuse the
		// partition of an existing entry.
		return result{"volumes": []esp.Volume{}, "error": err.Error()}, nil
	}
	return result{"volumes": vols}, nil
}

func (a *API) browse(params json.RawMessage) (any, error) {
	var p struct {
		Volume string `json:"volume"`
		Dir    string `json:"dir"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	entries, err := a.volumes.ListDir(p.Volume, p.Dir)
	if err != nil {
		return nil, err
	}
	return result{"entries": entries}, nil
}

func (a *API) backups(json.RawMessage) (any, error) {
	list, err := a.session.Backups()
	if err != nil {
		return nil, err
	}
	return result{"dir": a.session.BackupDir, "backups": list}, nil
}

func (a *API) backupNow(json.RawMessage) (any, error) {
	path, err := a.session.BackupNow()
	if err != nil {
		return nil, err
	}
	list, err := a.session.Backups()
	if err != nil {
		return nil, err
	}
	return result{"path": path, "backups": list}, nil
}

func (a *API) loadBackup(params json.RawMessage) (any, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := a.session.LoadBackup(p.Name); err != nil {
		return nil, err
	}
	return a.state(nil)
}

func (a *API) loadSnapshot(params json.RawMessage) (any, error) {
	var p struct {
		Snapshot string `json:"snapshot"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if len(p.Snapshot) > 4<<20 {
		return nil, fmt.Errorf("that file is too large to be a uefimgr backup")
	}
	if err := a.session.LoadSnapshot([]byte(p.Snapshot)); err != nil {
		return nil, err
	}
	return a.state(nil)
}

func (a *API) openBackups(json.RawMessage) (any, error) {
	if a.OpenFolder == nil || a.session.BackupDir == "" {
		return nil, fmt.Errorf("the backup folder cannot be opened from here: %s", a.session.BackupDir)
	}
	if err := a.OpenFolder(a.session.BackupDir); err != nil {
		return nil, err
	}
	return result{}, nil
}
