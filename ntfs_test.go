package libntfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"testing"
	"time"
)

type mockReaderAt struct {
	data []byte
}

func (m *mockReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}

	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type mockFileInfo struct {
	name string
	mode fs.FileMode
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return 0 }
func (m mockFileInfo) Mode() fs.FileMode  { return m.mode }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool        { return m.mode.IsDir() }
func (m mockFileInfo) Sys() interface{}   { return nil }

type mockReaderAtWithStat struct {
	data    []byte
	info    fs.FileInfo
	statErr error
}

func (m *mockReaderAtWithStat) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}

	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mockReaderAtWithStat) Stat() (fs.FileInfo, error) {
	if m.statErr != nil {
		return nil, m.statErr
	}
	return m.info, nil
}

// TestNTFSTimeConversion tests NTFS time conversion functions.
func TestNTFSTimeConversion(t *testing.T) {
	// Test zero time
	zeroTime := NTFSTimeToTime(0)
	if !zeroTime.IsZero() {
		t.Error("Zero NTFS time should convert to zero time")
	}

	// Test known time: 2020-01-01 00:00:00 UTC
	// NTFS time: 132231936000000000
	ntfsTime := uint64(132231936000000000)
	expectedTime := time.Date(2020, 1, 11, 5, 20, 0, 0, time.UTC)

	result := NTFSTimeToTime(ntfsTime)
	if !result.Equal(expectedTime) {
		t.Errorf("Time conversion failed: expected %v, got %v", expectedTime, result)
	}

	// Test round-trip conversion
	backToNTFS := TimeToNTFSTime(result)
	if backToNTFS != ntfsTime {
		t.Errorf("Round-trip conversion failed: expected %d, got %d", ntfsTime, backToNTFS)
	}
}

func TestOpenRejectsDirectoryInput(t *testing.T) {
	reader := &mockReaderAtWithStat{
		data: make([]byte, BootSectorSize),
		info: mockFileInfo{name: "testdir", mode: fs.ModeDir},
	}

	_, err := Open(reader)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInputIsDirectory) {
		t.Fatalf("expected ErrInputIsDirectory, got %v", err)
	}
}

// TestBinaryReader tests the BinaryReader functionality.
func TestBinaryReader(t *testing.T) {
	data := []byte{
		0x12, 0x34, // uint16: 0x3412
		0x56, 0x78, 0x9A, 0xBC, // uint32: 0xBC9A7856
		0xDE, 0xF0, 0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, // uint64
	}

	r := NewBinaryReader(data)

	val16, err := r.ReadUint16()
	if err != nil || val16 != 0x3412 {
		t.Errorf("ReadUint16 failed: expected 0x3412, got 0x%X, err=%v", val16, err)
	}

	val32, err := r.ReadUint32()
	if err != nil || val32 != 0xBC9A7856 {
		t.Errorf("ReadUint32 failed: expected 0xBC9A7856, got 0x%X, err=%v", val32, err)
	}

	val64, err := r.ReadUint64()
	expectedVal64 := uint64(0xBC9A78563412F0DE) // Corrected value
	if err != nil || val64 != expectedVal64 {
		t.Errorf("ReadUint64 failed: expected 0x%X, got 0x%X, err=%v", expectedVal64, val64, err)
	}
}

// TestAlignUp tests alignment functions.
func TestAlignUp(t *testing.T) {
	tests := []struct {
		value     int
		alignment int
		expected  int
	}{
		{0, 8, 0},
		{1, 8, 8},
		{7, 8, 8},
		{8, 8, 8},
		{9, 8, 16},
		{15, 8, 16},
		{16, 8, 16},
		{100, 512, 512},
		{513, 512, 1024},
	}

	for _, tt := range tests {
		result := AlignUp(tt.value, tt.alignment)
		if result != tt.expected {
			t.Errorf("AlignUp(%d, %d) = %d; expected %d",
				tt.value, tt.alignment, result, tt.expected)
		}
	}
}

// TestDataRunParsing tests data run parsing.
func TestDataRunParsing(t *testing.T) {
	// Example data run: length=1 cluster at cluster 100
	// Header: 0x11 (1 byte length, 1 byte offset)
	// Length: 0x01
	// Offset: 0x64 (100)
	dataRuns := []byte{0x11, 0x01, 0x64, 0x00}

	runs, err := parseDataRuns(dataRuns)
	if err != nil {
		t.Fatalf("Failed to parse data runs: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("Expected 1 run, got %d", len(runs))
	}

	if runs[0].LengthClusters != 1 {
		t.Errorf("Expected length 1, got %d", runs[0].LengthClusters)
	}

	if runs[0].StartCluster != 100 {
		t.Errorf("Expected cluster 100, got %d", runs[0].StartCluster)
	}
}

// TestAttributeTypeNames tests attribute type name lookup.
func TestAttributeTypeNames(t *testing.T) {
	tests := []struct {
		attrType uint32
		expected string
	}{
		{AttrTypeStandardInfo, "$STANDARD_INFORMATION"},
		{AttrTypeFileName, "$FILE_NAME"},
		{AttrTypeData, "$DATA"},
		{AttrTypeIndexRoot, "$INDEX_ROOT"},
		{0x12345, "UNKNOWN"},
	}

	for _, tt := range tests {
		result := GetAttributeTypeName(tt.attrType)
		if result != tt.expected {
			t.Errorf("GetAttributeTypeName(0x%X) = %s; expected %s",
				tt.attrType, result, tt.expected)
		}
	}
}

// TestBootSectorValidation tests boot sector validation.
func TestBootSectorValidation(t *testing.T) {
	// Create a minimal valid boot sector
	bs := make([]byte, BootSectorSize)

	// Set end marker
	bs[510] = 0x55
	bs[511] = 0xAA

	// Set bytes per sector (512)
	bs[11] = 0x00
	bs[12] = 0x02

	// Set sectors per cluster (8)
	bs[13] = 0x08

	// This should parse without error (though it won't have all fields)
	r := NewBinaryReader(bs)
	endMarker, _ := r.ReadUint16()
	r.Seek(510)
	endMarker, _ = r.ReadUint16()

	if endMarker != BootSectorMagic {
		t.Errorf("Boot sector end marker test failed: expected 0x%X, got 0x%X",
			BootSectorMagic, endMarker)
	}
}

// TestMFTMagicValues tests MFT magic value constants.
func TestMFTMagicValues(t *testing.T) {
	// "FILE" in little-endian ASCII
	fileBytes := []byte{'F', 'I', 'L', 'E'}
	fileMagic := uint32(fileBytes[0]) | uint32(fileBytes[1])<<8 |
		uint32(fileBytes[2])<<16 | uint32(fileBytes[3])<<24

	if fileMagic != MFTMagicFILE {
		t.Errorf("MFT FILE magic mismatch: expected 0x%X, got 0x%X",
			MFTMagicFILE, fileMagic)
	}

	// "BAAD" in little-endian ASCII
	baadBytes := []byte{'B', 'A', 'A', 'D'}
	baadMagic := uint32(baadBytes[0]) | uint32(baadBytes[1])<<8 |
		uint32(baadBytes[2])<<16 | uint32(baadBytes[3])<<24

	if baadMagic != MFTMagicBAAD {
		t.Errorf("MFT BAAD magic mismatch: expected 0x%X, got 0x%X",
			MFTMagicBAAD, baadMagic)
	}
}

// TestVariableIntReading tests variable-length integer parsing.
func TestVariableIntReading(t *testing.T) {
	tests := []struct {
		data     []byte
		size     int
		signed   bool
		expected int64
	}{
		{[]byte{0x01}, 1, false, 1},
		{[]byte{0xFF}, 1, false, 255},
		{[]byte{0xFF}, 1, true, -1},
		{[]byte{0x00, 0x01}, 2, false, 256},
		{[]byte{0xFF, 0xFF}, 2, true, -1},
		{[]byte{0x01, 0x00, 0x00, 0x00}, 4, false, 1},
	}

	for _, tt := range tests {
		result := readVariableInt(tt.data, tt.size, tt.signed)
		if result != tt.expected {
			t.Errorf("readVariableInt(%v, %d, %v) = %d; expected %d",
				tt.data, tt.size, tt.signed, result, tt.expected)
		}
	}
}

// TestUTF16StringConversion tests UTF-16 to UTF-8 conversion.
func TestUTF16StringConversion(t *testing.T) {
	// "Test" in UTF-16LE
	utf16Data := []byte{
		'T', 0x00,
		'e', 0x00,
		's', 0x00,
		't', 0x00,
	}

	r := NewBinaryReader(utf16Data)
	result, err := r.ReadUTF16String(4)
	if err != nil {
		t.Fatalf("Failed to read UTF-16 string: %v", err)
	}

	expected := "Test"
	if result != expected {
		t.Errorf("UTF-16 conversion failed: expected %q, got %q", expected, result)
	}
}

// TestFileReferenceReading tests 6-byte file reference parsing.
func TestFileReferenceReading(t *testing.T) {
	// File reference: 0x0000050000000005 (MFT entry 5)
	data := []byte{0x05, 0x00, 0x00, 0x00, 0x00, 0x05}

	r := NewBinaryReader(data)
	ref, err := r.ReadFileReference()
	if err != nil {
		t.Fatalf("Failed to read file reference: %v", err)
	}

	expected := uint64(0x050000000005)
	if ref != expected {
		t.Errorf("File reference mismatch: expected 0x%X, got 0x%X", expected, ref)
	}
}

// TestErrorWrapping tests error wrapping functions.
func TestErrorWrapping(t *testing.T) {
	baseErr := ErrInvalidMFTEntry

	volErr := wrapVolumeError("test", baseErr)
	if volErr == nil {
		t.Error("Expected non-nil error")
	}

	mftErr := wrapMFTError(5, "test", baseErr)
	if mftErr == nil {
		t.Error("Expected non-nil error")
	}

	attrErr := wrapAttributeError(AttrTypeData, "$DATA", "test", baseErr)
	if attrErr == nil {
		t.Error("Expected non-nil error")
	}
}

func TestNTFSDecompressCompressionUnitSymbolTokens(t *testing.T) {
	// Compressed sub-block of size 6 bytes:
	// 2-byte block header (0x8003), token-group header 0x00, symbols A B C.
	compData := []byte{0x03, 0x80, 0x00, 'A', 'B', 'C'}

	out, err := ntfsDecompressCompressionUnit(compData, 8)
	if err != nil {
		t.Fatalf("ntfsDecompressCompressionUnit returned error: %v", err)
	}

	if string(out[:3]) != "ABC" {
		t.Fatalf("unexpected decompressed prefix: got %q", string(out[:3]))
	}

	for i := 3; i < len(out); i++ {
		if out[i] != 0 {
			t.Fatalf("expected zero padding at index %d, got %d", i, out[i])
		}
	}
}

func TestFileReadAtCompressedNonResident(t *testing.T) {
	clusterSize := uint32(8)

	// Cluster 1 contains one compressed block that expands to "ABC" followed by zeros.
	// Data is laid out as 3 clusters to keep math simple:
	// - cluster 0: unused
	// - cluster 1: compressed payload
	// - cluster 2: unused
	data := make([]byte, 3*clusterSize)
	copy(data[int(clusterSize):int(2*clusterSize)], []byte{0x03, 0x80, 0x00, 'A', 'B', 'C', 0x00, 0x00})

	v := &Volume{
		reader:          &mockReaderAt{data: data},
		bytesPerCluster: clusterSize,
		clusterCount:    3,
	}

	attr := &Attribute{
		Header: AttributeHeader{Flags: AttrFlagCompressed},
		NonResident: &NonResidentAttribute{
			CompressionUnit: 2,
			RealSize:        3,
			DataRuns: []DataRun{
				{LengthClusters: 1, StartCluster: 1, IsSparse: false},
				{LengthClusters: 1, IsSparse: true},
			},
		},
	}

	f := &File{
		volume:   v,
		isDir:    false,
		size:     3,
		dataAttr: attr,
	}

	buf := make([]byte, 3)
	n, err := f.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt returned unexpected error: %v", err)
	}
	if n != 3 {
		t.Fatalf("ReadAt bytes read = %d, want 3", n)
	}
	if string(buf) != "ABC" {
		t.Fatalf("ReadAt data = %q, want %q", string(buf), "ABC")
	}
}

func TestFileReadSupport(t *testing.T) {
	t.Run("resident readable", func(t *testing.T) {
		f := &File{
			dataAttr: &Attribute{Resident: &ResidentAttribute{Value: []byte("abc")}},
		}

		support := f.ReadSupport()
		if !support.HasData || !support.Resident || !support.Readable {
			t.Fatalf("unexpected resident support: %+v", support)
		}
		if support.BlockingError != nil {
			t.Fatalf("unexpected blocking error: %v", support.BlockingError)
		}
	})

	t.Run("compressed non-resident readable", func(t *testing.T) {
		f := &File{
			dataAttr: &Attribute{
				Header:      AttributeHeader{Flags: AttrFlagCompressed},
				NonResident: &NonResidentAttribute{},
			},
		}

		support := f.ReadSupport()
		if !support.HasData || !support.NonResident || !support.Compressed || !support.Readable {
			t.Fatalf("unexpected compressed support: %+v", support)
		}
		if support.BlockingError != nil {
			t.Fatalf("unexpected blocking error: %v", support.BlockingError)
		}
	})

	t.Run("encrypted blocked", func(t *testing.T) {
		f := &File{
			dataAttr: &Attribute{
				Header:      AttributeHeader{Flags: AttrFlagEncrypted},
				NonResident: &NonResidentAttribute{},
			},
		}

		support := f.ReadSupport()
		if support.Readable {
			t.Fatalf("expected encrypted file to be unreadable: %+v", support)
		}
		if !errors.Is(support.BlockingError, ErrEncryptedData) {
			t.Fatalf("expected ErrEncryptedData, got %v", support.BlockingError)
		}
	})

	t.Run("directory blocked", func(t *testing.T) {
		f := &File{isDir: true}

		support := f.ReadSupport()
		if support.Readable {
			t.Fatalf("expected directory to be unreadable as file: %+v", support)
		}
		if !errors.Is(support.BlockingError, ErrNotFile) {
			t.Fatalf("expected ErrNotFile, got %v", support.BlockingError)
		}
	})
}

func TestAttributeReadSupport(t *testing.T) {
	t.Run("nil attribute", func(t *testing.T) {
		var attr *Attribute
		support := attr.ReadSupport()
		if support.HasData || support.Readable || support.BlockingError != nil {
			t.Fatalf("unexpected nil attribute support: %+v", support)
		}
	})

	t.Run("resident named stream readable", func(t *testing.T) {
		attr := &Attribute{
			Header:   AttributeHeader{Type: AttrTypeData},
			Resident: &ResidentAttribute{Name: "Zone.Identifier", Value: []byte("abc")},
		}

		support := attr.ReadSupport()
		if !support.HasData || !support.Resident || !support.Readable {
			t.Fatalf("unexpected resident attribute support: %+v", support)
		}
	})

	t.Run("compressed non-resident named stream readable", func(t *testing.T) {
		attr := &Attribute{
			Header: AttributeHeader{Type: AttrTypeData, Flags: AttrFlagCompressed},
			NonResident: &NonResidentAttribute{
				Name: "stream",
			},
		}

		support := attr.ReadSupport()
		if !support.HasData || !support.NonResident || !support.Compressed || !support.Readable {
			t.Fatalf("unexpected compressed attribute support: %+v", support)
		}
	})

	t.Run("encrypted attribute blocked", func(t *testing.T) {
		attr := &Attribute{
			Header:      AttributeHeader{Type: AttrTypeData, Flags: AttrFlagEncrypted},
			NonResident: &NonResidentAttribute{},
		}

		support := attr.ReadSupport()
		if support.Readable {
			t.Fatalf("expected encrypted attribute to be unreadable: %+v", support)
		}
		if !errors.Is(support.BlockingError, ErrEncryptedData) {
			t.Fatalf("expected ErrEncryptedData, got %v", support.BlockingError)
		}
	})
}

// TestBufferBounds tests that buffer operations respect boundaries.
func TestBufferBounds(t *testing.T) {
	data := []byte{1, 2, 3, 4, 5}
	r := NewBinaryReader(data)

	// Read all data
	for i := 0; i < 5; i++ {
		_, err := r.ReadUint8()
		if err != nil {
			t.Fatalf("Unexpected error at position %d: %v", i, err)
		}
	}

	// Try to read beyond buffer
	_, err := r.ReadUint8()
	if err != ErrBufferTooSmall {
		t.Errorf("Expected ErrBufferTooSmall, got %v", err)
	}
}

func TestParseResidentAttributeLargeValueCopy(t *testing.T) {
	valueLen := 66000
	buf := make([]byte, ResidentAttributeHeaderSize+valueLen)

	WriteUint32LE(buf, AttributeHeaderSize, uint32(valueLen))
	WriteUint16LE(buf, AttributeHeaderSize+4, ResidentAttributeHeaderSize)

	for i := 0; i < valueLen; i++ {
		buf[ResidentAttributeHeaderSize+i] = byte(i % 251)
	}

	header := &AttributeHeader{Length: uint32(len(buf))}
	res, err := parseResidentAttribute(buf, header)
	if err != nil {
		t.Fatalf("parseResidentAttribute failed: %v", err)
	}

	if got := len(res.Value); got != valueLen {
		t.Fatalf("expected value length %d, got %d", valueLen, got)
	}

	if res.Value[50000] != byte(50000%251) {
		t.Fatalf("resident value copy truncated or corrupted at high offset")
	}
}

func TestParseIndexRootRejectsInvalidEntryBounds(t *testing.T) {
	data := make([]byte, 32)
	WriteUint32LE(data, 16, 0x1000)
	WriteUint32LE(data, 20, 0x1008)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseIndexRoot panicked on malformed input: %v", r)
		}
	}()

	_, err := parseIndexRoot(data)
	if err == nil {
		t.Fatal("expected error for invalid index root bounds")
	}
}

func TestParseBootSectorRejectsZeroIndexRecordSize(t *testing.T) {
	boot := make([]byte, BootSectorSize)

	boot[11] = 0x00
	boot[12] = 0x02
	boot[13] = 0x08

	boot[64] = 0xF6 // -10 => 1024-byte MFT records
	boot[68] = 0x00 // invalid index record size

	boot[510] = 0x55
	boot[511] = 0xAA

	v := &Volume{reader: &mockReaderAt{data: boot}}
	err := v.parseBootSector()
	if err == nil {
		t.Fatal("expected parseBootSector to reject zero index record size")
	}

	if !errors.Is(err, ErrInvalidBootSector) {
		t.Fatalf("expected ErrInvalidBootSector, got %v", err)
	}
}

func TestGetMFTEntryOffsetAcrossMultipleDataRuns(t *testing.T) {
	v := &Volume{
		bytesPerCluster: 4096,
		mftRecordSize:   1024,
		mftDataRuns: []DataRun{
			{StartCluster: 100, LengthClusters: 1},
			{StartCluster: 105, LengthClusters: 1},
		},
	}

	offset, err := v.getMFTEntryOffset(4)
	if err != nil {
		t.Fatalf("getMFTEntryOffset failed: %v", err)
	}

	expected := int64(105 * 4096)
	if offset != expected {
		t.Fatalf("expected offset %d, got %d", expected, offset)
	}
}

func TestReadDataRunsAcrossMultipleRuns(t *testing.T) {
	const clusterSize = 4096
	const secondCluster = 105

	disk := make([]byte, (secondCluster+1)*clusterSize)
	for i := 0; i < clusterSize; i++ {
		disk[100*clusterSize+i] = 'A'
		disk[secondCluster*clusterSize+i] = 'B'
	}

	v := &Volume{
		reader:          &mockReaderAt{data: disk},
		bytesPerCluster: clusterSize,
	}

	runs := []DataRun{
		{StartCluster: 100, LengthClusters: 1},
		{StartCluster: secondCluster, LengthClusters: 1},
	}

	buf := make([]byte, 8)
	n, err := v.ReadDataRuns(runs, clusterSize-4, buf)
	if err != nil {
		t.Fatalf("ReadDataRuns failed: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("expected %d bytes read, got %d", len(buf), n)
	}

	if string(buf) != "AAAABBBB" {
		t.Fatalf("expected AAAABBBB, got %q", string(buf))
	}
}

func TestNormalizeNTFSPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{".", "/"},
		{"Windows/System32", "/Windows/System32"},
		{"\\Windows\\System32\\", "/Windows/System32"},
		{"C:\\Windows\\System32", "/Windows/System32"},
		{"C:Windows\\Temp", "/Windows/Temp"},
	}

	for _, tt := range tests {
		got := normalizeNTFSPath(tt.in)
		if got != tt.want {
			t.Fatalf("normalizeNTFSPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAddIndexDirEntryPrefersLongOverDOS(t *testing.T) {
	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)

	dos := IndexEntry{
		FileReference: 42,
		SequenceNum:   1,
		FileName: &FileName{
			Name:      "PROGRA~1",
			Namespace: NamespaceDOS,
		},
	}
	long := IndexEntry{
		FileReference: 42,
		SequenceNum:   1,
		FileName: &FileName{
			Name:      "Program Files",
			Namespace: NamespaceWin32,
		},
	}

	entries = addIndexDirEntry(entries, dos, dosIndexByRef, longSeenByRef, seenByName, nil)
	entries = addIndexDirEntry(entries, long, dosIndexByRef, longSeenByRef, seenByName, nil)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "Program Files" {
		t.Fatalf("expected long name replacement, got %q", entries[0].Name)
	}
}

func TestAddIndexDirEntryKeepsDistinctNamesForSameEntry(t *testing.T) {
	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)

	nameA := IndexEntry{
		FileReference: 55,
		SequenceNum:   1,
		FileName: &FileName{
			Name:      "alpha.txt",
			Namespace: NamespaceWin32,
		},
	}
	nameB := IndexEntry{
		FileReference: 55,
		SequenceNum:   1,
		FileName: &FileName{
			Name:      "beta.txt",
			Namespace: NamespaceWin32,
		},
	}

	entries = addIndexDirEntry(entries, nameA, dosIndexByRef, longSeenByRef, seenByName, nil)
	entries = addIndexDirEntry(entries, nameB, dosIndexByRef, longSeenByRef, seenByName, nil)

	if len(entries) != 2 {
		t.Fatalf("expected 2 distinct names, got %d", len(entries))
	}

	gotNames := []string{entries[0].Name, entries[1].Name}
	wantNames := []string{"alpha.txt", "beta.txt"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("unexpected names: got %v want %v", gotNames, wantNames)
	}
}

func TestAddIndexDirEntryResolverOverridesFileNameType(t *testing.T) {
	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)

	idx := IndexEntry{
		FileReference: 77,
		SequenceNum:   1,
		FileName: &FileName{
			Name:           "maybe-dir",
			Namespace:      NamespaceWin32,
			FileAttributes: 0,
		},
	}

	resolver := func(entryNum uint64) (bool, error) {
		if entryNum != 77 {
			t.Fatalf("unexpected entry number: got %d", entryNum)
		}
		return true, nil
	}

	entries = addIndexDirEntry(entries, idx, dosIndexByRef, longSeenByRef, seenByName, resolver)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].IsDirectory {
		t.Fatal("expected entry to be directory based on resolver")
	}
}

func TestAddIndexDirEntryResolverFallbackOnError(t *testing.T) {
	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)

	idx := IndexEntry{
		FileReference: 88,
		SequenceNum:   1,
		FileName: &FileName{
			Name:           "known-file",
			Namespace:      NamespaceWin32,
			FileAttributes: uint64(FileAttrArchive),
		},
	}

	resolver := func(entryNum uint64) (bool, error) {
		return false, fmt.Errorf("lookup failed")
	}

	entries = addIndexDirEntry(entries, idx, dosIndexByRef, longSeenByRef, seenByName, resolver)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].IsDirectory {
		t.Fatal("expected fallback to file-name attributes when resolver fails")
	}
}

func TestAddIndexDirEntryPrefersAllocatedOverDeletedDuplicate(t *testing.T) {
	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)

	deleted := IndexEntry{
		FileReference: 91,
		SequenceNum:   2,
		Deleted:       true,
		FileName: &FileName{
			Name:      "report.docx",
			Namespace: NamespaceWin32,
		},
	}
	allocated := deleted
	allocated.Deleted = false

	entries = addIndexDirEntry(entries, deleted, dosIndexByRef, longSeenByRef, seenByName, nil)
	entries = addIndexDirEntry(entries, allocated, dosIndexByRef, longSeenByRef, seenByName, nil)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Deleted {
		t.Fatal("expected allocated entry to replace deleted duplicate")
	}
}

func TestParseIndexEntriesRecoverDeletedFromSlack(t *testing.T) {
	name := "gone.txt"
	nameLen := len(name)
	entryLen := AlignUp(16+66+nameLen*2, 4)
	buf := make([]byte, entryLen)

	buf[0] = 123
	WriteUint16LE(buf, 6, 7)
	WriteUint16LE(buf, 8, uint16(16)) // typical deleted index entry header length
	WriteUint16LE(buf, 10, 0)         // deleted entries often have strlen/stream len zeroed
	buf[12] = 0

	streamStart := 16
	buf[streamStart] = 5
	WriteUint16LE(buf, streamStart+6, 3)
	WriteUint64LE(buf, streamStart+56, uint64(FileAttrArchive))
	buf[streamStart+64] = uint8(nameLen)
	buf[streamStart+65] = NamespaceWin32

	nameBytes := utf16LEFromString(name)
	copy(buf[streamStart+66:], nameBytes)

	entries, err := parseIndexEntriesRecoverDeleted(buf, 16)
	if err != nil {
		t.Fatalf("parseIndexEntriesRecoverDeleted failed: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected recovered deleted entry")
	}
	if entries[0].FileName == nil {
		t.Fatal("expected recovered FileName")
	}
	if !entries[0].Deleted {
		t.Fatal("expected recovered entry to be marked deleted")
	}
	if entries[0].FileName.Name != name {
		t.Fatalf("unexpected recovered name: got %q want %q", entries[0].FileName.Name, name)
	}
}

func TestMFTParentLookupSequences(t *testing.T) {
	allocated := &MFTEntry{SequenceNum: 9, Flags: MFTFlagInUse | MFTFlagDirectory}
	got := mftParentLookupSequences(allocated)
	if !reflect.DeepEqual(got, []uint16{9}) {
		t.Fatalf("allocated sequence lookup mismatch: got %v", got)
	}

	deleted := &MFTEntry{SequenceNum: 9, Flags: MFTFlagDirectory}
	got = mftParentLookupSequences(deleted)
	if !reflect.DeepEqual(got, []uint16{9, 8}) {
		t.Fatalf("deleted sequence lookup mismatch: got %v", got)
	}
}

func TestVolumeMaxMFTEntries(t *testing.T) {
	v := &Volume{
		bytesPerCluster: 4096,
		mftRecordSize:   1024,
		mftDataRuns: []DataRun{
			{LengthClusters: 1},
			{LengthClusters: 2},
		},
	}

	got := v.maxMFTEntries()
	if got != 12 {
		t.Fatalf("maxMFTEntries mismatch: got %d want %d", got, 12)
	}
}

func utf16LEFromString(s string) []byte {
	runes := []rune(s)
	out := make([]byte, len(runes)*2)
	for i, r := range runes {
		WriteUint16LE(out, i*2, uint16(r))
	}
	return out
}

func TestWrapPathErrorNil(t *testing.T) {
	err := wrapPathError("lookup", "/a", "/a", nil)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWrapPathErrorPreservesCauseAndContext(t *testing.T) {
	err := wrapPathError("lookup", "/Windows/System32/nope.dll", "/Windows/System32/nope.dll",
		fmt.Errorf("%w: /Windows/System32/nope.dll", ErrFileNotFound))

	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected errors.Is(..., ErrFileNotFound) to be true, got %v", err)
	}

	var pathErr *PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("expected PathError, got %T", err)
	}

	if pathErr.Op != "lookup" {
		t.Fatalf("unexpected op: %q", pathErr.Op)
	}
	if pathErr.Path != "/Windows/System32/nope.dll" {
		t.Fatalf("unexpected path: %q", pathErr.Path)
	}
	if pathErr.Component != "/Windows/System32/nope.dll" {
		t.Fatalf("unexpected component: %q", pathErr.Component)
	}
}

// Benchmark tests

func BenchmarkBinaryReaderUint32(b *testing.B) {
	data := make([]byte, 1024)
	for i := 0; i < len(data); i += 4 {
		data[i] = byte(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewBinaryReader(data)
		for r.Offset < len(data)-4 {
			r.ReadUint32()
		}
	}
}

func BenchmarkAlignUp(b *testing.B) {
	for i := 0; i < b.N; i++ {
		AlignUp(i%1000, 8)
	}
}

func BenchmarkNTFSTimeConversion(b *testing.B) {
	ntfsTime := uint64(132231936000000000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NTFSTimeToTime(ntfsTime)
	}
}
