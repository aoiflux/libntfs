package libntfs

import (
	"bytes"
	"errors"
	"testing"
)

// buildEAEntry assembles one $EA entry. nextEntryOffset is written verbatim so
// tests can pin the malformed cases; buildEAValue fills it in for well-formed
// chains.
func buildEAEntry(name string, value []byte, flags uint8, nextEntryOffset uint32) []byte {
	// Name is NUL-terminated on disk, and entries are padded to four bytes.
	size := AlignUp(eaEntryHeaderSize+len(name)+1+len(value), 4)
	entry := make([]byte, size)

	WriteUint32LE(entry, 0, nextEntryOffset)
	entry[4] = flags
	entry[5] = uint8(len(name))
	WriteUint16LE(entry, 6, uint16(len(value)))
	copy(entry[eaEntryHeaderSize:], name)
	copy(entry[eaEntryHeaderSize+len(name)+1:], value)

	return entry
}

// buildEAValue chains entries the way NTFS does, with each entry pointing at the
// next and the last carrying a zero.
func buildEAValue(entries ...[]byte) []byte {
	var out []byte

	for i, entry := range entries {
		chained := make([]byte, len(entry))
		copy(chained, entry)

		if i == len(entries)-1 {
			WriteUint32LE(chained, 0, 0)
		} else {
			WriteUint32LE(chained, 0, uint32(len(chained)))
		}

		out = append(out, chained...)
	}

	return out
}

// buildEAInfoValue assembles a $EA_INFORMATION value.
func buildEAInfoValue(packedSize, needCount uint16, unpackedSize uint32) []byte {
	buf := make([]byte, eaInfoSize)
	WriteUint16LE(buf, 0, packedSize)
	WriteUint16LE(buf, 2, needCount)
	WriteUint32LE(buf, 4, unpackedSize)
	return buf
}

func TestParseEAInformation(t *testing.T) {
	info, err := ParseEAInformation(buildEAInfoValue(120, 1, 160))
	if err != nil {
		t.Fatalf("ParseEAInformation failed: %v", err)
	}

	if info.PackedSize != 120 || info.NeedEACount != 1 || info.UnpackedSize != 160 {
		t.Errorf("unexpected $EA_INFORMATION: %+v", info)
	}
}

func TestParseEAInformationRejectsShortValue(t *testing.T) {
	_, err := ParseEAInformation(make([]byte, eaInfoSize-1))
	if !errors.Is(err, ErrInvalidExtendedAttribute) {
		t.Errorf("expected ErrInvalidExtendedAttribute, got %v", err)
	}
}

func TestParseExtendedAttributes(t *testing.T) {
	value := buildEAValue(
		buildEAEntry("LXATTRB", []byte{1, 2, 3, 4}, 0, 0),
		buildEAEntry("$LXUID", []byte{0xE8, 0x03, 0, 0}, EAFlagNeedEA, 0),
	)

	attrs, err := ParseExtendedAttributes(value)
	if err != nil {
		t.Fatalf("ParseExtendedAttributes failed: %v", err)
	}

	if len(attrs) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(attrs), attrs)
	}

	if attrs[0].Name != "LXATTRB" {
		t.Errorf("expected name LXATTRB, got %q", attrs[0].Name)
	}
	if !bytes.Equal(attrs[0].Value, []byte{1, 2, 3, 4}) {
		t.Errorf("unexpected value %v", attrs[0].Value)
	}
	if attrs[0].NeedEA() {
		t.Error("expected the first entry not to be need-EA")
	}

	if attrs[1].Name != "$LXUID" {
		t.Errorf("expected name $LXUID, got %q", attrs[1].Name)
	}
	if !attrs[1].NeedEA() {
		t.Error("expected the second entry to be need-EA")
	}
}

// TestParseExtendedAttributesStopsAtPadding covers the zero-filled slack NTFS
// leaves after the last entry, which must not read as an entry with no name.
func TestParseExtendedAttributesStopsAtPadding(t *testing.T) {
	value := buildEAValue(buildEAEntry("EA1", []byte("v"), 0, 0))
	value = append(value, make([]byte, 32)...)

	attrs, err := ParseExtendedAttributes(value)
	if err != nil {
		t.Fatalf("ParseExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(attrs), attrs)
	}
}

// TestParseExtendedAttributesRejectsNonAdvancingOffset covers the walk that would
// otherwise spin forever or read one entry's bytes as the next.
func TestParseExtendedAttributesRejectsNonAdvancingOffset(t *testing.T) {
	tests := []struct {
		name string
		next uint32
	}{
		{name: "zero-length step", next: 1},
		{name: "overlapping step", next: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := buildEAEntry("LOOP", []byte("value"), 0, tt.next)

			attrs, err := ParseExtendedAttributes(value)
			if !errors.Is(err, ErrInvalidExtendedAttribute) {
				t.Fatalf("expected ErrInvalidExtendedAttribute, got %v", err)
			}
			// The entry itself parsed, so it is still returned.
			if len(attrs) != 1 {
				t.Errorf("expected the readable prefix returned, got %+v", attrs)
			}
		})
	}
}

func TestParseExtendedAttributesRejectsTruncatedValue(t *testing.T) {
	value := buildEAValue(buildEAEntry("TRUNC", make([]byte, 32), 0, 0))
	value = value[:len(value)-8]

	if _, err := ParseExtendedAttributes(value); !errors.Is(err, ErrInvalidExtendedAttribute) {
		t.Errorf("expected ErrInvalidExtendedAttribute, got %v", err)
	}
}

func TestParseExtendedAttributesRejectsShortHeader(t *testing.T) {
	if _, err := ParseExtendedAttributes([]byte{1, 2, 3}); !errors.Is(err, ErrInvalidExtendedAttribute) {
		t.Errorf("expected ErrInvalidExtendedAttribute, got %v", err)
	}
}

func TestParseExtendedAttributesEmpty(t *testing.T) {
	attrs, err := ParseExtendedAttributes(nil)
	if err != nil {
		t.Fatalf("expected no error for an empty value, got %v", err)
	}
	if len(attrs) != 0 {
		t.Errorf("expected no entries, got %+v", attrs)
	}
}

// TestEachExtendedAttributeStopsEarly covers a caller that has found what it
// wants and does not want the rest walked.
func TestEachExtendedAttributeStopsEarly(t *testing.T) {
	value := buildEAValue(
		buildEAEntry("FIRST", []byte("a"), 0, 0),
		buildEAEntry("SECOND", []byte("b"), 0, 0),
	)

	stop := errors.New("stop")
	seen := 0

	err := EachExtendedAttribute(value, func(ExtendedAttribute) error {
		seen++
		return stop
	})

	if !errors.Is(err, stop) {
		t.Errorf("expected the caller's error returned, got %v", err)
	}
	if seen != 1 {
		t.Errorf("expected the walk to stop after 1 entry, saw %d", seen)
	}
}

func TestFileExtendedAttributesResident(t *testing.T) {
	value := buildEAValue(
		buildEAEntry("LXATTRB", []byte{9, 9}, 0, 0),
	)

	_, f := openFragmentTestFile(t, "wsl.txt",
		buildResidentAttr(AttrTypeEA, 2, value),
		buildResidentAttr(AttrTypeEAInfo, 3, buildEAInfoValue(uint16(len(value)), 0, uint32(len(value)))),
	)

	if !f.HasExtendedAttributes() {
		t.Error("expected HasExtendedAttributes to report the $EA attribute")
	}

	attrs, err := f.ExtendedAttributes()
	if err != nil {
		t.Fatalf("ExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 || attrs[0].Name != "LXATTRB" {
		t.Fatalf("unexpected entries: %+v", attrs)
	}

	info, err := f.EAInformation()
	if err != nil {
		t.Fatalf("EAInformation failed: %v", err)
	}
	if info == nil || int(info.PackedSize) != len(value) {
		t.Errorf("unexpected $EA_INFORMATION: %+v", info)
	}
}

// TestFileExtendedAttributesNonResident covers the layout WSL metadata usually
// lands in, where the value lives in clusters rather than the MFT record.
func TestFileExtendedAttributesNonResident(t *testing.T) {
	value := buildEAValue(
		buildEAEntry("LXXATTR", []byte("user.test=1"), 0, 0),
	)

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "big-ea.txt",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNonResidentEAAttr(t, 2, 6, 1, testClusterSize, uint64(len(value))),
		},
	})

	vol := newTestMFTVolume(t, [][]byte{record})
	writeTestCluster(t, vol, 6, value)

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := f.ExtendedAttributes()
	if err != nil {
		t.Fatalf("ExtendedAttributes failed: %v", err)
	}

	if len(attrs) != 1 || attrs[0].Name != "LXXATTR" {
		t.Fatalf("unexpected entries: %+v", attrs)
	}
	if string(attrs[0].Value) != "user.test=1" {
		t.Errorf("unexpected value %q", attrs[0].Value)
	}
}

func TestFileExtendedAttributesAbsent(t *testing.T) {
	_, f := openFragmentTestFile(t, "plain.txt",
		buildResidentAttr(AttrTypeData, 2, []byte("no eas")))

	if f.HasExtendedAttributes() {
		t.Error("expected no $EA attribute")
	}

	attrs, err := f.ExtendedAttributes()
	if err != nil {
		t.Fatalf("expected no error for a file without $EA, got %v", err)
	}
	if attrs != nil {
		t.Errorf("expected no entries, got %+v", attrs)
	}

	info, err := f.EAInformation()
	if err != nil {
		t.Fatalf("expected no error for a file without $EA_INFORMATION, got %v", err)
	}
	if info != nil {
		t.Errorf("expected nil $EA_INFORMATION, got %+v", info)
	}
}

// buildNonResidentEAAttr builds a non-resident $EA backed by a single run.
func buildNonResidentEAAttr(t testing.TB, attrID uint16,
	startCluster, clusters uint64, clusterSize uint32, realSize uint64,
) []byte {
	t.Helper()

	attr := buildNamedNonResidentDataAttr(t, attrID, "", startCluster, clusters, clusterSize, realSize)
	WriteUint32LE(attr, 0, AttrTypeEA)

	return attr
}

// writeTestCluster places bytes into a test volume's backing disk.
func writeTestCluster(t testing.TB, vol *Volume, cluster uint64, content []byte) {
	t.Helper()

	mock, ok := vol.reader.(*mockReaderAt)
	if !ok {
		t.Fatalf("expected a mockReaderAt backing the test volume, got %T", vol.reader)
	}

	offset := cluster * uint64(vol.bytesPerCluster)
	if offset+uint64(len(content)) > uint64(len(mock.data)) {
		t.Fatalf("cluster %d does not fit in the test disk", cluster)
	}

	copy(mock.data[offset:], content)
}
