//go:build windows

package esp

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetPartitionInfoEx     = 0x00070048
	ioctlDiskGetDriveGeometryEx     = 0x000700a0
	ioctlVolumeGetVolumeDiskExtents = 0x00560000
)

var volumeIDRE = regexp.MustCompile(`^\\\\\?\\Volume\{[0-9A-Fa-f-]{36}\}\\$`)

type windowsProvider struct{}

// Default returns the provider for the running system.
func Default() Provider { return windowsProvider{} }

// Volumes lists GPT partitions that firmware can read: EFI system
// partitions and other FAT volumes, with or without a drive letter.
func (windowsProvider) Volumes() ([]Volume, error) {
	buf := make([]uint16, windows.MAX_PATH+1)
	find, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("enumerate volumes: %w", err)
	}
	defer windows.FindVolumeClose(find)

	vols := []Volume{}
	for {
		if v, ok := describeVolume(windows.UTF16ToString(buf)); ok {
			vols = append(vols, v)
		}
		if err := windows.FindNextVolume(find, &buf[0], uint32(len(buf))); err != nil {
			break
		}
	}
	sortVolumes(vols)
	return vols, nil
}

func describeVolume(root string) (Volume, bool) {
	if !volumeIDRE.MatchString(root) {
		return Volume{}, false
	}
	rootp, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return Volume{}, false
	}
	if t := windows.GetDriveType(rootp); t != windows.DRIVE_FIXED && t != windows.DRIVE_REMOVABLE {
		return Volume{}, false
	}
	// The IOCTLs below need no access rights, so the device is opened
	// without any; that also works for volumes that are locked.
	devp, err := windows.UTF16PtrFromString(strings.TrimSuffix(root, `\`))
	if err != nil {
		return Volume{}, false
	}
	h, err := windows.CreateFile(devp, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return Volume{}, false
	}
	defer windows.CloseHandle(h)

	info := make([]byte, 1024)
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetPartitionInfoEx,
		nil, 0, &info[0], uint32(len(info)), &n, nil); err != nil {
		return Volume{}, false
	}
	geom := make([]byte, 256)
	var gn uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetDriveGeometryEx,
		nil, 0, &geom[0], uint32(len(geom)), &gn, nil); err != nil {
		return Volume{}, false
	}
	sector, err := parseBytesPerSector(geom[:gn])
	if err != nil {
		return Volume{}, false
	}
	hd, isESP, size, err := parsePartitionInfo(info[:n], sector)
	if err != nil {
		return Volume{}, false
	}

	v := Volume{
		ID:            root,
		Disk:          -1,
		Partition:     hd.PartitionNumber,
		SizeBytes:     size,
		IsESP:         isESP,
		PartitionGUID: hd.Signature.String(),
		MountPoints:   mountPoints(rootp),
		HD:            hd,
	}
	label := make([]uint16, windows.MAX_PATH+1)
	fs := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(rootp, &label[0], uint32(len(label)),
		nil, nil, nil, &fs[0], uint32(len(fs))); err == nil {
		v.Label = windows.UTF16ToString(label)
		v.FileSystem = windows.UTF16ToString(fs)
	}
	if !v.IsESP && !strings.HasPrefix(strings.ToUpper(v.FileSystem), "FAT") {
		return Volume{}, false
	}

	// VOLUME_DISK_EXTENTS: a count, then DISK_EXTENT records that start
	// with the disk number.
	ext := make([]byte, 256)
	var en uint32
	if err := windows.DeviceIoControl(h, ioctlVolumeGetVolumeDiskExtents,
		nil, 0, &ext[0], uint32(len(ext)), &en, nil); err == nil && en >= 12 && u32(ext) >= 1 {
		v.Disk = int(u32(ext[8:]))
	}
	return v, true
}

func mountPoints(rootp *uint16) []string {
	out := []string{}
	buf := make([]uint16, 1024)
	var n uint32
	if err := windows.GetVolumePathNamesForVolumeName(rootp, &buf[0], uint32(len(buf)), &n); err != nil {
		return out
	}
	// A list of NUL-terminated strings ending with an empty one.
	for start := 0; start < len(buf); {
		end := start
		for end < len(buf) && buf[end] != 0 {
			end++
		}
		if end == start {
			break
		}
		out = append(out, strings.TrimSuffix(windows.UTF16ToString(buf[start:end]), `\`))
		start = end + 1
	}
	return out
}

func volumePath(volumeID, path string) (string, error) {
	if !volumeIDRE.MatchString(volumeID) {
		return "", fmt.Errorf("unknown partition")
	}
	parts, err := splitPath(path)
	if err != nil {
		return "", err
	}
	return volumeID + strings.Join(parts, `\`), nil
}

func (windowsProvider) ListDir(volumeID, dir string) ([]DirEntry, error) {
	path, err := volumePath(volumeID, dir)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := []DirEntry{}
	for _, it := range items {
		switch {
		case it.IsDir():
			out = append(out, DirEntry{Name: it.Name(), Dir: true})
		case isLoader(it.Name()):
			e := DirEntry{Name: it.Name()}
			if info, err := it.Info(); err == nil {
				e.Size = info.Size()
			}
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out, nil
}

func (windowsProvider) FileExists(volumeID, path string) (bool, error) {
	full, err := volumePath(volumeID, path)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(full)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}
