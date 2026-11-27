// Package demo fills a simulated variable store with a believable boot
// configuration, so the interface can be run without touching firmware.
package demo

import (
	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
)

type entry struct {
	id       uint16
	name     string
	inactive bool
	path     efi.DevicePath
	options  []byte
}

// Seed writes the sample configuration. Its entries live on the simulated
// system partition that esp.NewSimulated describes.
func Seed(s store.Store) error {
	file := func(p string) efi.DevicePath {
		dp, err := efi.NewBootDevicePath(esp.SimulatedHD, p)
		if err != nil {
			panic(err)
		}
		return dp
	}
	// A network-boot entry: no partition and no file, as firmware creates.
	network := efi.DevicePath(nil)
	network = append(network, efi.Node{Type: 0x03, Subtype: 0x0b, Data: make([]byte, 33)}.Bytes()...)
	network = append(network, efi.Node{Type: 0x03, Subtype: 0x0c, Data: make([]byte, 23)}.Bytes()...)
	network = append(network, efi.EndNode()...)

	entries := []entry{
		{id: 0, name: "Windows Boot Manager", path: file(`\EFI\Microsoft\Boot\bootmgfw.efi`), options: []byte("WINDOWS\x00\x01\x00\x00\x00")},
		{id: 1, name: "ubuntu", path: file(`\EFI\ubuntu\shimx64.efi`)},
		{id: 2, name: "UEFI Shell", inactive: true, path: file(`\EFI\Shell\Shell.efi`)},
		{id: 3, name: "UEFI: PXE IPv4 Intel(R) Ethernet", path: network},
		{id: 7, name: "rEFInd (left over)", path: file(`\EFI\refind\refind_x64.efi`)},
	}
	for _, e := range entries {
		opt := &efi.LoadOption{Description: e.name, DevicePath: e.path, OptionalData: e.options}
		opt.SetActive(!e.inactive)
		raw, err := opt.MarshalBinary()
		if err != nil {
			return err
		}
		if err := s.Set(manager.VarName(e.id), raw); err != nil {
			return err
		}
	}
	vars := map[string][]byte{
		manager.BootOrderVar:              efi.BootOrderPayload([]uint16{0, 1, 2, 3}),
		manager.BootCurrentVar:            efi.Uint16Payload(0),
		manager.TimeoutVar:                efi.Uint16Payload(3),
		manager.OsIndicationsSupportedVar: efi.Uint64Payload(1),
	}
	for name, data := range vars {
		if err := s.Set(name, data); err != nil {
			return err
		}
	}
	return nil
}
