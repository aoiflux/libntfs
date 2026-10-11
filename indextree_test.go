package libntfs

import "testing"

// Builders for multi-level indexes: a node $INDEX_ROOT, INDX blocks that are
// themselves nodes, $BITMAP attributes, and arbitrary keys, so the tests can
// describe a B-tree the way NTFS lays one out rather than only the leaf-root
// shape buildIndexRootValue produces.

// noSubNode marks an index entry without a sub-node pointer.
const noSubNode = -1

// testIndexEntry builds one index entry. key is the indexed value ($FILE_NAME
// for $I30); subVCN, when not noSubNode, is stored in the entry's last eight
// bytes and sets IndexFlagNode.
func testIndexEntry(ref uint64, seq uint16, key []byte, subVCN int64) []byte {
	length := AlignUp(16+len(key), 8)
	if subVCN != noSubNode {
		length += 8
	}
	buf := make([]byte, length)
	WriteUint64LE(buf, 0, ref|uint64(seq)<<48)
	WriteUint16LE(buf, 8, uint16(length))
	WriteUint16LE(buf, 10, uint16(len(key)))
	copy(buf[16:], key)
	if subVCN != noSubNode {
		buf[12] |= IndexFlagNode
		WriteUint64LE(buf, length-8, uint64(subVCN))
	}
	return buf
}

// testViewIndexEntry builds an entry of a view index ($R, $SII, $SDH, $O, $Q),
// whose header holds a data offset and length instead of a file reference.
func testViewIndexEntry(key, data []byte, subVCN int64) []byte {
	length := AlignUp(16+len(key)+len(data), 8)
	if subVCN != noSubNode {
		length += 8
	}
	buf := make([]byte, length)
	WriteUint16LE(buf, 0, uint16(16+len(key)))
	WriteUint16LE(buf, 2, uint16(len(data)))
	WriteUint16LE(buf, 8, uint16(length))
	WriteUint16LE(buf, 10, uint16(len(key)))
	copy(buf[16:], key)
	copy(buf[16+len(key):], data)
	if subVCN != noSubNode {
		buf[12] |= IndexFlagNode
		WriteUint64LE(buf, length-8, uint64(subVCN))
	}
	return buf
}

// testIndexEnd builds the end-of-node entry, with a sub-node when subVCN is
// not noSubNode.
func testIndexEnd(subVCN int64) []byte {
	buf := testIndexEntry(0, 0, nil, subVCN)
	buf[12] |= IndexFlagLast
	return buf
}

// testFileNameKey builds an $I30 key: a $FILE_NAME naming parent.
func testFileNameKey(parent uint64, name string, attrs uint32) []byte {
	return buildFileNameAttrValue(parent, 1, name, NamespaceWin32, 0, 0, uint64(attrs))
}

// testIndexRoot builds an $INDEX_ROOT value. entries must end with an end
// entry; isNode sets the large-index flag.
func testIndexRoot(indexedType, collation, blockSize uint32, isNode bool, entries ...[]byte) []byte {
	var body []byte
	for _, e := range entries {
		body = append(body, e...)
	}
	buf := make([]byte, 32+len(body))
	WriteUint32LE(buf, 0, indexedType)
	WriteUint32LE(buf, 4, collation)
	WriteUint32LE(buf, 8, blockSize)
	buf[12] = 1
	WriteUint32LE(buf, 16, 16)
	WriteUint32LE(buf, 20, uint32(16+len(body)))
	WriteUint32LE(buf, 24, uint32(16+len(body)))
	if isNode {
		WriteUint32LE(buf, 28, IndexNodeNode)
	}
	copy(buf[32:], body)
	return buf
}

// testIndexBlock builds a fixup-protected INDX block. entries must end with an
// end entry.
func testIndexBlock(t testing.TB, blockSize int, vcn int64, isNode bool, entries ...[]byte) []byte {
	t.Helper()

	const usaOffset = 40
	usaCount := blockSize/UpdateSequenceStride + 1
	entriesStart := AlignUp(usaOffset+usaCount*2, 8)

	var body []byte
	for _, e := range entries {
		body = append(body, e...)
	}
	if entriesStart+len(body) > blockSize {
		t.Fatalf("index entries do not fit in a %d byte block", blockSize)
	}

	buf := make([]byte, blockSize)
	copy(buf[entriesStart:], body)
	WriteUint32LE(buf, 0, IndexAllocationMagic)
	WriteUint16LE(buf, 4, usaOffset)
	WriteUint16LE(buf, 6, uint16(usaCount))
	WriteUint64LE(buf, 8, 0x1122334455667788)
	WriteUint64LE(buf, 16, uint64(vcn))
	const nodeHeader = 24
	WriteUint32LE(buf, nodeHeader, uint32(entriesStart-nodeHeader))
	WriteUint32LE(buf, nodeHeader+4, uint32(entriesStart+len(body)-nodeHeader))
	WriteUint32LE(buf, nodeHeader+8, uint32(blockSize-nodeHeader))
	if isNode {
		WriteUint32LE(buf, nodeHeader+12, IndexNodeNode)
	}

	const usn = 0x0C0D
	WriteUint16LE(buf, usaOffset, usn)
	for i := 0; i < usaCount-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		WriteUint16LE(buf, usaOffset+2+i*2, ReadUint16LE(buf, sectorEnd))
		WriteUint16LE(buf, sectorEnd, usn)
	}
	return buf
}

// testNamedIndexAllocationAttr builds a named non-resident $INDEX_ALLOCATION
// backed by one run of single-byte fields.
func testNamedIndexAllocationAttr(t testing.TB, attrID uint16, name string, startCluster, clusters uint64, clusterSize uint32) []byte {
	t.Helper()
	return testNamedNonResidentAttr(t, AttrTypeIndexAllocation, attrID, name, startCluster, clusters, clusterSize)
}

// testNamedNonResidentAttr builds a named non-resident attribute of any type
// backed by one run of single-byte fields.
func testNamedNonResidentAttr(t testing.TB, attrType uint32, attrID uint16, name string, startCluster, clusters uint64, clusterSize uint32) []byte {
	t.Helper()

	if startCluster > 0x7F || clusters > 0x7F {
		t.Fatalf("test data run needs single-byte fields, got start=%d clusters=%d", startCluster, clusters)
	}

	nameBytes := utf16LEFromString(name)
	nameOffset := NonResidentAttributeHeaderSize
	dataRunOffset := AlignUp(nameOffset+len(nameBytes), 8)
	run := []byte{0x11, byte(clusters), byte(startCluster), 0x00}
	length := AlignUp(dataRunOffset+len(run), 8)
	attr := make([]byte, length)
	size := clusters * uint64(clusterSize)

	WriteUint32LE(attr, 0, attrType)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1
	attr[9] = uint8(len([]rune(name)))
	WriteUint16LE(attr, 10, uint16(nameOffset))
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0)
	WriteUint64LE(attr, 24, clusters-1)
	WriteUint16LE(attr, 32, uint16(dataRunOffset))
	WriteUint64LE(attr, 40, size)
	WriteUint64LE(attr, 48, size)
	WriteUint64LE(attr, 56, size)
	copy(attr[nameOffset:], nameBytes)
	copy(attr[dataRunOffset:], run)
	return attr
}

// testBitmapAttr builds a resident $BITMAP named name.
func testBitmapAttr(attrID uint16, name string, bits ...byte) []byte {
	return buildNamedResidentAttr(AttrTypeBitmap, attrID, name, bits)
}

// testDirRecord builds a directory record carrying the given index
// attributes.
func testDirRecord(t testing.TB, name string, attrs ...[]byte) []byte {
	t.Helper()
	return buildTestMFTRecord(t, testRecordOpts{
		flags:       MFTFlagInUse | MFTFlagDirectory,
		sequenceNum: 1,
		name:        name,
		namespace:   NamespaceWin32,
		extraAttrs:  attrs,
	})
}

// testImageWithBlocks builds a default-geometry image holding records and
// copies each INDX block to its cluster.
func testImageWithBlocks(t testing.TB, records map[uint64][]byte, blocks map[int][]byte) []byte {
	t.Helper()
	const clusterSize = 4096
	image := buildTestNTFSImageAt(t, records)
	for cluster, block := range blocks {
		if (cluster+1)*clusterSize > len(image) {
			t.Fatalf("cluster %d is past the end of the test image", cluster)
		}
		copy(image[cluster*clusterSize:], block)
	}
	return image
}

// readDirOf opens image and lists the directory at entryNum.
func readDirOf(t testing.TB, image []byte, entryNum uint64) ([]DirEntry, error) {
	t.Helper()
	v, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open volume: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	dir, err := v.Open(entryNum)
	if err != nil {
		t.Fatalf("Open(%d): %v", entryNum, err)
	}
	return dir.ReadDir()
}

func dirEntriesByName(entries []DirEntry) map[string]DirEntry {
	out := make(map[string]DirEntry, len(entries))
	for _, e := range entries {
		out[e.Name] = e
	}
	return out
}

// TestReadDirMarksEntriesOfFreedIndexBlocksDeleted covers NTFS-3: a block
// whose $BITMAP:$I30 bit is clear has been released by the directory, so the
// names still sitting in it are history, not live children.
func TestReadDirMarksEntriesOfFreedIndexBlocksDeleted(t *testing.T) {
	const (
		dirEntry  = 5
		blockSize = 4096
		cluster   = 8
	)

	root := testIndexRoot(AttrTypeFileName, CollationFileName, blockSize, false,
		testIndexEntry(30, 1, testFileNameKey(dirEntry, "keep.txt", FileAttrArchive), noSubNode),
		testIndexEnd(noSubNode),
	)
	block := testIndexBlock(t, blockSize, 0, false,
		testIndexEntry(31, 1, testFileNameKey(dirEntry, "gone.txt", FileAttrArchive), noSubNode),
		testIndexEnd(noSubNode),
	)
	dir := testDirRecord(t, "dir",
		buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", root),
		testNamedIndexAllocationAttr(t, 3, "$I30", cluster, 1, blockSize),
		testBitmapAttr(4, "$I30", 0x00),
	)

	entries, err := readDirOf(t, testImageWithBlocks(t, map[uint64][]byte{dirEntry: dir}, map[int][]byte{cluster: block}), dirEntry)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	byName := dirEntriesByName(entries)
	if e, ok := byName["keep.txt"]; !ok || e.Deleted {
		t.Fatalf("keep.txt = %+v (present %v), want live", e, ok)
	}
	if e, ok := byName["gone.txt"]; !ok || !e.Deleted {
		t.Fatalf("gone.txt = %+v (present %v), want Deleted", e, ok)
	}
}

// TestReadDirReadsAllocatedIndexBlocksLive is the companion: a set bit keeps
// the block's entries live, so honouring the bitmap costs nothing on a
// healthy directory.
func TestReadDirReadsAllocatedIndexBlocksLive(t *testing.T) {
	const (
		dirEntry  = 5
		blockSize = 4096
		cluster   = 8
	)

	root := testIndexRoot(AttrTypeFileName, CollationFileName, blockSize, true,
		testIndexEnd(0),
	)
	block := testIndexBlock(t, blockSize, 0, false,
		testIndexEntry(30, 1, testFileNameKey(dirEntry, "a.txt", FileAttrArchive), noSubNode),
		testIndexEntry(31, 1, testFileNameKey(dirEntry, "b.txt", FileAttrArchive), noSubNode),
		testIndexEnd(noSubNode),
	)
	dir := testDirRecord(t, "dir",
		buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", root),
		testNamedIndexAllocationAttr(t, 3, "$I30", cluster, 1, blockSize),
		testBitmapAttr(4, "$I30", 0x01),
	)

	entries, err := readDirOf(t, testImageWithBlocks(t, map[uint64][]byte{dirEntry: dir}, map[int][]byte{cluster: block}), dirEntry)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	byName := dirEntriesByName(entries)
	for _, name := range []string{"a.txt", "b.txt"} {
		if e, ok := byName[name]; !ok || e.Deleted {
			t.Fatalf("%s = %+v (present %v), want live", name, e, ok)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
}
