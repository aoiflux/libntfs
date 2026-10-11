package libntfs

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"
)

type walked struct {
	name  string
	depth int
	vcn   int64
}

func walkNames(t testing.TB, f *File, index string) ([]walked, error) {
	t.Helper()
	var out []walked
	err := f.WalkIndex(index, func(e IndexEntry, depth int, vcn int64) error {
		name := ""
		if e.FileName != nil {
			name = e.FileName.Name
		}
		out = append(out, walked{name, depth, vcn})
		return nil
	})
	return out, err
}

func TestWalkIndexVisitsInCollationOrder(t *testing.T) {
	v := openTestVolume(t, goodStrictParts(t).image(t), Options{})
	dir, err := v.Open(strictDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := walkNames(t, dir, "$I30")
	if err != nil {
		t.Fatalf("WalkIndex: %v", err)
	}
	want := []walked{{"a.txt", 1, 0}, {"b.txt", 0, -1}, {"c.txt", 1, 1}, {"d.txt", 1, 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("WalkIndex = %+v, want %+v", got, want)
	}

	stop := errors.New("stop")
	calls := 0
	if err := dir.WalkIndex("$I30", func(IndexEntry, int, int64) error { calls++; return stop }); err != stop || calls != 1 {
		t.Fatalf("WalkIndex did not stop on the callback's error: err %v after %d calls", err, calls)
	}
}

func TestWalkIndexTolerantSkipsDamage(t *testing.T) {
	parts := goodStrictParts(t)
	WriteUint16LE(parts.blocks[1], UpdateSequenceStride-2, 0xFFFF)
	dir, err := openTestVolume(t, parts.image(t), Options{}).Open(strictDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := walkNames(t, dir, "$I30")
	if !errors.Is(err, ErrCorruptIndex) {
		t.Fatalf("WalkIndex = %v, want ErrCorruptIndex", err)
	}
	if want := []walked{{"a.txt", 1, 0}, {"b.txt", 0, -1}}; !slices.Equal(got, want) {
		t.Fatalf("WalkIndex = %+v, want %+v", got, want)
	}
}

func TestReadIndexRootBlockAndBitmap(t *testing.T) {
	dir, err := openTestVolume(t, goodStrictParts(t).image(t), Options{}).Open(strictDir)
	if err != nil {
		t.Fatal(err)
	}

	root, err := dir.ReadIndexRoot("$I30")
	if err != nil {
		t.Fatalf("ReadIndexRoot: %v", err)
	}
	if root.AttributeType != AttrTypeFileName || root.CollationRule != CollationFileName ||
		root.IndexBlockSize != strictBlockSize || root.NodeHeader.Flags&IndexNodeNode == 0 {
		t.Fatalf("root header = %+v", root)
	}
	if len(root.Entries) != 2 || root.Entries[0].FileName.Name != "b.txt" || root.Entries[0].FileReference != 31 ||
		root.Entries[0].SubNodeVCN != 0 || root.Entries[1].Flags&IndexFlagLast == 0 || root.Entries[1].SubNodeVCN != 1 {
		t.Fatalf("root entries = %+v", root.Entries)
	}
	if !bytes.Equal(root.Entries[0].Key, strictKey("b.txt")) {
		t.Fatal("root entry Key is not the $FILE_NAME")
	}

	block, err := dir.ReadIndexBlock("$I30", 1)
	if err != nil {
		t.Fatalf("ReadIndexBlock: %v", err)
	}
	if block.VCN != 1 || block.IsNode || block.LSN != 0x1122334455667788 || len(block.Raw) != strictBlockSize {
		t.Fatalf("block header = VCN %d node %v LSN %#x raw %d", block.VCN, block.IsNode, block.LSN, len(block.Raw))
	}
	var names []string
	for _, e := range block.Entries {
		if e.FileName != nil {
			names = append(names, e.FileName.Name)
		}
	}
	if !slices.Equal(names, []string{"c.txt", "d.txt"}) {
		t.Fatalf("block names = %q", names)
	}

	if _, err := dir.ReadIndexBlock("$I30", 2); !errors.Is(err, ErrCorruptIndex) {
		t.Fatalf("ReadIndexBlock past the allocation = %v, want ErrCorruptIndex", err)
	}
	if _, err := dir.ReadIndexRoot("$R"); !errors.Is(err, ErrAttributeNotFound) {
		t.Fatalf("ReadIndexRoot($R) = %v, want ErrAttributeNotFound", err)
	}

	bitmap, err := dir.IndexBitmap("$I30")
	if err != nil || !bytes.Equal(bitmap, []byte{0x03}) {
		t.Fatalf("IndexBitmap = % x, %v", bitmap, err)
	}
	if _, err := dir.IndexBitmap("$R"); !errors.Is(err, ErrAttributeNotFound) {
		t.Fatalf("IndexBitmap($R) = %v, want ErrAttributeNotFound", err)
	}
}

func TestViewIndexEntriesCarryKeyAndData(t *testing.T) {
	key := appendUint32LE(nil, 0x100)
	data := []byte("descriptor header")
	root := testIndexRoot(0, CollationULong, strictBlockSize, false,
		testViewIndexEntry(key, data, noSubNode),
		testIndexEnd(noSubNode))
	record := buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagViewIndex, sequenceNum: 1, name: "$Secure", namespace: NamespaceWin32,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$SII", root)},
	})
	f, err := openTestVolume(t, buildTestNTFSImageAt(t, map[uint64][]byte{MFTEntrySecure: record}), Options{Strict: true}).Open(MFTEntrySecure)
	if err != nil {
		t.Fatal(err)
	}
	var got []IndexEntry
	if err := f.WalkIndex("$SII", func(e IndexEntry, _ int, _ int64) error { got = append(got, e); return nil }); err != nil {
		t.Fatalf("WalkIndex($SII): %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Key, key) || !bytes.Equal(got[0].Data, data) || got[0].FileName != nil {
		t.Fatalf("$SII entries = %+v", got)
	}
}

func TestWalkIndexOnSubClusterBlocks(t *testing.T) {
	const (
		cluster   = 128 << 10
		blockSize = 4096
		dirEntry  = 5
		allocAt   = 2
	)
	g := testGeometry{bytesPerSector: 512, sectorsPerCluster: 0xF8, clusterSize: cluster, mftCluster: 1, totalClusters: 3}
	root := testIndexRoot(AttrTypeFileName, CollationFileName, blockSize, true,
		testIndexEntry(30, 1, testFileNameKey(dirEntry, "m.txt", FileAttrArchive), 0),
		testIndexEnd(blockSize/512))
	dir := testDirRecord(t, "dir",
		buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", root),
		testNamedIndexAllocationAttr(t, 3, "$I30", allocAt, 1, cluster),
		testBitmapAttr(4, "$I30", 0x03))
	image := buildTestNTFSImageGeometry(t, g, map[uint64][]byte{dirEntry: dir})
	copy(image[allocAt*cluster:], testIndexBlock(t, blockSize, 0, false,
		testIndexEntry(31, 1, testFileNameKey(dirEntry, "a.txt", FileAttrArchive), noSubNode), testIndexEnd(noSubNode)))
	copy(image[allocAt*cluster+blockSize:], testIndexBlock(t, blockSize, blockSize/512, false,
		testIndexEntry(32, 1, testFileNameKey(dirEntry, "z.txt", FileAttrArchive), noSubNode), testIndexEnd(noSubNode)))

	f, err := openTestVolume(t, image, Options{}).Open(dirEntry)
	if err != nil {
		t.Fatal(err)
	}
	got, err := walkNames(t, f, "$I30")
	if err != nil {
		t.Fatalf("WalkIndex: %v", err)
	}
	if want := []walked{{"a.txt", 1, 0}, {"m.txt", 0, -1}, {"z.txt", 1, 8}}; !slices.Equal(got, want) {
		t.Fatalf("WalkIndex = %+v, want %+v", got, want)
	}
}

func TestFileNamesReturnsEveryLink(t *testing.T) {
	v := openTestVolume(t, hardLinkedImage(t), Options{})
	entry, err := v.GetMFTEntry(30)
	if err != nil {
		t.Fatal(err)
	}
	names, err := entry.FileNames()
	if err != nil {
		t.Fatalf("FileNames: %v", err)
	}
	var got []string
	for _, fn := range names {
		got = append(got, fn.Name)
	}
	if want := []string{"one", "two", "three"}; !slices.Equal(got, want) {
		t.Fatalf("FileNames = %q, want %q", got, want)
	}
	if names[2].Namespace != NamespacePOSIX {
		t.Fatalf("third name namespace = %d, want POSIX", names[2].Namespace)
	}

	empty := &MFTEntry{}
	if _, err := empty.FileNames(); !errors.Is(err, ErrAttributeNotFound) {
		t.Fatalf("FileNames on an entry with none = %v, want ErrAttributeNotFound", err)
	}
}

func TestDirEntryCarriesIndexFileName(t *testing.T) {
	parts := goodStrictParts(t)
	stamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	key := strictKey("a.txt")
	WriteUint64LE(key, 0x18, TimeToFiletime(stamp))
	parts.blocks[0] = testIndexBlock(t, strictBlockSize, 0, false,
		testIndexEntry(30, 1, key, noSubNode), testIndexEnd(noSubNode))

	entries, err := listDir(t, openTestVolume(t, parts.image(t), Options{}), strictDir)
	if err != nil {
		t.Fatal(err)
	}
	a := dirEntriesByName(entries)["a.txt"]
	if !a.MFTChangeTime.Equal(stamp) || a.Namespace != NamespaceWin32 ||
		a.ParentReference != MakeFileReference(strictDir, 1) || a.FileName == nil || a.FileName.Name != "a.txt" {
		t.Fatalf("a.txt = %+v", a)
	}
}

// reparseParts lays out $Extend (11), $Extend\$Reparse (26) and two reparse
// points: a symlink (40) and a junction (41).
type reparseParts struct {
	keys    [][]byte // $R keys, in index order
	records map[uint64][]byte
}

func reparseKey(tag uint32, entry uint64) []byte {
	return slices.Concat(appendUint32LE(nil, tag), appendUint64LE(nil, MakeFileReference(entry, 1)))
}

func appendUint64LE(b []byte, v uint64) []byte {
	return appendUint32LE(appendUint32LE(b, uint32(v)), uint32(v>>32))
}

func goodReparseParts(t testing.TB) *reparseParts {
	symlink, err := EncodeSymlinkReparse(`\??\C:\target`, `C:\target`, false)
	if err != nil {
		t.Fatal(err)
	}
	junction, err := EncodeMountPointReparse(`\??\C:\dir`, `C:\dir`)
	if err != nil {
		t.Fatal(err)
	}
	withReparse := func(name string, value []byte) []byte {
		return buildTestMFTRecord(t, testRecordOpts{
			flags: MFTFlagInUse, sequenceNum: 1, name: name, namespace: NamespaceWin32, parentRef: MFTEntryRoot,
			extraAttrs: [][]byte{buildResidentAttr(AttrTypeReparsePoint, 2, value)},
		})
	}
	return &reparseParts{
		keys: [][]byte{reparseKey(ReparsePointMountPoint, 41), reparseKey(ReparsePointSymlink, 40)},
		records: map[uint64][]byte{
			40: withReparse("link", symlink),
			41: withReparse("junction", junction),
			42: strictFile(t, "plain.txt"),
		},
	}
}

func (p *reparseParts) image(t testing.TB) []byte {
	t.Helper()
	var entries [][]byte
	for _, k := range p.keys {
		entries = append(entries, testViewIndexEntry(k, nil, noSubNode))
	}
	entries = append(entries, testIndexEnd(noSubNode))

	extend := buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagDirectory, sequenceNum: 1, name: "$Extend", namespace: NamespaceWin32,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30",
			testIndexRoot(AttrTypeFileName, CollationFileName, 4096, false,
				testIndexEntry(26, 1, testFileNameKey(MFTEntryExtend, "$Reparse", FileAttrViewIndex), noSubNode),
				testIndexEnd(noSubNode)))},
	})
	reparse := buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagViewIndex, sequenceNum: 1, name: "$Reparse", namespace: NamespaceWin32,
		parentRef: MFTEntryExtend, parentSeq: 1,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$R",
			testIndexRoot(0, CollationULongs, 4096, false, entries...))},
	})

	records := map[uint64][]byte{MFTEntryExtend: extend, 26: reparse}
	for n, r := range p.records {
		records[n] = r
	}
	return buildTestNTFSImageAt(t, records)
}

func TestVerifyReparseIndex(t *testing.T) {
	if err := openTestVolume(t, goodReparseParts(t).image(t), Options{}).VerifyReparseIndex(); err != nil {
		t.Fatalf("VerifyReparseIndex on an exact index: %v", err)
	}

	cases := []struct {
		name   string
		check  string
		mutate func(p *reparseParts)
	}{
		{"missing entry", "reparse-missing", func(p *reparseParts) {
			p.keys = p.keys[:1]
		}},
		{"extra entry", "reparse-extra", func(p *reparseParts) {
			p.keys = append(p.keys, reparseKey(ReparsePointSymlink, 42))
		}},
		{"wrong tag", "reparse-tag", func(p *reparseParts) {
			p.keys = [][]byte{reparseKey(ReparsePointSymlink, 40), reparseKey(ReparsePointSymlink, 41)}
		}},
		{"out of order", "collation", func(p *reparseParts) {
			p.keys[0], p.keys[1] = p.keys[1], p.keys[0]
		}},
		{"short key", "reparse-key", func(p *reparseParts) {
			p.keys[0] = p.keys[0][:8]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := goodReparseParts(t)
			tc.mutate(parts)
			err := openTestVolume(t, parts.image(t), Options{}).VerifyReparseIndex()
			var se *StrictError
			if !errors.As(err, &se) || se.Check != tc.check {
				t.Fatalf("VerifyReparseIndex = %v, want check %q", err, tc.check)
			}
		})
	}
}

// TestWalkIndexDepthTwo walks a three-level $I30: the root points at block 0,
// itself a node over blocks 2 and 3, and at leaf block 1. Every sub-node comes
// out before the entry that points at it, with its own depth and VCN.
func TestWalkIndexDepthTwo(t *testing.T) {
	parts := goodStrictParts(t)
	parts.rootEntries = [][]byte{
		testIndexEntry(36, 1, strictKey("m.txt"), 0),
		testIndexEnd(1),
	}
	parts.blocks = [][]byte{
		testIndexBlock(t, strictBlockSize, 0, true,
			testIndexEntry(32, 1, strictKey("c.txt"), 2),
			testIndexEnd(3)),
		testIndexBlock(t, strictBlockSize, 1, false,
			testIndexEntry(37, 1, strictKey("x.txt"), noSubNode),
			testIndexEntry(38, 1, strictKey("z.txt"), noSubNode),
			testIndexEnd(noSubNode)),
		testIndexBlock(t, strictBlockSize, 2, false,
			testIndexEntry(30, 1, strictKey("a.txt"), noSubNode),
			testIndexEntry(31, 1, strictKey("b.txt"), noSubNode),
			testIndexEnd(noSubNode)),
		testIndexBlock(t, strictBlockSize, 3, false,
			testIndexEntry(33, 1, strictKey("d.txt"), noSubNode),
			testIndexEnd(noSubNode)),
	}
	parts.bitmap = []byte{0x0F}
	image := parts.image(t)

	want := []walked{
		{"a.txt", 2, 2}, {"b.txt", 2, 2}, {"c.txt", 1, 0}, {"d.txt", 2, 3},
		{"m.txt", 0, -1}, {"x.txt", 1, 1}, {"z.txt", 1, 1},
	}
	for _, opts := range []Options{{}, {Strict: true}} {
		dir, err := openTestVolume(t, image, opts).Open(strictDir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := walkNames(t, dir, "$I30")
		if err != nil {
			t.Fatalf("WalkIndex (strict %v): %v", opts.Strict, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("WalkIndex (strict %v) = %+v, want %+v", opts.Strict, got, want)
		}
	}
}

// TestWalkIndexSkipsFreedBlock: a block whose bit is clear and which no entry
// points at is history, so the walk never visits it, in either mode.
func TestWalkIndexSkipsFreedBlock(t *testing.T) {
	parts := goodStrictParts(t)
	parts.blocks = append(parts.blocks, testIndexBlock(t, strictBlockSize, 2, false,
		testIndexEntry(34, 1, strictKey("freed.txt"), noSubNode),
		testIndexEnd(noSubNode)))
	parts.bitmap = []byte{0x03}
	image := parts.image(t)

	want := []walked{{"a.txt", 1, 0}, {"b.txt", 0, -1}, {"c.txt", 1, 1}, {"d.txt", 1, 1}}
	for _, opts := range []Options{{}, {Strict: true}} {
		dir, err := openTestVolume(t, image, opts).Open(strictDir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := walkNames(t, dir, "$I30")
		if err != nil {
			t.Fatalf("WalkIndex (strict %v): %v", opts.Strict, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("WalkIndex (strict %v) = %+v, want %+v", opts.Strict, got, want)
		}
	}
}

// TestWalkIndexReparseView walks a two-level $R, which collates by ULONGS
// (the tag, then each half of the file reference), and gets back the raw
// 12-byte keys in that order.
func TestWalkIndexReparseView(t *testing.T) {
	const (
		reparseEntry = 6
		blockSize    = 4096
		cluster      = 8
	)
	keys := [][]byte{
		reparseKey(ReparsePointMountPoint, 41),
		reparseKey(ReparsePointSymlink, 40),
		reparseKey(ReparsePointSymlink, 45),
	}
	root := testIndexRoot(0, CollationULongs, blockSize, true,
		testViewIndexEntry(keys[1], nil, 0),
		testIndexEnd(1))
	left := testIndexBlock(t, blockSize, 0, false,
		testViewIndexEntry(keys[0], nil, noSubNode),
		testIndexEnd(noSubNode))
	right := testIndexBlock(t, blockSize, 1, false,
		testViewIndexEntry(keys[2], nil, noSubNode),
		testIndexEnd(noSubNode))
	record := buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagViewIndex, sequenceNum: 1, name: "$Reparse", namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$R", root),
			testNamedIndexAllocationAttr(t, 3, "$R", cluster, 2, blockSize),
			testBitmapAttr(4, "$R", 0x03),
		},
	})
	image := testImageWithBlocks(t, map[uint64][]byte{reparseEntry: record},
		map[int][]byte{cluster: left, cluster + 1: right})

	for _, opts := range []Options{{}, {Strict: true}} {
		f, err := openTestVolume(t, image, opts).Open(reparseEntry)
		if err != nil {
			t.Fatal(err)
		}
		var got [][]byte
		var depths []int
		err = f.WalkIndex("$R", func(e IndexEntry, depth int, _ int64) error {
			if e.FileName != nil {
				t.Errorf("$R entry carries a FileName: %+v", e.FileName)
			}
			got = append(got, e.Key)
			depths = append(depths, depth)
			return nil
		})
		if err != nil {
			t.Fatalf("WalkIndex($R) (strict %v): %v", opts.Strict, err)
		}
		if !slices.EqualFunc(got, keys, bytes.Equal) || !slices.Equal(depths, []int{1, 0, 1}) {
			t.Fatalf("WalkIndex($R) (strict %v) = keys %x depths %v, want keys %x depths [1 0 1]",
				opts.Strict, got, depths, keys)
		}
	}
}
