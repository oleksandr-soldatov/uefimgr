package efi

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Device path node types and subtypes used by boot entries.
const (
	nodeTypeHardware   = 0x01
	nodeTypeACPI       = 0x02
	nodeTypeMessaging  = 0x03
	nodeTypeMedia      = 0x04
	nodeTypeBBS        = 0x05
	nodeTypeEnd        = 0x7f
	mediaSubtypeHD     = 0x01
	mediaSubtypeCDROM  = 0x02
	mediaSubtypeFile   = 0x04
	endSubtypeEntire   = 0xff
	partitionFormatGPT = 0x02
	partitionFormatMBR = 0x01
	signatureTypeGUID  = 0x02
	signatureTypeMBR   = 0x01

	// maxFilePathChars keeps a FilePath() node well inside the 16-bit node
	// length field.
	maxFilePathChars = 1024
)

// DevicePath is one raw EFI device path. It ends with an End-entire node; a
// multi-instance path may contain End-instance nodes before that.
type DevicePath []byte

type Node struct {
	Type    byte
	Subtype byte
	Data    []byte
}

// Bytes encodes the node including its 4-byte header.
func (n Node) Bytes() []byte {
	b := make([]byte, 4+len(n.Data))
	b[0], b[1] = n.Type, n.Subtype
	binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
	copy(b[4:], n.Data)
	return b
}

func (n Node) isFile() bool { return n.Type == nodeTypeMedia && n.Subtype == mediaSubtypeFile }
func (n Node) isEnd() bool  { return n.Type == nodeTypeEnd }

// scanPath walks one device path starting at b[0] and returns its nodes and
// the number of bytes it occupies, including the End-entire node.
func scanPath(b []byte) ([]Node, int, error) {
	var nodes []Node
	for i := 0; i < len(b); {
		if len(b)-i < 4 {
			return nil, 0, fmt.Errorf("truncated device path node header at offset %d", i)
		}
		typ, sub := b[i], b[i+1]
		n := int(binary.LittleEndian.Uint16(b[i+2 : i+4]))
		if n < 4 {
			return nil, 0, fmt.Errorf("device path node at offset %d has invalid length %d", i, n)
		}
		if i+n > len(b) {
			return nil, 0, fmt.Errorf("device path node at offset %d overruns buffer", i)
		}
		nodes = append(nodes, Node{Type: typ, Subtype: sub, Data: append([]byte(nil), b[i+4:i+n]...)})
		i += n
		if typ == nodeTypeEnd && sub == endSubtypeEntire {
			return nodes, i, nil
		}
	}
	return nil, 0, fmt.Errorf("device path has no End node")
}

// ParseNodes splits a single device path into nodes.
func (p DevicePath) ParseNodes() ([]Node, error) {
	nodes, used, err := scanPath(p)
	if err != nil {
		return nil, err
	}
	if used != len(p) {
		return nil, fmt.Errorf("data after End node")
	}
	return nodes, nil
}

// SplitDevicePaths splits a load option's FilePathList into its device paths.
// The first one is the boot target; the rest are loader-specific.
func SplitDevicePaths(list []byte) ([]DevicePath, error) {
	var paths []DevicePath
	for off := 0; off < len(list); {
		_, used, err := scanPath(list[off:])
		if err != nil {
			return nil, err
		}
		paths = append(paths, append(DevicePath(nil), list[off:off+used]...))
		off += used
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("empty device path")
	}
	return paths, nil
}

// Validate checks that p is one well-formed device path ending in an End node.
func (p DevicePath) Validate() error {
	if len(p) == 0 {
		return fmt.Errorf("empty device path")
	}
	_, err := p.ParseNodes()
	return err
}

// FilePath returns the file the path points at, joining consecutive
// FilePath() nodes the way firmware does, or "" if there is none.
func (p DevicePath) FilePath() string {
	nodes, err := p.ParseNodes()
	if err != nil {
		return ""
	}
	out := ""
	for _, n := range nodes {
		if !n.isFile() {
			continue
		}
		s, err := decodeUTF16Z(n.Data)
		if err != nil || s == "" {
			continue
		}
		switch {
		case out == "":
			out = s
		case strings.HasSuffix(out, `\`) || strings.HasPrefix(s, `\`):
			out = strings.TrimSuffix(out, `\`) + `\` + strings.TrimPrefix(s, `\`)
		default:
			out += `\` + s
		}
	}
	return out
}

// HasFilePath reports whether the path contains a FilePath() node.
func (p DevicePath) HasFilePath() bool {
	nodes, err := p.ParseNodes()
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.isFile() {
			return true
		}
	}
	return false
}

// CanReplaceFilePath reports why WithFilePath would refuse p, or nil if it
// would succeed for any valid file path.
func (p DevicePath) CanReplaceFilePath() error {
	_, _, err := p.fileNodes()
	return err
}

// fileNodes returns p's nodes and the index of its first FilePath() node,
// provided everything from there on is FilePath() or End nodes.
func (p DevicePath) fileNodes() ([]Node, int, error) {
	nodes, err := p.ParseNodes()
	if err != nil {
		return nil, 0, err
	}
	first := -1
	for i, n := range nodes {
		if n.isFile() {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, 0, fmt.Errorf("device path has no FilePath() node to change")
	}
	for _, n := range nodes[first:] {
		if !n.isFile() && !n.isEnd() {
			return nil, 0, fmt.Errorf("device path has nodes after its file path; it cannot be edited safely")
		}
	}
	return nodes, first, nil
}

// WithFilePath returns a copy of p whose FilePath() node(s) are replaced by a
// single node for path. Everything before the first FilePath() node, such as
// the HD() partition node, is kept byte for byte.
func (p DevicePath) WithFilePath(path string) (DevicePath, error) {
	path, err := NormalizeFilePath(path)
	if err != nil {
		return nil, err
	}
	nodes, first, err := p.fileNodes()
	if err != nil {
		return nil, err
	}
	var out DevicePath
	for _, n := range nodes[:first] {
		out = append(out, n.Bytes()...)
	}
	out = append(out, FilePathNode(path)...)
	out = append(out, EndNode()...)
	return out, nil
}

// HD returns the first HardDrive media node, if present.
func (p DevicePath) HD() (HDNode, bool) {
	nodes, err := p.ParseNodes()
	if err != nil {
		return HDNode{}, false
	}
	for _, n := range nodes {
		if n.Type == nodeTypeMedia && n.Subtype == mediaSubtypeHD {
			h, err := parseHDNode(n.Data)
			if err == nil {
				return h, true
			}
		}
	}
	return HDNode{}, false
}

var nodeNames = map[[2]byte]string{
	{nodeTypeHardware, 0x01}:  "Pci",
	{nodeTypeHardware, 0x04}:  "VenHw",
	{nodeTypeACPI, 0x01}:      "Acpi",
	{nodeTypeACPI, 0x02}:      "AcpiEx",
	{nodeTypeMessaging, 0x01}: "Ata",
	{nodeTypeMessaging, 0x02}: "Scsi",
	{nodeTypeMessaging, 0x05}: "USB",
	{nodeTypeMessaging, 0x0a}: "VenMsg",
	{nodeTypeMessaging, 0x0b}: "MAC",
	{nodeTypeMessaging, 0x0c}: "IPv4",
	{nodeTypeMessaging, 0x0d}: "IPv6",
	{nodeTypeMessaging, 0x0f}: "UsbClass",
	{nodeTypeMessaging, 0x12}: "Sata",
	{nodeTypeMessaging, 0x17}: "NVMe",
	{nodeTypeMessaging, 0x18}: "Uri",
	{nodeTypeMessaging, 0x1d}: "SD",
	{nodeTypeMessaging, 0x1f}: "eMMC",
	{nodeTypeMedia, 0x02}:     "CDROM",
	{nodeTypeMedia, 0x03}:     "VenMedia",
	{nodeTypeMedia, 0x06}:     "FvFile",
	{nodeTypeMedia, 0x07}:     "Fv",
	{nodeTypeBBS, 0x01}:       "BBS",
}

// Summary renders a compact, human-readable form such as
// HD(2,GPT,{...},0x800,0x1f000)/\EFI\Microsoft\Boot\bootmgfw.efi.
func (p DevicePath) Summary() string {
	nodes, err := p.ParseNodes()
	if err != nil {
		return fmt.Sprintf("invalid device path: %v", err)
	}
	var parts []string
	for _, n := range nodes {
		switch {
		case n.isEnd():
			if n.Subtype != endSubtypeEntire {
				parts = append(parts, ",")
			}
		case n.Type == nodeTypeMedia && n.Subtype == mediaSubtypeHD:
			if h, err := parseHDNode(n.Data); err == nil {
				parts = append(parts, h.Summary())
			} else {
				parts = append(parts, "HD(?)")
			}
		case n.isFile():
			s, _ := decodeUTF16Z(n.Data)
			parts = append(parts, s)
		default:
			if name, ok := nodeNames[[2]byte{n.Type, n.Subtype}]; ok {
				parts = append(parts, name+"(...)")
			} else {
				parts = append(parts, fmt.Sprintf("Path(%d,%d)", n.Type, n.Subtype))
			}
		}
	}
	return strings.Join(parts, "/")
}

// HDNode is a HardDrive media device path node.
type HDNode struct {
	PartitionNumber uint32
	PartitionStart  uint64 // in sectors
	PartitionSize   uint64 // in sectors
	Signature       GUID
	Format          byte // 1 = MBR, 2 = GPT
	SignatureType   byte // 1 = MBR signature, 2 = GUID
}

// GPTPartition describes a GPT partition by its number, its geometry in
// sectors and its unique partition GUID.
func GPTPartition(number uint32, start, size uint64, id GUID) HDNode {
	return HDNode{
		PartitionNumber: number,
		PartitionStart:  start,
		PartitionSize:   size,
		Signature:       id,
		Format:          partitionFormatGPT,
		SignatureType:   signatureTypeGUID,
	}
}

func (h HDNode) Summary() string {
	if h.Format == partitionFormatGPT {
		return fmt.Sprintf("HD(%d,GPT,%s,0x%x,0x%x)", h.PartitionNumber, h.Signature.String(), h.PartitionStart, h.PartitionSize)
	}
	return fmt.Sprintf("HD(%d,MBR,0x%x,0x%x,0x%x)", h.PartitionNumber,
		binary.LittleEndian.Uint32(h.Signature[:4]), h.PartitionStart, h.PartitionSize)
}

// IsGPT reports whether the node identifies a GPT partition by GUID.
func (h HDNode) IsGPT() bool {
	return h.Format == partitionFormatGPT && h.SignatureType == signatureTypeGUID
}

// Bytes encodes the node, including the 4-byte type/subtype/length header.
// The EFI specification defines a fixed 42-byte HardDrive node; for MBR the
// first four signature bytes are meaningful and the rest are zero.
func (h HDNode) Bytes() []byte {
	b := make([]byte, 42)
	b[0], b[1] = nodeTypeMedia, mediaSubtypeHD
	binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
	binary.LittleEndian.PutUint32(b[4:8], h.PartitionNumber)
	binary.LittleEndian.PutUint64(b[8:16], h.PartitionStart)
	binary.LittleEndian.PutUint64(b[16:24], h.PartitionSize)
	copy(b[24:40], h.Signature[:])
	if h.Format == 0 {
		h.Format = partitionFormatGPT
	}
	if h.SignatureType == 0 {
		if h.Format == partitionFormatMBR {
			h.SignatureType = signatureTypeMBR
		} else {
			h.SignatureType = signatureTypeGUID
		}
	}
	b[40] = h.Format
	b[41] = h.SignatureType
	return b
}

func parseHDNode(data []byte) (HDNode, error) {
	if len(data) < 38 {
		return HDNode{}, fmt.Errorf("HD node too short")
	}
	h := HDNode{
		PartitionNumber: binary.LittleEndian.Uint32(data[0:4]),
		PartitionStart:  binary.LittleEndian.Uint64(data[4:12]),
		PartitionSize:   binary.LittleEndian.Uint64(data[12:20]),
		Format:          data[36],
		SignatureType:   data[37],
	}
	copy(h.Signature[:], data[20:36])
	return h, nil
}

// NormalizeFilePath turns user input into the form stored in a FilePath()
// node: backslash separated and rooted at the partition.
func NormalizeFilePath(path string) (string, error) {
	path = strings.ReplaceAll(strings.TrimSpace(path), "/", `\`)
	if path == "" {
		return "", fmt.Errorf("EFI file path must not be empty")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("EFI file path contains a control character")
		}
	}
	if !strings.HasPrefix(path, `\`) {
		path = `\` + path
	}
	for strings.Contains(path, `\\`) {
		path = strings.ReplaceAll(path, `\\`, `\`)
	}
	if strings.HasSuffix(path, `\`) {
		return "", fmt.Errorf("EFI file path must name a file, not a directory")
	}
	if len(utf16.Encode([]rune(path))) > maxFilePathChars {
		return "", fmt.Errorf("EFI file path is longer than %d characters", maxFilePathChars)
	}
	return path, nil
}

// FilePathNode encodes a media FilePath() node: the 4-byte header followed by
// the NUL-terminated UTF-16 path. Callers pass a normalized path.
func FilePathNode(path string) []byte {
	units := utf16.Encode([]rune(path))
	b := make([]byte, 4+2*(len(units)+1))
	b[0], b[1] = nodeTypeMedia, mediaSubtypeFile
	binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[4+2*i:], u)
	}
	return b
}

// EndNode terminates a device path.
func EndNode() []byte {
	return []byte{nodeTypeEnd, endSubtypeEntire, 0x04, 0x00}
}

// NewBootDevicePath builds HD() + FilePath() + End() for a GPT partition.
func NewBootDevicePath(hd HDNode, efiPath string) (DevicePath, error) {
	if !hd.IsGPT() {
		return nil, fmt.Errorf("only GPT partitions are supported")
	}
	if hd.Signature.IsZero() {
		return nil, fmt.Errorf("partition GUID is zero")
	}
	efiPath, err := NormalizeFilePath(efiPath)
	if err != nil {
		return nil, err
	}
	var p DevicePath
	p = append(p, hd.Bytes()...)
	p = append(p, FilePathNode(efiPath)...)
	p = append(p, EndNode()...)
	return p, nil
}

func decodeUTF16Z(b []byte) (string, error) {
	var units []uint16
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i : i+2])
		if u == 0 {
			return string(utf16.Decode(units)), nil
		}
		units = append(units, u)
	}
	return "", fmt.Errorf("UTF-16 string is not NUL terminated")
}

func encodeUTF16Z(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*(len(units)+1))
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[2*i:], u)
	}
	return out
}
