package libntfs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// buildPathFixtureImage lays out a small tree so path reconstruction is tested
// against real parent references rather than a flat root:
//
//	/docs/reports/q3.txt   nested two directories deep
//	/loose.txt             directly under the root
//	/$ORPHAN/17            parent record 900 does not exist
//	                       entries 20 and 21 name each other
func buildPathFixtureImage(t testing.TB) []byte {
	t.Helper()

	const clusterSize = 4096

	// Real directory indexes, so a reconstructed path can be handed straight
	// back to OpenPath and resolve through the same tree it was derived from.
	root := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse | MFTFlagDirectory,
		name:      ".",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", buildIndexRootValue(clusterSize,
				buildIndexEntry(12, 2, buildFileNameAttrValue(
					MFTEntryRoot, 0, "docs", NamespaceWin32, 0, 0, FileAttrDupFileNameIndexPresent)),
				buildIndexEntry(15, 0, buildFileNameAttrValue(
					MFTEntryRoot, 0, "loose.txt", NamespaceWin32, 9, 4096, 0x20)),
			)),
		},
	})

	docs := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		name:        "docs",
		namespace:   NamespaceWin32,
		sequenceNum: 2,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", buildIndexRootValue(clusterSize,
				buildIndexEntry(13, 3, buildFileNameAttrValue(
					12, 2, "reports", NamespaceWin32, 0, 0, FileAttrDupFileNameIndexPresent)),
			)),
		},
	})

	reports := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		name:        "reports",
		namespace:   NamespaceWin32,
		parentRef:   12,
		parentSeq:   2,
		sequenceNum: 3,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", buildIndexRootValue(clusterSize,
				buildIndexEntry(14, 4, buildFileNameAttrValue(
					13, 3, "q3.txt", NamespaceWin32, 9, 4096, 0x20)),
			)),
		},
	})

	nested := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse,
		name:        "q3.txt",
		namespace:   NamespaceWin32,
		parentRef:   13,
		parentSeq:   3,
		sequenceNum: 4,
		extraAttrs:  [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("quarterly"))},
	})

	loose := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "loose.txt",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("top level"))},
	})

	// A deleted file whose parent directory record no longer exists, which is
	// the ordinary way a real volume produces an orphan.
	orphan := buildTestMFTRecord(t, testRecordOpts{
		flags:     0,
		name:      "vanished.txt",
		namespace: NamespaceWin32,
		parentRef: 900,
	})

	// A two-record cycle, which a corrupt tree can produce and a naive walk
	// would follow forever.
	cycleA := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse | MFTFlagDirectory,
		name:      "cycleA",
		namespace: NamespaceWin32,
		parentRef: 21,
	})
	cycleB := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse | MFTFlagDirectory,
		name:      "cycleB",
		namespace: NamespaceWin32,
		parentRef: 20,
	})

	return buildTestNTFSImageAt(t, map[uint64][]byte{
		MFTEntryRoot: root,
		12:           docs,
		13:           reports,
		14:           nested,
		15:           loose,
		17:           orphan,
		20:           cycleA,
		21:           cycleB,
	})
}

func newPathFixtureVolume(t testing.TB) *Volume {
	t.Helper()

	vol, err := OpenReadOnly(&mockReaderAt{data: buildPathFixtureImage(t)})
	if err != nil {
		t.Fatalf("OpenReadOnly failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	return vol
}

func TestPathFor(t *testing.T) {
	vol := newPathFixtureVolume(t)

	cases := []struct {
		name     string
		entryNum uint64
		want     string
	}{
		{"root", MFTEntryRoot, "/"},
		{"directly under root", 15, "/loose.txt"},
		{"one level down", 13, "/docs/reports"},
		{"two levels down", 14, "/docs/reports/q3.txt"},
		{"missing parent", 17, "/$ORPHAN/17"},
		{"cycle", 20, "/$ORPHAN/20"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vol.PathFor(tc.entryNum)
			if err != nil {
				t.Fatalf("PathFor(%d) failed: %v", tc.entryNum, err)
			}
			if got != tc.want {
				t.Fatalf("PathFor(%d) = %q, want %q", tc.entryNum, got, tc.want)
			}
		})
	}
}

// TestPathForRoundTripsWithOpenPath ties the two directions together: a path
// reconstructed from a record must open the same record.
func TestPathForRoundTripsWithOpenPath(t *testing.T) {
	vol := newPathFixtureVolume(t)

	path, err := vol.PathFor(14)
	if err != nil {
		t.Fatalf("PathFor failed: %v", err)
	}

	f, err := vol.OpenPath(path)
	if err != nil {
		t.Fatalf("OpenPath(%q) failed: %v", path, err)
	}

	if f.EntryNumber() != 14 {
		t.Fatalf("OpenPath(%q) opened entry %d, want 14", path, f.EntryNumber())
	}
}

// TestPathForUnreadableEntry pins the one case that is an error rather than an
// orphan: the entry the caller asked about, as opposed to one of its ancestors.
func TestPathForUnreadableEntry(t *testing.T) {
	vol := newPathFixtureVolume(t)

	// Entry 16 sits inside the MFT run but was never written, so it has no
	// record to read a name from.
	if _, err := vol.PathFor(16); err == nil {
		t.Fatal("expected an error for an entry that cannot be read")
	}
}

func TestPathForClosedVolume(t *testing.T) {
	vol := newPathFixtureVolume(t)
	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := vol.PathFor(14); !errors.Is(err, ErrVolumeClosed) {
		t.Fatalf("expected ErrVolumeClosed, got %v", err)
	}
}

func TestWalkFilesCarriesPathsAndParents(t *testing.T) {
	vol := newPathFixtureVolume(t)

	type seen struct {
		parentRef uint64
		parentSeq uint16
		entryNum  uint64
	}
	found := make(map[string]seen)

	err := vol.WalkFiles(context.Background(), WalkConfig{},
		func(path string, parentRef uint64, parentSeq uint16, e *MFTEntry) error {
			found[path] = seen{parentRef: parentRef, parentSeq: parentSeq, entryNum: uint64(e.RecordNumber)}
			return nil
		})
	if err != nil {
		t.Fatalf("WalkFiles failed: %v", err)
	}

	nested, ok := found["/docs/reports/q3.txt"]
	if !ok {
		t.Fatalf("expected /docs/reports/q3.txt in %v", keysOf(found))
	}
	if nested.parentRef != 13 {
		t.Errorf("parent reference = %d, want 13", nested.parentRef)
	}
	if nested.parentSeq != 3 {
		t.Errorf("parent sequence = %d, want 3", nested.parentSeq)
	}

	if _, ok := found["/docs/reports"]; !ok {
		t.Errorf("expected the intermediate directory in %v", keysOf(found))
	}
	if _, ok := found["/loose.txt"]; !ok {
		t.Errorf("expected /loose.txt in %v", keysOf(found))
	}
	if _, ok := found["/$ORPHAN/17"]; !ok {
		t.Errorf("expected the orphan in %v", keysOf(found))
	}
}

// TestWalkFilesMatchesPathFor keeps the bulk and single-entry resolvers honest
// about each other: they share the rules but not the implementation.
func TestWalkFilesMatchesPathFor(t *testing.T) {
	vol := newPathFixtureVolume(t)

	err := vol.WalkFiles(context.Background(), WalkConfig{},
		func(path string, _ uint64, _ uint16, e *MFTEntry) error {
			if strings.HasPrefix(path, OrphanDir) {
				return nil // Nameless records have no single-entry equivalent.
			}

			entryNum := uint64(e.RecordNumber)
			want, err := vol.PathFor(entryNum)
			if err != nil {
				t.Errorf("PathFor(%d) failed: %v", entryNum, err)
				return nil
			}
			if path != want {
				t.Errorf("entry %d: WalkFiles gave %q, PathFor gave %q", entryNum, path, want)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("WalkFiles failed: %v", err)
	}
}

// TestWalkFilesParallelMatchesSequential pins the same guarantee WalkMFT makes:
// the worker count is a tuning decision, not a semantic one.
func TestWalkFilesParallelMatchesSequential(t *testing.T) {
	vol := newPathFixtureVolume(t)

	collect := func(cfg WalkConfig) []string {
		t.Helper()

		var paths []string
		err := vol.WalkFiles(context.Background(), cfg,
			func(path string, _ uint64, _ uint16, _ *MFTEntry) error {
				paths = append(paths, path)
				return nil
			})
		if err != nil {
			t.Fatalf("WalkFiles failed: %v", err)
		}
		return paths
	}

	sequential := collect(WalkConfig{Workers: WorkersSequential})
	parallel := collect(WalkConfig{Workers: 4, BatchSize: 1})

	if len(sequential) != len(parallel) {
		t.Fatalf("path count differs: sequential %d, parallel %d", len(sequential), len(parallel))
	}
	for i := range sequential {
		if sequential[i] != parallel[i] {
			t.Fatalf("path %d differs: sequential %q, parallel %q", i, sequential[i], parallel[i])
		}
	}
}

func TestWalkFilesCancellation(t *testing.T) {
	vol := newPathFixtureVolume(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := vol.WalkFiles(ctx, WalkConfig{}, func(string, uint64, uint16, *MFTEntry) error {
		t.Error("callback ran under a cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestWalkFilesStopsOnCallbackError pins the contract it inherits from WalkMFT.
func TestWalkFilesStopsOnCallbackError(t *testing.T) {
	vol := newPathFixtureVolume(t)

	sentinel := errors.New("stop here")
	calls := 0

	err := vol.WalkFiles(context.Background(), WalkConfig{},
		func(string, uint64, uint16, *MFTEntry) error {
			calls++
			return sentinel
		})

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the callback's error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected the walk to stop after one call, got %d", calls)
	}
}

func TestReportCarriesIdentity(t *testing.T) {
	vol := newPathFixtureVolume(t)

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	var nested *NTFSFile
	for i := range report.Files {
		if report.Files[i].Filename == "/docs/reports/q3.txt" {
			nested = &report.Files[i]
			break
		}
	}
	if nested == nil {
		t.Fatal("expected /docs/reports/q3.txt in the report")
	}

	if nested.EntryNumber != 14 {
		t.Errorf("EntryNumber = %d, want 14", nested.EntryNumber)
	}
	if nested.SequenceNumber != 4 {
		t.Errorf("SequenceNumber = %d, want 4", nested.SequenceNumber)
	}
	if nested.ParentReference != 13 {
		t.Errorf("ParentReference = %d, want 13", nested.ParentReference)
	}
	if nested.ParentSequence != 3 {
		t.Errorf("ParentSequence = %d, want 3", nested.ParentSequence)
	}
	if len(nested.Streams) != 1 || nested.Streams[0].Name != "" {
		t.Errorf("expected one unnamed stream, got %+v", nested.Streams)
	}
}

// TestReportIdentityMatchesFileAccessors ties the report's identity fields to
// the ones a caller reads off an open File, so the two cannot drift.
func TestReportIdentityMatchesFileAccessors(t *testing.T) {
	vol := newPathFixtureVolume(t)

	f, err := vol.Open(14)
	if err != nil {
		t.Fatalf("Open(14) failed: %v", err)
	}

	if got := f.SequenceNumber(); got != 4 {
		t.Fatalf("SequenceNumber = %d, want 4", got)
	}

	ref := f.FileReference()
	if got := FileReferenceEntry(ref); got != 14 {
		t.Errorf("FileReferenceEntry = %d, want 14", got)
	}
	if got := FileReferenceSequence(ref); got != 4 {
		t.Errorf("FileReferenceSequence = %d, want 4", got)
	}
	if want := MakeFileReference(14, 4); ref != want {
		t.Errorf("FileReference = %#x, want %#x", ref, want)
	}
}

func TestReportContextCancellation(t *testing.T) {
	vol := newPathFixtureVolume(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := vol.ReportContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
