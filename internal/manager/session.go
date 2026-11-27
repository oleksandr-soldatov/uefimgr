package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
)

// ErrRisksNotAcknowledged is returned by Apply when the staged changes carry
// risks the caller has not accepted.
var ErrRisksNotAcknowledged = errors.New("these changes carry risks that have to be confirmed before they are written")

const defaultKeepBackups = 50

var backupNameRE = regexp.MustCompile(`^backup-[0-9]{8}-[0-9]{6}[0-9.]*(-auto|-manual)?(-[0-9]+)?\.json$`)

// Session stages edits against a copy of the firmware state and writes them
// in one step. Nothing reaches the base store until Apply.
type Session struct {
	BackupDir   string
	KeepBackups int

	mu    sync.Mutex
	base  store.Store
	draft *store.Overlay
}

func NewSession(base store.Store, backupDir string) *Session {
	return &Session{
		BackupDir:   backupDir,
		KeepBackups: defaultKeepBackups,
		base:        base,
		draft:       store.NewOverlay(base),
	}
}

// Current reads what the firmware holds now.
func (s *Session) Current() *Manager { return &Manager{Store: s.base} }

// Draft reads and edits the staged configuration.
func (s *Session) Draft() *Manager { return &Manager{Store: s.draft} }

// Discard drops every staged change.
func (s *Session) Discard() { s.draft.Reset() }

// Write is one firmware variable write a Plan would perform.
type Write struct {
	Op     string `json:"op"` // "create", "update" or "delete"
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// Plan is what Apply would do: the writes, in order, and the risks.
type Plan struct {
	Writes []Write `json:"writes"`
	Risks  []Risk  `json:"risks"`
}

func (s *Session) Plan() (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, _, err := s.plan()
	return plan, err
}

func (s *Session) plan() (*Plan, []store.Change, error) {
	changes, err := s.draft.Changes()
	if err != nil {
		return nil, nil, err
	}
	sortForApply(changes)
	plan := &Plan{Writes: []Write{}, Risks: []Risk{}}
	for _, c := range changes {
		plan.Writes = append(plan.Writes, describeChange(c))
	}
	if len(changes) > 0 {
		cur, err := s.Current().State()
		if err != nil {
			return nil, nil, err
		}
		draft, err := s.Draft().State()
		if err != nil {
			return nil, nil, err
		}
		plan.Risks = append(plan.Risks, Risks(cur, draft)...)
	}
	return plan, changes, nil
}

// sortForApply orders writes so that BootOrder never names a variable that
// does not exist, even if the sequence is interrupted: new and changed
// entries first, then BootOrder, then deletions, then the scalars.
func sortForApply(changes []store.Change) {
	rank := func(c store.Change) int {
		_, isEntry := ParseVarName(c.Name)
		switch {
		case isEntry && c.New != nil:
			return 0
		case c.Name == BootOrderVar:
			return 1
		case isEntry:
			return 2
		case c.Name == BootNextVar || c.Name == TimeoutVar:
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if ri, rj := rank(changes[i]), rank(changes[j]); ri != rj {
			return ri < rj
		}
		return changes[i].Name < changes[j].Name
	})
}

func describeChange(c store.Change) Write {
	w := Write{Name: c.Name, Op: "update"}
	switch {
	case c.New == nil:
		w.Op = "delete"
	case c.Old == nil:
		w.Op = "create"
	}
	value := c.New
	if value == nil {
		value = c.Old
	}
	switch c.Name {
	case BootOrderVar:
		ids, err := efi.ParseBootOrder(value)
		if err != nil {
			break
		}
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = VarName(id)
		}
		w.Detail = strings.Join(names, ", ")
		if c.New == nil {
			w.Detail = "the boot sequence becomes empty"
		}
	case BootNextVar:
		if v, err := efi.Uint16Value(value); err == nil {
			w.Detail = VarName(v)
		}
	case TimeoutVar:
		if v, err := efi.Uint16Value(value); err == nil {
			w.Detail = fmt.Sprintf("%d seconds", v)
		}
	case OsIndicationsVar:
		v, _ := efi.Uint64Value(c.New)
		if v&osIndicationsBootToFW != 0 {
			w.Detail = "open firmware setup on the next restart"
		} else {
			w.Detail = "cancel the request to open firmware setup"
		}
	default:
		if opt, err := efi.ParseLoadOption(value); err == nil {
			w.Detail = opt.Description
			if p := opt.DevicePath.FilePath(); p != "" {
				w.Detail += " — " + p
			}
			if c.New != nil && c.Old != nil {
				w.Detail += changedParts(c.Old, opt)
			}
		}
	}
	return w
}

func changedParts(oldRaw []byte, next *efi.LoadOption) string {
	old, err := efi.ParseLoadOption(oldRaw)
	if err != nil {
		return ""
	}
	var parts []string
	if old.Description != next.Description {
		parts = append(parts, "renamed")
	}
	if !bytes.Equal(old.DevicePath, next.DevicePath) {
		parts = append(parts, "new target")
	}
	if old.Active() != next.Active() {
		if next.Active() {
			parts = append(parts, "enabled")
		} else {
			parts = append(parts, "disabled")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	Written  int      `json:"written"`
	Backup   string   `json:"backup,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply writes the staged changes to the base store. It first saves a backup
// of the current configuration and refuses to continue if that fails. Risks
// have to be acknowledged. On a write error the remaining changes stay
// staged so the caller can retry.
func (s *Session) Apply(acknowledgeRisks bool) (*ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, changes, err := s.plan()
	if err != nil {
		return nil, err
	}
	res := &ApplyResult{}
	if len(changes) == 0 {
		return res, nil
	}
	if len(plan.Risks) > 0 && !acknowledgeRisks {
		return nil, ErrRisksNotAcknowledged
	}
	if s.BackupDir != "" {
		path, err := s.writeBackup("auto")
		if err != nil {
			return nil, fmt.Errorf("nothing was written because the backup failed: %w", err)
		}
		res.Backup = path
	}
	for i, c := range changes {
		var err error
		if c.New == nil {
			err = s.base.Delete(c.Name)
			if errors.Is(err, store.ErrNotFound) {
				err = nil
			}
		} else {
			err = s.base.Set(c.Name, c.New)
		}
		if err != nil {
			msg := fmt.Sprintf("writing %s failed after %d of %d changes: %v", c.Name, i, len(changes), err)
			if res.Backup != "" {
				msg += ". The previous configuration is saved in " + res.Backup
			}
			return nil, errors.New(msg)
		}
		res.Written++
	}
	for _, c := range changes {
		got, err := s.base.Get(c.Name)
		switch {
		case c.New == nil && !errors.Is(err, store.ErrNotFound):
			res.Warnings = append(res.Warnings, c.Name+" is still present after being deleted")
		case c.New != nil && (err != nil || !bytes.Equal(got, c.New)):
			res.Warnings = append(res.Warnings, c.Name+" reads back differently from what was written")
		}
	}
	s.draft.Reset()
	s.pruneBackups()
	return res, nil
}

// BackupNow saves the current firmware configuration and returns the path.
func (s *Session) BackupNow() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.BackupDir == "" {
		return "", fmt.Errorf("no backup folder is configured")
	}
	return s.writeBackup("manual")
}

func (s *Session) writeBackup(kind string) (string, error) {
	snap, err := s.Current().Snapshot()
	if err != nil {
		return "", err
	}
	raw, err := snap.JSON()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup folder: %w", err)
	}
	stamp := time.Now().Format("20060102-150405.000")
	for n := 0; ; n++ {
		name := fmt.Sprintf("backup-%s-%s.json", stamp, kind)
		if n > 0 {
			name = fmt.Sprintf("backup-%s-%s-%d.json", stamp, kind, n)
		}
		path := filepath.Join(s.BackupDir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("write backup: %w", err)
		}
		_, werr := f.Write(raw)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return "", fmt.Errorf("write backup: %w", werr)
		}
		return path, nil
	}
}

// BackupInfo describes one file in the backup folder.
type BackupInfo struct {
	Name    string    `json:"name"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"` // "auto" or "manual"
	Entries int       `json:"entries"`
	Error   string    `json:"error,omitempty"`
}

// Backups lists the backup folder, newest first.
func (s *Session) Backups() ([]BackupInfo, error) {
	out := []BackupInfo{}
	if s.BackupDir == "" {
		return out, nil
	}
	files, err := os.ReadDir(s.BackupDir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.IsDir() || !backupNameRE.MatchString(f.Name()) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		b := BackupInfo{Name: f.Name(), Time: info.ModTime(), Kind: "auto"}
		if strings.Contains(f.Name(), "-manual") {
			b.Kind = "manual"
		}
		if raw, err := os.ReadFile(filepath.Join(s.BackupDir, f.Name())); err != nil {
			b.Error = err.Error()
		} else if snap, err := ParseSnapshot(raw); err != nil {
			b.Error = err.Error()
		} else {
			for name := range snap.Vars {
				if _, ok := ParseVarName(name); ok {
					b.Entries++
				}
			}
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// LoadBackup stages the configuration saved in a file from the backup
// folder, replacing anything staged before.
func (s *Session) LoadBackup(name string) error {
	if !backupNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a backup file name", name)
	}
	raw, err := os.ReadFile(filepath.Join(s.BackupDir, name))
	if err != nil {
		return err
	}
	return s.LoadSnapshot(raw)
}

// LoadSnapshot stages the configuration saved in a backup, replacing
// anything staged before.
func (s *Session) LoadSnapshot(raw []byte) error {
	snap, err := ParseSnapshot(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draft.Reset()
	if err := s.Draft().LoadSnapshot(snap); err != nil {
		s.draft.Reset()
		return err
	}
	return nil
}

// pruneBackups keeps the newest automatic backups and leaves manual ones.
func (s *Session) pruneBackups() {
	if s.BackupDir == "" || s.KeepBackups <= 0 {
		return
	}
	all, err := s.Backups()
	if err != nil {
		return
	}
	kept := 0
	for _, b := range all {
		if b.Kind != "auto" {
			continue
		}
		kept++
		if kept > s.KeepBackups {
			os.Remove(filepath.Join(s.BackupDir, b.Name))
		}
	}
}
