package libntfs

import "fmt"

const (
	// reparseHeaderSize is the fixed header every reparse buffer starts with:
	// tag, data length, reserved.
	reparseHeaderSize = 8

	// reparseGUIDSize is the owner GUID that follows the header on third-party
	// reparse points. Microsoft-owned tags omit it.
	reparseGUIDSize = 16

	// reparseTagMicrosoft marks a tag owned by Microsoft, whose buffer carries no
	// owner GUID.
	reparseTagMicrosoft = 0x80000000

	// maxReparseValueBytes bounds how much of a $REPARSE_POINT is read. Windows
	// caps a reparse buffer at 16 KiB; the margin absorbs the header and any
	// unusual writer without letting a corrupt size drive the allocation.
	maxReparseValueBytes = 64 << 10

	// symlinkFlagRelative marks a symbolic link whose target is relative.
	symlinkFlagRelative = 0x00000001

	// lxSymlinkHeaderSize is the version field preceding a WSL symlink's UTF-8
	// target path.
	lxSymlinkHeaderSize = 4
)

// reparseTagNames maps the reparse tags libntfs recognises to readable names.
var reparseTagNames = map[uint32]string{
	ReparsePointSymlink:    "SYMLINK",
	ReparsePointMountPoint: "MOUNT_POINT",
	ReparsePointHSM:        "HSM",
	ReparsePointSIS:        "SIS",
	ReparsePointDFS:        "DFS",
	ReparsePointDedup:      "DEDUP",
	ReparsePointNFS:        "NFS",
	ReparsePointWOF:        "WOF",
	ReparsePointWCI:        "WCI",
	ReparsePointAppExec:    "APPEXECLINK",
	ReparsePointCloud:      "CLOUD",
	ReparsePointLXSymlink:  "LX_SYMLINK",
	ReparsePointAFUnix:     "AF_UNIX",
	ReparsePointLXFIFO:     "LX_FIFO",
	ReparsePointLXChr:      "LX_CHR",
	ReparsePointLXBlk:      "LX_BLK",
}

// ParseReparsePoint decodes a $REPARSE_POINT value.
//
// Both buffer shapes are handled: Microsoft-owned tags, whose data follows the
// header directly, and third-party tags, which carry a 16-byte owner GUID first.
// Data holds the tag-specific bytes with the header and any GUID stripped, so a
// caller reading it needs no knowledge of which shape it came from.
//
// It needs no Volume, so it works on attribute bytes carved out of a standalone
// $MFT.
func ParseReparsePoint(buf []byte) (*ReparsePoint, error) {
	if len(buf) < reparseHeaderSize {
		return nil, fmt.Errorf("%w: buffer is %d bytes, need %d for a header",
			ErrInvalidReparsePoint, len(buf), reparseHeaderSize)
	}

	reparse := &ReparsePoint{
		ReparseType: ReadUint32LE(buf, 0),
		DataLength:  ReadUint16LE(buf, 4),
		Reserved:    ReadUint16LE(buf, 6),
	}

	dataStart := reparseHeaderSize
	if !reparse.IsMicrosoft() {
		if len(buf) < reparseHeaderSize+reparseGUIDSize {
			return nil, fmt.Errorf(
				"%w: third-party tag 0x%08X needs a %d byte GUID, only %d bytes remain",
				ErrInvalidReparsePoint, reparse.ReparseType, reparseGUIDSize,
				len(buf)-reparseHeaderSize)
		}
		copy(reparse.GUID[:], buf[reparseHeaderSize:reparseHeaderSize+reparseGUIDSize])
		dataStart += reparseGUIDSize
	}

	// Trust the recorded length only as far as the buffer allows. A short
	// attribute with an overlong DataLength still yields the bytes it has.
	dataEnd := dataStart + int(reparse.DataLength)
	if dataEnd > len(buf) {
		dataEnd = len(buf)
	}

	reparse.Data = make([]byte, dataEnd-dataStart)
	copy(reparse.Data, buf[dataStart:dataEnd])

	return reparse, nil
}

// IsMicrosoft reports whether the reparse tag is Microsoft-owned. Microsoft tags
// carry no owner GUID; every third-party tag does.
func (r *ReparsePoint) IsMicrosoft() bool {
	return r != nil && r.ReparseType&reparseTagMicrosoft != 0
}

// TagName returns a readable name for the reparse tag, or its hex value when the
// tag is not one libntfs recognises.
func (r *ReparsePoint) TagName() string {
	if r == nil {
		return ""
	}
	if name, ok := reparseTagNames[r.ReparseType]; ok {
		return name
	}
	return fmt.Sprintf("0x%08X", r.ReparseType)
}

// IsRelative reports whether a symbolic link's target is relative to the link's
// own directory. It is meaningful only for ReparsePointSymlink and reports false
// for every other tag.
func (r *ReparsePoint) IsRelative() bool {
	if r == nil || r.ReparseType != ReparsePointSymlink || len(r.Data) < 12 {
		return false
	}
	return ReadUint32LE(r.Data, 8)&symlinkFlagRelative != 0
}

// Target returns the path a reparse point redirects to.
//
// substituteName is the target as the object manager resolves it, typically
// prefixed \??\ for an absolute path. printName is the form Windows shows a
// user, which is empty on many links. ok is false for tags that do not name a
// target — deduplication, cloud placeholders, WOF-compressed files, and any tag
// libntfs does not recognise — leaving Data for the caller to interpret.
//
// Symbolic links, mount points (junctions), and WSL symlinks are decoded.
func (r *ReparsePoint) Target() (substituteName, printName string, ok bool) {
	if r == nil {
		return "", "", false
	}

	switch r.ReparseType {
	case ReparsePointSymlink:
		return r.parseNameBuffer(12)
	case ReparsePointMountPoint:
		return r.parseNameBuffer(8)
	case ReparsePointLXSymlink:
		// A WSL symlink stores a version word followed by a UTF-8 path, not the
		// UTF-16 name buffer every other link tag uses.
		if len(r.Data) <= lxSymlinkHeaderSize {
			return "", "", false
		}
		target := string(r.Data[lxSymlinkHeaderSize:])
		return target, target, true
	default:
		return "", "", false
	}
}

// parseNameBuffer decodes the substitute and print names shared by the symlink
// and mount point layouts. pathOffset is where the UTF-16 path buffer begins,
// which differs because a symlink header carries a Flags word that a mount point
// does not. The recorded name offsets are relative to that path buffer.
func (r *ReparsePoint) parseNameBuffer(pathOffset int) (substituteName, printName string, ok bool) {
	if len(r.Data) < pathOffset {
		return "", "", false
	}

	substituteOffset := int(ReadUint16LE(r.Data, 0))
	substituteLength := int(ReadUint16LE(r.Data, 2))
	printOffset := int(ReadUint16LE(r.Data, 4))
	printLength := int(ReadUint16LE(r.Data, 6))

	pathBuffer := r.Data[pathOffset:]

	substituteName, subOK := utf16SliceString(pathBuffer, substituteOffset, substituteLength)
	printName, _ = utf16SliceString(pathBuffer, printOffset, printLength)

	// A print name is routinely absent, but a link with no substitute name names
	// no target at all.
	return substituteName, printName, subOK
}

// utf16SliceString decodes length bytes of UTF-16LE at offset within buf.
func utf16SliceString(buf []byte, offset, length int) (string, bool) {
	if length == 0 {
		return "", false
	}
	if offset < 0 || length < 0 || offset+length > len(buf) {
		return "", false
	}

	reader := NewBinaryReader(buf[offset : offset+length])
	value, err := reader.ReadUTF16String(length / 2)
	if err != nil {
		return "", false
	}

	return value, true
}

// ReparsePoint returns the file's reparse point, or nil when it has none.
//
// A directory carrying a reparse point is a junction or a symlink to a
// directory; libntfs reports it as a directory and does not follow the link, so
// ReadDir lists the reparse point's own (usually empty) index rather than the
// target's.
func (f *File) ReparsePoint() (*ReparsePoint, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	attr := f.entry.FindAttribute(AttrTypeReparsePoint, "")
	if attr == nil {
		return nil, nil
	}

	value, err := f.volume.readAttributeValue(attr, maxReparseValueBytes)
	if err != nil {
		return nil, fmt.Errorf("read $REPARSE_POINT on entry %d: %w", f.entryNum, err)
	}

	return ParseReparsePoint(value)
}

// IsReparsePoint reports whether the file carries a $REPARSE_POINT attribute.
func (f *File) IsReparsePoint() bool {
	return f.entry.FindAttribute(AttrTypeReparsePoint, "") != nil
}
