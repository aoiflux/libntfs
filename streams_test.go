package libntfs

import (
	"errors"
	"testing"
)

// adsRecordOpts describes a record carrying an unnamed stream plus alternates.
func recordWithStreams(t testing.TB, name string, flags uint16, extra ...[]byte) []byte {
	t.Helper()

	return buildTestMFTRecord(t, testRecordOpts{
		flags:      flags,
		name:       name,
		namespace:  NamespaceWin32,
		extraAttrs: extra,
	})
}

func TestFileStreamsListsAlternates(t *testing.T) {
	record := recordWithStreams(t, "host.txt", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("primary contents")),
		buildNamedResidentAttr(AttrTypeData, 3, "Zone.Identifier", []byte("[ZoneTransfer]")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	streams := f.Streams()
	if len(streams) != 2 {
		t.Fatalf("expected 2 streams, got %d: %+v", len(streams), streams)
	}

	if streams[0].Name != "" {
		t.Fatalf("expected the unnamed stream first, got %q", streams[0].Name)
	}
	if streams[0].IsAlternate() {
		t.Fatal("the unnamed stream must not report as alternate")
	}
	if streams[0].Size != uint64(len("primary contents")) {
		t.Fatalf("unnamed stream size mismatch: got %d", streams[0].Size)
	}

	if streams[1].Name != "Zone.Identifier" {
		t.Fatalf("alternate stream name mismatch: got %q", streams[1].Name)
	}
	if !streams[1].IsAlternate() {
		t.Fatal("the named stream must report as alternate")
	}
	if streams[1].Size != uint64(len("[ZoneTransfer]")) {
		t.Fatalf("alternate stream size mismatch: got %d", streams[1].Size)
	}
	if !streams[1].Resident {
		t.Fatal("expected the alternate stream to be resident")
	}
	if !streams[1].Support.Readable {
		t.Fatal("expected the alternate stream to be readable")
	}
}

func TestFileStreamsPutsUnnamedFirstDespiteOrder(t *testing.T) {
	// The alternate is written before the unnamed stream on disk.
	record := recordWithStreams(t, "reordered.txt", MFTFlagInUse,
		buildNamedResidentAttr(AttrTypeData, 2, "ads", []byte("alt")),
		buildResidentAttr(AttrTypeData, 3, []byte("main")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	streams := f.Streams()
	if len(streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(streams))
	}
	if streams[0].Name != "" {
		t.Fatalf("expected the unnamed stream first, got %q", streams[0].Name)
	}
}

func TestFileOpenStreamReadsAlternate(t *testing.T) {
	const payload = "[ZoneTransfer]\r\nZoneId=3\r\n"

	record := recordWithStreams(t, "download.exe", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("MZ")),
		buildNamedResidentAttr(AttrTypeData, 3, "Zone.Identifier", []byte(payload)),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	stream, err := f.OpenStream("Zone.Identifier")
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("stream contents mismatch: got %q want %q", got, payload)
	}

	// The host file's own stream must be unaffected.
	primary, err := f.ReadAll()
	if err != nil {
		t.Fatalf("primary ReadAll failed: %v", err)
	}
	if string(primary) != "MZ" {
		t.Fatalf("primary contents mismatch: got %q", primary)
	}
}

func TestFileOpenStreamCaseInsensitive(t *testing.T) {
	record := recordWithStreams(t, "host.txt", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("x")),
		buildNamedResidentAttr(AttrTypeData, 3, "Zone.Identifier", []byte("zone")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	stream, err := f.OpenStream("zone.identifier")
	if err != nil {
		t.Fatalf("case-insensitive OpenStream failed: %v", err)
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != "zone" {
		t.Fatalf("stream contents mismatch: got %q", got)
	}
}

func TestFileOpenStreamUnknownName(t *testing.T) {
	record := recordWithStreams(t, "host.txt", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("x")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if _, err := f.OpenStream("nope"); !errors.Is(err, ErrAttributeNotFound) {
		t.Fatalf("expected ErrAttributeNotFound, got %v", err)
	}
}

func TestFileOpenStreamEmptyNameIsIndependentHandle(t *testing.T) {
	record := recordWithStreams(t, "host.txt", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("abcdef")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Advance the original handle's read offset.
	buf := make([]byte, 3)
	if _, err := f.Read(buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	stream, err := f.OpenStream("")
	if err != nil {
		t.Fatalf("OpenStream(\"\") failed: %v", err)
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != "abcdef" {
		t.Fatalf("expected an independent read offset, got %q", got)
	}
}

func TestFileOpenStreamOnDirectoryIsReadable(t *testing.T) {
	// A directory has no unnamed $DATA stream but can still carry alternates.
	record := recordWithStreams(t, "Downloads", MFTFlagInUse|MFTFlagDirectory,
		buildNamedResidentAttr(AttrTypeData, 2, "marker", []byte("dir ads")),
	)

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	stream, err := f.OpenStream("marker")
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll on a directory's alternate stream failed: %v", err)
	}
	if string(got) != "dir ads" {
		t.Fatalf("stream contents mismatch: got %q", got)
	}

	// Reading the directory itself must still be refused.
	if _, err := f.ReadAll(); !errors.Is(err, ErrNotFile) {
		t.Fatalf("expected ErrNotFile reading the directory, got %v", err)
	}
}

func TestFileOpenStreamNonResident(t *testing.T) {
	const (
		clusterSize  = 4096
		startCluster = 12
		payload      = "non-resident alternate stream"
	)

	record := recordWithStreams(t, "big.bin", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("small")),
		buildNamedNonResidentDataAttr(t, 3, "ads", startCluster, 1, clusterSize, uint64(len(payload))),
	)

	image := buildTestNTFSImage(t, [][]byte{record})
	copy(image[startCluster*clusterSize:], payload)

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open entry failed: %v", err)
	}

	streams := f.Streams()
	if len(streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(streams))
	}

	stream, err := f.OpenStream("ads")
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	if stream.Size() != int64(len(payload)) {
		t.Fatalf("stream size mismatch: got %d want %d", stream.Size(), len(payload))
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("stream contents mismatch: got %q want %q", got, payload)
	}
}

func TestMFTEntryDataStreamsWithoutVolume(t *testing.T) {
	record := recordWithStreams(t, "standalone.txt", MFTFlagInUse,
		buildResidentAttr(AttrTypeData, 2, []byte("main")),
		buildNamedResidentAttr(AttrTypeData, 3, "hidden", []byte("payload")),
	)

	entry, err := ParseMFTRecord(record, 42, DefaultMFTRecordSize)
	if err != nil {
		t.Fatalf("ParseMFTRecord failed: %v", err)
	}

	streams := entry.DataStreams()
	if len(streams) != 2 {
		t.Fatalf("expected 2 $DATA streams, got %d", len(streams))
	}

	names := []string{attributeName(streams[0]), attributeName(streams[1])}
	if names[0] != "" || names[1] != "hidden" {
		t.Fatalf("stream names mismatch: got %v", names)
	}
}
