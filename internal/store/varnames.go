package store

import (
	"encoding/binary"
	"unicode/utf16"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

// parseVariableNames decodes the VARIABLE_NAME records that
// NtEnumerateSystemEnvironmentValuesEx returns and keeps the names that
// belong to the given vendor namespace.
//
//	ULONG NextEntryOffset; // from this record, 0 for the last one
//	GUID  VendorGuid;
//	WCHAR Name[];          // NUL terminated
func parseVariableNames(buf []byte, vendor efi.GUID) []string {
	const header = 4 + 16
	var names []string
	for off := 0; off+header <= len(buf); {
		next := int(binary.LittleEndian.Uint32(buf[off:]))
		end := len(buf)
		if next > 0 && off+next < end {
			end = off + next
		}
		var guid efi.GUID
		copy(guid[:], buf[off+4:off+header])
		if guid == vendor {
			var units []uint16
			for i := off + header; i+1 < end; i += 2 {
				u := binary.LittleEndian.Uint16(buf[i:])
				if u == 0 {
					break
				}
				units = append(units, u)
			}
			if len(units) > 0 {
				names = append(names, string(utf16.Decode(units)))
			}
		}
		if next < header {
			break
		}
		off += next
	}
	return names
}
