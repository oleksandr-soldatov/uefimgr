package esp

import (
	"fmt"
	"strings"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

// SimulatedHD is the partition the simulated system disk's ESP lives on. The
// sample entries of the simulated variable store point at it.
var SimulatedHD = efi.GPTPartition(1, 0x800, 0x32000, efi.MustParseGUID("11111111-2222-3333-4444-555555555555"))

type simVolume struct {
	Volume
	files []string
}

type simulated struct {
	volumes []simVolume
}

// NewSimulated returns a made-up disk layout for running the UI without
// firmware: a system ESP with no drive letter and a FAT32 USB stick.
func NewSimulated() Provider {
	usb := efi.GPTPartition(1, 0x800, 0x1d00000, efi.MustParseGUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))
	return &simulated{volumes: []simVolume{
		{
			Volume: Volume{
				ID: "sim-esp", Label: "SYSTEM", FileSystem: "FAT32", MountPoints: []string{},
				Disk: 0, Partition: 1, SizeBytes: 100 << 20, IsESP: true,
				PartitionGUID: SimulatedHD.Signature.String(), HD: SimulatedHD,
			},
			files: []string{
				`\EFI\Boot\bootx64.efi`,
				`\EFI\Microsoft\Boot\bootmgfw.efi`,
				`\EFI\Microsoft\Boot\bootmgr.efi`,
				`\EFI\Microsoft\Boot\memtest.efi`,
				`\EFI\Microsoft\Boot\BCD`,
				`\EFI\refind\refind_x64.efi`,
				`\EFI\refind\refind.conf`,
				`\EFI\ubuntu\grubx64.efi`,
				`\EFI\ubuntu\shimx64.efi`,
				`\EFI\ubuntu\grub.cfg`,
				`\EFI\Shell\Shell.efi`,
			},
		},
		{
			Volume: Volume{
				ID: "sim-usb", Label: "INSTALLER", FileSystem: "FAT32", MountPoints: []string{"E:"},
				Disk: 1, Partition: 1, SizeBytes: 14 << 30, IsESP: false,
				PartitionGUID: usb.Signature.String(), HD: usb,
			},
			files: []string{
				`\EFI\BOOT\BOOTX64.EFI`,
				`\EFI\BOOT\grubx64.efi`,
				`\boot\grub\grub.cfg`,
			},
		},
	}}
}

func (s *simulated) Volumes() ([]Volume, error) {
	out := make([]Volume, len(s.volumes))
	for i, v := range s.volumes {
		out[i] = v.Volume
	}
	return out, nil
}

func (s *simulated) find(id string) (*simVolume, error) {
	for i := range s.volumes {
		if s.volumes[i].ID == id {
			return &s.volumes[i], nil
		}
	}
	return nil, fmt.Errorf("unknown partition")
}

func (s *simulated) ListDir(volumeID, dir string) ([]DirEntry, error) {
	v, err := s.find(volumeID)
	if err != nil {
		return nil, err
	}
	parts, err := splitPath(dir)
	if err != nil {
		return nil, err
	}
	prefix := `\`
	if len(parts) > 0 {
		prefix = `\` + strings.Join(parts, `\`) + `\`
	}
	out := []DirEntry{}
	seen := map[string]bool{}
	found := len(parts) == 0
	for _, f := range v.files {
		if len(f) < len(prefix) || !strings.EqualFold(f[:len(prefix)], prefix) {
			continue
		}
		found = true
		rest := f[len(prefix):]
		name, _, isDir := strings.Cut(rest, `\`)
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		switch {
		case isDir:
			seen[key] = true
			out = append(out, DirEntry{Name: name, Dir: true})
		case isLoader(name):
			seen[key] = true
			out = append(out, DirEntry{Name: name, Size: 1 << 20})
		}
	}
	if !found {
		return nil, fmt.Errorf("read %s: directory not found", dir)
	}
	sortEntries(out)
	return out, nil
}

func (s *simulated) FileExists(volumeID, path string) (bool, error) {
	v, err := s.find(volumeID)
	if err != nil {
		return false, err
	}
	parts, err := splitPath(path)
	if err != nil {
		return false, err
	}
	want := `\` + strings.Join(parts, `\`)
	for _, f := range v.files {
		if strings.EqualFold(f, want) {
			return true, nil
		}
	}
	return false, nil
}
