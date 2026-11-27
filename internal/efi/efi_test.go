package efi

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func testGUID(t *testing.T) GUID {
	t.Helper()
	g, err := ParseGUID("{11111111-2222-3333-4444-555555555555}")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGUIDRoundTrip(t *testing.T) {
	g, err := ParseGUID("{" + GlobalVariableGUID + "}")
	if err != nil {
		t.Fatal(err)
	}
	if g.String() != "{"+GlobalVariableGUID+"}" {
		t.Fatalf("round trip: got %s", g.String())
	}
	// EFI GUID first field is little-endian on the wire.
	if g[0] != 0x61 || g[1] != 0xdf || g[2] != 0xe4 || g[3] != 0x8b {
		t.Fatalf("unexpected wire bytes % x", g[:4])
	}
	if _, err := ParseGUID("nope"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestHDNodeRoundTrip(t *testing.T) {
	hd := HDNode{
		PartitionNumber: 2,
		PartitionStart:  0x800,
		PartitionSize:   0x1f000,
		Signature:       testGUID(t),
		Format:          partitionFormatGPT,
		SignatureType:   signatureTypeGUID,
	}
	b := hd.Bytes()
	if len(b) != 42 {
		t.Fatalf("HD node length = %d", len(b))
	}
	got, err := parseHDNode(b[4:])
	if err != nil {
		t.Fatal(err)
	}
	if got != hd {
		t.Fatalf("round trip mismatch: %+v != %+v", got, hd)
	}
}

func TestBootDevicePath(t *testing.T) {
	hd := HDNode{
		PartitionNumber: 2,
		PartitionStart:  0x800,
		PartitionSize:   0x1f000,
		Signature:       testGUID(t),
		Format:          partitionFormatGPT,
		SignatureType:   signatureTypeGUID,
	}
	dp, err := NewBootDevicePath(hd, `\EFI\Microsoft\Boot\bootmgfw.efi`)
	if err != nil {
		t.Fatal(err)
	}
	if err := dp.Validate(); err != nil {
		t.Fatal(err)
	}
	want := `HD(2,GPT,{11111111-2222-3333-4444-555555555555},0x800,0x1f000)/\EFI\Microsoft\Boot\bootmgfw.efi`
	if got := dp.Summary(); got != want {
		t.Fatalf("summary:\n got %q\nwant %q", got, want)
	}
	if got := dp.FilePath(); got != `\EFI\Microsoft\Boot\bootmgfw.efi` {
		t.Fatalf("FilePath = %q", got)
	}
	if got, ok := dp.HD(); !ok || got != hd {
		t.Fatalf("HD() = %+v, %v", got, ok)
	}
}

func TestFilePathNodeIsExactSize(t *testing.T) {
	for _, p := range []string{`\EFI\a.efi`, `\EFI\ab.efi`, `\x`} {
		n := FilePathNode(p)
		want := 4 + 2*(len(p)+1)
		if len(n) != want {
			t.Fatalf("%q: node is %d bytes, want %d (no padding)", p, len(n), want)
		}
		if int(binary.LittleEndian.Uint16(n[2:4])) != len(n) {
			t.Fatalf("%q: header length mismatch", p)
		}
		s, err := decodeUTF16Z(n[4:])
		if err != nil || s != p {
			t.Fatalf("%q decoded as %q, %v", p, s, err)
		}
	}
}

func TestNormalizeFilePath(t *testing.T) {
	ok := map[string]string{
		`EFI/refind/refind_x64.efi`: `\EFI\refind\refind_x64.efi`,
		` \EFI\\a.efi `:             `\EFI\a.efi`,
		`\vmlinuz-linux`:            `\vmlinuz-linux`,
	}
	for in, want := range ok {
		got, err := NormalizeFilePath(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeFilePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", `\EFI\`, "\\EFI\\a\x00.efi", "\\EFI\\a\n.efi"} {
		if _, err := NormalizeFilePath(bad); err == nil {
			t.Fatalf("NormalizeFilePath(%q) succeeded", bad)
		}
	}
}

func TestCleanDescription(t *testing.T) {
	if got, err := CleanDescription("  Linux  "); err != nil || got != "Linux" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", "A\x00B", "tab\there", strings.Repeat("x", 256)} {
		if _, err := CleanDescription(bad); err == nil {
			t.Fatalf("CleanDescription(%q) succeeded", bad)
		}
	}
}

func TestWithFilePathKeepsPartition(t *testing.T) {
	hd := HDNode{PartitionNumber: 2, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	dp, err := NewBootDevicePath(hd, `\EFI\old\old.efi`)
	if err != nil {
		t.Fatal(err)
	}
	next, err := dp.WithFilePath(`EFI/new/new.efi`)
	if err != nil {
		t.Fatal(err)
	}
	if next.FilePath() != `\EFI\new\new.efi` {
		t.Fatalf("FilePath = %q", next.FilePath())
	}
	if got, ok := next.HD(); !ok || got != hd {
		t.Fatalf("HD changed: %+v", got)
	}
	if !bytes.Equal(next[:42], dp[:42]) {
		t.Fatal("partition node bytes changed")
	}
	noFile := DevicePath(append(hd.Bytes(), EndNode()...))
	if _, err := noFile.WithFilePath(`\x.efi`); err == nil {
		t.Fatal("expected an error for a path without a FilePath() node")
	}
}

func TestLoadOptionWithSeveralDevicePaths(t *testing.T) {
	hd := HDNode{PartitionNumber: 1, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	first, _ := NewBootDevicePath(hd, `\EFI\linux\vmlinuz.efi`)
	second, _ := NewBootDevicePath(hd, `\EFI\linux\initrd.img`)
	list := append(append(DevicePath(nil), first...), second...)
	raw := buildLoadOption(t, "Linux", list, []byte("quiet"))

	lo, err := ParseLoadOption(raw)
	if err != nil {
		t.Fatalf("a load option with two device paths is valid: %v", err)
	}
	if !bytes.Equal(lo.DevicePath, first) || !bytes.Equal(lo.ExtraPaths, second) {
		t.Fatal("device path list was not split at the first End node")
	}
	if lo.DevicePath.FilePath() != `\EFI\linux\vmlinuz.efi` {
		t.Fatalf("FilePath = %q", lo.DevicePath.FilePath())
	}
	out, err := lo.MarshalBinary()
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("round trip changed the payload: %v", err)
	}
}

func TestMultiInstanceDevicePath(t *testing.T) {
	hd := HDNode{PartitionNumber: 1, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	var dp DevicePath
	dp = append(dp, hd.Bytes()...)
	dp = append(dp, nodeTypeEnd, 0x01, 4, 0) // End-instance
	dp = append(dp, hd.Bytes()...)
	dp = append(dp, EndNode()...)
	if err := dp.Validate(); err != nil {
		t.Fatalf("multi-instance path rejected: %v", err)
	}
}

func TestLoadOptionKeepsMalformedDescriptionBytes(t *testing.T) {
	hd := HDNode{PartitionNumber: 1, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	dp, _ := NewBootDevicePath(hd, `\EFI\BOOT\BOOTX64.EFI`)
	raw := []byte{1, 0, 0, 0}
	raw = binary.LittleEndian.AppendUint16(raw, uint16(len(dp)))
	raw = append(raw, 'A', 0, 0x00, 0xd8, 'B', 0, 0, 0) // unpaired surrogate
	raw = append(raw, dp...)

	lo, err := ParseLoadOption(raw)
	if err != nil {
		t.Fatal(err)
	}
	lo.SetActive(false)
	out, err := lo.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), raw...)
	want[0] = 0
	if !bytes.Equal(out, want) {
		t.Fatal("toggling the active bit rewrote the description bytes")
	}

	lo.Description = "Renamed"
	out, err = lo.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseLoadOption(out)
	if err != nil || again.Description != "Renamed" {
		t.Fatalf("rename: %+v, %v", again, err)
	}
}

func TestMarshalRejectsNULInDescription(t *testing.T) {
	hd := HDNode{PartitionNumber: 1, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	dp, _ := NewBootDevicePath(hd, `\x.efi`)
	lo := &LoadOption{Attributes: LoadOptionActive, Description: "A\x00B", DevicePath: dp}
	if _, err := lo.MarshalBinary(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDevicePathWithoutEndRejected(t *testing.T) {
	if err := (DevicePath(FilePathNode(`\x.efi`))).Validate(); err == nil {
		t.Fatal("expected missing End node error")
	}
}

func buildLoadOption(t *testing.T, desc string, dp DevicePath, opt []byte) []byte {
	t.Helper()
	units := utf16.Encode([]rune(desc))
	b := make([]byte, 6)
	binary.LittleEndian.PutUint32(b[0:4], LoadOptionActive)
	binary.LittleEndian.PutUint16(b[4:6], uint16(len(dp)))
	for _, u := range units {
		var tmp [2]byte
		binary.LittleEndian.PutUint16(tmp[:], u)
		b = append(b, tmp[:]...)
	}
	b = append(b, 0, 0)
	b = append(b, dp...)
	return append(b, opt...)
}

func TestLoadOptionGolden(t *testing.T) {
	hd := HDNode{PartitionNumber: 1, PartitionStart: 0x800, PartitionSize: 0x1000,
		Signature: testGUID(t), Format: partitionFormatGPT, SignatureType: signatureTypeGUID}
	dp, err := NewBootDevicePath(hd, `\EFI\BOOT\BOOTX64.EFI`)
	if err != nil {
		t.Fatal(err)
	}
	raw := buildLoadOption(t, "Test Entry", dp, []byte{0xde, 0xad})
	lo, err := ParseLoadOption(raw)
	if err != nil {
		t.Fatal(err)
	}
	if lo.Description != "Test Entry" || lo.Attributes != LoadOptionActive {
		t.Fatalf("unexpected option: %+v", lo)
	}
	if !lo.Active() {
		t.Fatal("Active() = false")
	}
	if !bytes.Equal(lo.DevicePath, dp) {
		t.Fatal("device path mismatch")
	}
	if !bytes.Equal(lo.OptionalData, []byte{0xde, 0xad}) {
		t.Fatalf("optional data = % x", lo.OptionalData)
	}
	out, err := lo.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatal("marshal does not reproduce golden payload")
	}
}

func TestLoadOptionRejectsTruncated(t *testing.T) {
	cases := [][]byte{
		nil,
		{1, 0, 0, 0},
		{1, 0, 0, 0, 4, 0},                      // no description terminator
		{1, 0, 0, 0, 16, 0, 'A', 0, 0, 0, 1, 2}, // path overruns
	}
	for _, c := range cases {
		if _, err := ParseLoadOption(c); err == nil {
			t.Fatalf("expected error for % x", c)
		}
	}
}

func TestBootOrder(t *testing.T) {
	ids := []uint16{3, 0, 0xffff}
	got, err := ParseBootOrder(BootOrderPayload(ids))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatal("length mismatch")
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("index %d: got %d want %d", i, got[i], ids[i])
		}
	}
	if _, err := ParseBootOrder([]byte{1, 0, 2}); err == nil {
		t.Fatal("expected odd-length error")
	}
}
