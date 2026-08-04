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

type mockReadWriterAt struct {
	data []byte
}

func (m *mockReadWriterAt) ReadAt(p []byte, off int64) (int, error) {
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

func (m *mockReadWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}

	n := copy(m.data[off:], p)
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

func TestVolumeWriteAtSectorAlignedFallback(t *testing.T) {
	backend := &mockReadWriterAt{data: []byte("0123456789ABCDEF")}
	v := &Volume{
		reader:         backend,
		writer:         backend,
		bytesPerSector: 8,
	}

	n, err := v.WriteAt([]byte("XY"), 3)
	if err != nil {
		t.Fatalf("WriteAt returned unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("WriteAt bytes written = %d, want 2", n)
	}

	got := string(backend.data)
	want := "012XY56789ABCDEF"
	if got != want {
		t.Fatalf("backend after write = %q, want %q", got, want)
	}
}

func TestVolumeWriteAtAlignedPassthrough(t *testing.T) {
	backend := &mockReadWriterAt{data: []byte("0123456789ABCDEF")}
	v := &Volume{
		reader:         backend,
		writer:         backend,
		bytesPerSector: 8,
	}

	n, err := v.WriteAt([]byte("WXYZ1234"), 8)
	if err != nil {
		t.Fatalf("WriteAt returned unexpected error: %v", err)
	}
	if n != 8 {
		t.Fatalf("WriteAt bytes written = %d, want 8", n)
	}

	got := string(backend.data)
	want := "01234567WXYZ1234"
	if got != want {
		t.Fatalf("backend after write = %q, want %q", got, want)
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

func TestFileReadSequentialResident(t *testing.T) {
	f := &File{
		volume: &Volume{},
		isDir:  false,
		size:   7,
		dataAttr: &Attribute{
			Resident: &ResidentAttribute{Value: []byte("ABCDEFG")},
		},
	}

	buf := make([]byte, 3)

	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("first Read returned unexpected error: %v", err)
	}
	if n != 3 || string(buf[:n]) != "ABC" {
		t.Fatalf("first Read = (%d, %q), want (3, %q)", n, string(buf[:n]), "ABC")
	}

	n, err = f.Read(buf)
	if err != nil {
		t.Fatalf("second Read returned unexpected error: %v", err)
	}
	if n != 3 || string(buf[:n]) != "DEF" {
		t.Fatalf("second Read = (%d, %q), want (3, %q)", n, string(buf[:n]), "DEF")
	}

	n, err = f.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("third Read error = %v, want EOF", err)
	}
	if n != 1 || string(buf[:n]) != "G" {
		t.Fatalf("third Read = (%d, %q), want (1, %q)", n, string(buf[:n]), "G")
	}

	n, err = f.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("fourth Read error = %v, want EOF", err)
	}
	if n != 0 {
		t.Fatalf("fourth Read bytes = %d, want 0", n)
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

func TestFileWriteAtAcrossNonResidentRuns(t *testing.T) {
	const clusterSize = uint32(8)
	backend := &mockReadWriterAt{data: make([]byte, 64)}

	v := &Volume{
		reader:          backend,
		writer:          backend,
		bytesPerCluster: clusterSize,
	}

	f := &File{
		volume: v,
		size:   16,
		dataAttr: &Attribute{
			NonResident: &NonResidentAttribute{
				RealSize: 16,
				DataRuns: []DataRun{
					{LengthClusters: 1, StartCluster: 1},
					{LengthClusters: 1, StartCluster: 3},
				},
			},
		},
	}

	n, err := f.WriteAt([]byte("WXYZ"), 6)
	if err != nil {
		t.Fatalf("WriteAt returned unexpected error: %v", err)
	}
	if n != 4 {
		t.Fatalf("WriteAt bytes written = %d, want 4", n)
	}

	if got := backend.data[14]; got != 'W' {
		t.Fatalf("byte at offset 14 = %q, want 'W'", got)
	}
	if got := backend.data[15]; got != 'X' {
		t.Fatalf("byte at offset 15 = %q, want 'X'", got)
	}
	if got := backend.data[24]; got != 'Y' {
		t.Fatalf("byte at offset 24 = %q, want 'Y'", got)
	}
	if got := backend.data[25]; got != 'Z' {
		t.Fatalf("byte at offset 25 = %q, want 'Z'", got)
	}
}

func TestFileWriteAtReadOnlyVolume(t *testing.T) {
	f := &File{
		volume: &Volume{reader: &mockReaderAt{data: make([]byte, 64)}, bytesPerCluster: 8},
		size:   8,
		dataAttr: &Attribute{
			NonResident: &NonResidentAttribute{DataRuns: []DataRun{{LengthClusters: 1, StartCluster: 1}}},
		},
	}

	_, err := f.WriteAt([]byte("A"), 0)
	if !errors.Is(err, ErrReadOnlyVolume) {
		t.Fatalf("expected ErrReadOnlyVolume, got %v", err)
	}
}

func TestFileWriteAtResidentUnsupported(t *testing.T) {
	backend := &mockReadWriterAt{data: make([]byte, 64)}
	f := &File{
		volume: &Volume{reader: backend, writer: backend},
		size:   4,
		dataAttr: &Attribute{
			Resident: &ResidentAttribute{Value: []byte("ABCD")},
		},
	}

	_, err := f.WriteAt([]byte("Z"), 0)
	if !errors.Is(err, ErrWriteUnsupported) {
		t.Fatalf("expected ErrWriteUnsupported, got %v", err)
	}
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

func TestFindPrimaryDataAttributePrefersUnnamed(t *testing.T) {
	entry := &MFTEntry{
		Attributes: []*Attribute{
			{
				Header:   AttributeHeader{Type: AttrTypeData},
				Resident: &ResidentAttribute{Name: "Zone.Identifier", Value: []byte("named")},
			},
			{
				Header:      AttributeHeader{Type: AttrTypeData},
				NonResident: &NonResidentAttribute{Name: "", RealSize: 2048},
			},
		},
	}

	attr := entry.FindPrimaryDataAttribute()
	if attr == nil {
		t.Fatal("expected primary data attribute")
	}

	if attr.NonResident == nil || attr.NonResident.Name != "" {
		t.Fatal("expected unnamed non-resident stream to be selected")
	}
}

func TestFindPrimaryNonResidentDataAttributePrefersUnnamedNonResident(t *testing.T) {
	entry := &MFTEntry{
		Attributes: []*Attribute{
			{
				Header:      AttributeHeader{Type: AttrTypeData},
				NonResident: &NonResidentAttribute{Name: "named", RealSize: 1024},
			},
			{
				Header:      AttributeHeader{Type: AttrTypeData},
				NonResident: &NonResidentAttribute{Name: "", RealSize: 4096},
			},
		},
	}

	attr := entry.FindPrimaryNonResidentDataAttribute()
	if attr == nil {
		t.Fatal("expected non-resident primary data attribute")
	}

	if attr.NonResident == nil || attr.NonResident.Name != "" || attr.NonResident.RealSize != 4096 {
		t.Fatal("expected unnamed non-resident stream to be selected")
	}
}

func TestOpenPrefersUnnamedDataForSize(t *testing.T) {
	entry := &MFTEntry{
		Flags: MFTFlagInUse,
		Attributes: []*Attribute{
			{
				Header: AttributeHeader{Type: AttrTypeFileName},
				Resident: &ResidentAttribute{
					Value: buildFileNameAttrValue(5, 1, "sample.bin", NamespaceWin32, 8192, 8192, FileAttrArchive),
				},
			},
			{
				Header:   AttributeHeader{Type: AttrTypeData},
				Resident: &ResidentAttribute{Name: "Zone.Identifier", Value: []byte("ads")},
			},
			{
				Header:      AttributeHeader{Type: AttrTypeData},
				NonResident: &NonResidentAttribute{Name: "", RealSize: 8192},
			},
		},
	}

	v := &Volume{
		mftCache: map[uint64]*MFTEntry{42: entry},
	}

	f, err := v.Open(42)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if f.Size() != 8192 {
		t.Fatalf("expected unnamed stream size 8192, got %d", f.Size())
	}

	if f.dataAttr == nil || f.dataAttr.NonResident == nil || f.dataAttr.NonResident.Name != "" {
		t.Fatal("expected Open to select unnamed primary data stream")
	}
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

func buildFileNameAttrValue(parentRef uint64, parentSeq uint16, name string, namespace uint8,
	realSize uint64, allocatedSize uint64, attrs uint64,
) []byte {
	nameBytes := utf16LEFromString(name)
	buf := make([]byte, 66+len(nameBytes))

	WriteUint64LE(buf, 0, parentRef)
	WriteUint16LE(buf, 8, parentSeq)
	WriteUint64LE(buf, 40, allocatedSize)
	WriteUint64LE(buf, 48, realSize)
	WriteUint64LE(buf, 56, attrs)
	buf[64] = uint8(len([]rune(name)))
	buf[65] = namespace
	copy(buf[66:], nameBytes)

	return buf
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

// Standalone MFT record parsing

// testRecordOpts describes a synthetic MFT record for buildTestMFTRecord.
type testRecordOpts struct {
	recordSize    uint32 // 0 means DefaultMFTRecordSize
	magic         uint32 // 0 means MFTMagicFILE
	sequenceNum   uint16
	hardLinkCount uint16
	flags         uint16
	name          string
	namespace     uint8
	createTime    time.Time
	modifyTime    time.Time
	mftChangeTime time.Time
	accessTime    time.Time
	extraAttrs    [][]byte // Appended after $STANDARD_INFORMATION and $FILE_NAME
}

// buildResidentAttr assembles a resident attribute with no attribute name.
func buildResidentAttr(attrType uint32, attrID uint16, value []byte) []byte {
	length := AlignUp(ResidentAttributeHeaderSize+len(value), 8)
	attr := make([]byte, length)

	WriteUint32LE(attr, 0, attrType)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 0 // Resident
	attr[9] = 0 // No attribute name
	WriteUint16LE(attr, 10, ResidentAttributeHeaderSize)
	WriteUint16LE(attr, 14, attrID)
	WriteUint32LE(attr, 16, uint32(len(value)))
	WriteUint16LE(attr, 20, ResidentAttributeHeaderSize)
	copy(attr[ResidentAttributeHeaderSize:], value)

	return attr
}

// buildStandardInfoAttrValue builds a $STANDARD_INFORMATION value including the
// Windows 2000+ extended fields.
func buildStandardInfoAttrValue(create, modify, mftChange, access time.Time, fileAttrs uint32) []byte {
	buf := make([]byte, 72)

	WriteUint64LE(buf, 0, TimeToNTFSTime(create))
	WriteUint64LE(buf, 8, TimeToNTFSTime(modify))
	WriteUint64LE(buf, 16, TimeToNTFSTime(mftChange))
	WriteUint64LE(buf, 24, TimeToNTFSTime(access))
	WriteUint32LE(buf, 32, fileAttrs)

	return buf
}

// buildTestMFTRecord assembles a complete, fixup-stamped MFT record holding a
// $STANDARD_INFORMATION and a $FILE_NAME attribute.
func buildTestMFTRecord(t *testing.T, opts testRecordOpts) []byte {
	t.Helper()

	recordSize := opts.recordSize
	if recordSize == 0 {
		recordSize = DefaultMFTRecordSize
	}

	magic := opts.magic
	if magic == 0 {
		magic = MFTMagicFILE
	}

	const usaOffset = MFTEntryHeaderSize
	usaSize := uint16(recordSize/UpdateSequenceStride) + 1
	firstAttrOffset := AlignUp(usaOffset+int(usaSize)*2, 8)

	buf := make([]byte, recordSize)

	si := buildResidentAttr(AttrTypeStandardInfo, 0, buildStandardInfoAttrValue(
		opts.createTime, opts.modifyTime, opts.mftChangeTime, opts.accessTime, 0x20,
	))
	fn := buildResidentAttr(AttrTypeFileName, 1, buildFileNameAttrValue(
		5, 5, opts.name, opts.namespace, 4096, 4096, 0x20,
	))

	offset := firstAttrOffset
	for _, attr := range append([][]byte{si, fn}, opts.extraAttrs...) {
		if offset+len(attr) > len(buf) {
			t.Fatalf("test record too small for attributes: need %d, have %d", offset+len(attr), len(buf))
		}
		copy(buf[offset:], attr)
		offset += len(attr)
	}

	WriteUint32LE(buf, offset, 0xFFFFFFFF) // End-of-attributes marker
	usedSize := AlignUp(offset+4, 8)

	WriteUint32LE(buf, 0, magic)
	WriteUint16LE(buf, 4, usaOffset)
	WriteUint16LE(buf, 6, usaSize)
	WriteUint64LE(buf, 8, 0x1122334455667788) // $LogFile sequence number
	WriteUint16LE(buf, 16, opts.sequenceNum)
	WriteUint16LE(buf, 18, opts.hardLinkCount)
	WriteUint16LE(buf, 20, uint16(firstAttrOffset))
	WriteUint16LE(buf, 22, opts.flags)
	WriteUint32LE(buf, 24, uint32(usedSize))
	WriteUint32LE(buf, 28, recordSize)
	WriteUint64LE(buf, 32, 0) // Base record reference
	WriteUint16LE(buf, 40, 2) // Next attribute ID

	// Stamp the update sequence array the way NTFS does: stash the real last two
	// bytes of every sector in the array, then write the USN in their place.
	const usn = 0x0A0B
	WriteUint16LE(buf, usaOffset, usn)
	for i := 0; i < int(usaSize)-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		WriteUint16LE(buf, usaOffset+2+i*2, ReadUint16LE(buf, sectorEnd))
		WriteUint16LE(buf, sectorEnd, usn)
	}

	return buf
}

// newTestMFTVolume builds an in-memory volume whose $MFT occupies a single
// cluster run holding the supplied records in order.
func newTestMFTVolume(t *testing.T, records [][]byte) *Volume {
	t.Helper()

	const (
		clusterSize  = 4096
		recordSize   = DefaultMFTRecordSize
		startCluster = 10
	)

	disk := make([]byte, (startCluster+1)*clusterSize)
	for i, record := range records {
		offset := startCluster*clusterSize + i*recordSize
		if offset+len(record) > len(disk) {
			t.Fatalf("record %d does not fit in the test $MFT run", i)
		}
		copy(disk[offset:], record)
	}

	return &Volume{
		reader:          &mockReaderAt{data: disk},
		bytesPerCluster: clusterSize,
		mftRecordSize:   recordSize,
		mftDataRuns:     []DataRun{{StartCluster: startCluster, LengthClusters: 1}},
		mftCache:        make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
	}
}

// buildNonResidentDataAttr builds an unnamed non-resident $DATA attribute backed
// by a single data run. startCluster and clusters must each fit in one byte.
func buildNonResidentDataAttr(t *testing.T, attrID uint16, startCluster, clusters uint64, clusterSize uint32) []byte {
	t.Helper()

	if startCluster > 0x7F || clusters > 0x7F {
		t.Fatalf("test data run needs single-byte fields, got start=%d clusters=%d", startCluster, clusters)
	}

	// Data run: one length byte, one offset byte, then the list terminator.
	run := []byte{0x11, byte(clusters), byte(startCluster), 0x00}

	const dataRunOffset = NonResidentAttributeHeaderSize
	length := AlignUp(dataRunOffset+len(run), 8)
	attr := make([]byte, length)

	realSize := clusters * uint64(clusterSize)

	WriteUint32LE(attr, 0, AttrTypeData)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1 // Non-resident
	attr[9] = 0 // No attribute name
	WriteUint16LE(attr, 10, NonResidentAttributeHeaderSize)
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0)          // Starting VCN
	WriteUint64LE(attr, 24, clusters-1) // Last VCN
	WriteUint16LE(attr, 32, dataRunOffset)
	WriteUint64LE(attr, 40, realSize) // Allocated size
	WriteUint64LE(attr, 48, realSize) // Real size
	WriteUint64LE(attr, 56, realSize) // Initialized size
	copy(attr[dataRunOffset:], run)

	return attr
}

// buildTestBootSector builds a boot sector describing 1024-byte MFT records and
// 4096-byte index records.
func buildTestBootSector(bytesPerSector uint16, sectorsPerCluster uint8, totalSectors, mftCluster uint64) []byte {
	bs := make([]byte, BootSectorSize)

	bs[0], bs[1], bs[2] = 0xEB, 0x52, 0x90 // Jump instruction
	copy(bs[3:11], BootSectorOEMNameNTFS)
	WriteUint16LE(bs, 11, bytesPerSector)
	bs[13] = sectorsPerCluster
	WriteUint64LE(bs, 40, totalSectors)
	WriteUint64LE(bs, 48, mftCluster)
	WriteUint64LE(bs, 56, mftCluster) // $MFTMirr, unused by these tests
	bs[64] = 0xF6                     // -10, so 2^10 = 1024-byte MFT records
	bs[68] = 0xF4                     // -12, so 2^12 = 4096-byte index records
	WriteUint64LE(bs, 72, 0x1234567890ABCDEF)
	WriteUint16LE(bs, 510, BootSectorMagic)

	return bs
}

// buildTestNTFSImage assembles a minimal but genuinely openable NTFS image whose
// $MFT holds the supplied records. Record 0 is generated as $MFT itself, with a
// non-resident $DATA run covering the MFT, so Open and initializeMFT work.
func buildTestNTFSImage(t *testing.T, records [][]byte) []byte {
	t.Helper()

	const (
		bytesPerSector    = 512
		sectorsPerCluster = 8
		clusterSize       = bytesPerSector * sectorsPerCluster
		recordSize        = DefaultMFTRecordSize
		mftCluster        = 4
		totalClusters     = 16
	)

	// Records per cluster is exact for these sizes, so the MFT covers every slot.
	recordsPerCluster := clusterSize / recordSize
	mftClusters := uint64((len(records) + 1 + recordsPerCluster - 1) / recordsPerCluster)
	if mftClusters == 0 {
		mftClusters = 1
	}

	mftRecord := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "$MFT",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNonResidentDataAttr(t, 2, mftCluster, mftClusters, clusterSize),
		},
	})

	image := make([]byte, totalClusters*clusterSize)
	copy(image, buildTestBootSector(bytesPerSector, sectorsPerCluster,
		totalClusters*sectorsPerCluster, mftCluster))

	for i, record := range append([][]byte{mftRecord}, records...) {
		offset := mftCluster*clusterSize + i*recordSize
		if offset+len(record) > len(image) {
			t.Fatalf("record %d does not fit in the test image", i)
		}
		copy(image[offset:], record)
	}

	return image
}

func TestOpenImageAndWalkMFT(t *testing.T) {
	good := func(name string) []byte {
		return buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      name,
			namespace: NamespaceWin32,
		})
	}

	bad := good("corrupt.bin")
	WriteUint32LE(bad, 0, MFTMagicBAAD)

	// Slot 3 is left unallocated, and slot 4 is marked bad. Both sit between
	// good records so the walk has to step over them, not stop.
	image := buildTestNTFSImage(t, [][]byte{good("notes.txt"), bad, nil, good("last.dat")})

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	if vol.MFTRecordSize() != DefaultMFTRecordSize {
		t.Fatalf("MFT record size mismatch: got %d want %d", vol.MFTRecordSize(), DefaultMFTRecordSize)
	}

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}
	if count == 0 {
		t.Fatal("expected a non-zero MFT entry count")
	}
	if _, err := vol.GetMFTEntry(count); err == nil {
		t.Fatalf("expected entry %d to be out of range", count)
	}

	var names []string
	if err := vol.EachMFTEntry(func(entryNum uint64, e *MFTEntry) error {
		if entryNum >= count {
			t.Fatalf("walked past the reported bound: entry %d of %d", entryNum, count)
		}
		name, err := e.GetFileName()
		if err != nil {
			return nil
		}
		names = append(names, name.Name)
		return nil
	}); err != nil {
		t.Fatalf("EachMFTEntry failed: %v", err)
	}

	want := []string{"$MFT", "notes.txt", "last.dat"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("walk mismatch: got %v want %v", names, want)
	}
}

func TestParseMFTRecordParityWithOpenedImage(t *testing.T) {
	const (
		clusterSize = 4096
		mftCluster  = 4
		recordSize  = DefaultMFTRecordSize
	)

	create := time.Date(2022, 11, 9, 8, 7, 6, 0, time.UTC)
	record := buildTestMFTRecord(t, testRecordOpts{
		sequenceNum:   4,
		hardLinkCount: 2,
		flags:         MFTFlagInUse,
		name:          "parity.dat",
		namespace:     NamespaceWin32,
		createTime:    create,
		modifyTime:    create.Add(time.Hour),
		mftChangeTime: create.Add(2 * time.Hour),
		accessTime:    create.Add(3 * time.Hour),
	})

	image := buildTestNTFSImage(t, [][]byte{record})

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	// Entry 1, since the image generates $MFT into entry 0.
	viaVolume, err := vol.GetMFTEntry(1)
	if err != nil {
		t.Fatalf("GetMFTEntry failed: %v", err)
	}

	// Lift the same record straight out of the image, the way a caller holding
	// only an extracted $MFT would see it.
	offset := mftCluster*clusterSize + recordSize
	raw := make([]byte, recordSize)
	copy(raw, image[offset:offset+recordSize])

	viaBuffer, err := ParseMFTRecord(raw, 1, vol.MFTRecordSize())
	if err != nil {
		t.Fatalf("ParseMFTRecord failed: %v", err)
	}

	if viaBuffer.Magic != viaVolume.Magic ||
		viaBuffer.Flags != viaVolume.Flags ||
		viaBuffer.SequenceNum != viaVolume.SequenceNum ||
		viaBuffer.HardLinkCount != viaVolume.HardLinkCount ||
		viaBuffer.UsedSize != viaVolume.UsedSize ||
		viaBuffer.AllocatedSize != viaVolume.AllocatedSize {
		t.Fatalf("header mismatch:\nstandalone %+v\nvolume     %+v", viaBuffer, viaVolume)
	}

	bufName, err := viaBuffer.GetFileName()
	if err != nil {
		t.Fatalf("standalone GetFileName failed: %v", err)
	}
	volName, err := viaVolume.GetFileName()
	if err != nil {
		t.Fatalf("volume GetFileName failed: %v", err)
	}
	if !reflect.DeepEqual(bufName, volName) {
		t.Fatalf("file name mismatch:\nstandalone %+v\nvolume     %+v", bufName, volName)
	}

	bufSI, err := viaBuffer.GetStandardInformation()
	if err != nil {
		t.Fatalf("standalone GetStandardInformation failed: %v", err)
	}
	volSI, err := viaVolume.GetStandardInformation()
	if err != nil {
		t.Fatalf("volume GetStandardInformation failed: %v", err)
	}
	if !reflect.DeepEqual(bufSI, volSI) {
		t.Fatalf("standard information mismatch:\nstandalone %+v\nvolume     %+v", bufSI, volSI)
	}
}

func TestParseMFTRecordRoundTrip(t *testing.T) {
	create := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	modify := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	mftChange := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	access := time.Date(2024, 4, 5, 6, 7, 8, 0, time.UTC)

	record := buildTestMFTRecord(t, testRecordOpts{
		sequenceNum:   7,
		hardLinkCount: 2,
		flags:         MFTFlagInUse | MFTFlagDirectory,
		name:          "Windows",
		namespace:     NamespaceWin32,
		createTime:    create,
		modifyTime:    modify,
		mftChangeTime: mftChange,
		accessTime:    access,
	})

	entry, err := ParseMFTRecord(record, 42, DefaultMFTRecordSize)
	if err != nil {
		t.Fatalf("ParseMFTRecord failed: %v", err)
	}

	if entry.Magic != MFTMagicFILE {
		t.Fatalf("magic mismatch: got 0x%X want 0x%X", entry.Magic, MFTMagicFILE)
	}
	if entry.RecordNumber != 42 {
		t.Fatalf("record number mismatch: got %d want 42", entry.RecordNumber)
	}
	if entry.SequenceNum != 7 {
		t.Fatalf("sequence number mismatch: got %d want 7", entry.SequenceNum)
	}
	if entry.HardLinkCount != 2 {
		t.Fatalf("hard link count mismatch: got %d want 2", entry.HardLinkCount)
	}
	if !entry.IsInUse() {
		t.Fatal("expected entry to be in use")
	}
	if !entry.IsDirectory() {
		t.Fatal("expected entry to be a directory")
	}

	name, err := entry.GetFileName()
	if err != nil {
		t.Fatalf("GetFileName failed: %v", err)
	}
	if name.Name != "Windows" {
		t.Fatalf("name mismatch: got %q want %q", name.Name, "Windows")
	}

	si, err := entry.GetStandardInformation()
	if err != nil {
		t.Fatalf("GetStandardInformation failed: %v", err)
	}

	macTimes := []struct {
		label string
		got   time.Time
		want  time.Time
	}{
		{"create", si.CreateTime, create},
		{"modify", si.ModifyTime, modify},
		{"mftChange", si.MFTChangeTime, mftChange},
		{"access", si.AccessTime, access},
	}
	for _, tc := range macTimes {
		want := NTFSTimeToTime(TimeToNTFSTime(tc.want))
		if !tc.got.Equal(want) {
			t.Fatalf("%s time mismatch: got %v want %v", tc.label, tc.got, want)
		}
	}
}

func TestParseMFTRecordDefaultRecordSize(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "notes.txt",
		namespace: NamespaceWin32,
	})

	entry, err := ParseMFTRecord(record, 0, 0)
	if err != nil {
		t.Fatalf("ParseMFTRecord with default record size failed: %v", err)
	}

	if entry.AllocatedSize != DefaultMFTRecordSize {
		t.Fatalf("allocated size mismatch: got %d want %d", entry.AllocatedSize, DefaultMFTRecordSize)
	}

	name, err := entry.GetFileName()
	if err != nil {
		t.Fatalf("GetFileName failed: %v", err)
	}
	if name.Name != "notes.txt" {
		t.Fatalf("name mismatch: got %q", name.Name)
	}
}

func TestParseMFTRecordRejectsBadRecordSize(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{name: "a.txt", namespace: NamespaceWin32})

	for _, recordSize := range []uint32{256, MaxMFTEntrySize * 2} {
		_, err := ParseMFTRecord(record, 0, recordSize)
		if err == nil {
			t.Fatalf("expected record size %d to be rejected", recordSize)
		}
		if !errors.Is(err, ErrInvalidMFTEntry) {
			t.Fatalf("record size %d: expected ErrInvalidMFTEntry, got %v", recordSize, err)
		}
	}
}

func TestParseMFTRecordShortBuffer(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{name: "a.txt", namespace: NamespaceWin32})

	_, err := ParseMFTRecord(record[:512], 0, DefaultMFTRecordSize)
	if err == nil {
		t.Fatal("expected short buffer to be rejected")
	}
	if !errors.Is(err, ErrInvalidMFTEntry) {
		t.Fatalf("expected ErrInvalidMFTEntry, got %v", err)
	}
}

func TestParseMFTRecordBadMagic(t *testing.T) {
	tests := []struct {
		name  string
		magic uint32
	}{
		{"unallocated", MFTMagicZERO},
		{"bad", MFTMagicBAAD},
		{"garbage", 0xDEADBEEF},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := buildTestMFTRecord(t, testRecordOpts{name: "a.txt", namespace: NamespaceWin32})
			WriteUint32LE(record, 0, tc.magic)

			_, err := ParseMFTRecord(record, 0, DefaultMFTRecordSize)
			if err == nil {
				t.Fatalf("expected magic 0x%X to be rejected", tc.magic)
			}
			if !errors.Is(err, ErrInvalidMFTEntry) {
				t.Fatalf("expected ErrInvalidMFTEntry, got %v", err)
			}
		})
	}
}

func TestParseMFTRecordFixupMismatch(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{name: "a.txt", namespace: NamespaceWin32})

	// Corrupt the USN stamped at the end of the first sector.
	WriteUint16LE(record, UpdateSequenceStride-2, 0xFFFF)

	_, err := ParseMFTRecord(record, 0, DefaultMFTRecordSize)
	if err == nil {
		t.Fatal("expected fixup mismatch to be rejected")
	}
	if !errors.Is(err, ErrUpdateSequence) {
		t.Fatalf("expected ErrUpdateSequence, got %v", err)
	}
}

func TestParseMFTRecordParityWithVolume(t *testing.T) {
	create := time.Date(2023, 6, 1, 12, 0, 0, 0, time.UTC)
	record := buildTestMFTRecord(t, testRecordOpts{
		sequenceNum:   3,
		hardLinkCount: 1,
		flags:         MFTFlagInUse,
		name:          "parity.dat",
		namespace:     NamespaceWin32,
		createTime:    create,
		modifyTime:    create.Add(time.Hour),
		mftChangeTime: create.Add(2 * time.Hour),
		accessTime:    create.Add(3 * time.Hour),
	})

	// Both paths apply fixups in place, so each needs its own copy.
	standalone := make([]byte, len(record))
	copy(standalone, record)

	v := newTestMFTVolume(t, [][]byte{record})

	viaVolume, err := v.GetMFTEntry(0)
	if err != nil {
		t.Fatalf("GetMFTEntry failed: %v", err)
	}

	viaBuffer, err := ParseMFTRecord(standalone, 0, v.MFTRecordSize())
	if err != nil {
		t.Fatalf("ParseMFTRecord failed: %v", err)
	}

	if viaBuffer.Magic != viaVolume.Magic ||
		viaBuffer.Flags != viaVolume.Flags ||
		viaBuffer.SequenceNum != viaVolume.SequenceNum ||
		viaBuffer.HardLinkCount != viaVolume.HardLinkCount ||
		viaBuffer.UsedSize != viaVolume.UsedSize ||
		viaBuffer.AllocatedSize != viaVolume.AllocatedSize {
		t.Fatalf("header mismatch:\nstandalone %+v\nvolume     %+v", viaBuffer, viaVolume)
	}

	if len(viaBuffer.Attributes) != len(viaVolume.Attributes) {
		t.Fatalf("attribute count mismatch: got %d want %d",
			len(viaBuffer.Attributes), len(viaVolume.Attributes))
	}
	for i := range viaBuffer.Attributes {
		if viaBuffer.Attributes[i].Header.Type != viaVolume.Attributes[i].Header.Type {
			t.Fatalf("attribute %d type mismatch: got 0x%X want 0x%X", i,
				viaBuffer.Attributes[i].Header.Type, viaVolume.Attributes[i].Header.Type)
		}
	}

	bufName, err := viaBuffer.GetFileName()
	if err != nil {
		t.Fatalf("standalone GetFileName failed: %v", err)
	}
	volName, err := viaVolume.GetFileName()
	if err != nil {
		t.Fatalf("volume GetFileName failed: %v", err)
	}
	if bufName.Name != volName.Name {
		t.Fatalf("name mismatch: standalone %q volume %q", bufName.Name, volName.Name)
	}

	bufSI, err := viaBuffer.GetStandardInformation()
	if err != nil {
		t.Fatalf("standalone GetStandardInformation failed: %v", err)
	}
	volSI, err := viaVolume.GetStandardInformation()
	if err != nil {
		t.Fatalf("volume GetStandardInformation failed: %v", err)
	}
	if !reflect.DeepEqual(bufSI, volSI) {
		t.Fatalf("standard information mismatch:\nstandalone %+v\nvolume     %+v", bufSI, volSI)
	}
}

func TestMFTEntryCount(t *testing.T) {
	records := make([][]byte, 4)
	for i := range records {
		records[i] = buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      fmt.Sprintf("file%d.txt", i),
			namespace: NamespaceWin32,
		})
	}

	v := newTestMFTVolume(t, records)

	count, err := v.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}
	if count != uint64(len(records)) {
		t.Fatalf("count mismatch: got %d want %d", count, len(records))
	}

	if _, err := v.GetMFTEntry(count - 1); err != nil {
		t.Fatalf("last entry should be addressable: %v", err)
	}
	if _, err := v.GetMFTEntry(count); err == nil {
		t.Fatal("expected entry at count to be out of range")
	}
}

func TestMFTEntryCountUninitialized(t *testing.T) {
	empty := &Volume{mftRecordSize: DefaultMFTRecordSize}
	if _, err := empty.MFTEntryCount(); !errors.Is(err, ErrInvalidMFTEntry) {
		t.Fatalf("expected ErrInvalidMFTEntry for uninitialized MFT, got %v", err)
	}

	v := newTestMFTVolume(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{name: "a.txt", namespace: NamespaceWin32}),
	})
	if err := v.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := v.MFTEntryCount(); !errors.Is(err, ErrVolumeClosed) {
		t.Fatalf("expected ErrVolumeClosed, got %v", err)
	}
}

func TestEachMFTEntry(t *testing.T) {
	good := func(name string) []byte {
		return buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      name,
			namespace: NamespaceWin32,
		})
	}

	bad := good("bad.txt")
	WriteUint32LE(bad, 0, MFTMagicBAAD)

	// Entry 2 is left as zeros, standing in for an unallocated record.
	v := newTestMFTVolume(t, [][]byte{good("first.txt"), bad, nil, good("last.txt")})

	var visited []uint64
	if err := v.EachMFTEntry(func(entryNum uint64, e *MFTEntry) error {
		if e == nil {
			t.Fatalf("entry %d: nil entry passed to callback", entryNum)
		}
		visited = append(visited, entryNum)
		return nil
	}); err != nil {
		t.Fatalf("EachMFTEntry failed: %v", err)
	}

	if !reflect.DeepEqual(visited, []uint64{0, 3}) {
		t.Fatalf("expected to visit entries 0 and 3, visited %v", visited)
	}

	sentinel := errors.New("stop")
	var seen int
	err := v.EachMFTEntry(func(entryNum uint64, e *MFTEntry) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error to propagate, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected iteration to stop after the first callback, ran %d times", seen)
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

func BenchmarkGetMFTEntryOffsetManyRuns(b *testing.B) {
	const (
		clusterSize = uint32(4096)
		runCount    = 4096
	)

	runs := make([]DataRun, runCount)
	for i := 0; i < runCount; i++ {
		runs[i] = DataRun{LengthClusters: 1, StartCluster: int64(i + 100)}
	}

	v := &Volume{
		bytesPerCluster: clusterSize,
		mftRecordSize:   1024,
		mftDataRuns:     runs,
	}
	v.rebuildMFTRunIndex()

	// Pick an entry that lands near the end of the run list.
	entryNum := (uint64(runCount)*uint64(clusterSize))/uint64(v.mftRecordSize) - 2

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.getMFTEntryOffset(entryNum); err != nil {
			b.Fatalf("getMFTEntryOffset failed: %v", err)
		}
	}
}

func BenchmarkReadDataRunsSmallWindowManyRuns(b *testing.B) {
	const (
		clusterSize = uint32(4096)
		runCount    = 2048
	)

	data := make([]byte, int(clusterSize)*(runCount+128))
	for i := range data {
		data[i] = byte(i)
	}

	runs := make([]DataRun, runCount)
	for i := 0; i < runCount; i++ {
		runs[i] = DataRun{LengthClusters: 1, StartCluster: int64(i + 64)}
	}

	v := &Volume{
		reader:          &mockReaderAt{data: data},
		bytesPerCluster: clusterSize,
	}

	buf := make([]byte, 4096)
	offset := int64(int(clusterSize)*(runCount-8) + 128)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := v.ReadDataRuns(runs, offset, buf)
		if err != nil && !errors.Is(err, io.EOF) {
			b.Fatalf("ReadDataRuns failed: %v", err)
		}
		if n == 0 {
			b.Fatalf("ReadDataRuns returned 0 bytes")
		}
	}
}
