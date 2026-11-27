//go:build !windows

package store

// FirmwareStore is unavailable off Windows. Use OpenFile for simulation.
type FirmwareStore struct{}

func NewFirmware() *FirmwareStore { return &FirmwareStore{} }

func (s *FirmwareStore) Describe() string {
	return "UEFI firmware variables (unavailable on this platform)"
}
func (s *FirmwareStore) Prepare() error             { return ErrUnsupported }
func (s *FirmwareStore) Get(string) ([]byte, error) { return nil, ErrUnsupported }
func (s *FirmwareStore) Set(string, []byte) error   { return ErrUnsupported }
func (s *FirmwareStore) Delete(string) error        { return ErrUnsupported }
func (s *FirmwareStore) Names() ([]string, error)   { return nil, ErrUnsupported }

func IsElevated() bool { return false }
