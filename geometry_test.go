package libntfs

import "testing"

// The boot sector's sectors-per-cluster byte is a count up to 0x80 and, above
// that, a negative power of two: 0xF8 is 2^8 sectors and 0xF4 is 2^12. NTFS
// supports clusters up to 2 MiB since Windows 10 1709.
func TestOpenLargeClusterGeometry(t *testing.T) {
	cases := []struct {
		name    string
		spc     uint8
		cluster int
	}{
		{"64 KiB (count 0x80)", 0x80, 64 << 10},
		{"128 KiB (shift 0xF8)", 0xF8, 128 << 10},
		{"256 KiB (shift 0xF7)", 0xF7, 256 << 10},
		{"2 MiB (shift 0xF4)", 0xF4, 2 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := testGeometry{
				bytesPerSector:    512,
				sectorsPerCluster: tc.spc,
				clusterSize:       tc.cluster,
				mftCluster:        1,
				totalClusters:     3,
			}
			image := buildTestNTFSImageGeometry(t, g, map[uint64][]byte{
				5: testDirRecord(t, ".",
					buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30",
						testIndexRoot(AttrTypeFileName, CollationFileName, 4096, false, testIndexEnd(noSubNode)))),
			})
			v, err := Open(&mockReaderAt{data: image})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer v.Close()
			if got := v.BytesPerCluster(); got != uint32(tc.cluster) {
				t.Fatalf("BytesPerCluster = %d, want %d", got, tc.cluster)
			}
			if _, err := v.Open(5); err != nil {
				t.Fatalf("Open(5): %v", err)
			}
		})
	}
}

// TestOpenRejectsOversizedClusterShift guards the shift: 0xF3 would be 2^13
// sectors, 4 MiB clusters, which NTFS does not define.
func TestOpenRejectsOversizedClusterShift(t *testing.T) {
	image := buildTestNTFSImageAt(t, nil)
	image[13] = 0xF3
	if _, err := Open(&mockReaderAt{data: image}); err == nil {
		t.Fatal("expected 4 MiB clusters to be rejected")
	}
	image[13] = 0x81 // 2^127 sectors
	if _, err := Open(&mockReaderAt{data: image}); err == nil {
		t.Fatal("expected a shift of 127 to be rejected")
	}
}

// TestReadDirOnLargeClusters lists a directory whose INDX blocks are smaller
// than a cluster, so its VCNs count 512-byte units: block 1 is VCN 8.
func TestReadDirOnLargeClusters(t *testing.T) {
	const (
		cluster   = 128 << 10
		blockSize = 4096
		dirEntry  = 5
		allocAt   = 2
	)
	g := testGeometry{
		bytesPerSector:    512,
		sectorsPerCluster: 0xF8,
		clusterSize:       cluster,
		mftCluster:        1,
		totalClusters:     3,
	}

	root := testIndexRoot(AttrTypeFileName, CollationFileName, blockSize, true,
		testIndexEntry(30, 1, testFileNameKey(dirEntry, "m.txt", FileAttrArchive), 0),
		testIndexEnd(blockSize/512),
	)
	left := testIndexBlock(t, blockSize, 0, false,
		testIndexEntry(31, 1, testFileNameKey(dirEntry, "a.txt", FileAttrArchive), noSubNode),
		testIndexEnd(noSubNode),
	)
	right := testIndexBlock(t, blockSize, blockSize/512, false,
		testIndexEntry(32, 1, testFileNameKey(dirEntry, "z.txt", FileAttrArchive), noSubNode),
		testIndexEnd(noSubNode),
	)
	dir := testDirRecord(t, "dir",
		buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", root),
		testNamedIndexAllocationAttr(t, 3, "$I30", allocAt, 1, cluster),
		testBitmapAttr(4, "$I30", 0x03),
	)

	image := buildTestNTFSImageGeometry(t, g, map[uint64][]byte{dirEntry: dir})
	copy(image[allocAt*cluster:], left)
	copy(image[allocAt*cluster+blockSize:], right)

	entries, err := readDirOf(t, image, dirEntry)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	byName := dirEntriesByName(entries)
	for _, name := range []string{"a.txt", "m.txt", "z.txt"} {
		if e, ok := byName[name]; !ok || e.Deleted {
			t.Fatalf("%s = %+v (present %v), want live", name, e, ok)
		}
	}
}
