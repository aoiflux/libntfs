package libntfs

import (
	"slices"
	"testing"
	"time"
)

// testRawMFTRecord builds a fixup-stamped MFT record holding exactly attrs,
// with baseRef in the header (0 for a base record).
func testRawMFTRecord(t testing.TB, seq, flags uint16, baseRef uint64, attrs ...[]byte) []byte {
	t.Helper()

	const (
		recordSize = DefaultMFTRecordSize
		usaOffset  = MFTEntryHeaderSize
		usaCount   = recordSize/UpdateSequenceStride + 1
	)
	firstAttr := AlignUp(usaOffset+usaCount*2, 8)

	buf := make([]byte, recordSize)
	offset := firstAttr
	for _, attr := range attrs {
		if offset+len(attr)+8 > recordSize {
			t.Fatalf("attributes do not fit in a %d byte record", recordSize)
		}
		copy(buf[offset:], attr)
		offset += len(attr)
	}
	WriteUint32LE(buf, offset, AttrTypeEnd)

	WriteUint32LE(buf, 0, MFTMagicFILE)
	WriteUint16LE(buf, 4, usaOffset)
	WriteUint16LE(buf, 6, usaCount)
	WriteUint16LE(buf, 16, seq)
	WriteUint16LE(buf, 18, 1)
	WriteUint16LE(buf, 20, uint16(firstAttr))
	WriteUint16LE(buf, 22, flags)
	WriteUint32LE(buf, 24, uint32(AlignUp(offset+4, 8)))
	WriteUint32LE(buf, 28, recordSize)
	WriteUint64LE(buf, 32, baseRef)
	WriteUint16LE(buf, 40, uint16(len(attrs)))

	const usn = 0x0A0B
	WriteUint16LE(buf, usaOffset, usn)
	for i := 0; i < usaCount-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		WriteUint16LE(buf, usaOffset+2+i*2, ReadUint16LE(buf, sectorEnd))
		WriteUint16LE(buf, sectorEnd, usn)
	}
	return buf
}

// testAttrListEntry builds one $ATTRIBUTE_LIST entry.
func testAttrListEntry(attrType uint32, name string, startVCN, recordRef uint64, attrID uint16) []byte {
	nameBytes := utf16LEFromString(name)
	length := AlignUp(26+len(nameBytes), 8)
	buf := make([]byte, length)
	WriteUint32LE(buf, 0, attrType)
	WriteUint16LE(buf, 4, uint16(length))
	buf[6] = uint8(len([]rune(name)))
	buf[7] = 26
	WriteUint64LE(buf, 8, startVCN)
	WriteUint64LE(buf, 16, recordRef)
	WriteUint16LE(buf, 24, attrID)
	copy(buf[26:], nameBytes)
	return buf
}

// testFileNameAttr builds a resident $FILE_NAME attribute naming parent.
func testFileNameAttr(attrID uint16, parent uint64, name string, namespace uint8) []byte {
	return buildResidentAttr(AttrTypeFileName, attrID,
		buildFileNameAttrValue(parent, 1, name, namespace, 0, 0, FileAttrArchive))
}

// hardLinkedImage builds a file (entry 30) with three hard links whose second
// and third $FILE_NAME sit in an extension record (entry 31), as Windows lays
// out a file whose names outgrow its base record.
func hardLinkedImage(t testing.TB) []byte {
	t.Helper()

	const base, ext = 30, 31
	stamp := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	list := slices.Concat(
		testAttrListEntry(AttrTypeStandardInfo, "", 0, MakeFileReference(base, 1), 0),
		testAttrListEntry(AttrTypeFileName, "", 0, MakeFileReference(base, 1), 1),
		testAttrListEntry(AttrTypeFileName, "", 0, MakeFileReference(ext, 1), 0),
		testAttrListEntry(AttrTypeFileName, "", 0, MakeFileReference(ext, 1), 1),
	)
	baseRecord := testRawMFTRecord(t, 1, MFTFlagInUse, 0,
		buildResidentAttr(AttrTypeStandardInfo, 0, buildStandardInfoAttrValue(
			stamp, stamp, stamp, stamp, FileAttrArchive, 0)),
		testFileNameAttr(1, MFTEntryRoot, "one", NamespaceWin32),
		buildResidentAttr(AttrTypeAttributeList, 2, list),
	)
	extRecord := testRawMFTRecord(t, 1, MFTFlagInUse, MakeFileReference(base, 1),
		testFileNameAttr(0, MFTEntryRoot, "two", NamespaceWin32),
		testFileNameAttr(1, MFTEntryRoot, "three", NamespacePOSIX),
	)
	return buildTestNTFSImageAt(t, map[uint64][]byte{base: baseRecord, ext: extRecord})
}

// TestAttributeListKeepsEveryExtensionFileName covers NTFS-5: two $FILE_NAMEs
// in one extension record both matched the first by type and name, and an
// extension $FILE_NAME was dropped outright when the base record held one.
func TestAttributeListKeepsEveryExtensionFileName(t *testing.T) {
	v, err := Open(&mockReaderAt{data: hardLinkedImage(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer v.Close()

	entry, err := v.GetMFTEntry(30)
	if err != nil {
		t.Fatalf("GetMFTEntry: %v", err)
	}
	var names []string
	for _, attr := range entry.FindAllAttributes(AttrTypeFileName) {
		fn, err := residentFileName(attr)
		if err != nil {
			t.Fatalf("parse $FILE_NAME: %v", err)
		}
		names = append(names, fn.Name)
	}
	if want := []string{"one", "two", "three"}; !slices.Equal(names, want) {
		t.Fatalf("$FILE_NAMEs = %q, want %q", names, want)
	}
}

// residentFileName parses a resident $FILE_NAME attribute.
func residentFileName(attr *Attribute) (*FileName, error) {
	if attr.Resident == nil {
		return nil, ErrInvalidAttribute
	}
	return ParseFileName(attr.Resident.Value)
}
