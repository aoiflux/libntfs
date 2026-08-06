package libntfs

import (
	"fmt"
	"strings"
	"time"
)

// Extended attribute names the Windows Subsystem for Linux writes.
//
// WSL2 stores everything in one LXATTRB structure. WSL1 predates it and stores
// the same fields as separate attributes. Both are matched case-insensitively,
// and with or without the leading '$', since the two generations and the tools
// that read them are not consistent about it.
const (
	EANameLXATTRB = "$LXATTRB" // WSL2: mode, owner, device, and POSIX timestamps
	EANameLXXATTR = "$LXXATTR" // WSL2: user extended attributes
	EANameLXUID   = "$LXUID"   // WSL1: owner
	EANameLXGID   = "$LXGID"   // WSL1: group
	EANameLXMOD   = "$LXMOD"   // WSL1: mode
	EANameLXDEV   = "$LXDEV"   // WSL1: device number
)

// linuxAttributesSize is the LXATTRB structure's length.
const linuxAttributesSize = 0x38

// POSIX file type bits, the top of a mode word.
const (
	linuxModeFormatMask = 0o170000
	linuxModeSocket     = 0o140000
	linuxModeSymlink    = 0o120000
	linuxModeRegular    = 0o100000
	linuxModeBlock      = 0o060000
	linuxModeDirectory  = 0o040000
	linuxModeChar       = 0o020000
	linuxModeFIFO       = 0o010000

	linuxModeSetUID = 0o4000
	linuxModeSetGID = 0o2000
	linuxModeSticky = 0o1000
)

// LinuxAttributes is the POSIX metadata the Windows Subsystem for Linux stores
// in a file's extended attributes.
//
// This is metadata no Windows tool surfaces and no NTFS structure otherwise
// records: the owner, group, and permission bits a file has when seen from
// inside WSL, and the POSIX timestamps, which are tracked separately from the
// NTFS ones in $STANDARD_INFORMATION and can disagree with them.
//
// Version is 1 for the layout libntfs decodes. A different version is returned
// rather than rejected, since the fields before it still parsed, but its later
// fields should not be trusted.
type LinuxAttributes struct {
	Flags    uint16
	Version  uint16
	Mode     uint32 // POSIX mode, file type bits included
	UID      uint32
	GID      uint32
	DeviceID uint32

	AccessTime time.Time
	ModifyTime time.Time
	ChangeTime time.Time

	// FromLegacyAttributes reports that these fields were assembled from WSL1's
	// separate $LXUID, $LXGID, $LXMOD, and $LXDEV attributes rather than read
	// from one LXATTRB. WSL1 records no POSIX timestamps, so those are zero.
	FromLegacyAttributes bool
}

// Permissions returns the mode's permission bits, with the file type stripped.
func (a *LinuxAttributes) Permissions() uint32 {
	if a == nil {
		return 0
	}
	return a.Mode & 0o7777
}

// FileType returns the POSIX file type as a name, or "unknown" when the mode
// carries no type the standard defines.
func (a *LinuxAttributes) FileType() string {
	if a == nil {
		return ""
	}

	switch a.Mode & linuxModeFormatMask {
	case linuxModeSocket:
		return "socket"
	case linuxModeSymlink:
		return "symlink"
	case linuxModeRegular:
		return "regular"
	case linuxModeBlock:
		return "block"
	case linuxModeDirectory:
		return "directory"
	case linuxModeChar:
		return "character"
	case linuxModeFIFO:
		return "fifo"
	default:
		return "unknown"
	}
}

// IsSymlink reports whether the mode marks a POSIX symbolic link. A WSL symlink
// is stored as a reparse point too, so this and (*File).ReparsePoint agree.
func (a *LinuxAttributes) IsSymlink() bool {
	return a != nil && a.Mode&linuxModeFormatMask == linuxModeSymlink
}

// ModeString renders the mode the way ls does, as in "-rwxr-xr-x".
func (a *LinuxAttributes) ModeString() string {
	if a == nil {
		return ""
	}

	var out strings.Builder
	out.WriteByte(linuxTypeChar(a.Mode))

	// Owner, group, and other, each with its special bit folded into the execute
	// position the way ls shows it.
	specials := [3]uint32{linuxModeSetUID, linuxModeSetGID, linuxModeSticky}
	specialChars := [3][2]byte{{'S', 's'}, {'S', 's'}, {'T', 't'}}

	for group := 0; group < 3; group++ {
		shift := uint(6 - group*3)
		bits := (a.Mode >> shift) & 0o7

		out.WriteByte(permChar(bits&0o4 != 0, 'r'))
		out.WriteByte(permChar(bits&0o2 != 0, 'w'))

		switch {
		case a.Mode&specials[group] != 0 && bits&0o1 != 0:
			out.WriteByte(specialChars[group][1])
		case a.Mode&specials[group] != 0:
			out.WriteByte(specialChars[group][0])
		default:
			out.WriteByte(permChar(bits&0o1 != 0, 'x'))
		}
	}

	return out.String()
}

// String renders the attributes as mode, owner, and group.
func (a *LinuxAttributes) String() string {
	if a == nil {
		return ""
	}
	return fmt.Sprintf("%s %d:%d", a.ModeString(), a.UID, a.GID)
}

func linuxTypeChar(mode uint32) byte {
	switch mode & linuxModeFormatMask {
	case linuxModeSocket:
		return 's'
	case linuxModeSymlink:
		return 'l'
	case linuxModeBlock:
		return 'b'
	case linuxModeDirectory:
		return 'd'
	case linuxModeChar:
		return 'c'
	case linuxModeFIFO:
		return 'p'
	default:
		return '-'
	}
}

func permChar(set bool, char byte) byte {
	if set {
		return char
	}
	return '-'
}

// ParseLinuxAttributes decodes an LXATTRB extended attribute value.
//
// It needs no Volume, so it works on extended attribute bytes carved out of a
// standalone $MFT.
func ParseLinuxAttributes(buf []byte) (*LinuxAttributes, error) {
	if len(buf) < linuxAttributesSize {
		return nil, fmt.Errorf("%w: LXATTRB is %d bytes, need %d",
			ErrInvalidExtendedAttribute, len(buf), linuxAttributesSize)
	}

	return &LinuxAttributes{
		Flags:      ReadUint16LE(buf, 0x00),
		Version:    ReadUint16LE(buf, 0x02),
		Mode:       ReadUint32LE(buf, 0x04),
		UID:        ReadUint32LE(buf, 0x08),
		GID:        ReadUint32LE(buf, 0x0C),
		DeviceID:   ReadUint32LE(buf, 0x10),
		AccessTime: unixTime(ReadUint64LE(buf, 0x20), ReadUint32LE(buf, 0x14)),
		ModifyTime: unixTime(ReadUint64LE(buf, 0x28), ReadUint32LE(buf, 0x18)),
		ChangeTime: unixTime(ReadUint64LE(buf, 0x30), ReadUint32LE(buf, 0x1C)),
	}, nil
}

// unixTime converts POSIX seconds and nanoseconds to a time, leaving a zero
// timestamp as the zero time rather than 1970.
func unixTime(seconds uint64, nanoseconds uint32) time.Time {
	if seconds == 0 && nanoseconds == 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), int64(nanoseconds)).UTC()
}

// LinuxAttributes returns the POSIX metadata the Windows Subsystem for Linux
// stored on this file, or nil when it carries none.
//
// WSL2's single LXATTRB attribute is preferred. When it is absent, WSL1's
// separate $LXUID, $LXGID, $LXMOD, and $LXDEV attributes are assembled into the
// same shape, with FromLegacyAttributes set so a caller can tell which it got.
func (f *File) LinuxAttributes() (*LinuxAttributes, error) {
	eas, err := f.ExtendedAttributes()
	if err != nil {
		return nil, err
	}

	if len(eas) == 0 {
		return nil, nil
	}

	for _, ea := range eas {
		if !matchesEAName(ea.Name, EANameLXATTRB) {
			continue
		}

		attrs, err := ParseLinuxAttributes(ea.Value)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", f.entryNum, err)
		}

		return attrs, nil
	}

	return legacyLinuxAttributes(eas), nil
}

// legacyLinuxAttributes assembles WSL1's separate attributes into the shape
// WSL2 writes as one. It returns nil when none of them are present, so a file
// with unrelated extended attributes is not reported as having POSIX metadata.
func legacyLinuxAttributes(eas []ExtendedAttribute) *LinuxAttributes {
	attrs := &LinuxAttributes{FromLegacyAttributes: true}
	found := false

	for _, ea := range eas {
		value, ok := eaUint32(ea.Value)
		if !ok {
			continue
		}

		switch {
		case matchesEAName(ea.Name, EANameLXUID):
			attrs.UID = value
		case matchesEAName(ea.Name, EANameLXGID):
			attrs.GID = value
		case matchesEAName(ea.Name, EANameLXMOD):
			attrs.Mode = value
		case matchesEAName(ea.Name, EANameLXDEV):
			attrs.DeviceID = value
		default:
			continue
		}

		found = true
	}

	if !found {
		return nil
	}

	return attrs
}

// matchesEAName compares an extended attribute name against a known one,
// ignoring case and a leading '$'. WSL's two generations and the tools that read
// them disagree about the prefix, so matching on it would miss real metadata.
func matchesEAName(name, want string) bool {
	return strings.EqualFold(strings.TrimPrefix(name, "$"), strings.TrimPrefix(want, "$"))
}

// eaUint32 reads WSL1's little-endian value, which is four bytes.
func eaUint32(value []byte) (uint32, bool) {
	if len(value) < 4 {
		return 0, false
	}
	return ReadUint32LE(value, 0), true
}
