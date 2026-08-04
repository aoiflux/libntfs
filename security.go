package libntfs

import (
	"fmt"
	"strconv"
	"strings"
)

// Security descriptor structure sizes.
const (
	SecurityDescriptorHeaderSize = 20 // Self-relative descriptor header
	SIDHeaderSize                = 8  // Revision, sub-authority count, authority
	ACLHeaderSize                = 8  // Revision, size, ACE count
	ACEHeaderSize                = 8  // Type, flags, size, access mask
	SDSEntryHeaderSize           = 20 // $SDS per-entry header
)

// Security descriptor control flags.
const (
	SEOwnerDefaulted    = 0x0001
	SEGroupDefaulted    = 0x0002
	SEDACLPresent       = 0x0004
	SEDACLDefaulted     = 0x0008
	SESACLPresent       = 0x0010
	SESACLDefaulted     = 0x0020
	SEDACLAutoInherited = 0x0400
	SESACLAutoInherited = 0x0800
	SEDACLProtected     = 0x1000
	SESACLProtected     = 0x2000
	SESelfRelative      = 0x8000
)

// ACE types.
const (
	ACETypeAccessAllowed       = 0x00
	ACETypeAccessDenied        = 0x01
	ACETypeSystemAudit         = 0x02
	ACETypeSystemAlarm         = 0x03
	ACETypeAccessAllowedObject = 0x05
	ACETypeAccessDeniedObject  = 0x06
	ACETypeSystemAuditObject   = 0x07
)

// ACE inheritance and audit flags.
const (
	ACEFlagObjectInherit      = 0x01
	ACEFlagContainerInherit   = 0x02
	ACEFlagNoPropagateInherit = 0x04
	ACEFlagInheritOnly        = 0x08
	ACEFlagInherited          = 0x10
	ACEFlagSuccessfulAccess   = 0x40
	ACEFlagFailedAccess       = 0x80
)

// File access rights carried in an ACE access mask.
const (
	AccessReadData        = 0x00000001
	AccessWriteData       = 0x00000002
	AccessAppendData      = 0x00000004
	AccessReadEA          = 0x00000008
	AccessWriteEA         = 0x00000010
	AccessExecute         = 0x00000020
	AccessDeleteChild     = 0x00000040
	AccessReadAttributes  = 0x00000080
	AccessWriteAttributes = 0x00000100
	AccessDelete          = 0x00010000
	AccessReadControl     = 0x00020000
	AccessWriteDAC        = 0x00040000
	AccessWriteOwner      = 0x00080000
	AccessSynchronize     = 0x00100000
	AccessGenericAll      = 0x10000000
	AccessGenericExecute  = 0x20000000
	AccessGenericWrite    = 0x40000000
	AccessGenericRead     = 0x80000000
)

// aceTypeNames maps ACE type codes to readable names.
var aceTypeNames = map[uint8]string{
	ACETypeAccessAllowed:       "ACCESS_ALLOWED",
	ACETypeAccessDenied:        "ACCESS_DENIED",
	ACETypeSystemAudit:         "SYSTEM_AUDIT",
	ACETypeSystemAlarm:         "SYSTEM_ALARM",
	ACETypeAccessAllowedObject: "ACCESS_ALLOWED_OBJECT",
	ACETypeAccessDeniedObject:  "ACCESS_DENIED_OBJECT",
	ACETypeSystemAuditObject:   "SYSTEM_AUDIT_OBJECT",
}

// accessRightNames pairs each access mask bit with its name, ordered so output
// reads from specific file rights to broad generic rights.
var accessRightNames = []struct {
	mask uint32
	name string
}{
	{AccessReadData, "ReadData"},
	{AccessWriteData, "WriteData"},
	{AccessAppendData, "AppendData"},
	{AccessReadEA, "ReadEA"},
	{AccessWriteEA, "WriteEA"},
	{AccessExecute, "Execute"},
	{AccessDeleteChild, "DeleteChild"},
	{AccessReadAttributes, "ReadAttributes"},
	{AccessWriteAttributes, "WriteAttributes"},
	{AccessDelete, "Delete"},
	{AccessReadControl, "ReadControl"},
	{AccessWriteDAC, "WriteDAC"},
	{AccessWriteOwner, "WriteOwner"},
	{AccessSynchronize, "Synchronize"},
	{AccessGenericAll, "GenericAll"},
	{AccessGenericExecute, "GenericExecute"},
	{AccessGenericWrite, "GenericWrite"},
	{AccessGenericRead, "GenericRead"},
}

// wellKnownSIDs maps common SID strings to their conventional account names.
var wellKnownSIDs = map[string]string{
	"S-1-0-0":      "NULL",
	"S-1-1-0":      "Everyone",
	"S-1-2-0":      "LOCAL",
	"S-1-3-0":      "CREATOR OWNER",
	"S-1-3-1":      "CREATOR GROUP",
	"S-1-5-7":      "ANONYMOUS LOGON",
	"S-1-5-11":     "Authenticated Users",
	"S-1-5-13":     "Terminal Server User",
	"S-1-5-18":     "NT AUTHORITY\\SYSTEM",
	"S-1-5-19":     "NT AUTHORITY\\LOCAL SERVICE",
	"S-1-5-20":     "NT AUTHORITY\\NETWORK SERVICE",
	"S-1-5-32-544": "BUILTIN\\Administrators",
	"S-1-5-32-545": "BUILTIN\\Users",
	"S-1-5-32-546": "BUILTIN\\Guests",
	"S-1-5-32-547": "BUILTIN\\Power Users",
	"S-1-5-32-551": "BUILTIN\\Backup Operators",
}

// SID is a Windows security identifier.
type SID struct {
	Revision            uint8
	IdentifierAuthority uint64 // 48-bit, stored big-endian on disk
	SubAuthorities      []uint32
}

// String renders the SID in standard S-R-I-S-S notation.
func (s *SID) String() string {
	if s == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("S-")
	b.WriteString(strconv.FormatUint(uint64(s.Revision), 10))
	b.WriteByte('-')
	b.WriteString(strconv.FormatUint(s.IdentifierAuthority, 10))

	for _, sub := range s.SubAuthorities {
		b.WriteByte('-')
		b.WriteString(strconv.FormatUint(uint64(sub), 10))
	}

	return b.String()
}

// WellKnownName returns the conventional account name for well-known SIDs such
// as S-1-5-32-544, and reports whether one was found.
func (s *SID) WellKnownName() (string, bool) {
	if s == nil {
		return "", false
	}

	name, ok := wellKnownSIDs[s.String()]
	return name, ok
}

// ACE is a single access control entry.
type ACE struct {
	Type    uint8
	Flags   uint8
	Mask    uint32
	Trustee *SID // Nil for object ACE types this package does not decode
}

// TypeName returns the readable name of the ACE type.
func (a ACE) TypeName() string {
	if name, ok := aceTypeNames[a.Type]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(0x%02X)", a.Type)
}

// RightsNames decodes the access mask into readable right names. Bits with no
// known name are reported as a trailing hex value so nothing is silently lost.
func (a ACE) RightsNames() []string {
	var (
		names     []string
		remaining = a.Mask
	)

	for _, right := range accessRightNames {
		if a.Mask&right.mask != 0 {
			names = append(names, right.name)
			remaining &^= right.mask
		}
	}

	if remaining != 0 {
		names = append(names, fmt.Sprintf("0x%08X", remaining))
	}

	return names
}

// IsInherited reports whether this ACE was inherited from a parent container.
func (a ACE) IsInherited() bool {
	return a.Flags&ACEFlagInherited != 0
}

// ACL is an access control list holding zero or more ACEs.
type ACL struct {
	Revision uint8
	ACEs     []ACE
}

// SecurityDescriptor is a parsed self-relative Windows security descriptor.
//
// A nil DACL means no DACL is present, which grants everyone full access. A
// non-nil DACL with no ACEs is the opposite: it denies everyone. Callers making
// access determinations must distinguish the two.
type SecurityDescriptor struct {
	Revision uint8
	Control  uint16
	Owner    *SID
	Group    *SID
	DACL     *ACL
	SACL     *ACL
	Raw      []byte // The descriptor bytes this was parsed from
}

// ParseSecurityDescriptor parses a self-relative security descriptor, the form
// stored in $Secure:$SDS and in $SECURITY_DESCRIPTOR attributes. It requires no
// Volume, so it can be pointed at descriptor bytes from any source.
func ParseSecurityDescriptor(buf []byte) (*SecurityDescriptor, error) {
	if len(buf) < SecurityDescriptorHeaderSize {
		return nil, fmt.Errorf("%w: descriptor is %d bytes, need at least %d",
			ErrInvalidSecurityDescriptor, len(buf), SecurityDescriptorHeaderSize)
	}

	sd := &SecurityDescriptor{
		Revision: buf[0],
		Control:  ReadUint16LE(buf, 2),
		Raw:      buf,
	}

	ownerOffset := ReadUint32LE(buf, 4)
	groupOffset := ReadUint32LE(buf, 8)
	saclOffset := ReadUint32LE(buf, 12)
	daclOffset := ReadUint32LE(buf, 16)

	var err error

	if ownerOffset != 0 {
		if sd.Owner, err = parseSIDAt(buf, ownerOffset); err != nil {
			return nil, fmt.Errorf("owner SID: %w", err)
		}
	}

	if groupOffset != 0 {
		if sd.Group, err = parseSIDAt(buf, groupOffset); err != nil {
			return nil, fmt.Errorf("group SID: %w", err)
		}
	}

	// The presence flags are authoritative: a zero offset with the flag set
	// means a present-but-empty list, which is not the same as absent.
	if sd.Control&SEDACLPresent != 0 && daclOffset != 0 {
		if sd.DACL, err = parseACLAt(buf, daclOffset); err != nil {
			return nil, fmt.Errorf("DACL: %w", err)
		}
	}

	if sd.Control&SESACLPresent != 0 && saclOffset != 0 {
		if sd.SACL, err = parseACLAt(buf, saclOffset); err != nil {
			return nil, fmt.Errorf("SACL: %w", err)
		}
	}

	return sd, nil
}

// parseSIDAt parses a SID located at the given offset within buf.
func parseSIDAt(buf []byte, offset uint32) (*SID, error) {
	if int(offset)+SIDHeaderSize > len(buf) {
		return nil, fmt.Errorf("%w: SID offset %d beyond %d bytes",
			ErrInvalidSecurityDescriptor, offset, len(buf))
	}

	data := buf[offset:]
	subCount := int(data[1])

	// A SID carries at most 15 sub-authorities.
	if subCount > 15 {
		return nil, fmt.Errorf("%w: SID declares %d sub-authorities",
			ErrInvalidSecurityDescriptor, subCount)
	}

	if SIDHeaderSize+subCount*4 > len(data) {
		return nil, fmt.Errorf("%w: SID with %d sub-authorities exceeds buffer",
			ErrInvalidSecurityDescriptor, subCount)
	}

	sid := &SID{
		Revision:       data[0],
		SubAuthorities: make([]uint32, subCount),
	}

	// The identifier authority is 6 bytes, big-endian, unlike everything else.
	for i := 2; i < 8; i++ {
		sid.IdentifierAuthority = sid.IdentifierAuthority<<8 | uint64(data[i])
	}

	for i := 0; i < subCount; i++ {
		sid.SubAuthorities[i] = ReadUint32LE(data, SIDHeaderSize+i*4)
	}

	return sid, nil
}

// parseACLAt parses an ACL located at the given offset within buf.
func parseACLAt(buf []byte, offset uint32) (*ACL, error) {
	if int(offset)+ACLHeaderSize > len(buf) {
		return nil, fmt.Errorf("%w: ACL offset %d beyond %d bytes",
			ErrInvalidSecurityDescriptor, offset, len(buf))
	}

	data := buf[offset:]
	aclSize := int(ReadUint16LE(data, 2))
	aceCount := int(ReadUint16LE(data, 4))

	if aclSize > len(data) {
		aclSize = len(data)
	}

	acl := &ACL{Revision: data[0]}

	aceOffset := ACLHeaderSize
	for i := 0; i < aceCount; i++ {
		if aceOffset+ACEHeaderSize > aclSize {
			break // Truncated ACL: keep the ACEs already decoded
		}

		aceSize := int(ReadUint16LE(data, aceOffset+2))
		if aceSize < ACEHeaderSize || aceOffset+aceSize > aclSize {
			break
		}

		ace := ACE{
			Type:  data[aceOffset],
			Flags: data[aceOffset+1],
			Mask:  ReadUint32LE(data, aceOffset+4),
		}

		// Standard ACE types carry the trustee SID immediately after the header.
		// Object ACEs interpose GUIDs, so their trustee is left unparsed rather
		// than misread from the wrong offset.
		if isStandardACEType(ace.Type) {
			if sid, err := parseSIDAt(data[:aceOffset+aceSize], uint32(aceOffset+ACEHeaderSize)); err == nil {
				ace.Trustee = sid
			}
		}

		acl.ACEs = append(acl.ACEs, ace)
		aceOffset += aceSize
	}

	return acl, nil
}

// isStandardACEType reports whether the ACE places its trustee SID directly
// after the fixed header.
func isStandardACEType(aceType uint8) bool {
	switch aceType {
	case ACETypeAccessAllowed, ACETypeAccessDenied, ACETypeSystemAudit, ACETypeSystemAlarm:
		return true
	default:
		return false
	}
}
