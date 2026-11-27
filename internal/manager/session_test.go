package manager_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
)

func newSession(t *testing.T) (*manager.Session, *store.FileStore, string) {
	t.Helper()
	fs := seed(t)
	dir := filepath.Join(t.TempDir(), "backups")
	return manager.NewSession(fs, dir), fs, dir
}

func writeNames(p *manager.Plan) []string {
	var out []string
	for _, w := range p.Writes {
		out = append(out, w.Op+" "+w.Name)
	}
	return out
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	return names
}

func TestSessionStagesUntilApply(t *testing.T) {
	s, fs, dir := newSession(t)
	before, _ := fs.Names()

	id, err := s.Draft().Add(manager.AddOptions{Description: "rEFInd", DevicePath: devicePath(t, `\EFI\refind\refind_x64.efi`), Position: 0})
	must(t, err)
	must(t, s.Draft().Remove(1))
	secs := uint16(9)
	must(t, s.Draft().SetTimeout(&secs))

	after, _ := fs.Names()
	if !reflect.DeepEqual(before, after) || len(backupFiles(t, dir)) != 0 {
		t.Fatal("staging must not write variables or backups")
	}
	if got := state(t, s.Current()).Order; !reflect.DeepEqual(got, []uint16{0, 1}) {
		t.Fatalf("current order = %v", got)
	}
	if got := state(t, s.Draft()).Order; !reflect.DeepEqual(got, []uint16{id, 0}) {
		t.Fatalf("draft order = %v", got)
	}

	plan, err := s.Plan()
	must(t, err)
	want := []string{"create Boot0002", "update BootOrder", "delete Boot0001", "update Timeout"}
	if got := writeNames(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v\nwant  %v", got, want)
	}
	if len(plan.Risks) != 0 {
		t.Fatalf("risks = %+v", plan.Risks)
	}

	res, err := s.Apply(false)
	must(t, err)
	if res.Written != 4 || res.Backup == "" || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}
	st := state(t, s.Current())
	if !reflect.DeepEqual(st.Order, []uint16{id, 0}) || *st.Timeout != 9 {
		t.Fatalf("applied state = %+v", st)
	}
	if plan, _ := s.Plan(); len(plan.Writes) != 0 {
		t.Fatalf("draft not cleared: %v", writeNames(plan))
	}

	// The backup holds the configuration from before the change.
	raw, err := os.ReadFile(res.Backup)
	must(t, err)
	snap, err := manager.ParseSnapshot(raw)
	must(t, err)
	if _, ok := snap.Vars["Boot0001"]; !ok {
		t.Fatal("backup is missing the deleted entry")
	}
	if _, ok := snap.Vars["Boot0002"]; ok {
		t.Fatal("backup contains the new entry")
	}
}

func TestSessionDiscard(t *testing.T) {
	s, _, _ := newSession(t)
	must(t, s.Draft().Remove(1))
	s.Discard()
	plan, err := s.Plan()
	must(t, err)
	if len(plan.Writes) != 0 {
		t.Fatalf("plan after discard = %v", writeNames(plan))
	}
	res, err := s.Apply(false)
	must(t, err)
	if res.Written != 0 || res.Backup != "" {
		t.Fatalf("applying nothing must do nothing: %+v", res)
	}
}

func TestSessionRefusesRisksUntilAcknowledged(t *testing.T) {
	s, fs, dir := newSession(t)
	must(t, s.Draft().SetActive(0, false))
	must(t, s.Draft().SetActive(1, false))
	plan, err := s.Plan()
	must(t, err)
	if len(plan.Risks) != 2 {
		t.Fatalf("risks = %+v", plan.Risks)
	}
	if _, err := s.Apply(false); !errors.Is(err, manager.ErrRisksNotAcknowledged) {
		t.Fatalf("Apply(false) = %v", err)
	}
	if len(backupFiles(t, dir)) != 0 {
		t.Fatal("a refused apply must not leave a backup behind")
	}
	raw, _ := fs.Get("Boot0000")
	if opt, _ := efi.ParseLoadOption(raw); !opt.Active() {
		t.Fatal("a refused apply wrote to the store")
	}
	if _, err := s.Apply(true); err != nil {
		t.Fatal(err)
	}
	raw, _ = fs.Get("Boot0000")
	if opt, _ := efi.ParseLoadOption(raw); opt.Active() {
		t.Fatal("acknowledged apply did not write")
	}
}

func TestSessionApplyRefusedWhenBackupFails(t *testing.T) {
	s, fs, _ := newSession(t)
	blocker := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(blocker, nil, 0o600))
	s.BackupDir = filepath.Join(blocker, "backups")
	must(t, s.Draft().Remove(1))
	if _, err := s.Apply(false); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := fs.Get("Boot0001"); err != nil {
		t.Fatal("the change was written although the backup failed")
	}
}

// flakyStore fails writes to one variable until healed.
type flakyStore struct {
	*store.FileStore
	failOn string
}

func (f *flakyStore) Set(name string, data []byte) error {
	if name == f.failOn {
		return errors.New("firmware said no")
	}
	return f.FileStore.Set(name, data)
}

func TestSessionKeepsUnwrittenChangesAfterFailure(t *testing.T) {
	flaky := &flakyStore{FileStore: seed(t), failOn: manager.BootOrderVar}
	s := manager.NewSession(flaky, filepath.Join(t.TempDir(), "backups"))
	_, err := s.Draft().Add(manager.AddOptions{Description: "New", DevicePath: devicePath(t, `\n.efi`), Position: 0})
	must(t, err)

	_, err = s.Apply(false)
	if err == nil || !strings.Contains(err.Error(), "BootOrder") || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "backup-") {
		t.Fatalf("the error must say where the backup is: %v", err)
	}
	// The entry was written, the order was not: the order is still staged.
	plan, err := s.Plan()
	must(t, err)
	if got := writeNames(plan); !reflect.DeepEqual(got, []string{"update BootOrder"}) {
		t.Fatalf("remaining plan = %v", got)
	}
	flaky.failOn = ""
	if _, err := s.Apply(false); err != nil {
		t.Fatal(err)
	}
	if got := state(t, s.Current()).Order; !reflect.DeepEqual(got, []uint16{2, 0, 1}) {
		t.Fatalf("order = %v", got)
	}
}

// forgetfulStore accepts writes to one variable and drops them.
type forgetfulStore struct {
	*store.FileStore
	drop string
}

func (f *forgetfulStore) Set(name string, data []byte) error {
	if name == f.drop {
		return nil
	}
	return f.FileStore.Set(name, data)
}

func TestSessionWarnsWhenAWriteDoesNotStick(t *testing.T) {
	s := manager.NewSession(&forgetfulStore{FileStore: seed(t), drop: manager.TimeoutVar}, "")
	secs := uint16(60)
	must(t, s.Draft().SetTimeout(&secs))
	res, err := s.Apply(false)
	must(t, err)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Timeout") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestSessionRestoreFromBackup(t *testing.T) {
	s, fs, dir := newSession(t)
	path, err := s.BackupNow()
	must(t, err)
	if !strings.HasSuffix(path, "-manual.json") {
		t.Fatalf("path = %s", path)
	}

	// Wipe the configuration the way a forced delete of every entry would.
	must(t, fs.Delete("Boot0000"))
	must(t, fs.Delete("Boot0001"))
	must(t, fs.Delete(manager.BootOrderVar))
	must(t, fs.Delete(manager.TimeoutVar))

	must(t, s.Draft().SetTimeout(func() *uint16 { v := uint16(1); return &v }()))
	must(t, s.LoadBackup(filepath.Base(path)))
	plan, err := s.Plan()
	must(t, err)
	want := []string{"create Boot0000", "create Boot0001", "create BootOrder", "create Timeout"}
	if got := writeNames(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v", got)
	}
	if _, err := s.Apply(false); err != nil {
		t.Fatal(err)
	}
	st := state(t, s.Current())
	if !reflect.DeepEqual(st.Order, []uint16{0, 1}) || *st.Timeout != 5 {
		t.Fatalf("restored state = %+v", st)
	}

	for _, bad := range []string{"../vars.json", `..\vars.json`, "backup.json", "/etc/passwd", ""} {
		if err := s.LoadBackup(bad); err == nil {
			t.Errorf("LoadBackup(%q) succeeded", bad)
		}
	}
	if len(backupFiles(t, dir)) != 2 {
		t.Fatalf("backups = %v", backupFiles(t, dir))
	}
}

func TestSessionRestoreFlagsLosingTheBootedEntry(t *testing.T) {
	s, _, _ := newSession(t)
	// A backup from another machine: same variable name, different loader.
	other, err := store.OpenFile(filepath.Join(t.TempDir(), "other.json"))
	must(t, err)
	must(t, other.Set("Boot0000", payload(t, "Other machine", true, `\EFI\other\loader.efi`)))
	must(t, other.Set(manager.BootOrderVar, efi.BootOrderPayload([]uint16{0})))
	snap, err := (&manager.Manager{Store: other}).Snapshot()
	must(t, err)
	raw, err := snap.JSON()
	must(t, err)

	must(t, s.LoadSnapshot(raw))
	plan, err := s.Plan()
	must(t, err)
	if riskCodes(plan.Risks) != "booted-entry-lost" {
		t.Fatalf("risks = %+v", plan.Risks)
	}
}

func TestSessionBackupListAndPruning(t *testing.T) {
	s, _, dir := newSession(t)
	s.KeepBackups = 3
	manual, err := s.BackupNow()
	must(t, err)
	for i := 0; i < 6; i++ {
		secs := uint16(10 + i)
		must(t, s.Draft().SetTimeout(&secs))
		if _, err := s.Apply(false); err != nil {
			t.Fatal(err)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "backup-20200101-000000.000-auto.json"), []byte("{broken"), 0o600))

	list, err := s.Backups()
	must(t, err)
	auto, manualCount, broken := 0, 0, 0
	for _, b := range list {
		switch {
		case b.Error != "":
			broken++
		case b.Kind == "manual":
			manualCount++
			if b.Entries != 2 {
				t.Fatalf("manual backup lists %d entries", b.Entries)
			}
		default:
			auto++
		}
	}
	if auto != 3 || manualCount != 1 || broken != 1 {
		t.Fatalf("auto=%d manual=%d broken=%d in %+v", auto, manualCount, broken, list)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatal("manual backups must never be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("pruning removed a file that is not a backup")
	}
}

func TestSessionDoesNotReuseAnIDStagedForDeletion(t *testing.T) {
	s, _, _ := newSession(t)
	must(t, s.Draft().Remove(1))
	id, err := s.Draft().Add(manager.AddOptions{Description: "New", DevicePath: devicePath(t, `\n.efi`), Position: -1})
	must(t, err)
	if id != 2 {
		t.Fatalf("new entry took Boot%04X; Boot0001 is only staged for deletion and must not be reused", id)
	}
	plan, err := s.Plan()
	must(t, err)
	want := []string{"create Boot0002", "update BootOrder", "delete Boot0001"}
	if got := writeNames(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v", got)
	}
}
