package libntfs

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// usnRecordOpts describes a synthetic USN record.
type usnRecordOpts struct {
	version    uint16
	usn        int64
	fileRef    uint64
	fileSeq    uint16
	parentRef  uint64
	parentSeq  uint16
	timestamp  time.Time
	reason     uint32
	sourceInfo uint32
	securityID uint32
	fileAttrs  uint32
	name       string
}

// buildUSNRecord assembles a V2, V3, or V4 record.
func buildUSNRecord(t testing.TB, opts usnRecordOpts) []byte {
	t.Helper()

	version := opts.version
	if version == 0 {
		version = 2
	}

	nameBytes := utf16LEFromString(opts.name)

	var headerSize int
	switch version {
	case 2:
		headerSize = USNRecordV2HeaderSize
	case 3:
		headerSize = USNRecordV3HeaderSize
	case 4:
		headerSize = USNRecordV4HeaderSize
	default:
		t.Fatalf("unsupported test record version %d", version)
	}

	length := AlignUp(headerSize+len(nameBytes), 8)
	if version == 4 {
		length = AlignUp(headerSize, 8)
	}

	buf := make([]byte, length)
	WriteUint32LE(buf, 0, uint32(length))
	WriteUint16LE(buf, 4, version)
	WriteUint16LE(buf, 6, 0)

	// The file reference packs a 48-bit entry number with a 16-bit sequence.
	fileRef := opts.fileRef | uint64(opts.fileSeq)<<48
	parentRef := opts.parentRef | uint64(opts.parentSeq)<<48

	switch version {
	case 2:
		WriteUint64LE(buf, 8, fileRef)
		WriteUint64LE(buf, 16, parentRef)
		WriteUint64LE(buf, 24, uint64(opts.usn))
		WriteUint64LE(buf, 32, TimeToNTFSTime(opts.timestamp))
		WriteUint32LE(buf, 40, opts.reason)
		WriteUint32LE(buf, 44, opts.sourceInfo)
		WriteUint32LE(buf, 48, opts.securityID)
		WriteUint32LE(buf, 52, opts.fileAttrs)
		WriteUint16LE(buf, 56, uint16(len(nameBytes)))
		WriteUint16LE(buf, 58, uint16(headerSize))
		copy(buf[headerSize:], nameBytes)

	case 3:
		// V3 widens both references to 128 bits; NTFS leaves the tail zeroed.
		WriteUint64LE(buf, 8, fileRef)
		WriteUint64LE(buf, 24, parentRef)
		WriteUint64LE(buf, 40, uint64(opts.usn))
		WriteUint64LE(buf, 48, TimeToNTFSTime(opts.timestamp))
		WriteUint32LE(buf, 56, opts.reason)
		WriteUint32LE(buf, 60, opts.sourceInfo)
		WriteUint32LE(buf, 64, opts.securityID)
		WriteUint32LE(buf, 68, opts.fileAttrs)
		WriteUint16LE(buf, 72, uint16(len(nameBytes)))
		WriteUint16LE(buf, 74, uint16(headerSize))
		copy(buf[headerSize:], nameBytes)

	case 4:
		WriteUint64LE(buf, 8, fileRef)
		WriteUint64LE(buf, 24, parentRef)
		WriteUint64LE(buf, 40, uint64(opts.usn))
		WriteUint32LE(buf, 48, opts.reason)
		WriteUint32LE(buf, 52, opts.sourceInfo)
	}

	return buf
}

func TestParseUSNRecordV2(t *testing.T) {
	stamp := time.Date(2025, 7, 4, 12, 30, 0, 0, time.UTC)

	buf := buildUSNRecord(t, usnRecordOpts{
		version:    2,
		usn:        0x1234568,
		fileRef:    117,
		fileSeq:    3,
		parentRef:  5,
		parentSeq:  1,
		timestamp:  stamp,
		reason:     USNReasonFileCreate | USNReasonClose,
		securityID: 264,
		fileAttrs:  FileAttrArchive,
		name:       "invoice.pdf",
	})

	record, consumed, err := ParseUSNRecord(buf)
	if err != nil {
		t.Fatalf("ParseUSNRecord failed: %v", err)
	}
	if consumed != len(buf) {
		t.Fatalf("consumed mismatch: got %d want %d", consumed, len(buf))
	}

	if record.MajorVersion != 2 {
		t.Fatalf("version mismatch: got %d", record.MajorVersion)
	}
	if record.Name != "invoice.pdf" {
		t.Fatalf("name mismatch: got %q", record.Name)
	}
	if record.USN != 0x1234568 {
		t.Fatalf("USN mismatch: got %d", record.USN)
	}
	if record.FileReference != 117 {
		t.Fatalf("file reference mismatch: got %d", record.FileReference)
	}
	if record.FileSequence != 3 {
		t.Fatalf("file sequence mismatch: got %d", record.FileSequence)
	}
	if record.ParentReference != 5 {
		t.Fatalf("parent reference mismatch: got %d", record.ParentReference)
	}
	if record.SecurityID != 264 {
		t.Fatalf("security ID mismatch: got %d", record.SecurityID)
	}
	if want := NTFSTimeToTime(TimeToNTFSTime(stamp)); !record.Timestamp.Equal(want) {
		t.Fatalf("timestamp mismatch: got %v want %v", record.Timestamp, want)
	}
	if record.IsDirectory() {
		t.Fatal("expected a file, not a directory")
	}

	if got := record.ReasonNames(); !reflect.DeepEqual(got, []string{"FileCreate", "Close"}) {
		t.Fatalf("reason names mismatch: got %v", got)
	}
}

func TestParseUSNRecordV3(t *testing.T) {
	stamp := time.Date(2025, 8, 1, 9, 0, 0, 0, time.UTC)

	buf := buildUSNRecord(t, usnRecordOpts{
		version:   3,
		usn:       999,
		fileRef:   4242,
		fileSeq:   7,
		parentRef: 88,
		timestamp: stamp,
		reason:    USNReasonRenameNewName,
		fileAttrs: FileAttrDirectory,
		name:      "Projects",
	})

	record, consumed, err := ParseUSNRecord(buf)
	if err != nil {
		t.Fatalf("ParseUSNRecord failed: %v", err)
	}
	if consumed != len(buf) {
		t.Fatalf("consumed mismatch: got %d want %d", consumed, len(buf))
	}

	if record.MajorVersion != 3 {
		t.Fatalf("version mismatch: got %d", record.MajorVersion)
	}
	if record.Name != "Projects" {
		t.Fatalf("name mismatch: got %q", record.Name)
	}
	if record.FileReference != 4242 || record.FileSequence != 7 {
		t.Fatalf("file reference mismatch: got %d seq %d", record.FileReference, record.FileSequence)
	}
	if record.ParentReference != 88 {
		t.Fatalf("parent reference mismatch: got %d", record.ParentReference)
	}
	if !record.IsDirectory() {
		t.Fatal("expected a directory")
	}
	if got := record.ReasonNames(); !reflect.DeepEqual(got, []string{"RenameNewName"}) {
		t.Fatalf("reason names mismatch: got %v", got)
	}
}

func TestParseUSNRecordV4HasNoNameOrTimestamp(t *testing.T) {
	buf := buildUSNRecord(t, usnRecordOpts{
		version:   4,
		usn:       555,
		fileRef:   99,
		parentRef: 5,
		reason:    USNReasonDataExtend,
	})

	record, consumed, err := ParseUSNRecord(buf)
	if err != nil {
		t.Fatalf("ParseUSNRecord failed: %v", err)
	}
	if consumed != len(buf) {
		t.Fatalf("consumed mismatch: got %d want %d", consumed, len(buf))
	}

	if record.MajorVersion != 4 {
		t.Fatalf("version mismatch: got %d", record.MajorVersion)
	}
	if record.USN != 555 || record.FileReference != 99 {
		t.Fatalf("V4 common fields mismatch: usn=%d ref=%d", record.USN, record.FileReference)
	}
	if record.Name != "" {
		t.Fatalf("V4 records carry no name, got %q", record.Name)
	}
	if !record.Timestamp.IsZero() {
		t.Fatalf("V4 records carry no timestamp, got %v", record.Timestamp)
	}
}

func TestParseUSNRecordRejects(t *testing.T) {
	valid := buildUSNRecord(t, usnRecordOpts{name: "a.txt"})

	t.Run("zero length", func(t *testing.T) {
		_, _, err := ParseUSNRecord(make([]byte, 64))
		if !errors.Is(err, ErrInvalidUSNRecord) {
			t.Fatalf("expected ErrInvalidUSNRecord, got %v", err)
		}
	})

	t.Run("implausible length", func(t *testing.T) {
		buf := make([]byte, len(valid))
		copy(buf, valid)
		WriteUint32LE(buf, 0, USNMaxRecordLength+8)

		if _, _, err := ParseUSNRecord(buf); !errors.Is(err, ErrInvalidUSNRecord) {
			t.Fatalf("expected ErrInvalidUSNRecord, got %v", err)
		}
	})

	t.Run("unknown version", func(t *testing.T) {
		buf := make([]byte, len(valid))
		copy(buf, valid)
		WriteUint16LE(buf, 4, 9)

		if _, _, err := ParseUSNRecord(buf); !errors.Is(err, ErrInvalidUSNRecord) {
			t.Fatalf("expected ErrInvalidUSNRecord, got %v", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		if _, _, err := ParseUSNRecord(valid[:len(valid)-8]); err == nil {
			t.Fatal("expected a truncated record to be rejected")
		}
	})
}

// buildUSNJournal assembles a $J buffer: sparse-style zeros, then records.
func buildUSNJournal(t testing.TB, leadingZeros int, records ...[]byte) []byte {
	t.Helper()

	buf := make([]byte, leadingZeros)
	for _, record := range records {
		buf = append(buf, record...)
	}

	return buf
}

func usnTestRecords(t testing.TB) [][]byte {
	t.Helper()

	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	return [][]byte{
		buildUSNRecord(t, usnRecordOpts{version: 2, usn: 100, fileRef: 30, parentRef: 5,
			timestamp: base, reason: USNReasonFileCreate, name: "first.txt"}),
		buildUSNRecord(t, usnRecordOpts{version: 3, usn: 200, fileRef: 31, parentRef: 5,
			timestamp: base.Add(time.Minute), reason: USNReasonDataExtend, name: "second.log"}),
		buildUSNRecord(t, usnRecordOpts{version: 4, usn: 300, fileRef: 32, parentRef: 5,
			reason: USNReasonDataOverwrite}),
		buildUSNRecord(t, usnRecordOpts{version: 2, usn: 400, fileRef: 33, parentRef: 5,
			timestamp: base.Add(2 * time.Minute), reason: USNReasonFileDelete, name: "third.tmp"}),
	}
}

func TestEachUSNRecordSkipsSparseAndWalksAll(t *testing.T) {
	journal := buildUSNJournal(t, 4096, usnTestRecords(t)...)

	var names []string
	var usns []int64
	if err := EachUSNRecord(journal, func(r *USNRecord) error {
		names = append(names, r.Name)
		usns = append(usns, r.USN)
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}

	// The V4 record contributes an empty name but must still be visited.
	if !reflect.DeepEqual(names, []string{"first.txt", "second.log", "", "third.tmp"}) {
		t.Fatalf("names mismatch: got %v", names)
	}
	if !reflect.DeepEqual(usns, []int64{100, 200, 300, 400}) {
		t.Fatalf("USNs mismatch: got %v", usns)
	}
}

func TestEachUSNRecordStopsOnCallbackError(t *testing.T) {
	journal := buildUSNJournal(t, 0, usnTestRecords(t)...)

	sentinel := errors.New("stop")
	seen := 0

	err := EachUSNRecord(journal, func(r *USNRecord) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the callback error to propagate, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected iteration to stop after the first record, ran %d times", seen)
	}
}

func TestEachUSNRecordSurvivesDamage(t *testing.T) {
	records := usnTestRecords(t)

	damaged := make([]byte, len(records[1]))
	copy(damaged, records[1])
	WriteUint16LE(damaged, 4, 77) // Unknown version

	journal := buildUSNJournal(t, 64, records[0], damaged, records[3])

	var names []string
	if err := EachUSNRecord(journal, func(r *USNRecord) error {
		names = append(names, r.Name)
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}

	if !reflect.DeepEqual(names, []string{"first.txt", "third.tmp"}) {
		t.Fatalf("expected the damaged record to be skipped, got %v", names)
	}
}

func TestEachUSNRecordTruncatedTail(t *testing.T) {
	records := usnTestRecords(t)
	journal := buildUSNJournal(t, 0, records[0], records[1])

	// Lop off the tail of the final record.
	journal = journal[:len(journal)-8]

	var names []string
	if err := EachUSNRecord(journal, func(r *USNRecord) error {
		names = append(names, r.Name)
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}

	if !reflect.DeepEqual(names, []string{"first.txt"}) {
		t.Fatalf("expected only the complete record, got %v", names)
	}
}

// newUSNTestVolume builds an image with a $Extend directory indexing a $UsnJrnl
// entry whose $J stream is sparse at the front, as on a real volume.
func newUSNTestVolume(t testing.TB, journal []byte, sparseClusters uint64) *Volume {
	t.Helper()

	const (
		clusterSize  = 4096
		journalEntry = 24
		dataCluster  = 12
	)

	// $Extend is a directory whose index names $UsnJrnl.
	indexRoot := buildIndexRootValue(clusterSize,
		buildIndexEntry(journalEntry, 1,
			buildFileNameAttrValue(MFTEntryExtend, 1, USNJournalPath, NamespaceWin32, 0, 0, 0)))

	extend := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse | MFTFlagDirectory,
		name:       "$Extend",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", indexRoot)},
	})

	sparseBytes := sparseClusters * clusterSize
	usnJrnl := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      USNJournalPath,
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildSparseThenDataAttr(t, 2, USNJournalStream,
				sparseClusters, dataCluster, 1, clusterSize, sparseBytes+uint64(len(journal))),
		},
	})

	image := buildTestNTFSImageAt(t, map[uint64][]byte{
		MFTEntryExtend: extend,
		journalEntry:   usnJrnl,
	})
	copy(image[dataCluster*clusterSize:], journal)

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	return vol
}

func TestVolumeOpenUSNJournal(t *testing.T) {
	records := usnTestRecords(t)
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, records...), 1)

	journal, err := vol.OpenUSNJournal()
	if err != nil {
		t.Fatalf("OpenUSNJournal failed: %v", err)
	}

	if journal.Size() <= 0 {
		t.Fatalf("expected a non-empty journal, got %d bytes", journal.Size())
	}
}

func TestVolumeEachUSNRecord(t *testing.T) {
	records := usnTestRecords(t)
	// One sparse cluster ahead of the records, which the walk must skip over.
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, records...), 1)

	var usns []int64
	var names []string
	if err := vol.EachUSNRecord(func(r *USNRecord) error {
		usns = append(usns, r.USN)
		names = append(names, r.Name)
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}

	if !reflect.DeepEqual(usns, []int64{100, 200, 300, 400}) {
		t.Fatalf("USNs mismatch: got %v", usns)
	}
	if !reflect.DeepEqual(names, []string{"first.txt", "second.log", "", "third.tmp"}) {
		t.Fatalf("names mismatch: got %v", names)
	}
}

func TestVolumeEachUSNRecordStopsOnCallbackError(t *testing.T) {
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, usnTestRecords(t)...), 1)

	sentinel := errors.New("stop")
	seen := 0

	err := vol.EachUSNRecord(func(r *USNRecord) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the callback error to propagate, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected iteration to stop after the first record, ran %d times", seen)
	}
}

func TestVolumeOpenUSNJournalMissing(t *testing.T) {
	// A volume with no $Extend directory at all.
	vol := newTestMFTVolume(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{flags: MFTFlagInUse, name: "a.txt", namespace: NamespaceWin32}),
	})

	if _, err := vol.OpenUSNJournal(); err == nil {
		t.Fatal("expected OpenUSNJournal to fail without $Extend")
	}
}

func TestUSNFirstAllocatedOffsetSkipsSparseLead(t *testing.T) {
	const (
		clusterSize    = 4096
		sparseClusters = 3
	)

	records := usnTestRecords(t)
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, records...), sparseClusters)

	journal, err := vol.OpenUSNJournal()
	if err != nil {
		t.Fatalf("OpenUSNJournal failed: %v", err)
	}

	got := usnFirstAllocatedOffset(journal, vol.BytesPerCluster())
	if want := int64(sparseClusters * clusterSize); got != want {
		t.Fatalf("sparse lead offset mismatch: got %d want %d", got, want)
	}

	// And the walk still finds every record after skipping that region.
	var usns []int64
	if err := vol.EachUSNRecord(func(r *USNRecord) error {
		usns = append(usns, r.USN)
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}
	if !reflect.DeepEqual(usns, []int64{100, 200, 300, 400}) {
		t.Fatalf("USNs mismatch: got %v", usns)
	}
}

func TestWalkUSNRecordsCarriesAcrossChunkBoundary(t *testing.T) {
	records := usnTestRecords(t)
	journal := buildUSNJournal(t, 0, records...)

	// Whole-buffer walk gives the reference result.
	var want []int64
	if err := EachUSNRecord(journal, func(r *USNRecord) error {
		want = append(want, r.USN)
		return nil
	}); err != nil {
		t.Fatalf("reference walk failed: %v", err)
	}

	// Splitting mid-record at every possible position must give the same answer,
	// which is what the chunked volume reader depends on.
	for split := 1; split < len(journal); split++ {
		var got []int64
		collect := func(r *USNRecord) error {
			got = append(got, r.USN)
			return nil
		}

		consumed, err := walkUSNRecords(journal[:split], false, collect)
		if err != nil {
			t.Fatalf("split %d: first chunk failed: %v", split, err)
		}

		carry := append([]byte{}, journal[consumed:split]...)
		rest := append(carry, journal[split:]...)

		if _, err := walkUSNRecords(rest, true, collect); err != nil {
			t.Fatalf("split %d: second chunk failed: %v", split, err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: records mismatch: got %v want %v", split, got, want)
		}
	}
}
