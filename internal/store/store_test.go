package store

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf16"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

func newFile(t *testing.T) *FileStore {
	t.Helper()
	fs, err := OpenFile(filepath.Join(t.TempDir(), "vars.json"))
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFileStoreEmptySetDeletes(t *testing.T) {
	fs := newFile(t)
	if err := fs.Set("BootOrder", []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("BootOrder", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Get("BootOrder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty write must delete the variable like firmware does, got %v", err)
	}
	if err := fs.Set("Missing", nil); err != nil {
		t.Fatalf("empty write of a missing variable: %v", err)
	}
	if err := fs.Delete("Missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete of a missing variable = %v", err)
	}
}

func TestFileStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vars.json")
	fs, _ := OpenFile(path)
	fs.Set("Timeout", []byte{5, 0})
	again, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := again.Get("Timeout"); err != nil || !reflect.DeepEqual(v, []byte{5, 0}) {
		t.Fatalf("got %v, %v", v, err)
	}
}

func TestOverlayStagesWithoutTouchingBase(t *testing.T) {
	base := newFile(t)
	base.Set("Boot0000", []byte{1})
	base.Set("Boot0001", []byte{2})
	o := NewOverlay(base)

	o.Set("Boot0000", []byte{9})
	o.Set("Boot0002", []byte{3})
	if err := o.Delete("Boot0001"); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete("Boot0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}

	if v, _ := base.Get("Boot0000"); !reflect.DeepEqual(v, []byte{1}) {
		t.Fatal("base was modified")
	}
	if _, err := base.Get("Boot0002"); !errors.Is(err, ErrNotFound) {
		t.Fatal("base gained a variable")
	}
	if v, _ := o.Get("Boot0000"); !reflect.DeepEqual(v, []byte{9}) {
		t.Fatalf("overlay Get = %v", v)
	}
	if _, err := o.Get("Boot0001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted variable still visible")
	}
	names, err := o.Names()
	if err != nil || !reflect.DeepEqual(names, []string{"Boot0000", "Boot0002"}) {
		t.Fatalf("Names = %v, %v", names, err)
	}

	changes, err := o.Changes()
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Name: "Boot0000", Old: []byte{1}, New: []byte{9}},
		{Name: "Boot0001", Old: []byte{2}},
		{Name: "Boot0002", New: []byte{3}},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("Changes = %+v", changes)
	}
}

func TestOverlayIgnoresNoOps(t *testing.T) {
	base := newFile(t)
	base.Set("Timeout", []byte{5, 0})
	o := NewOverlay(base)
	o.Set("Timeout", []byte{7, 0})
	o.Set("Timeout", []byte{5, 0})
	o.Set("BootNext", nil)
	o.Set("Boot0005", []byte{1})
	o.Delete("Boot0005")
	changes, err := o.Changes()
	if err != nil || len(changes) != 0 {
		t.Fatalf("writes that end where they started are not changes: %+v, %v", changes, err)
	}
	o.Set("Timeout", []byte{9, 0})
	o.Reset()
	if v, _ := o.Get("Timeout"); !reflect.DeepEqual(v, []byte{5, 0}) {
		t.Fatalf("Reset left %v", v)
	}
}

func variableNameRecord(vendor efi.GUID, name string, last bool) []byte {
	units := append(utf16.Encode([]rune(name)), 0)
	size := 20 + 2*len(units)
	if size%4 != 0 {
		size += 4 - size%4
	}
	b := make([]byte, size)
	if !last {
		binary.LittleEndian.PutUint32(b, uint32(size))
	}
	copy(b[4:], vendor[:])
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[20+2*i:], u)
	}
	return b
}

func TestParseVariableNames(t *testing.T) {
	global := efi.MustParseGUID(efi.GlobalVariableGUID)
	other := efi.MustParseGUID("11111111-2222-3333-4444-555555555555")
	var buf []byte
	buf = append(buf, variableNameRecord(global, "BootOrder", false)...)
	buf = append(buf, variableNameRecord(other, "Boot0009", false)...)
	buf = append(buf, variableNameRecord(global, "Boot0003", false)...)
	buf = append(buf, variableNameRecord(global, "Timeout", true)...)

	got := parseVariableNames(buf, global)
	if !reflect.DeepEqual(got, []string{"BootOrder", "Boot0003", "Timeout"}) {
		t.Fatalf("got %v", got)
	}
	for cut := 0; cut < len(buf); cut++ {
		parseVariableNames(buf[:cut], global) // must not panic on truncation
	}
	binary.LittleEndian.PutUint32(buf, 3) // nonsense offset must not loop
	parseVariableNames(buf, global)
}
