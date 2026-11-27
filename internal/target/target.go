// Package target decides where a boot entry starts from: the partition and
// the loader file on it. It lists the partitions an entry can be pointed at
// and turns the one chosen, plus a loader path as typed, into the entry's
// device path.
package target

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
)

type Kind string

const (
	// Keep stays on the partition the entry points at now. Only an existing
	// entry offers it.
	Keep Kind = "keep"
	// Volume is a partition that discovery found.
	Volume Kind = "volume"
	// Entry is the partition another entry points at.
	Entry Kind = "entry"
)

// Partition is one place an entry can be pointed at.
type Partition struct {
	// Key identifies the choice when it is passed back to Resolve.
	Key  string `json:"key"`
	Kind Kind   `json:"kind"`
	// Volume is set when the partition can be browsed. A loader path on it
	// is checked before it is accepted.
	Volume *esp.Volume `json:"volume,omitempty"`
	// GUID is the GPT partition GUID, when there is one.
	GUID string `json:"partitionGuid,omitempty"`
	// DevicePath summarizes the current target (Keep).
	DevicePath string `json:"devicePath,omitempty"`
	// EntryName and EntryDescription name the entry whose partition is
	// reused (Entry).
	EntryName        string `json:"entryName,omitempty"`
	EntryDescription string `json:"entryDescription,omitempty"`
}

// Options is what an entry's target can be set to.
type Options struct {
	// Editable is false when only the entry's name can be changed, as for a
	// network boot entry; Reason says why, and Partitions is empty.
	Editable   bool        `json:"editable"`
	Reason     string      `json:"reason,omitempty"`
	Partitions []Partition `json:"partitions"`
	// VolumesError says why partition discovery failed. Partitions then
	// holds only those that other entries point at.
	VolumesError string `json:"volumesError,omitempty"`
}

// Targets resolves boot targets against a set of entries, normally the
// staged ones, and the partitions discovery finds.
type Targets struct {
	entries *manager.Manager
	volumes esp.Provider
}

func New(entries *manager.Manager, volumes esp.Provider) *Targets {
	return &Targets{entries: entries, volumes: volumes}
}

// Options lists where entry id can be pointed at, or a new entry when id is
// nil. Each partition appears once: the current one first, then the
// discovered volumes, then partitions only other entries know about.
func (t *Targets) Options(id *uint16) (*Options, error) {
	st, err := t.entries.State()
	if err != nil {
		return nil, err
	}
	out := &Options{Editable: true, Partitions: []Partition{}}
	vols, err := t.volumes.Volumes()
	if err != nil {
		out.VolumesError = err.Error()
		vols = nil
	}
	seen := map[string]bool{}

	if id != nil {
		cur, err := t.entries.DevicePathOf(*id)
		if err != nil {
			return nil, err
		}
		if cur.CanReplaceFilePath() != nil {
			return &Options{Reason: lockedReason(cur), Partitions: []Partition{}}, nil
		}
		keep := Partition{Key: string(Keep), Kind: Keep, DevicePath: cur.Summary()}
		if hd, ok := gptPartition(cur); ok {
			keep.GUID = hd.Signature.String()
			keep.Volume = volumeOn(vols, hd)
			seen[keep.GUID] = true
		}
		out.Partitions = append(out.Partitions, keep)
	}

	for i := range vols {
		v := &vols[i]
		guid := v.HD.Signature.String()
		if seen[guid] {
			continue
		}
		seen[guid] = true
		out.Partitions = append(out.Partitions, Partition{Key: string(Volume) + ":" + v.ID, Kind: Volume, Volume: v, GUID: guid})
	}

	for _, list := range [][]manager.Entry{st.Entries, st.Unordered} {
		for _, e := range list {
			if e.PartitionGUID == "" || seen[e.PartitionGUID] || (id != nil && e.ID == *id) {
				continue
			}
			seen[e.PartitionGUID] = true
			out.Partitions = append(out.Partitions, Partition{
				Key: string(Entry) + ":" + e.Name, Kind: Entry, GUID: e.PartitionGUID,
				EntryName: e.Name, EntryDescription: e.Description,
			})
		}
	}
	return out, nil
}

// Resolve returns the device path for a new entry (id nil) or the one entry
// id should switch to. partition is the Key of one of the Options and path
// is the loader path as typed.
//
// For an existing entry a nil path with a nil error means the target stays as
// it is: partition is empty, or the choice and path describe what the entry
// already points at, however the path is spelled.
func (t *Targets) Resolve(id *uint16, partition, path string) (efi.DevicePath, error) {
	var cur efi.DevicePath
	if id != nil {
		if partition == "" {
			return nil, nil
		}
		var err error
		if cur, err = t.entries.DevicePathOf(*id); err != nil {
			return nil, err
		}
		if cur.CanReplaceFilePath() != nil {
			return nil, fmt.Errorf("%s: %s", manager.VarName(*id), lockedReason(cur))
		}
	} else if partition == "" {
		return nil, fmt.Errorf("choose the partition the loader is on")
	}
	path, err := efi.NormalizeFilePath(path)
	if err != nil {
		return nil, err
	}

	var next efi.DevicePath
	var vol *esp.Volume
	kind, arg, _ := strings.Cut(partition, ":")
	switch Kind(kind) {
	case Keep:
		if cur == nil {
			return nil, fmt.Errorf("a new entry has no partition to keep")
		}
		if was, err := efi.NormalizeFilePath(cur.FilePath()); err == nil && was == path {
			return nil, nil
		}
		if hd, ok := gptPartition(cur); ok {
			vol = t.volumeFor(hd)
		}
		next, err = cur.WithFilePath(path)
	case Volume:
		var v esp.Volume
		if v, err = esp.Find(t.volumes, arg); err == nil {
			vol = &v
			next, err = efi.NewBootDevicePath(v.HD, path)
		}
	case Entry:
		var hd efi.HDNode
		if hd, err = t.partitionOf(arg); err == nil {
			vol = t.volumeFor(hd)
			next, err = efi.NewBootDevicePath(hd, path)
		}
	default:
		return nil, fmt.Errorf("unknown partition choice %q", partition)
	}
	if err != nil {
		return nil, err
	}
	if cur != nil && bytes.Equal(next, cur) {
		return nil, nil
	}
	if err := t.checkFile(vol, path); err != nil {
		return nil, err
	}
	return next, nil
}

// partitionOf returns the GPT partition the entry named name points at.
func (t *Targets) partitionOf(name string) (efi.HDNode, error) {
	id, ok := manager.ParseVarName(name)
	if !ok {
		return efi.HDNode{}, fmt.Errorf("unknown partition choice %q", string(Entry)+":"+name)
	}
	dp, err := t.entries.DevicePathOf(id)
	if err != nil {
		return efi.HDNode{}, err
	}
	hd, ok := gptPartition(dp)
	if !ok {
		return efi.HDNode{}, fmt.Errorf("%s does not point at a GPT partition, so its partition cannot be reused", name)
	}
	return hd, nil
}

func (t *Targets) volumeFor(hd efi.HDNode) *esp.Volume {
	vols, err := t.volumes.Volumes()
	if err != nil {
		return nil
	}
	return volumeOn(vols, hd)
}

// checkFile refuses a loader path that is known not to exist. When the
// partition cannot be read, the path is taken on trust.
func (t *Targets) checkFile(vol *esp.Volume, path string) error {
	if vol == nil {
		return nil
	}
	ok, err := t.volumes.FileExists(vol.ID, path)
	if err != nil || ok {
		return nil
	}
	return fmt.Errorf("%s does not exist on that partition", path)
}

// gptPartition returns the GPT partition a device path points at. A GPT
// partition is identified by its GUID alone.
func gptPartition(dp efi.DevicePath) (efi.HDNode, bool) {
	hd, ok := dp.HD()
	if !ok || !hd.IsGPT() {
		return efi.HDNode{}, false
	}
	return hd, true
}

func volumeOn(vols []esp.Volume, hd efi.HDNode) *esp.Volume {
	for i := range vols {
		if vols[i].HD.IsGPT() && vols[i].HD.Signature == hd.Signature {
			v := vols[i]
			return &v
		}
	}
	return nil
}

func lockedReason(dp efi.DevicePath) string {
	if !dp.HasFilePath() {
		return "This entry does not point at a file on a partition, so only its name can be changed here."
	}
	return "Other device path parts follow this entry's loader file, and uefimgr does not rewrite those, so only its name can be changed here."
}
