package manager_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
)

var testHD = efi.GPTPartition(1, 0x800, 0x1f000, efi.MustParseGUID("11111111-2222-3333-4444-555555555555"))

func devicePath(t *testing.T, file string) efi.DevicePath {
	t.Helper()
	dp, err := efi.NewBootDevicePath(testHD, file)
	if err != nil {
		t.Fatal(err)
	}
	return dp
}

func payload(t *testing.T, desc string, active bool, file string) []byte {
	t.Helper()
	lo := &efi.LoadOption{Description: desc, DevicePath: devicePath(t, file)}
	lo.SetActive(active)
	raw, err := lo.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seed returns a store holding Windows (Boot0000, booted) and Linux
// (Boot0001), in that order, with a 5 second timeout.
func seed(t *testing.T) *store.FileStore {
	t.Helper()
	fs, err := store.OpenFile(filepath.Join(t.TempDir(), "vars.json"))
	must(t, err)
	must(t, fs.Set("Boot0000", payload(t, "Windows Boot Manager", true, `\EFI\Microsoft\Boot\bootmgfw.efi`)))
	must(t, fs.Set("Boot0001", payload(t, "Linux", true, `\EFI\linux\grubx64.efi`)))
	must(t, fs.Set(manager.BootOrderVar, efi.BootOrderPayload([]uint16{0, 1})))
	must(t, fs.Set(manager.BootCurrentVar, efi.Uint16Payload(0)))
	must(t, fs.Set(manager.TimeoutVar, efi.Uint16Payload(5)))
	return fs
}

func state(t *testing.T, m *manager.Manager) *manager.State {
	t.Helper()
	st, err := m.State()
	must(t, err)
	return st
}

func TestState(t *testing.T) {
	m := &manager.Manager{Store: seed(t)}
	st := state(t, m)
	if st.BootCurrent == nil || *st.BootCurrent != 0 {
		t.Fatalf("BootCurrent = %v", st.BootCurrent)
	}
	if st.Timeout == nil || *st.Timeout != 5 {
		t.Fatalf("Timeout = %v", st.Timeout)
	}
	if len(st.Entries) != 2 || st.Entries[1].Description != "Linux" {
		t.Fatalf("entries = %+v", st.Entries)
	}
	e := st.Entries[0]
	if e.FilePath != `\EFI\Microsoft\Boot\bootmgfw.efi` || !e.Active {
		t.Fatalf("entry = %+v", e)
	}
	if e.PartitionGUID != "{11111111-2222-3333-4444-555555555555}" || e.PartitionNumber != 1 {
		t.Fatalf("partition = %q #%d", e.PartitionGUID, e.PartitionNumber)
	}
}

func TestStateWithoutBootOrderIsEmptyNotAnError(t *testing.T) {
	fs := seed(t)
	must(t, fs.Delete(manager.BootOrderVar))
	st := state(t, &manager.Manager{Store: fs})
	if len(st.Order) != 0 || len(st.Entries) != 0 {
		t.Fatalf("order = %v", st.Order)
	}
	if len(st.Unordered) != 2 {
		t.Fatalf("entries left behind must show as unordered: %+v", st.Unordered)
	}
}

func TestStateListsEntriesOutsideBootOrder(t *testing.T) {
	fs := seed(t)
	must(t, fs.Set("Boot0007", payload(t, "Orphan", true, `\EFI\orphan\o.efi`)))
	must(t, fs.Set("Boot000a", []byte{1, 2, 3})) // lower-case name is not a boot entry
	st := state(t, &manager.Manager{Store: fs})
	if len(st.Unordered) != 1 || st.Unordered[0].ID != 7 || st.Unordered[0].OrderIndex != -1 {
		t.Fatalf("unordered = %+v", st.Unordered)
	}
}

func TestStateMarksMissingAndUndecodable(t *testing.T) {
	fs := seed(t)
	must(t, fs.Delete("Boot0001"))
	must(t, fs.Set("Boot0002", []byte{1, 2, 3}))
	must(t, fs.Set(manager.BootOrderVar, efi.BootOrderPayload([]uint16{0, 1, 2, 0})))
	st := state(t, &manager.Manager{Store: fs})
	if !reflect.DeepEqual(st.Order, []uint16{0, 1, 2}) {
		t.Fatalf("repeated IDs must be dropped: %v", st.Order)
	}
	if !st.Entries[1].Broken || !st.Entries[2].Broken || st.Entries[0].Broken {
		t.Fatalf("entries = %+v", st.Entries)
	}
	if len(st.Warnings) == 0 {
		t.Fatal("expected a warning about the repeated ID")
	}
}

func TestAdd(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	must(t, fs.Set("Boot0002", payload(t, "Orphan", true, `\o.efi`)))

	id, err := m.Add(manager.AddOptions{Description: "rEFInd", DevicePath: devicePath(t, `\EFI\refind\refind_x64.efi`), Position: -1})
	must(t, err)
	if id != 3 {
		t.Fatalf("new entry took ID %d; it must skip the orphan Boot0002", id)
	}
	id2, err := m.Add(manager.AddOptions{Description: "First", DevicePath: devicePath(t, `\first.efi`), Position: 0})
	must(t, err)
	st := state(t, m)
	if !reflect.DeepEqual(st.Order, []uint16{id2, 0, 1, 3}) {
		t.Fatalf("order = %v", st.Order)
	}
	if e, _ := st.Find(3); !e.Active || e.Description != "rEFInd" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestAddInactive(t *testing.T) {
	m := &manager.Manager{Store: seed(t)}
	id, err := m.Add(manager.AddOptions{Description: "Off", DevicePath: devicePath(t, `\x.efi`), Inactive: true, Position: -1})
	must(t, err)
	if e, _ := state(t, m).Find(id); e.Active {
		t.Fatal("an entry created inactive must be inactive")
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	before, _ := fs.Names()
	bad := []manager.AddOptions{
		{Description: "", DevicePath: devicePath(t, `\x.efi`)},
		{Description: "A\x00B", DevicePath: devicePath(t, `\x.efi`)},
		{Description: "ok", DevicePath: nil},
		{Description: "ok", DevicePath: efi.DevicePath{1, 2, 3}},
	}
	for _, o := range bad {
		if _, err := m.Add(o); err == nil {
			t.Fatalf("Add(%+v) succeeded", o)
		}
	}
	after, _ := fs.Names()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a rejected Add left variables behind: %v", after)
	}
}

// The name an entry already has is not re-validated, so submitting an edit
// form unchanged writes nothing, even for names a new entry could not get.
func TestEditWithUnchangedNameWritesNothing(t *testing.T) {
	for _, name := range []string{"ubuntu ", ""} {
		fs := seed(t)
		raw := payload(t, name, true, `\EFI\ubuntu\shimx64.efi`)
		must(t, fs.Set("Boot0001", raw))
		m := &manager.Manager{Store: fs}
		same := name
		must(t, m.Edit(1, manager.EditOptions{Description: &same, DevicePath: devicePath(t, `\EFI\ubuntu\shimx64.efi`)}))
		if got, _ := fs.Get("Boot0001"); !bytes.Equal(got, raw) {
			t.Fatalf("%q: unchanged edit rewrote the entry", name)
		}
		must(t, m.Edit(1, manager.EditOptions{Description: &same, DevicePath: devicePath(t, `\EFI\ubuntu\grubx64.efi`)}))
		opt, err := efi.ParseLoadOption(mustGet(t, fs, "Boot0001"))
		must(t, err)
		if opt.Description != name || opt.DevicePath.FilePath() != `\EFI\ubuntu\grubx64.efi` {
			t.Fatalf("%q: repointing kept %q -> %q", name, opt.Description, opt.DevicePath.FilePath())
		}
	}
	m := &manager.Manager{Store: seed(t)}
	empty := ""
	if err := m.Edit(1, manager.EditOptions{Description: &empty}); err == nil {
		t.Fatal("a named entry must not be renamed to nothing")
	}
}

func mustGet(t *testing.T, s store.Store, name string) []byte {
	t.Helper()
	raw, err := s.Get(name)
	must(t, err)
	return raw
}

func TestEditKeepsWhatItDoesNotTouch(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	second := devicePath(t, `\EFI\linux\initrd.img`)
	lo := &efi.LoadOption{
		Attributes:   efi.LoadOptionActive | efi.LoadOptionCategoryBoot,
		Description:  "Linux",
		DevicePath:   devicePath(t, `\EFI\linux\vmlinuz.efi`),
		ExtraPaths:   second,
		OptionalData: []byte("root=/dev/sda2"),
	}
	raw, err := lo.MarshalBinary()
	must(t, err)
	must(t, fs.Set("Boot0001", raw))

	name := "Linux (renamed)"
	must(t, m.Edit(1, manager.EditOptions{Description: &name}))
	next, err := devicePath(t, `\EFI\linux\vmlinuz.efi`).WithFilePath(`\EFI\linux\new.efi`)
	must(t, err)
	must(t, m.Edit(1, manager.EditOptions{DevicePath: next}))

	got, _ := fs.Get("Boot0001")
	opt, err := efi.ParseLoadOption(got)
	must(t, err)
	if opt.Description != name || opt.DevicePath.FilePath() != `\EFI\linux\new.efi` {
		t.Fatalf("edit not applied: %q %q", opt.Description, opt.DevicePath.FilePath())
	}
	if opt.Attributes != lo.Attributes || !bytes.Equal(opt.ExtraPaths, second) || string(opt.OptionalData) != "root=/dev/sda2" {
		t.Fatalf("edit changed more than it was asked to: %+v", opt)
	}
	if e, _ := state(t, m).Find(1); e.ExtraPaths != 1 || e.OptionalDataLen != 14 {
		t.Fatalf("entry = %+v", e)
	}

	bad := "A\x00B"
	if err := m.Edit(1, manager.EditOptions{Description: &bad}); err == nil {
		t.Fatal("expected an error for a NUL in the description")
	}
	if err := m.Edit(9, manager.EditOptions{Description: &name}); err == nil {
		t.Fatal("expected an error for a missing entry")
	}
}

func TestSetActiveNextTimeout(t *testing.T) {
	m := &manager.Manager{Store: seed(t)}
	must(t, m.SetActive(1, false))
	if e, _ := state(t, m).Find(1); e.Active {
		t.Fatal("still active")
	}
	must(t, m.SetActive(1, true))

	one := uint16(1)
	must(t, m.SetNext(&one))
	if st := state(t, m); st.BootNext == nil || *st.BootNext != 1 {
		t.Fatalf("BootNext = %v", st.BootNext)
	}
	nine := uint16(9)
	if err := m.SetNext(&nine); err == nil {
		t.Fatal("BootNext must name an existing entry")
	}
	must(t, m.SetNext(nil))
	must(t, m.SetNext(nil))
	if st := state(t, m); st.BootNext != nil {
		t.Fatal("BootNext not cleared")
	}

	secs := uint16(30)
	must(t, m.SetTimeout(&secs))
	if st := state(t, m); *st.Timeout != 30 {
		t.Fatalf("Timeout = %d", *st.Timeout)
	}
	must(t, m.SetTimeout(nil))
	if st := state(t, m); st.Timeout != nil {
		t.Fatal("Timeout not cleared")
	}
}

func TestSetOrder(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	must(t, fs.Set("Boot0005", payload(t, "Orphan", true, `\o.efi`)))
	must(t, m.SetOrder([]uint16{1, 5, 0}))
	st := state(t, m)
	if !reflect.DeepEqual(st.Order, []uint16{1, 5, 0}) || len(st.Unordered) != 0 {
		t.Fatalf("order = %v, unordered = %+v", st.Order, st.Unordered)
	}
	if err := m.SetOrder([]uint16{1, 1}); err == nil {
		t.Fatal("expected a duplicate error")
	}
	if err := m.SetOrder([]uint16{1, 9}); err == nil {
		t.Fatal("expected a missing-entry error")
	}
}

func TestRemove(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	one := uint16(1)
	must(t, m.SetNext(&one))
	must(t, m.Remove(1))
	st := state(t, m)
	if !reflect.DeepEqual(st.Order, []uint16{0}) || st.BootNext != nil {
		t.Fatalf("order = %v, BootNext = %v", st.Order, st.BootNext)
	}
	if _, err := fs.Get("Boot0001"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("variable not deleted")
	}

	must(t, fs.Set("Boot0004", payload(t, "Orphan", true, `\o.efi`)))
	must(t, m.Remove(4))
	if err := m.Remove(4); err == nil {
		t.Fatal("removing a missing entry must fail")
	}

	// Removing the last entry leaves no BootOrder at all, as on firmware.
	must(t, m.Remove(0))
	if _, err := fs.Get(manager.BootOrderVar); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BootOrder = %v", err)
	}
	st = state(t, m)
	if len(st.Entries) != 0 {
		t.Fatalf("entries = %+v", st.Entries)
	}
	if _, err := m.Add(manager.AddOptions{Description: "Back", DevicePath: devicePath(t, `\b.efi`), Position: -1}); err != nil {
		t.Fatalf("adding to an empty configuration: %v", err)
	}
}

func TestSetFirmwareUI(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	if err := m.SetFirmwareUI(true); err == nil {
		t.Fatal("must refuse when the firmware does not advertise support")
	}
	must(t, fs.Set(manager.OsIndicationsSupportedVar, efi.Uint64Payload(0x41)))
	must(t, fs.Set(manager.OsIndicationsVar, efi.Uint64Payload(0x40)))
	if st := state(t, m); !st.FirmwareUISupported || st.FirmwareUIRequested {
		t.Fatalf("state = %+v", st)
	}
	must(t, m.SetFirmwareUI(true))
	raw, _ := fs.Get(manager.OsIndicationsVar)
	if v, _ := efi.Uint64Value(raw); v != 0x41 {
		t.Fatalf("OsIndications = %#x; other bits must be kept", v)
	}
	if st := state(t, m); !st.FirmwareUIRequested {
		t.Fatal("request not visible")
	}
	must(t, m.SetFirmwareUI(false))
	raw, _ = fs.Get(manager.OsIndicationsVar)
	if v, _ := efi.Uint64Value(raw); v != 0x40 {
		t.Fatalf("OsIndications = %#x", v)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	fs := seed(t)
	m := &manager.Manager{Store: fs}
	must(t, fs.Set("Boot0006", payload(t, "Orphan", false, `\o.efi`)))
	must(t, fs.Set("Boot0008", []byte{9, 9, 9})) // undecodable entries are saved as they are
	snap, err := m.Snapshot()
	must(t, err)
	raw, err := snap.JSON()
	must(t, err)
	want, _ := fs.Names()

	must(t, m.Remove(1))
	must(t, m.Remove(6))
	extra, err := m.Add(manager.AddOptions{Description: "Extra", DevicePath: devicePath(t, `\extra.efi`), Position: 0})
	must(t, err)
	must(t, m.SetNext(&extra))
	must(t, m.SetTimeout(nil))

	parsed, err := manager.ParseSnapshot(raw)
	must(t, err)
	must(t, m.LoadSnapshot(parsed))
	got, _ := fs.Names()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variables after restore = %v, want %v", got, want)
	}
	st := state(t, m)
	if !reflect.DeepEqual(st.Order, []uint16{0, 1}) || st.BootNext != nil || st.Timeout == nil || *st.Timeout != 5 {
		t.Fatalf("state after restore = %+v", st)
	}
}

func TestParseSnapshotRejects(t *testing.T) {
	cases := map[string]string{
		"not json":         `nope`,
		"wrong format":     `{"format":2,"vars":{"BootOrder":"AAA="}}`,
		"no variables":     `{"format":1,"vars":{}}`,
		"foreign variable": `{"format":1,"vars":{"SecureBoot":"AQ=="}}`,
		"lower-case name":  `{"format":1,"vars":{"Boot000a":"AQ=="}}`,
		"short scalar":     `{"format":1,"vars":{"Timeout":"AQ=="}}`,
		"dangling order":   `{"format":1,"vars":{"BootOrder":"BQA="}}`,
		"odd order":        `{"format":1,"vars":{"BootOrder":"AAAA"}}`,
	}
	for name, raw := range cases {
		if _, err := manager.ParseSnapshot([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func riskCodes(risks []manager.Risk) string {
	var codes []string
	for _, r := range risks {
		codes = append(codes, r.Code)
	}
	return strings.Join(codes, ",")
}

func TestRisks(t *testing.T) {
	type edit func(t *testing.T, m *manager.Manager)
	cases := []struct {
		name string
		edit edit
		want string
	}{
		{"reorder is safe", func(t *testing.T, m *manager.Manager) { must(t, m.SetOrder([]uint16{1, 0})) }, ""},
		{"disable the other entry", func(t *testing.T, m *manager.Manager) { must(t, m.SetActive(1, false)) }, ""},
		{"delete the booted entry", func(t *testing.T, m *manager.Manager) { must(t, m.Remove(0)) }, "booted-entry-lost"},
		{"disable the booted entry", func(t *testing.T, m *manager.Manager) { must(t, m.SetActive(0, false)) }, "booted-entry-lost"},
		{"repoint the booted entry", func(t *testing.T, m *manager.Manager) {
			must(t, m.Edit(0, manager.EditOptions{DevicePath: devicePath(t, `\EFI\other.efi`)}))
		}, "booted-entry-lost"},
		{"rename the booted entry", func(t *testing.T, m *manager.Manager) {
			n := "Windows"
			must(t, m.Edit(0, manager.EditOptions{Description: &n}))
		}, ""},
		{"take the booted entry out of the order", func(t *testing.T, m *manager.Manager) { must(t, m.SetOrder([]uint16{1})) }, "booted-entry-lost"},
		{"delete the booted entry but keep a twin", func(t *testing.T, m *manager.Manager) {
			_, err := m.Add(manager.AddOptions{Description: "Twin", DevicePath: devicePath(t, `\EFI\Microsoft\Boot\bootmgfw.efi`), Position: -1})
			must(t, err)
			must(t, m.Remove(0))
		}, ""},
		{"disable everything", func(t *testing.T, m *manager.Manager) {
			must(t, m.SetActive(0, false))
			must(t, m.SetActive(1, false))
		}, "no-bootable-entry,booted-entry-lost"},
		{"delete everything", func(t *testing.T, m *manager.Manager) {
			must(t, m.Remove(0))
			must(t, m.Remove(1))
		}, "no-bootable-entry,booted-entry-lost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := seed(t)
			draft := store.NewOverlay(base)
			c.edit(t, &manager.Manager{Store: draft})
			got := riskCodes(manager.Risks(state(t, &manager.Manager{Store: base}), state(t, &manager.Manager{Store: draft})))
			if got != c.want {
				t.Fatalf("risks = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRisksDoNotBlockRepairs(t *testing.T) {
	base := seed(t)
	must(t, base.Set("Boot0000", payload(t, "Windows Boot Manager", false, `\EFI\Microsoft\Boot\bootmgfw.efi`)))
	must(t, base.Set("Boot0001", payload(t, "Linux", false, `\EFI\linux\grubx64.efi`)))
	draft := store.NewOverlay(base)
	must(t, (&manager.Manager{Store: draft}).Remove(1))
	risks := manager.Risks(state(t, &manager.Manager{Store: base}), state(t, &manager.Manager{Store: draft}))
	if len(risks) != 0 {
		t.Fatalf("nothing was bootable before either, so nothing new is at risk: %+v", risks)
	}
}
