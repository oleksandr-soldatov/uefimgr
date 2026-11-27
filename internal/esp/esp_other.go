//go:build !windows

package esp

type unsupported struct{}

// Default returns the provider for the running system. Off Windows there is
// none; use NewSimulated with the simulated variable store.
func Default() Provider { return unsupported{} }

func (unsupported) Volumes() ([]Volume, error)                 { return nil, ErrUnsupported }
func (unsupported) ListDir(string, string) ([]DirEntry, error) { return nil, ErrUnsupported }
func (unsupported) FileExists(string, string) (bool, error)    { return false, ErrUnsupported }
