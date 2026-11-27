package efi

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// EFI_LOAD_OPTION attribute bits (UEFI spec, "Load Options").
const (
	LoadOptionActive         uint32 = 0x00000001
	LoadOptionForceReconnect uint32 = 0x00000002
	LoadOptionHidden         uint32 = 0x00000008
	LoadOptionCategoryBoot   uint32 = 0x00000100
	LoadOptionCategoryMask   uint32 = 0x00001f00
)

const maxDescriptionChars = 255

// LoadOption is EFI_LOAD_OPTION as stored in Boot####.
//
//	UINT32 Attributes;
//	UINT16 FilePathListLength;
//	CHAR16 Description[];
//	EFI_DEVICE_PATH_PROTOCOL FilePathList[];
//	UINT8 OptionalData[];
type LoadOption struct {
	Attributes  uint32
	Description string
	// DevicePath is FilePathList[0], the boot target.
	DevicePath DevicePath
	// ExtraPaths holds any further FilePathList device paths verbatim.
	ExtraPaths   []byte
	OptionalData []byte

	// The description bytes as read from firmware. They are written back
	// unchanged unless Description was edited, so an entry survives a
	// parse/marshal cycle byte for byte even if its UTF-16 is malformed.
	descRaw    []byte
	descParsed string
}

// Active reports whether the entry is enabled (LOAD_OPTION_ACTIVE).
func (o *LoadOption) Active() bool { return o.Attributes&LoadOptionActive != 0 }

// SetActive sets or clears LOAD_OPTION_ACTIVE.
func (o *LoadOption) SetActive(active bool) {
	if active {
		o.Attributes |= LoadOptionActive
	} else {
		o.Attributes &^= LoadOptionActive
	}
}

// CleanDescription validates a user-supplied entry description.
func CleanDescription(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("the name must not be empty")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("the name contains a control character")
		}
	}
	if len(utf16.Encode([]rune(s))) > maxDescriptionChars {
		return "", fmt.Errorf("the name is longer than %d characters", maxDescriptionChars)
	}
	return s, nil
}

// MarshalBinary returns the exact variable payload.
func (o *LoadOption) MarshalBinary() ([]byte, error) {
	if o == nil {
		return nil, fmt.Errorf("nil load option")
	}
	if err := o.DevicePath.Validate(); err != nil {
		return nil, fmt.Errorf("invalid device path: %w", err)
	}
	if len(o.ExtraPaths) > 0 {
		if _, err := SplitDevicePaths(o.ExtraPaths); err != nil {
			return nil, fmt.Errorf("invalid additional device path: %w", err)
		}
	}
	listLen := len(o.DevicePath) + len(o.ExtraPaths)
	if listLen > 0xffff {
		return nil, fmt.Errorf("device path list too long: %d bytes", listLen)
	}
	desc := o.descRaw
	if desc == nil || o.Description != o.descParsed {
		if strings.ContainsRune(o.Description, 0) {
			return nil, fmt.Errorf("the name contains a NUL character")
		}
		desc = encodeUTF16Z(o.Description)
	}
	b := make([]byte, 6, 6+len(desc)+listLen+len(o.OptionalData))
	binary.LittleEndian.PutUint32(b[0:4], o.Attributes)
	binary.LittleEndian.PutUint16(b[4:6], uint16(listLen))
	b = append(b, desc...)
	b = append(b, o.DevicePath...)
	b = append(b, o.ExtraPaths...)
	b = append(b, o.OptionalData...)
	return b, nil
}

// ParseLoadOption decodes a firmware Boot#### payload.
func ParseLoadOption(b []byte) (*LoadOption, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("load option too short: %d bytes", len(b))
	}
	attrs := binary.LittleEndian.Uint32(b[0:4])
	listLen := int(binary.LittleEndian.Uint16(b[4:6]))
	rest := b[6:]

	var units []uint16
	descEnd := -1
	for i := 0; i+1 < len(rest); i += 2 {
		u := binary.LittleEndian.Uint16(rest[i : i+2])
		if u == 0 {
			descEnd = i + 2
			break
		}
		units = append(units, u)
	}
	if descEnd < 0 {
		return nil, fmt.Errorf("unterminated description")
	}
	if listLen == 0 {
		return nil, fmt.Errorf("empty device path")
	}
	if descEnd+listLen > len(rest) {
		return nil, fmt.Errorf("device path length %d overruns option (%d bytes available)", listLen, len(rest)-descEnd)
	}
	list := rest[descEnd : descEnd+listLen]
	paths, err := SplitDevicePaths(list)
	if err != nil {
		return nil, err
	}
	desc := string(utf16.Decode(units))
	return &LoadOption{
		Attributes:   attrs,
		Description:  desc,
		DevicePath:   paths[0],
		ExtraPaths:   append([]byte(nil), list[len(paths[0]):]...),
		OptionalData: append([]byte(nil), rest[descEnd+listLen:]...),
		descRaw:      append([]byte(nil), rest[:descEnd]...),
		descParsed:   desc,
	}, nil
}

// BootOrderPayload encodes BootOrder: an array of little-endian uint16 IDs.
func BootOrderPayload(ids []uint16) []byte {
	b := make([]byte, 2*len(ids))
	for i, id := range ids {
		binary.LittleEndian.PutUint16(b[2*i:], id)
	}
	return b
}

// ParseBootOrder decodes a BootOrder array.
func ParseBootOrder(b []byte) ([]uint16, error) {
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("BootOrder has odd length %d", len(b))
	}
	ids := make([]uint16, len(b)/2)
	for i := range ids {
		ids[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return ids, nil
}

// Uint16Value decodes a single-uint16 variable such as BootNext or Timeout.
func Uint16Value(b []byte) (uint16, error) {
	if len(b) < 2 {
		return 0, fmt.Errorf("variable too short: %d bytes", len(b))
	}
	return binary.LittleEndian.Uint16(b), nil
}

// Uint16Payload encodes a single-uint16 variable.
func Uint16Payload(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

// Uint64Payload encodes a single-uint64 variable such as OsIndications.
func Uint64Payload(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// Uint64Value decodes a uint64 variable.
func Uint64Value(b []byte) (uint64, error) {
	if len(b) < 8 {
		return 0, fmt.Errorf("variable too short: %d bytes", len(b))
	}
	return binary.LittleEndian.Uint64(b), nil
}
