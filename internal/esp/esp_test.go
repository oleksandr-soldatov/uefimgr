package esp

import (
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

var testID = efi.MustParseGUID("{11111111-2222-3333-4444-555555555555}")

func partitionInfo(style uint32, start, length uint64, number uint32, typ, id efi.GUID) []byte {
	b := make([]byte, 144)
	binary.LittleEndian.PutUint32(b[offPartitionStyle:], style)
	binary.LittleEndian.PutUint64(b[offStartingOffset:], start)
	binary.LittleEndian.PutUint64(b[offPartitionLength:], length)
	binary.LittleEndian.PutUint32(b[offPartitionNumber:], number)
	copy(b[offGPTType:], typ[:])
	copy(b[offGPTUniqueID:], id[:])
	return b
}

func TestParsePartitionInfo(t *testing.T) {
	b := partitionInfo(1, 1048576, 100*1024*1024, 2, efi.ESPTypeGUID, testID)
	hd, isESP, size, err := parsePartitionInfo(b, 512)
	if err != nil {
		t.Fatal(err)
	}
	if hd.PartitionNumber != 2 || hd.PartitionStart != 2048 || hd.PartitionSize != 204800 {
		t.Fatalf("hd = %+v", hd)
	}
	if hd.Signature != testID || !hd.IsGPT() {
		t.Fatalf("hd = %+v", hd)
	}
	if !isESP || size != 100*1024*1024 {
		t.Fatalf("isESP = %v, size = %d", isESP, size)
	}

	basicData := efi.MustParseGUID("ebd0a0a2-b9e5-4433-87c0-68b6b72699c7")
	_, isESP, _, err = parsePartitionInfo(partitionInfo(1, 1048576, 1048576, 3, basicData, testID), 512)
	if err != nil || isESP {
		t.Fatalf("a data partition is not an ESP: %v, %v", isESP, err)
	}
}

func TestParsePartitionInfo4K(t *testing.T) {
	hd, _, _, err := parsePartitionInfo(partitionInfo(1, 4096, 4096*8, 1, efi.ESPTypeGUID, testID), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if hd.PartitionStart != 1 || hd.PartitionSize != 8 {
		t.Fatalf("hd = %+v", hd)
	}
}

func TestParsePartitionInfoRejects(t *testing.T) {
	if _, _, _, err := parsePartitionInfo(partitionInfo(0, 512, 512, 1, efi.ESPTypeGUID, testID), 512); !errors.Is(err, ErrNotGPT) {
		t.Fatalf("MBR: %v", err)
	}
	cases := map[string]struct {
		buf    []byte
		sector uint64
	}{
		"short buffer":   {make([]byte, 20), 512},
		"zero GUID":      {partitionInfo(1, 512, 512, 1, efi.ESPTypeGUID, efi.GUID{}), 512},
		"misaligned":     {partitionInfo(1, 513, 512, 1, efi.ESPTypeGUID, testID), 512},
		"unknown sector": {partitionInfo(1, 512, 512, 1, efi.ESPTypeGUID, testID), 0},
		"4K as 512e":     {partitionInfo(1, 512, 512, 1, efi.ESPTypeGUID, testID), 4096},
	}
	for name, c := range cases {
		if _, _, _, err := parsePartitionInfo(c.buf, c.sector); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseBytesPerSector(t *testing.T) {
	geom := make([]byte, 32)
	binary.LittleEndian.PutUint32(geom[20:], 4096)
	if got, err := parseBytesPerSector(geom); err != nil || got != 4096 {
		t.Fatalf("got %d, %v", got, err)
	}
	binary.LittleEndian.PutUint32(geom[20:], 0)
	if _, err := parseBytesPerSector(geom); err == nil {
		t.Fatal("a zero sector size must be an error, not a guess")
	}
	if _, err := parseBytesPerSector(geom[:8]); err == nil {
		t.Fatal("short geometry accepted")
	}
}

func TestSplitPath(t *testing.T) {
	got, err := splitPath(`\EFI/Microsoft\\Boot\`)
	if err != nil || !reflect.DeepEqual(got, []string{"EFI", "Microsoft", "Boot"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{`\EFI\..\..\Windows`, `..`, `\EFI\.\x`, `C:\Windows`, `\EFI\a*`, "\\EFI\\a\x00b"} {
		if _, err := splitPath(bad); err == nil {
			t.Errorf("splitPath(%q) accepted", bad)
		}
	}
}

func TestSimulatedProvider(t *testing.T) {
	p := NewSimulated()
	vols, err := p.Volumes()
	if err != nil || len(vols) != 2 || !vols[0].IsESP || vols[0].HD != SimulatedHD {
		t.Fatalf("volumes = %+v, %v", vols, err)
	}
	root, err := p.ListDir("sim-esp", `\`)
	if err != nil || !reflect.DeepEqual(root, []DirEntry{{Name: "EFI", Dir: true}}) {
		t.Fatalf("root = %+v, %v", root, err)
	}
	boot, err := p.ListDir("sim-esp", `\efi\microsoft\boot`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range boot {
		names = append(names, e.Name)
	}
	if !reflect.DeepEqual(names, []string{"bootmgfw.efi", "bootmgr.efi", "memtest.efi"}) {
		t.Fatalf("only loaders are listed, got %v", names)
	}
	if _, err := p.ListDir("sim-esp", `\nope`); err == nil {
		t.Fatal("missing directory accepted")
	}
	if _, err := p.ListDir("other", `\`); err == nil {
		t.Fatal("unknown volume accepted")
	}
	if ok, err := p.FileExists("sim-esp", `EFI/refind/REFIND_X64.EFI`); err != nil || !ok {
		t.Fatalf("FileExists = %v, %v", ok, err)
	}
	if ok, _ := p.FileExists("sim-esp", `\EFI\refind\missing.efi`); ok {
		t.Fatal("missing file reported as present")
	}
	if v, err := Find(p, "sim-usb"); err != nil || v.Label != "INSTALLER" {
		t.Fatalf("Find = %+v, %v", v, err)
	}
	if _, err := Find(p, "gone"); err == nil {
		t.Fatal("Find accepted an unknown volume")
	}
}
