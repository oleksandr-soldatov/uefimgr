package efi

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// GlobalVariableGUID is the EFI global variable namespace. BootOrder,
// Boot####, BootNext, BootCurrent, Timeout and OsIndications all live here.
const GlobalVariableGUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// ESPTypeGUID is the GPT partition type of an EFI System Partition.
var ESPTypeGUID = MustParseGUID("c12a7328-f81f-11d2-ba4b-00a0c93ec93b")

// GUID is an EFI/UEFI GUID in on-wire byte order.
//
// The first three fields are little-endian and the last two are big-endian,
// matching both the EFI specification and the in-memory layout of a Windows
// GUID. That means GPT partition IDs read from Windows can be copied into an
// HD() device path node verbatim.
type GUID [16]byte

func ParseGUID(s string) (GUID, error) {
	var g GUID
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "{")
	t = strings.TrimSuffix(t, "}")
	parts := strings.Split(t, "-")
	if len(parts) != 5 ||
		len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 ||
		len(parts[3]) != 4 || len(parts[4]) != 12 {
		return g, fmt.Errorf("invalid GUID %q", s)
	}
	b, err := hex.DecodeString(parts[0] + parts[1] + parts[2] + parts[3] + parts[4])
	if err != nil {
		return g, fmt.Errorf("invalid GUID %q: %w", s, err)
	}
	g[0], g[1], g[2], g[3] = b[3], b[2], b[1], b[0]
	g[4], g[5] = b[5], b[4]
	g[6], g[7] = b[7], b[6]
	copy(g[8:], b[8:])
	return g, nil
}

func MustParseGUID(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

func (g GUID) String() string {
	return fmt.Sprintf("{%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x}",
		g[3], g[2], g[1], g[0],
		g[5], g[4],
		g[7], g[6],
		g[8], g[9],
		g[10], g[11], g[12], g[13], g[14], g[15])
}

func (g GUID) IsZero() bool {
	for _, b := range g {
		if b != 0 {
			return false
		}
	}
	return true
}
