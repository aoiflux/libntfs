package libntfs

import (
	"errors"
	"testing"
)

// newWritableResidentVolume builds a writable image holding one file whose
// $DATA is resident, and returns the volume plus its backing bytes.
func newWritableResidentVolume(t testing.TB, content string) (*Volume, *mockReadWriterAt) {
	t.Helper()

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "resident.txt",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte(content))},
	})

	backend := &mockReadWriterAt{data: buildTestNTFSImage(t, [][]byte{record})}

	vol, err := Open(backend)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	if !vol.IsWritable() {
		t.Fatal("expected a writable volume")
	}

	return vol, backend
}

func TestFileWriteAtResidentRoundTrip(t *testing.T) {
	const original = "hello resident world"

	vol, backend := newWritableResidentVolume(t, original)

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	n, err := f.WriteAt([]byte("BRAVE"), 6)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5 bytes written, got %d", n)
	}

	// "BRAVE" replaces the five bytes at offset 6, leaving the length unchanged.
	const want = "hello BRAVEent world"

	// The in-memory handle must reflect the write immediately.
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != want {
		t.Fatalf("in-memory contents mismatch: got %q want %q", got, want)
	}

	// Reopening from the same bytes proves the record was written and still
	// parses, fixups and all.
	reopened, err := Open(&mockReaderAt{data: backend.data})
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer reopened.Close()

	f2, err := reopened.Open(1)
	if err != nil {
		t.Fatalf("reopen entry failed: %v", err)
	}

	got2, err := f2.ReadAll()
	if err != nil {
		t.Fatalf("reopened ReadAll failed: %v", err)
	}
	if string(got2) != want {
		t.Fatalf("on-disk contents mismatch: got %q want %q", got2, want)
	}

	// The rest of the record must be untouched.
	name, err := f2.entry.GetFileName()
	if err != nil {
		t.Fatalf("reopened GetFileName failed: %v", err)
	}
	if name.Name != "resident.txt" {
		t.Fatalf("file name damaged by the write: got %q", name.Name)
	}
}

func TestFileWriteAtResidentEvictsCache(t *testing.T) {
	vol, _ := newWritableResidentVolume(t, "cached contents")

	// Prime the MFT cache.
	if _, err := vol.GetMFTEntry(1); err != nil {
		t.Fatalf("GetMFTEntry failed: %v", err)
	}

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	if _, err := f.WriteAt([]byte("HACKED"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// A fresh lookup must come from disk, not the pre-write cache entry.
	entry, err := vol.GetMFTEntry(1)
	if err != nil {
		t.Fatalf("GetMFTEntry after write failed: %v", err)
	}

	attr := entry.FindAttribute(AttrTypeData, "")
	if attr == nil || attr.Resident == nil {
		t.Fatal("expected a resident $DATA attribute")
	}
	if got := string(attr.Resident.Value); got != "HACKED contents" {
		t.Fatalf("cache served stale content: got %q", got)
	}
}

func TestFileWriteAtResidentRejectsGrowth(t *testing.T) {
	vol, _ := newWritableResidentVolume(t, "small")

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	// Writing past the end of a resident value would need the attribute resized
	// and every later attribute shifted, so it is refused outright.
	_, err = f.WriteAt([]byte("much longer than the value"), 0)
	if !errors.Is(err, ErrWriteOutOfRange) && !errors.Is(err, ErrWriteUnsupported) {
		t.Fatalf("expected the growth to be refused, got %v", err)
	}
}

func TestFileWriteAtResidentRejectsOutOfRangeOffset(t *testing.T) {
	vol, _ := newWritableResidentVolume(t, "0123456789")

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	if _, err := f.WriteAt([]byte("x"), 64); err == nil {
		t.Fatal("expected an out-of-range offset to be refused")
	}

	// A write that starts in range but runs past the end must also be refused.
	if _, err := f.WriteAt([]byte("xxxx"), 8); !errors.Is(err, ErrWriteOutOfRange) {
		t.Fatalf("expected ErrWriteOutOfRange, got %v", err)
	}
}

func TestFileWriteAtResidentReadOnlyVolume(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "resident.txt",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("read only"))},
	})

	// mockReaderAt has no WriteAt, so the volume opens read-only.
	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{record})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	if _, err := f.WriteAt([]byte("X"), 0); !errors.Is(err, ErrReadOnlyVolume) {
		t.Fatalf("expected ErrReadOnlyVolume, got %v", err)
	}
}

// TestUpdateSequenceRoundTripAtSectorBoundary covers the subtle case: a byte
// living in the final two bytes of a sector is stored in the update sequence
// array rather than in place, so a write there must survive the reversal.
func TestUpdateSequenceRoundTripAtSectorBoundary(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "boundary.txt",
		namespace: NamespaceWin32,
	})

	updateSeqOffset := ReadUint16LE(record, 4)
	updateSeqSize := ReadUint16LE(record, 6)

	// Move into fixed-up form, as the write path works on.
	if err := applyUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
		t.Fatalf("applyUpdateSequence failed: %v", err)
	}

	// Patch the last two bytes of the first sector.
	sectorEnd := UpdateSequenceStride - 2
	WriteUint16LE(record, sectorEnd, 0xBEEF)

	if err := reverseUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
		t.Fatalf("reverseUpdateSequence failed: %v", err)
	}

	// On disk the sector tail must now hold the sequence number, not the value.
	usn := ReadUint16LE(record, int(updateSeqOffset))
	if got := ReadUint16LE(record, sectorEnd); got != usn {
		t.Fatalf("sector tail should hold the USN 0x%X, got 0x%X", usn, got)
	}

	// Reading it back must recover the patched value.
	if err := applyUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
		t.Fatalf("second applyUpdateSequence failed: %v", err)
	}
	if got := ReadUint16LE(record, sectorEnd); got != 0xBEEF {
		t.Fatalf("value at the sector boundary was lost: got 0x%X want 0xBEEF", got)
	}
}

func TestWriteMFTRecordRejectsReadOnlyVolume(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "a.txt",
		namespace: NamespaceWin32,
	})

	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{record})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	raw, offset, err := vol.readMFTRecordRaw(1)
	if err != nil {
		t.Fatalf("readMFTRecordRaw failed: %v", err)
	}

	if err := vol.writeMFTRecord(1, raw, offset); !errors.Is(err, ErrReadOnlyVolume) {
		t.Fatalf("expected ErrReadOnlyVolume, got %v", err)
	}
}

func TestReadMFTRecordRawAppliesFixups(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "fixups.txt",
		namespace: NamespaceWin32,
	})

	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{record})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	raw, offset, err := vol.readMFTRecordRaw(1)
	if err != nil {
		t.Fatalf("readMFTRecordRaw failed: %v", err)
	}

	if got := ReadUint32LE(raw, 0); got != MFTMagicFILE {
		t.Fatalf("magic mismatch: got 0x%X", got)
	}
	if offset <= 0 {
		t.Fatalf("expected a positive record offset, got %d", offset)
	}

	// The buffer must be in fixed-up form: each sector tail holds the real value
	// stashed in the update sequence array, not the sequence number itself.
	updateSeqOffset := int(ReadUint16LE(raw, 4))
	updateSeqSize := int(ReadUint16LE(raw, 6))
	usn := ReadUint16LE(raw, updateSeqOffset)

	for i := 0; i < updateSeqSize-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		stashed := ReadUint16LE(raw, updateSeqOffset+2+i*2)

		if got := ReadUint16LE(raw, sectorEnd); got != stashed {
			t.Fatalf("sector %d tail is 0x%X, want the stashed value 0x%X", i, got, stashed)
		}
		if stashed == usn {
			t.Fatalf("sector %d tail still holds the USN, so fixups were not applied", i)
		}
	}
}

func TestAttributeRecordOffsetIsRecorded(t *testing.T) {
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "offsets.txt",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("payload"))},
	})

	entry, err := ParseMFTRecord(record, 3, DefaultMFTRecordSize)
	if err != nil {
		t.Fatalf("ParseMFTRecord failed: %v", err)
	}

	prev := -1
	for i, attr := range entry.Attributes {
		if attr.Header.RecordOffset <= prev {
			t.Fatalf("attribute %d has non-increasing record offset %d", i, attr.Header.RecordOffset)
		}
		if attr.Header.RecordOffset < MFTEntryHeaderSize {
			t.Fatalf("attribute %d offset %d is inside the record header", i, attr.Header.RecordOffset)
		}

		// The recorded offset must actually point at that attribute's type.
		if got := ReadUint32LE(record, attr.Header.RecordOffset); got != attr.Header.Type {
			t.Fatalf("attribute %d offset %d points at type 0x%X, want 0x%X",
				i, attr.Header.RecordOffset, got, attr.Header.Type)
		}

		prev = attr.Header.RecordOffset
	}
}
