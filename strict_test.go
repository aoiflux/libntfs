package libntfs

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// strictParts describes a directory (entry 5) holding a.txt to d.txt (30 to
// 33) in a two-level $I30, on an image with a real $UpCase. Each strict check
// is exercised by breaking one part of it.
type strictParts struct {
	rootNode    bool
	rootEntries [][]byte
	blocks      [][]byte // block i sits at cluster strictAllocCluster+i
	bitmap      []byte   // nil leaves $BITMAP:$I30 out
	records     map[uint64][]byte
}

const (
	strictDir          = 5
	strictBlockSize    = 4096
	strictAllocCluster = 16
	strictUpCaseAt     = 32
)

func strictKey(name string) []byte {
	return testFileNameKey(strictDir, name, FileAttrArchive)
}

func strictFile(t testing.TB, name string) []byte {
	return buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse, sequenceNum: 1, name: name, namespace: NamespaceWin32,
		parentRef: strictDir, parentSeq: 1,
	})
}

func goodStrictParts(t testing.TB) *strictParts {
	return &strictParts{
		rootNode: true,
		rootEntries: [][]byte{
			testIndexEntry(31, 1, strictKey("b.txt"), 0),
			testIndexEnd(1),
		},
		blocks: [][]byte{
			testIndexBlock(t, strictBlockSize, 0, false,
				testIndexEntry(30, 1, strictKey("a.txt"), noSubNode),
				testIndexEnd(noSubNode)),
			testIndexBlock(t, strictBlockSize, 1, false,
				testIndexEntry(32, 1, strictKey("c.txt"), noSubNode),
				testIndexEntry(33, 1, strictKey("d.txt"), noSubNode),
				testIndexEnd(noSubNode)),
		},
		bitmap: []byte{0x03},
		records: map[uint64][]byte{
			30: strictFile(t, "a.txt"),
			31: strictFile(t, "b.txt"),
			32: strictFile(t, "c.txt"),
			33: strictFile(t, "d.txt"),
			MFTEntryUpCase: buildTestMFTRecord(t, testRecordOpts{
				flags: MFTFlagInUse, sequenceNum: 1, name: "$UpCase", namespace: NamespaceWin32,
				extraAttrs: [][]byte{buildNonResidentDataAttr(t, 2, strictUpCaseAt, 32, 4096)},
			}),
		},
	}
}

func (p *strictParts) image(t testing.TB) []byte {
	t.Helper()
	attrs := [][]byte{
		buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30",
			testIndexRoot(AttrTypeFileName, CollationFileName, strictBlockSize, p.rootNode, p.rootEntries...)),
		testNamedIndexAllocationAttr(t, 3, "$I30", strictAllocCluster, uint64(len(p.blocks)), strictBlockSize),
	}
	if p.bitmap != nil {
		attrs = append(attrs, testBitmapAttr(4, "$I30", p.bitmap...))
	}
	records := map[uint64][]byte{strictDir: testDirRecord(t, ".", attrs...)}
	for n, r := range p.records {
		records[n] = r
	}

	g := defaultTestGeometry
	g.totalClusters = 64
	image := buildTestNTFSImageGeometry(t, g, records)
	for i, block := range p.blocks {
		copy(image[(strictAllocCluster+i)*strictBlockSize:], block)
	}
	copy(image[strictUpCaseAt*4096:], DefaultUpCase().Bytes())
	return image
}

func openTestVolume(t testing.TB, image []byte, opts Options) *Volume {
	t.Helper()
	v, err := OpenWithOptions(&mockReaderAt{data: image}, opts)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

func listDir(t testing.TB, v *Volume, entryNum uint64) ([]DirEntry, error) {
	t.Helper()
	dir, err := v.Open(entryNum)
	if err != nil {
		t.Fatalf("Open(%d): %v", entryNum, err)
	}
	return dir.ReadDir()
}

func liveNames(entries []DirEntry) []string {
	var names []string
	for _, e := range entries {
		if !e.Deleted {
			names = append(names, e.Name)
		}
	}
	slices.Sort(names)
	return names
}

var strictGoodNames = []string{"a.txt", "b.txt", "c.txt", "d.txt"}

func TestStrictAcceptsWellFormedDirectory(t *testing.T) {
	v := openTestVolume(t, goodStrictParts(t).image(t), Options{ReadOnly: true, Strict: true})
	if !v.IsStrict() {
		t.Fatal("IsStrict = false")
	}
	entries, err := listDir(t, v, strictDir)
	if err != nil {
		t.Fatalf("strict ReadDir: %v", err)
	}
	if got := liveNames(entries); !slices.Equal(got, strictGoodNames) {
		t.Fatalf("names = %q, want %q", got, strictGoodNames)
	}
	if _, err := v.OpenPath("/c.txt"); err != nil {
		t.Fatalf("strict OpenPath: %v", err)
	}
}

// TestStrictRejectsEachFault breaks one thing at a time. Strict mode must
// name the check; the default mode must read past it, returning wantDefault
// as the live listing with no error unless defaultErr is set.
func TestStrictRejectsEachFault(t *testing.T) {
	cases := []struct {
		name        string
		check       string
		mutate      func(t testing.TB, p *strictParts)
		wantDefault []string
		defaultErr  bool
	}{
		{"keys out of order", "collation", func(t testing.TB, p *strictParts) {
			p.blocks[1] = testIndexBlock(t, strictBlockSize, 1, false,
				testIndexEntry(33, 1, strictKey("d.txt"), noSubNode),
				testIndexEntry(32, 1, strictKey("c.txt"), noSubNode),
				testIndexEnd(noSubNode))
		}, strictGoodNames, false},
		{"block records the wrong VCN", "vcn", func(t testing.TB, p *strictParts) {
			p.blocks[1] = testIndexBlock(t, strictBlockSize, 5, false,
				testIndexEntry(32, 1, strictKey("c.txt"), noSubNode),
				testIndexEntry(33, 1, strictKey("d.txt"), noSubNode),
				testIndexEnd(noSubNode))
		}, strictGoodNames, false},
		{"leaf root with sub-nodes", "node-flag", func(t testing.TB, p *strictParts) {
			p.rootNode = false
		}, strictGoodNames, false},
		{"torn block", "partial-listing", func(t testing.TB, p *strictParts) {
			WriteUint16LE(p.blocks[1], UpdateSequenceStride-2, 0xFFFF)
		}, []string{"a.txt", "b.txt"}, true},
		{"reachable block free in $BITMAP", "bitmap-clear", func(t testing.TB, p *strictParts) {
			p.bitmap = []byte{0x01}
		}, []string{"a.txt", "b.txt"}, false},
		{"unreachable block in use in $BITMAP", "bitmap-unreachable", func(t testing.TB, p *strictParts) {
			p.blocks = append(p.blocks, testIndexBlock(t, strictBlockSize, 2, false, testIndexEnd(noSubNode)))
			p.bitmap = []byte{0x07}
		}, strictGoodNames, false},
		{"no $BITMAP", "bitmap-missing", func(t testing.TB, p *strictParts) {
			p.bitmap = nil
		}, strictGoodNames, false},
		{"block reached twice", "revisit", func(t testing.TB, p *strictParts) {
			p.rootEntries = [][]byte{testIndexEntry(31, 1, strictKey("b.txt"), 0), testIndexEnd(0)}
		}, strictGoodNames, false},
		{"record not in use", "record-not-in-use", func(t testing.TB, p *strictParts) {
			p.records[33] = buildTestMFTRecord(t, testRecordOpts{
				sequenceNum: 1, name: "d.txt", namespace: NamespaceWin32, parentRef: strictDir, parentSeq: 1})
		}, strictGoodNames, false},
		{"stale sequence number", "sequence", func(t testing.TB, p *strictParts) {
			p.blocks[1] = testIndexBlock(t, strictBlockSize, 1, false,
				testIndexEntry(32, 1, strictKey("c.txt"), noSubNode),
				testIndexEntry(33, 2, strictKey("d.txt"), noSubNode),
				testIndexEnd(noSubNode))
		}, strictGoodNames, false},
		{"record has another name", "file-name", func(t testing.TB, p *strictParts) {
			p.records[32] = strictFile(t, "cc.txt")
		}, strictGoodNames, false},
		{"entry names another parent", "parent", func(t testing.TB, p *strictParts) {
			p.blocks[0] = testIndexBlock(t, strictBlockSize, 0, false,
				testIndexEntry(30, 1, testFileNameKey(6, "a.txt", FileAttrArchive), noSubNode),
				testIndexEnd(noSubNode))
		}, strictGoodNames, false},
		{"file record flagged directory", "directory-flag", func(t testing.TB, p *strictParts) {
			p.records[33] = buildTestMFTRecord(t, testRecordOpts{
				flags: MFTFlagInUse | MFTFlagDirectory, sequenceNum: 1, name: "d.txt",
				namespace: NamespaceWin32, parentRef: strictDir, parentSeq: 1})
		}, strictGoodNames, false},
		{"$DATA runs short of allocated size", "data-coverage", func(t testing.TB, p *strictParts) {
			data := buildNonResidentDataAttr(t, 2, 20, 1, 4096)
			WriteUint64LE(data, 40, 2*4096)
			p.records[30] = buildTestMFTRecord(t, testRecordOpts{
				flags: MFTFlagInUse, sequenceNum: 1, name: "a.txt", namespace: NamespaceWin32,
				parentRef: strictDir, parentSeq: 1, extraAttrs: [][]byte{data}})
		}, strictGoodNames, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := goodStrictParts(t)
			tc.mutate(t, parts)
			image := parts.image(t)

			_, err := listDir(t, openTestVolume(t, image, Options{Strict: true}), strictDir)
			var se *StrictError
			if !errors.As(err, &se) {
				t.Fatalf("strict ReadDir = %v, want a *StrictError", err)
			}
			if se.Check != tc.check {
				t.Fatalf("strict check = %q (%v), want %q", se.Check, err, tc.check)
			}
			if !errors.Is(err, ErrStrictViolation) || se.Record == 0 {
				t.Fatalf("StrictError %v does not match ErrStrictViolation or names no record", err)
			}

			entries, err := listDir(t, openTestVolume(t, image, Options{}), strictDir)
			if (err != nil) != tc.defaultErr {
				t.Fatalf("default ReadDir error = %v, want error %v", err, tc.defaultErr)
			}
			if got := liveNames(entries); !slices.Equal(got, tc.wantDefault) {
				t.Fatalf("default listing = %q, want %q", got, tc.wantDefault)
			}
		})
	}
}

func TestStrictErrorNamesTheBlock(t *testing.T) {
	parts := goodStrictParts(t)
	parts.blocks[1] = testIndexBlock(t, strictBlockSize, 5, false, testIndexEnd(noSubNode))
	_, err := listDir(t, openTestVolume(t, parts.image(t), Options{Strict: true}), strictDir)
	var se *StrictError
	if !errors.As(err, &se) || se.Record != strictDir || se.Index != "$I30" || se.VCN != 1 {
		t.Fatalf("StrictError = %+v, want record 5, index $I30, VCN 1", se)
	}
}

func TestStrictOpenChecksDataCoverage(t *testing.T) {
	parts := goodStrictParts(t)
	data := buildNonResidentDataAttr(t, 2, 20, 1, 4096)
	WriteUint64LE(data, 48, 3*4096) // real size past allocated size
	parts.records[30] = buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse, sequenceNum: 1, name: "a.txt", namespace: NamespaceWin32,
		parentRef: strictDir, parentSeq: 1, extraAttrs: [][]byte{data}})
	image := parts.image(t)

	_, err := openTestVolume(t, image, Options{Strict: true}).Open(30)
	var se *StrictError
	if !errors.As(err, &se) || se.Check != "data-coverage" || se.Record != 30 {
		t.Fatalf("strict Open = %v, want a data-coverage StrictError on record 30", err)
	}
	if _, err := openTestVolume(t, image, Options{}).Open(30); err != nil {
		t.Fatalf("default Open = %v", err)
	}
}

func TestStrictOpenPathFailsOnPartialDirectory(t *testing.T) {
	parts := goodStrictParts(t)
	WriteUint16LE(parts.blocks[1], UpdateSequenceStride-2, 0xFFFF)
	image := parts.image(t)

	if _, err := openTestVolume(t, image, Options{Strict: true}).OpenPath("/a.txt"); !errors.Is(err, ErrStrictViolation) {
		t.Fatalf("strict OpenPath = %v, want ErrStrictViolation", err)
	}
	if _, err := openTestVolume(t, image, Options{}).OpenPath("/a.txt"); err != nil {
		t.Fatalf("default OpenPath = %v", err)
	}
}

// TestDirectoryFlagWin32BitOnly covers NTFS-R4: an index key carrying only the
// Win32 FILE_ATTRIBUTE_DIRECTORY bit (0x10) is a directory to the tolerant
// ReadDir, but strict mode wants the 0x10000000 bit Windows and ntfs-3g write
// into a directory's $FILE_NAME, and rejects it.
func TestDirectoryFlagWin32BitOnly(t *testing.T) {
	parts := goodStrictParts(t)
	parts.blocks[0] = testIndexBlock(t, strictBlockSize, 0, false,
		testIndexEntry(30, 1, testFileNameKey(strictDir, "a.txt", FileAttrDirectory), noSubNode),
		testIndexEnd(noSubNode))
	parts.records[30] = buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagDirectory, sequenceNum: 1, name: "a.txt", namespace: NamespaceWin32,
		parentRef: strictDir, parentSeq: 1,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30",
			testIndexRoot(AttrTypeFileName, CollationFileName, strictBlockSize, false, testIndexEnd(noSubNode)))},
	})
	image := parts.image(t)

	entries, err := listDir(t, openTestVolume(t, image, Options{}), strictDir)
	if err != nil {
		t.Fatalf("default ReadDir: %v", err)
	}
	if a := dirEntriesByName(entries)["a.txt"]; !a.IsDirectory {
		t.Fatalf("default ReadDir: a.txt = %+v, want a directory", a)
	}

	_, err = listDir(t, openTestVolume(t, image, Options{Strict: true}), strictDir)
	var se *StrictError
	if !errors.As(err, &se) || se.Check != "directory-flag" {
		t.Fatalf("strict ReadDir = %v, want check directory-flag", err)
	}
	if !strings.Contains(err.Error(), "lacks 0x10000000") {
		t.Fatalf("strict error %q does not name the missing 0x10000000 bit", err)
	}
}

// TestDirectoryFlagWin32BitOnFileRecord is the other half: a key that carries
// only the Win32 0x10 bit lists as a directory by default, so strict mode has
// to refuse it when the record it names is a plain file.
func TestDirectoryFlagWin32BitOnFileRecord(t *testing.T) {
	parts := goodStrictParts(t)
	parts.blocks[0] = testIndexBlock(t, strictBlockSize, 0, false,
		testIndexEntry(30, 1, testFileNameKey(strictDir, "a.txt", FileAttrDirectory), noSubNode),
		testIndexEnd(noSubNode))
	image := parts.image(t)

	entries, err := listDir(t, openTestVolume(t, image, Options{}), strictDir)
	if err != nil {
		t.Fatalf("default ReadDir: %v", err)
	}
	if a := dirEntriesByName(entries)["a.txt"]; !a.IsDirectory {
		t.Fatalf("default ReadDir: a.txt = %+v, want it listed as a directory", a)
	}

	_, err = listDir(t, openTestVolume(t, image, Options{Strict: true}), strictDir)
	var se *StrictError
	if !errors.As(err, &se) || se.Check != "directory-flag" || se.Entry != 30 {
		t.Fatalf("strict ReadDir = %v, want check directory-flag on entry 30", err)
	}
}
