//go:build windows

package store

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
)

const (
	attrNonVolatile   uint32 = 0x00000001
	attrBootService   uint32 = 0x00000002
	attrRuntime       uint32 = 0x00000004
	bootVarAttributes        = attrNonVolatile | attrBootService | attrRuntime

	firmwareTypeBIOS = 1

	// SystemEnvironmentNameInformation: enumerate VARIABLE_NAME records.
	variableInformationNames = 1
	statusBufferTooSmall     = 0xC0000023

	// IDs probed when the firmware cannot be enumerated.
	probeLimit = 0x0100

	namesCacheTTL = 2 * time.Second
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	ntdll    = windows.NewLazySystemDLL("ntdll.dll")

	procGetFirmwareVariable   = kernel32.NewProc("GetFirmwareEnvironmentVariableW")
	procSetFirmwareVariableEx = kernel32.NewProc("SetFirmwareEnvironmentVariableExW")
	procSetFirmwareVariable   = kernel32.NewProc("SetFirmwareEnvironmentVariableW")
	procGetFirmwareType       = kernel32.NewProc("GetFirmwareType")
	procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
	procEnumerateVariables    = ntdll.NewProc("NtEnumerateSystemEnvironmentValuesEx")
)

// FirmwareStore reads and writes the EFI global variable namespace through
// the Windows firmware environment API.
type FirmwareStore struct {
	guid     *uint16
	guidBin  efi.GUID
	prepOnce sync.Once
	prepErr  error

	mu      sync.Mutex
	names   []string
	namesAt time.Time
}

func NewFirmware() *FirmwareStore {
	guid, _ := windows.UTF16PtrFromString("{" + efi.GlobalVariableGUID + "}")
	return &FirmwareStore{guid: guid, guidBin: efi.MustParseGUID(efi.GlobalVariableGUID)}
}

func (s *FirmwareStore) Describe() string { return "UEFI firmware variables" }

// Prepare checks that the machine booted through UEFI and enables
// SeSystemEnvironmentPrivilege, which both reads and writes need.
func (s *FirmwareStore) Prepare() error {
	s.prepOnce.Do(func() { s.prepErr = prepare() })
	return s.prepErr
}

func prepare() error {
	if procGetFirmwareType.Find() == nil {
		var kind uint32
		r1, _, _ := procGetFirmwareType.Call(uintptr(unsafe.Pointer(&kind)))
		if r1 != 0 && kind == firmwareTypeBIOS {
			return ErrNotUEFI
		}
	}
	return enablePrivilege("SeSystemEnvironmentPrivilege")
}

func (s *FirmwareStore) Get(name string) ([]byte, error) {
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// The API does not report the required size, so grow the buffer until
	// the value fits.
	for size := 4096; size <= 1<<20; size *= 2 {
		buf := make([]byte, size)
		r1, _, e := procGetFirmwareVariable.Call(
			uintptr(unsafe.Pointer(namep)),
			uintptr(unsafe.Pointer(s.guid)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(size),
		)
		runtime.KeepAlive(namep)
		if r1 != 0 {
			return append([]byte(nil), buf[:int(r1)]...), nil
		}
		if e != windows.ERROR_INSUFFICIENT_BUFFER {
			return nil, describeErr("read", name, e)
		}
	}
	return nil, fmt.Errorf("read %s: variable is larger than 1 MiB", name)
}

func (s *FirmwareStore) Set(name string, data []byte) error {
	if len(data) == 0 {
		err := s.Delete(name)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.Prepare(); err != nil {
		return err
	}
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	s.invalidateNames()
	r1, _, e := procSetFirmwareVariableEx.Call(
		uintptr(unsafe.Pointer(namep)),
		uintptr(unsafe.Pointer(s.guid)),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(bootVarAttributes),
	)
	runtime.KeepAlive(namep)
	runtime.KeepAlive(data)
	if r1 == 0 {
		return describeErr("write", name, e)
	}
	return nil
}

// Delete removes a variable. Windows documents a zero size on the non-Ex
// call as the way to delete.
func (s *FirmwareStore) Delete(name string) error {
	if err := s.Prepare(); err != nil {
		return err
	}
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	s.invalidateNames()
	r1, _, e := procSetFirmwareVariable.Call(
		uintptr(unsafe.Pointer(namep)),
		uintptr(unsafe.Pointer(s.guid)),
		0, 0,
	)
	runtime.KeepAlive(namep)
	if r1 == 0 {
		return describeErr("delete", name, e)
	}
	return nil
}

// Names lists the variables in the global namespace. It asks the kernel to
// enumerate them and, if that is refused, probes the low Boot#### IDs.
func (s *FirmwareStore) Names() ([]string, error) {
	s.mu.Lock()
	if s.names != nil && time.Since(s.namesAt) < namesCacheTTL {
		out := append([]string(nil), s.names...)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	names, err := s.enumerate()
	if err != nil {
		names = s.probe()
	}
	sort.Strings(names)

	s.mu.Lock()
	s.names = append([]string{}, names...)
	s.namesAt = time.Now()
	s.mu.Unlock()
	return names, nil
}

func (s *FirmwareStore) invalidateNames() {
	s.mu.Lock()
	s.names = nil
	s.mu.Unlock()
}

func (s *FirmwareStore) enumerate() ([]string, error) {
	if err := procEnumerateVariables.Find(); err != nil {
		return nil, err
	}
	size := uint32(64 << 10)
	for attempt := 0; attempt < 5; attempt++ {
		buf := make([]byte, size)
		length := size
		status, _, _ := procEnumerateVariables.Call(
			variableInformationNames,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&length)),
		)
		switch uint32(status) {
		case 0:
			if int(length) > len(buf) {
				length = uint32(len(buf))
			}
			return parseVariableNames(buf[:length], s.guidBin), nil
		case statusBufferTooSmall:
			if length <= size {
				length = size * 2
			}
			size = length
		default:
			return nil, fmt.Errorf("enumerate firmware variables: NTSTATUS 0x%08x", uint32(status))
		}
	}
	return nil, fmt.Errorf("enumerate firmware variables: buffer kept growing")
}

func (s *FirmwareStore) probe() []string {
	var names []string
	for id := 0; id < probeLimit; id++ {
		name := fmt.Sprintf("Boot%04X", id)
		if _, err := s.Get(name); err == nil {
			names = append(names, name)
		}
	}
	return names
}

func enablePrivilege(name string) error {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token)
	if err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()

	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namep, &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue(%s): %w", name, err)
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}

	// Called directly because success with ERROR_NOT_ALL_ASSIGNED means the
	// token does not hold the privilege, and the wrapper hides that.
	r1, _, e := procAdjustTokenPrivileges.Call(
		uintptr(token), 0,
		uintptr(unsafe.Pointer(&tp)), 0, 0, 0,
	)
	runtime.KeepAlive(&tp)
	if r1 == 0 {
		return fmt.Errorf("AdjustTokenPrivileges: %w", e)
	}
	if e == windows.ERROR_NOT_ALL_ASSIGNED {
		return fmt.Errorf("%s is not held: uefimgr must run as administrator", name)
	}
	return nil
}

// IsElevated reports whether the process token is elevated.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

func describeErr(op, name string, e error) error {
	switch e {
	case windows.ERROR_ENVVAR_NOT_FOUND:
		return ErrNotFound
	case windows.ERROR_INVALID_FUNCTION:
		return ErrNotUEFI
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_PRIVILEGE_NOT_HELD:
		return fmt.Errorf("%s %s: access denied (%v): uefimgr must run as administrator", op, name, e)
	case windows.ERROR_SUCCESS:
		return fmt.Errorf("%s %s: firmware rejected the request", op, name)
	default:
		return fmt.Errorf("%s %s: %w", op, name, e)
	}
}
