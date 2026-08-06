package libntfs

import "fmt"

const (
	// eaEntryHeaderSize is the fixed part of one $EA entry, before its name.
	eaEntryHeaderSize = 8

	// eaInfoSize is the size of a $EA_INFORMATION value.
	eaInfoSize = 8

	// maxEAValueBytes bounds how much of a non-resident $EA is read. NTFS records
	// the packed size in a uint16, so a well-formed $EA cannot exceed 64 KiB;
	// the margin here absorbs unusual writers without letting a corrupt size
	// drive the allocation.
	maxEAValueBytes = 256 << 10
)

// EAFlagNeedEA marks an extended attribute that a reader must understand to
// interpret the file correctly. NTFS refuses to open such a file to callers that
// do not support extended attributes.
const EAFlagNeedEA = 0x80

// EAInfo is the $EA_INFORMATION attribute, the index NTFS keeps alongside $EA.
//
// It is what a directory index carries for a file with extended attributes, so
// a non-zero PackedSize in an index entry is a reliable signal that $EA exists
// without reading the file's own record.
type EAInfo struct {
	PackedSize   uint16 // Size of the packed $EA value in bytes
	NeedEACount  uint16 // Number of entries carrying EAFlagNeedEA
	UnpackedSize uint32 // Size the entries occupy unpacked
}

// ExtendedAttribute is one entry from a file's $EA attribute.
//
// Names are stored as ASCII and are conventionally uppercase, though NTFS itself
// does not enforce a case. Values are opaque bytes whose meaning belongs to
// whoever wrote them.
//
// The Windows Subsystem for Linux stores POSIX metadata here, which is the main
// forensic use for extended attributes on a modern volume: LXATTRB holds mode,
// owner, and the POSIX timestamps; LXXATTR holds user extended attributes; and
// $LXUID, $LXGID, and $LXMOD carry those fields for files created by WSL1.
type ExtendedAttribute struct {
	Name  string // ASCII name as stored
	Value []byte // Opaque value bytes
	Flags uint8  // Entry flags; see EAFlagNeedEA
}

// NeedEA reports whether a reader must understand this entry to interpret the
// file correctly.
func (e ExtendedAttribute) NeedEA() bool {
	return e.Flags&EAFlagNeedEA != 0
}

// String renders the entry as its name and value length.
func (e ExtendedAttribute) String() string {
	if e.NeedEA() {
		return fmt.Sprintf("%s (%d bytes, need-EA)", e.Name, len(e.Value))
	}
	return fmt.Sprintf("%s (%d bytes)", e.Name, len(e.Value))
}

// ParseEAInformation decodes a $EA_INFORMATION value.
//
// It needs no Volume, so it works on attribute bytes carved out of a standalone
// $MFT as readily as on a mounted image.
func ParseEAInformation(buf []byte) (*EAInfo, error) {
	if len(buf) < eaInfoSize {
		return nil, fmt.Errorf("%w: $EA_INFORMATION is %d bytes, need %d",
			ErrInvalidExtendedAttribute, len(buf), eaInfoSize)
	}

	return &EAInfo{
		PackedSize:   ReadUint16LE(buf, 0),
		NeedEACount:  ReadUint16LE(buf, 2),
		UnpackedSize: ReadUint32LE(buf, 4),
	}, nil
}

// ParseExtendedAttributes decodes every entry in a $EA value.
//
// Entries that parse are returned even when a later one is malformed, so a
// damaged $EA still yields its readable prefix alongside the error. Callers that
// want all-or-nothing should discard the slice when err is non-nil.
//
// It needs no Volume, so it works on attribute bytes extracted from a standalone
// $MFT.
func ParseExtendedAttributes(buf []byte) ([]ExtendedAttribute, error) {
	var attrs []ExtendedAttribute

	err := EachExtendedAttribute(buf, func(ea ExtendedAttribute) error {
		attrs = append(attrs, ea)
		return nil
	})

	return attrs, err
}

// EachExtendedAttribute walks a $EA value, calling fn for each entry.
//
// Walking stops and the error is returned if fn returns one, so a caller can
// stop early once it has found the entry it wants.
func EachExtendedAttribute(buf []byte, fn func(ExtendedAttribute) error) error {
	offset := 0

	for offset < len(buf) {
		if len(buf)-offset < eaEntryHeaderSize {
			return fmt.Errorf("%w: entry at %d is %d bytes, need %d for a header",
				ErrInvalidExtendedAttribute, offset, len(buf)-offset, eaEntryHeaderSize)
		}

		nextEntryOffset := ReadUint32LE(buf, offset)
		flags := buf[offset+4]
		nameLength := int(buf[offset+5])
		valueLength := int(ReadUint16LE(buf, offset+6))

		// A zeroed header is padding after the last entry, not an entry with an
		// empty name. NTFS pads $EA out to an allocation boundary.
		if nextEntryOffset == 0 && flags == 0 && nameLength == 0 && valueLength == 0 {
			return nil
		}

		// Name is NUL-terminated, so the value starts one byte past its end.
		nameStart := offset + eaEntryHeaderSize
		nameEnd := nameStart + nameLength
		valueStart := nameEnd + 1
		valueEnd := valueStart + valueLength

		if valueEnd > len(buf) {
			return fmt.Errorf("%w: entry at %d needs %d bytes, only %d remain",
				ErrInvalidExtendedAttribute, offset, valueEnd-offset, len(buf)-offset)
		}

		value := make([]byte, valueLength)
		copy(value, buf[valueStart:valueEnd])

		if err := fn(ExtendedAttribute{
			Name:  string(buf[nameStart:nameEnd]),
			Value: value,
			Flags: flags,
		}); err != nil {
			return err
		}

		// Zero marks the last entry.
		if nextEntryOffset == 0 {
			return nil
		}

		// An offset that does not clear this entry would make the next entry
		// overlap it, and one that does not advance at all would spin forever.
		entrySize := valueEnd - offset
		if int(nextEntryOffset) < entrySize {
			return fmt.Errorf("%w: entry at %d advances %d bytes over a %d byte entry",
				ErrInvalidExtendedAttribute, offset, nextEntryOffset, entrySize)
		}

		next := offset + int(nextEntryOffset)
		if next > len(buf) {
			// The last entry points past the end of the value rather than
			// carrying a zero. Nothing follows, so this is a clean end.
			return nil
		}

		offset = next
	}

	return nil
}

// ExtendedAttributes returns the file's extended attributes.
//
// A file with no $EA attribute returns no entries and no error, since having no
// extended attributes is the normal case rather than a failure.
//
// A non-resident $EA is read through its data runs, which is how the Windows
// Subsystem for Linux's metadata is usually stored.
func (f *File) ExtendedAttributes() ([]ExtendedAttribute, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	attr := f.entry.FindAttribute(AttrTypeEA, "")
	if attr == nil {
		return nil, nil
	}

	value, err := f.volume.readAttributeValue(attr, maxEAValueBytes)
	if err != nil {
		return nil, fmt.Errorf("read $EA on entry %d: %w", f.entryNum, err)
	}

	return ParseExtendedAttributes(value)
}

// EAInformation returns the file's $EA_INFORMATION, or nil when the file has no
// extended attributes.
func (f *File) EAInformation() (*EAInfo, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	attr := f.entry.FindAttribute(AttrTypeEAInfo, "")
	if attr == nil {
		return nil, nil
	}

	value, err := f.volume.readAttributeValue(attr, eaInfoSize)
	if err != nil {
		return nil, fmt.Errorf("read $EA_INFORMATION on entry %d: %w", f.entryNum, err)
	}

	return ParseEAInformation(value)
}

// HasExtendedAttributes reports whether the file carries a $EA attribute.
func (f *File) HasExtendedAttributes() bool {
	return f.entry.FindAttribute(AttrTypeEA, "") != nil
}
