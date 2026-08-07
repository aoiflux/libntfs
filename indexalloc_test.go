package libntfs

import (
	"errors"
	"strings"
	"testing"
)

// buildIndexAllocationRecord builds an INDX record holding the supplied index
// entries, stamped with an update sequence array the way NTFS writes one.
//
// recordSize must be a multiple of UpdateSequenceStride, since the fixup array
// carries one slot per sector.
func buildIndexAllocationRecord(t testing.TB, recordSize int, vcn uint64, entries ...[]byte) []byte {
	t.Helper()

	if recordSize%UpdateSequenceStride != 0 {
		t.Fatalf("index record size %d is not a multiple of %d", recordSize, UpdateSequenceStride)
	}

	const usaOffset = 40 // Where NTFS puts the fixup array in an INDX record
	usaSize := recordSize/UpdateSequenceStride + 1
	entriesStart := AlignUp(usaOffset+usaSize*2, 8)

	last := make([]byte, 16)
	WriteUint16LE(last, 8, 16) // Entry length
	last[12] = IndexFlagLast

	var body []byte
	for _, entry := range entries {
		body = append(body, entry...)
	}
	body = append(body, last...)

	buf := make([]byte, recordSize)
	if entriesStart+len(body) > recordSize {
		t.Fatalf("index entries do not fit in a %d byte record", recordSize)
	}
	copy(buf[entriesStart:], body)

	WriteUint32LE(buf, 0, IndexAllocationMagic)
	WriteUint16LE(buf, 4, usaOffset)
	WriteUint16LE(buf, 6, uint16(usaSize))
	WriteUint64LE(buf, 8, 0x1122334455667788) // $LogFile sequence number
	WriteUint64LE(buf, 16, vcn)

	// Node header offsets are relative to the node header itself, at byte 24.
	const nodeHeader = indexRecordHeaderSize
	WriteUint32LE(buf, nodeHeader, uint32(entriesStart-nodeHeader))
	WriteUint32LE(buf, nodeHeader+4, uint32(entriesStart+len(body)-nodeHeader))
	WriteUint32LE(buf, nodeHeader+8, uint32(recordSize-nodeHeader))
	WriteUint32LE(buf, nodeHeader+12, 0) // Leaf node

	// Stash the real last two bytes of every sector in the array, then write
	// the USN in their place, exactly as applyUpdateSequence expects to undo.
	const usn = 0x0C0D
	WriteUint16LE(buf, usaOffset, usn)
	for i := 0; i < usaSize-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		WriteUint16LE(buf, usaOffset+2+i*2, ReadUint16LE(buf, sectorEnd))
		WriteUint16LE(buf, sectorEnd, usn)
	}

	return buf
}

// buildIndexAllocationAttr builds a named non-resident $INDEX_ALLOCATION
// attribute backed by a single data run.
func buildIndexAllocationAttr(t testing.TB, attrID uint16, startCluster, clusters uint64, clusterSize uint32) []byte {
	t.Helper()

	if startCluster > 0x7F || clusters > 0x7F {
		t.Fatalf("test data run needs single-byte fields, got start=%d clusters=%d", startCluster, clusters)
	}

	nameBytes := utf16LEFromString("$I30")
	nameOffset := NonResidentAttributeHeaderSize
	dataRunOffset := AlignUp(nameOffset+len(nameBytes), 8)

	run := []byte{0x11, byte(clusters), byte(startCluster), 0x00}
	length := AlignUp(dataRunOffset+len(run), 8)
	attr := make([]byte, length)

	size := clusters * uint64(clusterSize)

	WriteUint32LE(attr, 0, AttrTypeIndexAllocation)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1 // Non-resident
	attr[9] = 4 // "$I30"
	WriteUint16LE(attr, 10, uint16(nameOffset))
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0)          // Starting VCN
	WriteUint64LE(attr, 24, clusters-1) // Last VCN
	WriteUint16LE(attr, 32, uint16(dataRunOffset))
	WriteUint64LE(attr, 40, size) // Allocated size
	WriteUint64LE(attr, 48, size) // Real size
	WriteUint64LE(attr, 56, size) // Initialized size
	copy(attr[nameOffset:], nameBytes)
	copy(attr[dataRunOffset:], run)

	return attr
}

// splitIndexDirectoryImage builds an image with one directory whose index is
// split across both attributes: $INDEX_ROOT carries the 8.3 names and the INDX
// block in $INDEX_ALLOCATION carries the long names for the same files.
//
// That is the on-disk shape that panicked in v0.3.0, and it is entirely
// ordinary -- B-tree order decides which name lands where, not corruption.
func splitIndexDirectoryImage(t testing.TB) []byte {
	t.Helper()

	const (
		clusterSize     = 4096
		indexRecordSize = 4096
		indexCluster    = 8 // Past the $MFT run in buildTestNTFSImageAt
		dirEntry        = 5
	)

	fileName := func(name string, namespace uint8) []byte {
		return buildFileNameAttrValue(dirEntry, 1, name, namespace, 4096, 1024, 0x20)
	}

	// $INDEX_ROOT: the 8.3 names, at positions 0 and 1.
	root := buildIndexRootValue(indexRecordSize,
		buildIndexEntry(30, 1, fileName("AAAAAA~1.TXT", NamespaceDOS)),
		buildIndexEntry(31, 1, fileName("BBBBBB~1.TXT", NamespaceDOS)),
	)

	// $INDEX_ALLOCATION: the matching long names, in reverse order so the
	// replacement does not happen to line up with the append order.
	indx := buildIndexAllocationRecord(t, indexRecordSize, 0,
		buildIndexEntry(31, 1, fileName("beta long name.txt", NamespaceWin32)),
		buildIndexEntry(30, 1, fileName("alpha long name.txt", NamespaceWin32)),
	)

	dir := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		sequenceNum: 1,
		name:        "split",
		namespace:   NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", root),
			buildIndexAllocationAttr(t, 3, indexCluster, 1, clusterSize),
		},
	})

	image := buildTestNTFSImageAt(t, map[uint64][]byte{dirEntry: dir})
	if (indexCluster+1)*clusterSize > len(image) {
		t.Fatalf("test image is too small to hold the index record")
	}
	copy(image[indexCluster*clusterSize:], indx)

	return image
}

// TestReadDirMergesNamesSplitAcrossRootAndAllocation is the end-to-end form of
// the v0.3.0 panic: ReadDir walked $INDEX_ROOT into one slice and
// $INDEX_ALLOCATION into another while sharing the dedup maps between them, so
// resolving the long name for a file whose 8.3 name came from $INDEX_ROOT
// indexed a slice that did not hold it.
func TestReadDirMergesNamesSplitAcrossRootAndAllocation(t *testing.T) {
	v, err := Open(&mockReaderAt{data: splitIndexDirectoryImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer v.Close()

	dir, err := v.Open(5)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	entries, err := dir.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	byRef := make(map[uint64]string, len(entries))
	for _, entry := range entries {
		if existing, dup := byRef[entry.EntryNum]; dup {
			t.Fatalf("entry %d listed twice: %q and %q", entry.EntryNum, existing, entry.Name)
		}
		byRef[entry.EntryNum] = entry.Name
	}

	want := map[uint64]string{
		30: "alpha long name.txt",
		31: "beta long name.txt",
	}
	for ref, name := range want {
		if byRef[ref] != name {
			t.Errorf("entry %d: got %q, want %q", ref, byRef[ref], name)
		}
	}
	if len(entries) != len(want) {
		t.Fatalf("expected %d entries, got %d: %v", len(want), len(entries), byRef)
	}
}

// TestReadDirReportsPartialOnCorruptIndexRecord asserts the degradation
// contract: a damaged INDX block costs its own names, not the whole listing.
func TestReadDirReportsPartialOnCorruptIndexRecord(t *testing.T) {
	const (
		clusterSize  = 4096
		indexCluster = 8
	)

	image := splitIndexDirectoryImage(t)

	// Break the fixup of the INDX record. The record is skipped; the entries
	// $INDEX_ROOT already yielded must survive.
	base := indexCluster * clusterSize
	WriteUint16LE(image, base+UpdateSequenceStride-2, 0xFFFF)

	v, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer v.Close()

	dir, err := v.Open(5)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	entries, err := dir.ReadDir()
	if err == nil {
		t.Fatal("expected a partial-result error for a corrupt index record")
	}

	var partial *PartialDirError
	if !errors.As(err, &partial) {
		t.Fatalf("expected *PartialDirError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrCorruptIndex) || !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("expected the error to wrap ErrCorruptIndex and ErrInvalidIndex, got %v", err)
	}
	if partial.EntryNum != 5 {
		t.Errorf("expected the directory entry number 5, got %d", partial.EntryNum)
	}
	if partial.Skipped != 1 {
		t.Errorf("expected 1 skipped index record, got %d", partial.Skipped)
	}

	// The 8.3 names from $INDEX_ROOT are what survives.
	if len(entries) != 2 {
		t.Fatalf("expected the $INDEX_ROOT entries to survive, got %d: %+v", len(entries), entries)
	}
	if partial.Recovered != len(entries) {
		t.Errorf("Recovered=%d disagrees with %d returned entries", partial.Recovered, len(entries))
	}
	for _, entry := range entries {
		if entry.Name != "AAAAAA~1.TXT" && entry.Name != "BBBBBB~1.TXT" {
			t.Errorf("unexpected surviving entry %q", entry.Name)
		}
	}
}

// TestListFilesKeepsPartialResults guards the convenience wrappers, which
// discarded the recovered entries when ReadDir started returning both.
func TestListFilesKeepsPartialResults(t *testing.T) {
	const (
		clusterSize  = 4096
		indexCluster = 8
	)

	image := splitIndexDirectoryImage(t)
	WriteUint16LE(image, indexCluster*clusterSize+UpdateSequenceStride-2, 0xFFFF)

	v, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer v.Close()

	dir, err := v.Open(5)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	files, err := dir.ListFiles()
	if !isPartialDirError(err) {
		t.Fatalf("expected a partial-result error, got %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 recovered files, got %d", len(files))
	}
}

// TestVolumeRejectsUndersizedIndexRecord covers the second unchecked read on
// the ReadDir path: an INDX record smaller than its own header would have been
// indexed at offsets 0, 4 and 6 regardless.
func TestVolumeRejectsUndersizedIndexRecord(t *testing.T) {
	image := splitIndexDirectoryImage(t)

	// ClustersPerIndexRecord = -1, so 2^1 = 2 bytes per index record.
	image[68] = 0xFF

	_, err := Open(&mockReaderAt{data: image})
	if err == nil {
		t.Fatal("expected an undersized index record size to be rejected")
	}
	if !errors.Is(err, ErrInvalidBootSector) {
		t.Fatalf("expected ErrInvalidBootSector, got %v", err)
	}
}

// corruptSubtreeImage builds a small directory tree in which one subdirectory
// is partly readable and another is not readable at all, so a recursive
// descent has to meet both failure modes in a single walk.
//
//	/ (5)
//	├── partial   (6)  valid $INDEX_ROOT, damaged INDX block
//	├── unreadable(7)  unparsable $INDEX_ROOT
//	└── ok.txt    (8)
func corruptSubtreeImage(t testing.TB) []byte {
	t.Helper()

	const (
		clusterSize     = 4096
		indexRecordSize = 4096
		indexCluster    = 8
		rootEntry       = 5
		dirAttrs        = uint64(FileAttrDupFileNameIndexPresent)
	)

	child := func(name string, namespace uint8, attrs uint64) []byte {
		return buildFileNameAttrValue(rootEntry, 1, name, namespace, 4096, 1024, attrs)
	}

	rootIndex := buildIndexRootValue(indexRecordSize,
		buildIndexEntry(6, 1, child("partial", NamespaceWin32, dirAttrs)),
		buildIndexEntry(7, 1, child("unreadable", NamespaceWin32, dirAttrs)),
		buildIndexEntry(8, 1, child("ok.txt", NamespaceWin32, 0x20)),
	)

	root := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		sequenceNum: 1,
		name:        ".",
		namespace:   NamespaceWin32,
		extraAttrs:  [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", rootIndex)},
	})

	// "partial": one name in $INDEX_ROOT, and an INDX block that will be
	// corrupted below.
	partialIndex := buildIndexRootValue(indexRecordSize,
		buildIndexEntry(9, 1, buildFileNameAttrValue(6, 1, "survivor.txt", NamespaceWin32, 4096, 1024, 0x20)),
	)
	partial := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		sequenceNum: 1,
		name:        "partial",
		namespace:   NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", partialIndex),
			buildIndexAllocationAttr(t, 3, indexCluster, 1, clusterSize),
		},
	})

	// "unreadable": an $INDEX_ROOT too short to parse at all.
	unreadable := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		sequenceNum: 1,
		name:        "unreadable",
		namespace:   NamespaceWin32,
		extraAttrs:  [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", make([]byte, 8))},
	})

	okFile := buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse,
		sequenceNum: 1,
		name:        "ok.txt",
		namespace:   NamespaceWin32,
	})

	indx := buildIndexAllocationRecord(t, indexRecordSize, 0,
		buildIndexEntry(10, 1, buildFileNameAttrValue(6, 1, "lost.txt", NamespaceWin32, 4096, 1024, 0x20)),
	)

	image := buildTestNTFSImageAt(t, map[uint64][]byte{
		5: root, 6: partial, 7: unreadable, 8: okFile,
	})
	copy(image[indexCluster*clusterSize:], indx)

	// Break the INDX fixup so the block is skipped but $INDEX_ROOT survives.
	WriteUint16LE(image, indexCluster*clusterSize+UpdateSequenceStride-2, 0xFFFF)

	return image
}

// TestRecursiveWalkSurvivesCorruptSubtree is the objective this work exists
// for: a descent over a damaged volume must reach everything reachable, report
// what it could not read, and finish. In v0.3.0 the first bad directory took
// the goroutine down with it.
//
// The test deliberately installs no recover(). A panic anywhere below fails it.
func TestRecursiveWalkSurvivesCorruptSubtree(t *testing.T) {
	v, err := Open(&mockReaderAt{data: corruptSubtreeImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer v.Close()

	var (
		visited   []string
		partials  = map[string]*PartialDirError{}
		unreadble = map[string]error{}
	)

	var walk func(dir *File, path string)
	walk = func(dir *File, path string) {
		entries, err := dir.ReadDir()

		var partial *PartialDirError
		switch {
		case errors.As(err, &partial):
			partials[path] = partial // Keep going: entries still holds names
		case err != nil:
			unreadble[path] = err // Skip this subtree, but not the walk
			return
		}

		for _, entry := range entries {
			full := path + "/" + entry.Name
			visited = append(visited, full)

			if !entry.IsDirectory {
				continue
			}
			child, err := v.Open(entry.EntryNum)
			if err != nil {
				unreadble[full] = err
				continue
			}
			walk(child, full)
		}
	}

	root, err := v.Open(5)
	if err != nil {
		t.Fatalf("Open root failed: %v", err)
	}
	walk(root, "")

	// Everything reachable was reached, including the survivor below the
	// partially damaged directory and the sibling after the unreadable one.
	want := []string{"/partial", "/partial/survivor.txt", "/unreadable", "/ok.txt"}
	got := strings.Join(visited, ",")
	for _, name := range want {
		if !strings.Contains(got, name) {
			t.Errorf("walk never visited %q; visited %v", name, visited)
		}
	}

	// The damage was reported rather than swallowed.
	if partials["/partial"] == nil {
		t.Errorf("expected a PartialDirError for /partial, got %v", partials)
	} else if partials["/partial"].Skipped != 1 {
		t.Errorf("expected 1 skipped index record under /partial, got %d", partials["/partial"].Skipped)
	}

	if err := unreadble["/unreadable"]; err == nil {
		t.Errorf("expected /unreadable to report an error, got %v", unreadble)
	} else if !errors.Is(err, ErrCorruptIndex) {
		t.Errorf("expected ErrCorruptIndex for /unreadable, got %v", err)
	}

	// The unreadable subtree cost only itself.
	if len(unreadble) != 1 {
		t.Errorf("expected exactly one unreadable path, got %v", unreadble)
	}
}

// TestAddIndexDirEntryDropsNamelessEntries covers a shape found on a real
// volume: a $FILE_NAME with a zero name length parses cleanly and produces an
// entry a caller can do nothing with. 39 of them turned up in a 33k-file
// image, in index slack and in records damaged mid-write.
func TestAddIndexDirEntryDropsNamelessEntries(t *testing.T) {
	acc := newDirEntryAccumulator(nil)

	acc.add(IndexEntry{
		FileReference: 12, SequenceNum: 1,
		FileName: &FileName{Name: "", Namespace: NamespaceWin32},
	})
	acc.add(IndexEntry{
		FileReference: 13, SequenceNum: 1,
		FileName: &FileName{Name: "", Namespace: NamespaceDOS},
	})
	acc.add(IndexEntry{
		FileReference: 14, SequenceNum: 1,
		FileName: &FileName{Name: "real.txt", Namespace: NamespaceWin32},
	})

	if len(acc.entries) != 1 {
		t.Fatalf("expected only the named entry, got %d: %+v", len(acc.entries), acc.entries)
	}
	if acc.entries[0].Name != "real.txt" {
		t.Fatalf("unexpected entry %q", acc.entries[0].Name)
	}
}
