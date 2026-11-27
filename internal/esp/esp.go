// Package esp finds the partitions a boot entry can point at and lets the
// caller browse them for loader files.
package esp

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

// ErrNotGPT means the volume belongs to an MBR disk; the entries this tool
// writes identify a partition by its GPT GUID.
var ErrNotGPT = errors.New("partition is not GPT")

// ErrUnsupported means partition discovery is not available on this system.
var ErrUnsupported = errors.New("partition discovery is only available on Windows")

// Volume is a partition that firmware can read a loader from.
type Volume struct {
	// ID is opaque: a volume GUID path on Windows.
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	FileSystem    string   `json:"fileSystem"`
	MountPoints   []string `json:"mountPoints"`
	Disk          int      `json:"disk"` // -1 when unknown
	Partition     uint32   `json:"partition"`
	SizeBytes     uint64   `json:"sizeBytes"`
	IsESP         bool     `json:"isEsp"`
	PartitionGUID string   `json:"partitionGuid"`

	HD efi.HDNode `json:"-"`
}

// DirEntry is a directory or a loader file on a volume.
type DirEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// Provider discovers volumes and reads their directories.
type Provider interface {
	Volumes() ([]Volume, error)
	// ListDir returns the subdirectories and .efi files of dir.
	ListDir(volumeID, dir string) ([]DirEntry, error)
	FileExists(volumeID, path string) (bool, error)
}

// Find returns the volume with the given ID.
func Find(p Provider, id string) (Volume, error) {
	vols, err := p.Volumes()
	if err != nil {
		return Volume{}, err
	}
	for _, v := range vols {
		if v.ID == id {
			return v, nil
		}
	}
	return Volume{}, fmt.Errorf("that partition is no longer available")
}

// sortVolumes puts EFI system partitions first, then orders by disk.
func sortVolumes(vols []Volume) {
	sort.SliceStable(vols, func(i, j int) bool {
		a, b := vols[i], vols[j]
		if a.IsESP != b.IsESP {
			return a.IsESP
		}
		if a.Disk != b.Disk {
			return a.Disk < b.Disk
		}
		return a.Partition < b.Partition
	})
}

// sortEntries lists directories before files, each alphabetically.
func sortEntries(entries []DirEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

func isLoader(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".efi")
}

// splitPath turns a path inside a volume into its components and rejects
// anything that could step outside the volume.
func splitPath(path string) ([]string, error) {
	var parts []string
	for _, p := range strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }) {
		if p == "." || p == ".." {
			return nil, fmt.Errorf("path must not contain %q", p)
		}
		if strings.ContainsAny(p, `:*?"<>|`) {
			return nil, fmt.Errorf("path contains a character that is not allowed in a file name")
		}
		for _, r := range p {
			if r < 0x20 {
				return nil, fmt.Errorf("path contains a control character")
			}
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// Offsets in PARTITION_INFORMATION_EX on 64-bit Windows. The GPT part of
// the union starts at 32: PartitionType, then PartitionId, each a GUID.
const (
	partitionStyleGPT  = 1
	offPartitionStyle  = 0
	offStartingOffset  = 8
	offPartitionLength = 16
	offPartitionNumber = 24
	offGPTType         = 32
	offGPTUniqueID     = 48
	minPartitionInfo   = offGPTUniqueID + 16
)

// parsePartitionInfo decodes PARTITION_INFORMATION_EX into an HD() node and
// reports whether the partition is an EFI system partition. sector is the
// disk's logical sector size.
func parsePartitionInfo(buf []byte, sector uint64) (hd efi.HDNode, isESP bool, sizeBytes uint64, err error) {
	if len(buf) < minPartitionInfo {
		return hd, false, 0, fmt.Errorf("short partition info (%d bytes)", len(buf))
	}
	if u32(buf[offPartitionStyle:]) != partitionStyleGPT {
		return hd, false, 0, ErrNotGPT
	}
	if sector < 512 || sector%512 != 0 {
		return hd, false, 0, fmt.Errorf("implausible sector size %d", sector)
	}
	start := u64(buf[offStartingOffset:])
	length := u64(buf[offPartitionLength:])
	if start%sector != 0 || length%sector != 0 {
		return hd, false, 0, fmt.Errorf("partition geometry is not aligned to %d-byte sectors", sector)
	}
	var typ, id efi.GUID
	copy(typ[:], buf[offGPTType:offGPTType+16])
	copy(id[:], buf[offGPTUniqueID:offGPTUniqueID+16])
	if id.IsZero() {
		return hd, false, 0, fmt.Errorf("GPT partition has a zero unique GUID")
	}
	hd = efi.GPTPartition(u32(buf[offPartitionNumber:]), start/sector, length/sector, id)
	return hd, typ == efi.ESPTypeGUID, length, nil
}

// parseBytesPerSector reads BytesPerSector from DISK_GEOMETRY_EX.
func parseBytesPerSector(geom []byte) (uint64, error) {
	if len(geom) < 24 {
		return 0, fmt.Errorf("short disk geometry (%d bytes)", len(geom))
	}
	bps := uint64(u32(geom[20:]))
	if bps < 512 || bps%512 != 0 {
		return 0, fmt.Errorf("implausible sector size %d", bps)
	}
	return bps, nil
}

func u32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func u64(b []byte) uint64 {
	return uint64(u32(b)) | uint64(u32(b[4:]))<<32
}
