// Package manager implements boot-entry operations on top of a variable store.
package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
)

const (
	BootCurrentVar            = "BootCurrent"
	BootNextVar               = "BootNext"
	BootOrderVar              = "BootOrder"
	TimeoutVar                = "Timeout"
	OsIndicationsVar          = "OsIndications"
	OsIndicationsSupportedVar = "OsIndicationsSupported"

	// EFI_OS_INDICATIONS_BOOT_TO_FW_UI requests the firmware setup screen on
	// the next boot.
	osIndicationsBootToFW = 1 << 0
)

// Boot#### names use upper-case hex digits; firmware ignores other spellings.
var bootVarRE = regexp.MustCompile(`^Boot[0-9A-F]{4}$`)

func VarName(id uint16) string { return fmt.Sprintf("Boot%04X", id) }

func ParseVarName(name string) (uint16, bool) {
	if !bootVarRE.MatchString(name) {
		return 0, false
	}
	v, err := strconv.ParseUint(name[4:], 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// Manager reads and edits the boot configuration held in Store. It never
// decides whether an edit is safe; see Risks and Session for that.
type Manager struct {
	Store store.Store
}

type Entry struct {
	ID              uint16 `json:"id"`
	Name            string `json:"name"`
	OrderIndex      int    `json:"orderIndex"` // -1 when not in BootOrder
	Description     string `json:"description"`
	Active          bool   `json:"active"`
	Attributes      uint32 `json:"attributes"`
	DevicePath      string `json:"devicePath"`
	FilePath        string `json:"filePath,omitempty"`
	PartitionGUID   string `json:"partitionGuid,omitempty"`
	PartitionNumber uint32 `json:"partitionNumber,omitempty"`
	ExtraPaths      int    `json:"extraPaths,omitempty"`
	OptionalDataLen int    `json:"optionalDataLen"`
	Broken          bool   `json:"broken,omitempty"`
	Error           string `json:"error,omitempty"`
	Raw             []byte `json:"raw,omitempty"`
}

type State struct {
	Store       string  `json:"store"`
	BootCurrent *uint16 `json:"bootCurrent,omitempty"`
	BootNext    *uint16 `json:"bootNext,omitempty"`
	Timeout     *uint16 `json:"timeout,omitempty"`
	// FirmwareUISupported says the firmware accepts a request to open its
	// setup screen on the next boot; FirmwareUIRequested says one is pending.
	FirmwareUISupported bool `json:"firmwareUiSupported"`
	FirmwareUIRequested bool `json:"firmwareUiRequested"`
	// Order is BootOrder with repeated IDs dropped.
	Order   []uint16 `json:"order"`
	Entries []Entry  `json:"entries"`
	// Unordered lists Boot#### variables that BootOrder does not mention.
	Unordered []Entry  `json:"unordered"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Find returns the entry with the given ID from either list.
func (st *State) Find(id uint16) (Entry, bool) {
	for _, e := range st.Entries {
		if e.ID == id {
			return e, true
		}
	}
	for _, e := range st.Unordered {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

func (m *Manager) State() (*State, error) {
	st := &State{Store: m.Store.Describe(), Order: []uint16{}, Entries: []Entry{}, Unordered: []Entry{}}

	// A missing BootOrder is an empty one: firmware deletes the variable
	// when the last ID is removed.
	orderRaw, err := m.Store.Get(BootOrderVar)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read BootOrder: %w", err)
	}
	order, err := efi.ParseBootOrder(orderRaw)
	if err != nil {
		return nil, fmt.Errorf("parse BootOrder: %w", err)
	}
	seen := map[uint16]bool{}
	for _, id := range order {
		if seen[id] {
			st.Warnings = append(st.Warnings, fmt.Sprintf("BootOrder lists %s more than once; the repeat is ignored", VarName(id)))
			continue
		}
		seen[id] = true
		st.Order = append(st.Order, id)
	}

	if v, err := m.readUint16(BootCurrentVar); err == nil {
		st.BootCurrent = v
	}
	if v, err := m.readUint16(BootNextVar); err == nil {
		st.BootNext = v
		if _, err := m.Store.Get(VarName(*v)); errors.Is(err, store.ErrNotFound) {
			st.Warnings = append(st.Warnings, fmt.Sprintf("BootNext points at %s, which does not exist", VarName(*v)))
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		st.Warnings = append(st.Warnings, "BootNext: "+err.Error())
	}
	if v, err := m.readUint16(TimeoutVar); err == nil {
		st.Timeout = v
	} else if !errors.Is(err, store.ErrNotFound) {
		st.Warnings = append(st.Warnings, "Timeout: "+err.Error())
	}
	st.FirmwareUISupported = m.readUint64(OsIndicationsSupportedVar)&osIndicationsBootToFW != 0
	st.FirmwareUIRequested = m.readUint64(OsIndicationsVar)&osIndicationsBootToFW != 0

	for i, id := range st.Order {
		st.Entries = append(st.Entries, m.entry(id, i))
	}

	if l, ok := m.Store.(store.Lister); ok {
		names, err := l.Names()
		if err != nil {
			st.Warnings = append(st.Warnings, "Could not list firmware variables, so entries outside the boot sequence are not shown: "+err.Error())
		}
		var ids []uint16
		for _, name := range names {
			if id, ok := ParseVarName(name); ok && !seen[id] {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			st.Unordered = append(st.Unordered, m.entry(id, -1))
		}
	}
	return st, nil
}

func (m *Manager) entry(id uint16, orderIndex int) Entry {
	e := Entry{ID: id, Name: VarName(id), OrderIndex: orderIndex}
	raw, err := m.Store.Get(e.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		e.Broken = true
		e.Error = "BootOrder refers to this entry, but the variable does not exist"
		e.Description = "(missing)"
		return e
	case err != nil:
		e.Broken = true
		e.Error = err.Error()
		e.Description = "(unreadable)"
		return e
	}
	e.Raw = raw
	opt, err := efi.ParseLoadOption(raw)
	if err != nil {
		e.Broken = true
		e.Error = "uefimgr could not decode this entry: " + err.Error()
		e.Description = "(unrecognized)"
		return e
	}
	e.Description = opt.Description
	e.Active = opt.Active()
	e.Attributes = opt.Attributes
	e.DevicePath = opt.DevicePath.Summary()
	e.FilePath = opt.DevicePath.FilePath()
	e.OptionalDataLen = len(opt.OptionalData)
	if hd, ok := opt.DevicePath.HD(); ok && hd.IsGPT() {
		e.PartitionGUID = hd.Signature.String()
		e.PartitionNumber = hd.PartitionNumber
	}
	if len(opt.ExtraPaths) > 0 {
		if paths, err := efi.SplitDevicePaths(opt.ExtraPaths); err == nil {
			e.ExtraPaths = len(paths)
		}
	}
	return e
}

func (m *Manager) readUint16(name string) (*uint16, error) {
	raw, err := m.Store.Get(name)
	if err != nil {
		return nil, err
	}
	v, err := efi.Uint16Value(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (m *Manager) readUint64(name string) uint64 {
	raw, err := m.Store.Get(name)
	if err != nil {
		return 0
	}
	v, _ := efi.Uint64Value(raw)
	return v
}

type AddOptions struct {
	Description  string
	DevicePath   efi.DevicePath
	Inactive     bool
	OptionalData []byte
	// Position is the index in BootOrder to insert at. Negative appends.
	Position int
}

// Add creates a new Boot#### entry and inserts it into BootOrder.
func (m *Manager) Add(o AddOptions) (uint16, error) {
	desc, err := efi.CleanDescription(o.Description)
	if err != nil {
		return 0, err
	}
	opt := &efi.LoadOption{
		Description:  desc,
		DevicePath:   o.DevicePath,
		OptionalData: o.OptionalData,
	}
	opt.SetActive(!o.Inactive)
	payload, err := opt.MarshalBinary()
	if err != nil {
		return 0, err
	}
	st, err := m.State()
	if err != nil {
		return 0, err
	}
	id, ok := m.firstFreeID(st)
	if !ok {
		return 0, fmt.Errorf("every Boot#### ID is in use")
	}
	pos := o.Position
	if pos < 0 || pos > len(st.Order) {
		pos = len(st.Order)
	}
	order := make([]uint16, 0, len(st.Order)+1)
	order = append(order, st.Order[:pos]...)
	order = append(order, id)
	order = append(order, st.Order[pos:]...)

	// Entry first, order second: BootOrder never names a missing variable.
	if err := m.Store.Set(VarName(id), payload); err != nil {
		return 0, err
	}
	if err := m.Store.Set(BootOrderVar, efi.BootOrderPayload(order)); err != nil {
		_ = m.deleteVar(VarName(id))
		return 0, err
	}
	return id, nil
}

type EditOptions struct {
	// Description replaces the entry's name when non-nil and different from
	// it. The name an entry already has is kept as it is, even one that a
	// new entry could not be given, such as an empty one.
	Description *string
	// DevicePath replaces the boot target when non-nil. Additional device
	// paths and optional data are kept.
	DevicePath efi.DevicePath
}

// Edit changes an existing entry in place. An edit that changes nothing
// writes nothing.
func (m *Manager) Edit(id uint16, o EditOptions) error {
	opt, err := m.load(id)
	if err != nil {
		return err
	}
	changed := false
	if o.Description != nil && *o.Description != opt.Description {
		desc, err := efi.CleanDescription(*o.Description)
		if err != nil {
			return err
		}
		changed = desc != opt.Description
		opt.Description = desc
	}
	if o.DevicePath != nil && !bytes.Equal(o.DevicePath, opt.DevicePath) {
		changed = true
		opt.DevicePath = o.DevicePath
	}
	if !changed {
		return nil
	}
	payload, err := opt.MarshalBinary()
	if err != nil {
		return err
	}
	return m.Store.Set(VarName(id), payload)
}

// SetActive enables or disables an entry without removing it.
func (m *Manager) SetActive(id uint16, active bool) error {
	opt, err := m.load(id)
	if err != nil {
		return err
	}
	opt.SetActive(active)
	payload, err := opt.MarshalBinary()
	if err != nil {
		return err
	}
	return m.Store.Set(VarName(id), payload)
}

// SetOrder replaces BootOrder. Every ID must name an existing entry; the set
// of entries may differ from the current order.
func (m *Manager) SetOrder(ids []uint16) error {
	seen := map[uint16]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("%s appears twice in the new boot sequence", VarName(id))
		}
		seen[id] = true
		if _, err := m.Store.Get(VarName(id)); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%s does not exist", VarName(id))
			}
			return err
		}
	}
	return m.Store.Set(BootOrderVar, efi.BootOrderPayload(ids))
}

// SetNext sets the one-shot BootNext entry, or clears it when id is nil.
func (m *Manager) SetNext(id *uint16) error {
	if id == nil {
		return m.deleteVar(BootNextVar)
	}
	if _, err := m.load(*id); err != nil {
		return err
	}
	return m.Store.Set(BootNextVar, efi.Uint16Payload(*id))
}

// SetTimeout sets the firmware menu timeout in seconds, or clears it.
func (m *Manager) SetTimeout(seconds *uint16) error {
	if seconds == nil {
		return m.deleteVar(TimeoutVar)
	}
	return m.Store.Set(TimeoutVar, efi.Uint16Payload(*seconds))
}

// Remove takes an entry out of BootOrder and deletes its variable.
func (m *Manager) Remove(id uint16) error {
	st, err := m.State()
	if err != nil {
		return err
	}
	_, getErr := m.Store.Get(VarName(id))
	exists := getErr == nil
	inOrder := containsID(st.Order, id)
	if !exists && !inOrder {
		return fmt.Errorf("%s does not exist", VarName(id))
	}
	if inOrder {
		order := make([]uint16, 0, len(st.Order))
		for _, x := range st.Order {
			if x != id {
				order = append(order, x)
			}
		}
		// Order first, entry second: BootOrder never names a missing variable.
		if err := m.Store.Set(BootOrderVar, efi.BootOrderPayload(order)); err != nil {
			return err
		}
	}
	if st.BootNext != nil && *st.BootNext == id {
		if err := m.deleteVar(BootNextVar); err != nil {
			return err
		}
	}
	return m.deleteVar(VarName(id))
}

// SetFirmwareUI requests, or cancels a request for, the firmware setup
// screen on the next boot.
func (m *Manager) SetFirmwareUI(on bool) error {
	cur := m.readUint64(OsIndicationsVar)
	if on {
		if m.readUint64(OsIndicationsSupportedVar)&osIndicationsBootToFW == 0 {
			return fmt.Errorf("this firmware does not support booting into its setup screen on request")
		}
		return m.Store.Set(OsIndicationsVar, efi.Uint64Payload(cur|osIndicationsBootToFW))
	}
	if cur&osIndicationsBootToFW == 0 {
		return nil
	}
	return m.Store.Set(OsIndicationsVar, efi.Uint64Payload(cur&^osIndicationsBootToFW))
}

// DevicePathOf returns the boot target of an entry.
func (m *Manager) DevicePathOf(id uint16) (efi.DevicePath, error) {
	opt, err := m.load(id)
	if err != nil {
		return nil, err
	}
	return opt.DevicePath, nil
}

func (m *Manager) load(id uint16) (*efi.LoadOption, error) {
	raw, err := m.Store.Get(VarName(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s does not exist", VarName(id))
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", VarName(id), err)
	}
	opt, err := efi.ParseLoadOption(raw)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be decoded, so it cannot be changed: %w", VarName(id), err)
	}
	return opt, nil
}

func (m *Manager) deleteVar(name string) error {
	if err := m.Store.Delete(name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// firstFreeID picks the lowest ID with no variable and no BootOrder slot.
func (m *Manager) firstFreeID(st *State) (uint16, bool) {
	used := map[uint16]bool{}
	for _, id := range st.Order {
		used[id] = true
	}
	for _, e := range st.Unordered {
		used[e.ID] = true
	}
	occupied := func(name string) bool {
		if o, ok := m.Store.(interface{ Occupied(string) bool }); ok {
			return o.Occupied(name)
		}
		_, err := m.Store.Get(name)
		return !errors.Is(err, store.ErrNotFound)
	}
	for i := 0; i <= 0xffff; i++ {
		id := uint16(i)
		if !used[id] && !occupied(VarName(id)) {
			return id, true
		}
	}
	return 0, false
}

func containsID(ids []uint16, id uint16) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Snapshot captures every variable needed to reproduce the boot
// configuration. OsIndications is left out: it is a one-shot request.
type Snapshot struct {
	Format    int               `json:"format"`
	CreatedAt time.Time         `json:"createdAt"`
	Source    string            `json:"source"`
	Note      string            `json:"note,omitempty"`
	Vars      map[string][]byte `json:"vars"`
}

const snapshotFormat = 1

func (m *Manager) Snapshot() (*Snapshot, error) {
	st, err := m.State()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		Format:    snapshotFormat,
		CreatedAt: time.Now().UTC(),
		Source:    m.Store.Describe(),
		Note:      "uefimgr firmware boot configuration backup",
		Vars:      map[string][]byte{},
	}
	for _, name := range []string{BootOrderVar, BootNextVar, TimeoutVar} {
		if raw, err := m.Store.Get(name); err == nil {
			s.Vars[name] = raw
		}
	}
	for _, list := range [][]Entry{st.Entries, st.Unordered} {
		for _, e := range list {
			if e.Raw != nil {
				s.Vars[e.Name] = e.Raw
			}
		}
	}
	return s, nil
}

func (s *Snapshot) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseSnapshot decodes and validates a backup file.
func ParseSnapshot(raw []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("this is not a uefimgr backup: %w", err)
	}
	if s.Format != snapshotFormat {
		return nil, fmt.Errorf("unsupported backup format %d", s.Format)
	}
	// BootCurrent is read-only and OsIndications is a one-shot request that
	// older backups may still contain.
	delete(s.Vars, BootCurrentVar)
	delete(s.Vars, OsIndicationsVar)
	if len(s.Vars) == 0 {
		return nil, fmt.Errorf("backup contains no variables")
	}
	var order []uint16
	for name, data := range s.Vars {
		switch name {
		case BootOrderVar:
			ids, err := efi.ParseBootOrder(data)
			if err != nil {
				return nil, fmt.Errorf("backup BootOrder: %w", err)
			}
			order = ids
		case BootNextVar, TimeoutVar:
			if _, err := efi.Uint16Value(data); err != nil {
				return nil, fmt.Errorf("backup %s: %w", name, err)
			}
		default:
			if _, ok := ParseVarName(name); !ok {
				return nil, fmt.Errorf("backup contains unexpected variable %q", name)
			}
			if len(data) == 0 {
				return nil, fmt.Errorf("backup %s is empty", name)
			}
		}
	}
	for _, id := range order {
		if _, ok := s.Vars[VarName(id)]; !ok {
			return nil, fmt.Errorf("backup BootOrder refers to %s, which the backup does not contain", VarName(id))
		}
	}
	return &s, nil
}

// LoadSnapshot makes the store match a backup: its entries, order, BootNext
// and Timeout. Entries the backup does not contain are deleted.
func (m *Manager) LoadSnapshot(s *Snapshot) error {
	cur, err := m.State()
	if err != nil {
		return err
	}
	for name, data := range s.Vars {
		if _, ok := ParseVarName(name); ok {
			if err := m.Store.Set(name, data); err != nil {
				return err
			}
		}
	}
	if err := m.Store.Set(BootOrderVar, s.Vars[BootOrderVar]); err != nil {
		return err
	}
	for _, list := range [][]Entry{cur.Entries, cur.Unordered} {
		for _, e := range list {
			if _, keep := s.Vars[e.Name]; !keep {
				if err := m.deleteVar(e.Name); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range []string{BootNextVar, TimeoutVar} {
		if err := m.Store.Set(name, s.Vars[name]); err != nil {
			return err
		}
	}
	return nil
}

// Risk is a way a staged configuration could stop the machine from booting.
type Risk struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Risks compares a staged configuration with the current one and reports
// dangers the change introduces. Problems that already exist are not
// reported, so repairing a broken configuration is never blocked.
func Risks(current, draft *State) []Risk {
	var risks []Risk
	if hasBootable(current) && !hasBootable(draft) {
		risks = append(risks, Risk{
			Code:    "no-bootable-entry",
			Message: "No enabled, readable entry would be left in the boot sequence. The firmware would have nothing in this list to start.",
		})
	}
	if current.BootCurrent != nil {
		if booted, ok := current.Find(*current.BootCurrent); ok && !booted.Broken {
			target := bootTarget(booted)
			if target != nil && reaches(current, target) && !reaches(draft, target) {
				risks = append(risks, Risk{
					Code: "booted-entry-lost",
					Message: fmt.Sprintf("%s (%s) is what this system started from. After this change no enabled entry in the boot sequence would point at the same loader.",
						booted.Name, booted.Description),
				})
			}
		}
	}
	return risks
}

func hasBootable(st *State) bool {
	for _, e := range st.Entries {
		if e.Active && !e.Broken {
			return true
		}
	}
	return false
}

func bootTarget(e Entry) efi.DevicePath {
	opt, err := efi.ParseLoadOption(e.Raw)
	if err != nil {
		return nil
	}
	return opt.DevicePath
}

// reaches reports whether an enabled entry in the boot order points at target.
func reaches(st *State, target efi.DevicePath) bool {
	for _, e := range st.Entries {
		if e.Active && !e.Broken && bytes.Equal(bootTarget(e), target) {
			return true
		}
	}
	return false
}
