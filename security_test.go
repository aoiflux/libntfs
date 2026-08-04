package libntfs

import (
	"errors"
	"reflect"
	"testing"
)

// buildSID assembles a SID with the given authority and sub-authorities.
func buildSID(authority uint64, subs ...uint32) []byte {
	buf := make([]byte, SIDHeaderSize+len(subs)*4)

	buf[0] = 1 // Revision
	buf[1] = uint8(len(subs))
	for i := 0; i < 6; i++ {
		// The identifier authority is 6 bytes, big-endian.
		buf[2+i] = byte(authority >> (8 * (5 - i)))
	}
	for i, sub := range subs {
		WriteUint32LE(buf, SIDHeaderSize+i*4, sub)
	}

	return buf
}

// buildACE assembles a standard ACE carrying a trustee SID.
func buildACE(aceType, flags uint8, mask uint32, sid []byte) []byte {
	size := ACEHeaderSize + len(sid)
	buf := make([]byte, size)

	buf[0] = aceType
	buf[1] = flags
	WriteUint16LE(buf, 2, uint16(size))
	WriteUint32LE(buf, 4, mask)
	copy(buf[ACEHeaderSize:], sid)

	return buf
}

// buildACL assembles an ACL around the supplied ACEs.
func buildACL(aces ...[]byte) []byte {
	size := ACLHeaderSize
	for _, ace := range aces {
		size += len(ace)
	}

	buf := make([]byte, size)
	buf[0] = 2 // ACL revision
	WriteUint16LE(buf, 2, uint16(size))
	WriteUint16LE(buf, 4, uint16(len(aces)))

	offset := ACLHeaderSize
	for _, ace := range aces {
		copy(buf[offset:], ace)
		offset += len(ace)
	}

	return buf
}

// buildSecurityDescriptor assembles a self-relative descriptor. A nil dacl
// leaves the DACL-present control flag clear.
func buildSecurityDescriptor(owner, group, dacl []byte) []byte {
	control := uint16(SESelfRelative)
	if dacl != nil {
		control |= SEDACLPresent
	}

	buf := make([]byte, SecurityDescriptorHeaderSize)
	buf[0] = 1 // Revision
	WriteUint16LE(buf, 2, control)

	appendAt := func(payload []byte, offsetField int) {
		if payload == nil {
			return
		}
		WriteUint32LE(buf, offsetField, uint32(len(buf)))
		buf = append(buf, payload...)
	}

	appendAt(owner, 4)
	appendAt(group, 8)
	appendAt(dacl, 16)

	return buf
}

// buildSDSEntry wraps a descriptor in its $SDS entry header.
func buildSDSEntry(securityID uint32, offset uint64, descriptor []byte) []byte {
	length := SDSEntryHeaderSize + len(descriptor)
	buf := make([]byte, AlignUp(length, 16))

	WriteUint32LE(buf, 0, 0xDEADBEEF) // Hash, unused by the parser
	WriteUint32LE(buf, 4, securityID)
	WriteUint64LE(buf, 8, offset)
	WriteUint32LE(buf, 16, uint32(length))
	copy(buf[SDSEntryHeaderSize:], descriptor)

	return buf
}

func TestSIDString(t *testing.T) {
	tests := []struct {
		sid  []byte
		want string
	}{
		{buildSID(5, 32, 544), "S-1-5-32-544"},
		{buildSID(5, 18), "S-1-5-18"},
		{buildSID(1, 0), "S-1-1-0"},
		{buildSID(5, 21, 1004336348, 1177238915, 682003330, 512), "S-1-5-21-1004336348-1177238915-682003330-512"},
	}

	for _, tc := range tests {
		sid, err := parseSIDAt(tc.sid, 0)
		if err != nil {
			t.Fatalf("parseSIDAt failed: %v", err)
		}
		if got := sid.String(); got != tc.want {
			t.Fatalf("SID string mismatch: got %q want %q", got, tc.want)
		}
	}
}

func TestSIDWellKnownName(t *testing.T) {
	sid, err := parseSIDAt(buildSID(5, 32, 544), 0)
	if err != nil {
		t.Fatalf("parseSIDAt failed: %v", err)
	}

	name, ok := sid.WellKnownName()
	if !ok {
		t.Fatal("expected S-1-5-32-544 to be well known")
	}
	if name != "BUILTIN\\Administrators" {
		t.Fatalf("well-known name mismatch: got %q", name)
	}

	custom, err := parseSIDAt(buildSID(5, 21, 1, 2, 3, 1001), 0)
	if err != nil {
		t.Fatalf("parseSIDAt failed: %v", err)
	}
	if _, ok := custom.WellKnownName(); ok {
		t.Fatal("a domain user SID must not be reported as well known")
	}
}

func TestParseSecurityDescriptor(t *testing.T) {
	owner := buildSID(5, 32, 544)
	group := buildSID(5, 18)
	dacl := buildACL(
		buildACE(ACETypeAccessAllowed, 0, AccessGenericAll, buildSID(5, 18)),
		buildACE(ACETypeAccessDenied, ACEFlagInherited, AccessWriteData|AccessDelete, buildSID(1, 0)),
	)

	sd, err := ParseSecurityDescriptor(buildSecurityDescriptor(owner, group, dacl))
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor failed: %v", err)
	}

	if sd.Revision != 1 {
		t.Fatalf("revision mismatch: got %d", sd.Revision)
	}
	if sd.Control&SEDACLPresent == 0 {
		t.Fatal("expected the DACL-present control flag")
	}
	if got := sd.Owner.String(); got != "S-1-5-32-544" {
		t.Fatalf("owner mismatch: got %q", got)
	}
	if got := sd.Group.String(); got != "S-1-5-18" {
		t.Fatalf("group mismatch: got %q", got)
	}
	if sd.SACL != nil {
		t.Fatal("expected no SACL")
	}

	if sd.DACL == nil {
		t.Fatal("expected a DACL")
	}
	if len(sd.DACL.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs, got %d", len(sd.DACL.ACEs))
	}

	allow := sd.DACL.ACEs[0]
	if allow.TypeName() != "ACCESS_ALLOWED" {
		t.Fatalf("first ACE type mismatch: got %s", allow.TypeName())
	}
	if allow.Trustee.String() != "S-1-5-18" {
		t.Fatalf("first ACE trustee mismatch: got %q", allow.Trustee.String())
	}
	if allow.IsInherited() {
		t.Fatal("first ACE should not be inherited")
	}
	if got := allow.RightsNames(); !reflect.DeepEqual(got, []string{"GenericAll"}) {
		t.Fatalf("first ACE rights mismatch: got %v", got)
	}

	deny := sd.DACL.ACEs[1]
	if deny.TypeName() != "ACCESS_DENIED" {
		t.Fatalf("second ACE type mismatch: got %s", deny.TypeName())
	}
	if !deny.IsInherited() {
		t.Fatal("second ACE should be inherited")
	}
	if got := deny.RightsNames(); !reflect.DeepEqual(got, []string{"WriteData", "Delete"}) {
		t.Fatalf("second ACE rights mismatch: got %v", got)
	}
}

func TestParseSecurityDescriptorRejectsShortBuffer(t *testing.T) {
	_, err := ParseSecurityDescriptor(make([]byte, 8))
	if !errors.Is(err, ErrInvalidSecurityDescriptor) {
		t.Fatalf("expected ErrInvalidSecurityDescriptor, got %v", err)
	}
}

func TestParseSecurityDescriptorAbsentDACL(t *testing.T) {
	// No DACL at all grants everyone access, which must stay distinguishable
	// from a present-but-empty DACL that denies everyone.
	sd, err := ParseSecurityDescriptor(buildSecurityDescriptor(buildSID(5, 18), nil, nil))
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor failed: %v", err)
	}
	if sd.DACL != nil {
		t.Fatal("expected a nil DACL when the present flag is clear")
	}

	empty, err := ParseSecurityDescriptor(buildSecurityDescriptor(buildSID(5, 18), nil, buildACL()))
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor failed: %v", err)
	}
	if empty.DACL == nil {
		t.Fatal("expected a non-nil empty DACL")
	}
	if len(empty.DACL.ACEs) != 0 {
		t.Fatalf("expected an empty DACL, got %d ACEs", len(empty.DACL.ACEs))
	}
}

func TestRightsNamesReportsUnknownBits(t *testing.T) {
	ace := ACE{Mask: AccessReadData | 0x00000200}

	got := ace.RightsNames()
	want := []string{"ReadData", "0x00000200"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rights mismatch: got %v want %v", got, want)
	}
}

func TestParseSDSStreamDedupesMirroredEntries(t *testing.T) {
	first := buildSecurityDescriptor(buildSID(5, 32, 544), buildSID(5, 18),
		buildACL(buildACE(ACETypeAccessAllowed, 0, AccessGenericAll, buildSID(5, 18))))
	second := buildSecurityDescriptor(buildSID(5, 21, 1, 2, 3, 1001), nil, nil)

	var sds []byte
	sds = append(sds, buildSDSEntry(256, 0, first)...)
	sds = append(sds, buildSDSEntry(257, uint64(len(sds)), second)...)
	// The mirror copy repeats both entries; they must not be double counted.
	sds = append(sds, buildSDSEntry(256, 0, first)...)

	descriptors := parseSDSStream(sds)
	if len(descriptors) != 2 {
		t.Fatalf("expected 2 descriptors, got %d", len(descriptors))
	}

	if got := descriptors[256].Owner.String(); got != "S-1-5-32-544" {
		t.Fatalf("security ID 256 owner mismatch: got %q", got)
	}
	if got := descriptors[257].Owner.String(); got != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("security ID 257 owner mismatch: got %q", got)
	}
}

func TestParseSDSStreamSkipsDamagedEntries(t *testing.T) {
	good := buildSecurityDescriptor(buildSID(5, 18), nil, nil)

	var sds []byte
	// A zero-filled slot, as found in $SDS padding.
	sds = append(sds, make([]byte, 32)...)
	// An entry claiming an impossible length.
	damaged := buildSDSEntry(900, 0, good)
	WriteUint32LE(damaged, 16, 0xFFFFFF)
	sds = append(sds, damaged...)
	// A healthy entry after the damage, which must still be found.
	sds = append(sds, buildSDSEntry(901, uint64(len(sds)), good)...)

	descriptors := parseSDSStream(sds)
	if _, found := descriptors[901]; !found {
		t.Fatalf("expected to recover security ID 901 after damage, got %v", descriptors)
	}
}

// newSecureTestVolume builds an image whose $Secure entry carries a $SDS stream.
func newSecureTestVolume(t testing.TB, sds []byte, files map[uint64][]byte) *Volume {
	t.Helper()

	records := map[uint64][]byte{
		MFTEntrySecure: buildTestMFTRecord(t, testRecordOpts{
			flags:      MFTFlagInUse,
			name:       "$Secure",
			namespace:  NamespaceWin32,
			extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeData, 2, SDSStreamName, sds)},
		}),
	}
	for entryNum, record := range files {
		records[entryNum] = record
	}

	vol, err := Open(&mockReaderAt{data: buildTestNTFSImageAt(t, records)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	return vol
}

func TestVolumeSecurityDescriptorByID(t *testing.T) {
	descriptor := buildSecurityDescriptor(buildSID(5, 32, 544), buildSID(5, 18),
		buildACL(buildACE(ACETypeAccessAllowed, 0, AccessGenericRead, buildSID(1, 0))))

	vol := newSecureTestVolume(t, buildSDSEntry(256, 0, descriptor), nil)

	sd, err := vol.SecurityDescriptorByID(256)
	if err != nil {
		t.Fatalf("SecurityDescriptorByID failed: %v", err)
	}
	if got := sd.Owner.String(); got != "S-1-5-32-544" {
		t.Fatalf("owner mismatch: got %q", got)
	}

	if _, err := vol.SecurityDescriptorByID(999); !errors.Is(err, ErrSecurityDescriptorNotFound) {
		t.Fatalf("expected ErrSecurityDescriptorNotFound, got %v", err)
	}
}

func TestVolumeEachSecurityDescriptorIsOrdered(t *testing.T) {
	descriptor := buildSecurityDescriptor(buildSID(5, 18), nil, nil)

	var sds []byte
	for _, id := range []uint32{300, 100, 200} {
		sds = append(sds, buildSDSEntry(id, uint64(len(sds)), descriptor)...)
	}

	vol := newSecureTestVolume(t, sds, nil)

	var ids []uint32
	if err := vol.EachSecurityDescriptor(func(id uint32, sd *SecurityDescriptor) error {
		if sd == nil {
			t.Fatalf("nil descriptor for id %d", id)
		}
		ids = append(ids, id)
		return nil
	}); err != nil {
		t.Fatalf("EachSecurityDescriptor failed: %v", err)
	}

	if !reflect.DeepEqual(ids, []uint32{100, 200, 300}) {
		t.Fatalf("expected ascending security IDs, got %v", ids)
	}
}

func TestFileSecurityDescriptorViaSecurityID(t *testing.T) {
	const securityID = 264

	descriptor := buildSecurityDescriptor(buildSID(5, 32, 545), buildSID(5, 18),
		buildACL(buildACE(ACETypeAccessAllowed, 0, AccessReadData, buildSID(5, 11))))

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "secured.txt",
		namespace:  NamespaceWin32,
		securityID: securityID,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("data"))},
	})

	vol := newSecureTestVolume(t, buildSDSEntry(securityID, 0, descriptor),
		map[uint64][]byte{20: record})

	f, err := vol.Open(20)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	sd, err := f.SecurityDescriptor()
	if err != nil {
		t.Fatalf("SecurityDescriptor failed: %v", err)
	}

	if got := sd.Owner.String(); got != "S-1-5-32-545" {
		t.Fatalf("owner mismatch: got %q", got)
	}
	if len(sd.DACL.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(sd.DACL.ACEs))
	}
	if got := sd.DACL.ACEs[0].Trustee.String(); got != "S-1-5-11" {
		t.Fatalf("trustee mismatch: got %q", got)
	}
}

func TestFileSecurityDescriptorPrefersOwnAttribute(t *testing.T) {
	const securityID = 300

	// The $SDS entry for this security ID names a different owner, so whichever
	// source wins is unambiguous.
	viaSDS := buildSecurityDescriptor(buildSID(5, 18), nil, nil)
	viaAttr := buildSecurityDescriptor(buildSID(5, 32, 544), nil, nil)

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "legacy.txt",
		namespace:  NamespaceWin32,
		securityID: securityID,
		extraAttrs: [][]byte{
			buildResidentAttr(AttrTypeSecurityDesc, 2, viaAttr),
			buildResidentAttr(AttrTypeData, 3, []byte("data")),
		},
	})

	vol := newSecureTestVolume(t, buildSDSEntry(securityID, 0, viaSDS),
		map[uint64][]byte{20: record})

	f, err := vol.Open(20)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	sd, err := f.SecurityDescriptor()
	if err != nil {
		t.Fatalf("SecurityDescriptor failed: %v", err)
	}

	if got := sd.Owner.String(); got != "S-1-5-32-544" {
		t.Fatalf("expected the entry's own $SECURITY_DESCRIPTOR to win, got owner %q", got)
	}
}
