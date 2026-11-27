package target_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
	"github.com/oleksandr-soldatov/uefimgr/internal/target"
)

var (
	espHD   = efi.GPTPartition(1, 0x800, 0x32000, efi.MustParseGUID("11111111-2222-3333-4444-555555555555"))
	otherHD = efi.GPTPartition(3, 0x100000, 0x80000, efi.MustParseGUID("99999999-8888-7777-6666-555555555555"))
)

// volumes is a provider with one ESP holding the given files.
type volumes struct {
	files   map[string]bool
	err     error // returned by Volumes
	fileErr error // returned by FileExists
}

func (v *volumes) Volumes() ([]esp.Volume, error) {
	if v.err != nil {
		return nil, v.err
	}
	return []esp.Volume{{ID: "esp", Label: "SYSTEM", IsESP: true, PartitionGUID: espHD.Signature.String(), HD: espHD}}, nil
}

func (v *volumes) ListDir(string, string) ([]esp.DirEntry, error) { return nil, nil }

func (v *volumes) FileExists(id, path string) (bool, error) {
	if v.fileErr != nil {
		return false, v.fileErr
	}
	return v.files[path], nil
}

type fixture struct {
	t       *testing.T
	store   *store.FileStore
	volumes *volumes
	targets *target.Targets
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fs, err := store.OpenFile(filepath.Join(t.TempDir(), "vars.json"))
	if err != nil {
		t.Fatal(err)
	}
	vols := &volumes{files: map[string]bool{`\EFI\Boot\bootx64.efi`: true, `\EFI\new\new.efi`: true}}
	f := &fixture{t: t, store: fs, volumes: vols, targets: target.New(&manager.Manager{Store: fs}, vols)}
	f.entry(0, "Windows", rawPath(espHD.Bytes(), efi.FilePathNode(`\EFI\Microsoft\Boot\bootmgfw.efi`)))
	f.entry(1, "Linux", rawPath(otherHD.Bytes(), efi.FilePathNode(`\EFI\linux\grubx64.efi`)))
	f.entry(2, "Linux fallback", rawPath(otherHD.Bytes(), efi.FilePathNode(`\EFI\linux\fallback.efi`)))
	if err := fs.Set(manager.BootOrderVar, efi.BootOrderPayload([]uint16{0, 1, 2})); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) entry(id uint16, desc string, dp efi.DevicePath) {
	f.t.Helper()
	opt := &efi.LoadOption{Description: desc, DevicePath: dp}
	opt.SetActive(true)
	raw, err := opt.MarshalBinary()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Set(manager.VarName(id), raw); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) devicePath(id uint16) efi.DevicePath {
	f.t.Helper()
	dp, err := (&manager.Manager{Store: f.store}).DevicePathOf(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return dp
}

func rawPath(nodes ...[]byte) efi.DevicePath {
	var dp efi.DevicePath
	for _, n := range nodes {
		dp = append(dp, n...)
	}
	return append(dp, efi.EndNode()...)
}

func keys(opts *target.Options, err error) []string {
	if err != nil {
		return []string{"error: " + err.Error()}
	}
	var out []string
	for _, p := range opts.Partitions {
		out = append(out, p.Key)
	}
	return out
}

func id(n uint16) *uint16 { return &n }

func TestOptionsOfferEachPartitionOnce(t *testing.T) {
	f := newFixture(t)
	if got := keys(f.targets.Options(nil)); !reflect.DeepEqual(got, []string{"volume:esp", "entry:Boot0001"}) {
		t.Fatalf("new entry = %v", got)
	}
	// Boot0001's own partition is kept, so Boot0002 on the same partition is
	// not offered a second time.
	if got := keys(f.targets.Options(id(1))); !reflect.DeepEqual(got, []string{"keep", "volume:esp"}) {
		t.Fatalf("Boot0001 = %v", got)
	}
	opts, err := f.targets.Options(id(0))
	if got := keys(opts, err); !reflect.DeepEqual(got, []string{"keep", "entry:Boot0001"}) {
		t.Fatalf("Boot0000 = %v", got)
	}
	if opts.Partitions[0].Volume == nil || opts.Partitions[1].EntryDescription != "Linux" {
		t.Fatalf("partitions = %+v", opts.Partitions)
	}
}

func TestOptionsWithoutDiscovery(t *testing.T) {
	f := newFixture(t)
	f.volumes.err = errors.New("access denied")
	opts, err := f.targets.Options(nil)
	if got := keys(opts, err); !reflect.DeepEqual(got, []string{"entry:Boot0000", "entry:Boot0001"}) {
		t.Fatalf("partitions = %v", got)
	}
	if opts.VolumesError != "access denied" {
		t.Fatalf("volumes error = %q", opts.VolumesError)
	}
}

func TestEditabilityMatchesWhatResolveAccepts(t *testing.T) {
	vendor := efi.Node{Type: 0x04, Subtype: 0x03, Data: make([]byte, 16)}.Bytes()
	network := efi.Node{Type: 0x03, Subtype: 0x0b, Data: make([]byte, 33)}.Bytes()
	cases := map[string]efi.DevicePath{
		"file only":       rawPath(efi.FilePathNode(`\EFI\Boot\bootx64.efi`)),
		"split file path": rawPath(espHD.Bytes(), efi.FilePathNode(`\EFI`), efi.FilePathNode(`Boot\bootx64.efi`)),
		"node after file": rawPath(espHD.Bytes(), efi.FilePathNode(`\EFI\Boot\bootx64.efi`), vendor),
		"no file at all":  rawPath(network),
	}
	for name, dp := range cases {
		f := newFixture(t)
		f.entry(5, name, dp)
		opts, err := f.targets.Options(id(5))
		if err != nil {
			t.Fatal(err)
		}
		_, resolveErr := f.targets.Resolve(id(5), "keep", `\EFI\new\new.efi`)
		if opts.Editable != (resolveErr == nil) {
			t.Errorf("%s: editable = %v but Resolve returned %v", name, opts.Editable, resolveErr)
		}
		if !opts.Editable && (opts.Reason == "" || len(opts.Partitions) != 0) {
			t.Errorf("%s: options = %+v", name, opts)
		}
	}
}

func TestResolveKeepsWhatThePathAlreadySays(t *testing.T) {
	f := newFixture(t)
	f.entry(5, "odd spelling", rawPath(espHD.Bytes(), efi.FilePathNode(`EFI\Boot`), efi.FilePathNode(`bootx64.efi`)))
	for _, path := range []string{`EFI\Boot\bootx64.efi`, `\EFI\Boot\bootx64.efi`, ` /EFI//Boot/bootx64.efi `} {
		dp, err := f.targets.Resolve(id(5), "keep", path)
		if err != nil || dp != nil {
			t.Fatalf("%q: got %v, %v; want no change", path, dp, err)
		}
	}
	if dp, err := f.targets.Resolve(id(5), "", "anything"); err != nil || dp != nil {
		t.Fatalf("no partition choice: got %v, %v", dp, err)
	}
	// Choosing the volume the entry is already on, with the same file, is
	// no change either.
	f.entry(6, "plain", rawPath(espHD.Bytes(), efi.FilePathNode(`\EFI\Boot\bootx64.efi`)))
	if dp, err := f.targets.Resolve(id(6), "volume:esp", `\EFI\Boot\bootx64.efi`); err != nil || dp != nil {
		t.Fatalf("same volume and file: got %v, %v", dp, err)
	}
}

func TestResolveKeepPreservesNodesBeforeTheFile(t *testing.T) {
	f := newFixture(t)
	pci := efi.Node{Type: 0x01, Subtype: 0x01, Data: []byte{0, 0x1d}}.Bytes()
	f.entry(5, "long form", rawPath(pci, espHD.Bytes(), efi.FilePathNode(`\EFI\old\old.efi`)))
	dp, err := f.targets.Resolve(id(5), "keep", `EFI/new/new.efi`)
	if err != nil {
		t.Fatal(err)
	}
	want := rawPath(pci, espHD.Bytes(), efi.FilePathNode(`\EFI\new\new.efi`))
	if !bytes.Equal(dp, want) {
		t.Fatalf("got %s, want %s", dp.Summary(), want.Summary())
	}
	if _, err := f.targets.Resolve(id(5), "keep", `\EFI\missing.efi`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing file on a browsable partition: %v", err)
	}
}

func TestResolveNewEntry(t *testing.T) {
	f := newFixture(t)
	dp, err := f.targets.Resolve(nil, "volume:esp", `\EFI\Boot\bootx64.efi`)
	if err != nil {
		t.Fatal(err)
	}
	if hd, _ := dp.HD(); hd != espHD || dp.FilePath() != `\EFI\Boot\bootx64.efi` {
		t.Fatalf("got %s", dp.Summary())
	}
	// Boot0001's partition cannot be browsed, so the path is taken on trust.
	dp, err = f.targets.Resolve(nil, "entry:Boot0001", `\EFI\linux\anything.efi`)
	if err != nil {
		t.Fatal(err)
	}
	if hd, _ := dp.HD(); hd != otherHD {
		t.Fatalf("got %s", dp.Summary())
	}

	f.entry(5, "network", rawPath(efi.Node{Type: 0x03, Subtype: 0x0b, Data: make([]byte, 33)}.Bytes()))
	for partition, want := range map[string]string{
		"":               "choose the partition",
		"keep":           "no partition to keep",
		"volume:gone":    "no longer available",
		"entry:Boot0005": "GPT partition",
		"entry:Boot0009": "does not exist",
		"entry:nope":     "unknown partition choice",
		"floppy:a":       "unknown partition choice",
	} {
		if _, err := f.targets.Resolve(nil, partition, `\EFI\Boot\bootx64.efi`); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want one containing %q", partition, err, want)
		}
	}
	if _, err := f.targets.Resolve(nil, "volume:esp", ""); err == nil {
		t.Error("an empty loader path must be refused")
	}
}

func TestResolveTrustsPathWhenFileCheckFails(t *testing.T) {
	f := newFixture(t)
	f.volumes.fileErr = errors.New("volume is locked")
	if _, err := f.targets.Resolve(nil, "volume:esp", `\EFI\unknown.efi`); err != nil {
		t.Fatalf("an unreadable partition must not block the path: %v", err)
	}
	if got := f.devicePath(0); got.FilePath() != `\EFI\Microsoft\Boot\bootmgfw.efi` {
		t.Fatalf("Resolve must not write: %s", got.Summary())
	}
}
