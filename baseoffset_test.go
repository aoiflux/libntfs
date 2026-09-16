package libntfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// baseOffsetTestBase is where the fixture partition is planted inside the disk
// image. It is a whole number of sectors, which Open requires.
const baseOffsetTestBase = 1 << 20

// The three fixture records, at entry numbers Report() does not skip.
const (
	baseOffsetResidentEntry   = 12
	baseOffsetFragmentedEntry = 13
	baseOffsetSparseEntry     = 14
)

// buildBaseOffsetFixtureImage assembles a volume whose files actually occupy the
// image, which the report fixture does not: its runs point past the end, so
// nothing there can be read back and compared.
//
// It holds one file of each shape whose offsets this change touches — resident,
// fragmented across two runs, and sparse followed by data — with recognisable
// bytes in each allocated cluster so a wrong offset names wrong content rather
// than merely a different number.
func buildBaseOffsetFixtureImage(t testing.TB) []byte {
	t.Helper()

	const (
		clusterSize = 4096
		firstRun    = 8
		secondRun   = 12
		sparseData  = 10
		realSize    = 2 * clusterSize
	)

	image := buildTestNTFSImageAt(t, map[uint64][]byte{
		baseOffsetResidentEntry: buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "resident.txt",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildResidentAttr(AttrTypeData, 2, []byte("resident content")),
			},
		}),
		baseOffsetFragmentedEntry: buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "fragmented.bin",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildTwoRunDataAttr(t, 2, firstRun, 1, secondRun, 1, clusterSize, realSize),
			},
		}),
		baseOffsetSparseEntry: buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "sparse.bin",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildSparseThenDataAttr(t, 2, "", 1, sparseData, 1, clusterSize, realSize),
			},
		}),
	})

	// Distinct filler per cluster, so a fragment read from the wrong place is
	// visibly the wrong bytes rather than more zeroes.
	for _, cluster := range []int{firstRun, secondRun, sparseData} {
		for i := range clusterSize {
			image[cluster*clusterSize+i] = byte(cluster*7 + i)
		}
	}

	return image
}

// openBaseOffsetPair opens the same volume twice: once through a reader scoped
// to the partition with no base, and once over the whole image with the base
// set to where the partition starts.
//
// Both handles describe identical bytes, so anything that differs between them
// is a coordinate space and nothing else.
func openBaseOffsetPair(t testing.TB) (image, disk []byte, scoped, absolute *Volume) {
	t.Helper()

	image = buildBaseOffsetFixtureImage(t)

	disk = make([]byte, baseOffsetTestBase+len(image))
	copy(disk[baseOffsetTestBase:], image)

	section := io.NewSectionReader(&mockReaderAt{data: disk}, baseOffsetTestBase, int64(len(image)))
	scoped, err := OpenWithOptions(section, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("opening the scoped reader failed: %v", err)
	}
	t.Cleanup(func() { scoped.Close() })

	absolute, err = OpenWithOptions(&mockReaderAt{data: disk}, Options{
		ReadOnly:   true,
		BaseOffset: baseOffsetTestBase,
	})
	if err != nil {
		t.Fatalf("opening the whole image failed: %v", err)
	}
	t.Cleanup(func() { absolute.Close() })

	return image, disk, scoped, absolute
}

// TestBaseOffsetReportedOffsetsAreImageAbsolute is the property the whole
// BaseOffset design exists for.
//
// A caller who hands the library a whole disk and says where the partition
// starts must get offsets comparable with ranges expressed in whole-disk
// coordinates. The check is deliberately mechanical: every reported offset from
// the based volume differs from the scoped one by exactly the base, and the
// bytes at each offset, read from the image that offset was reported against,
// are the same bytes. Offsets that merely agree could both be wrong.
func TestBaseOffsetReportedOffsetsAreImageAbsolute(t *testing.T) {
	image, disk, scoped, absolute := openBaseOffsetPair(t)

	var (
		checked       int
		sawSparse     bool
		sawResident   bool
		sawFragmented bool
	)

	for _, entry := range []uint64{
		baseOffsetResidentEntry, baseOffsetFragmentedEntry, baseOffsetSparseEntry,
	} {
		scopedFile, err := scoped.Open(entry)
		if err != nil {
			t.Fatalf("entry %d: Open on the scoped volume failed: %v", entry, err)
		}
		absoluteFile, err := absolute.Open(entry)
		if err != nil {
			t.Fatalf("entry %d: Open on the based volume failed: %v", entry, err)
		}

		want, err := scopedFile.Fragments()
		if err != nil {
			t.Fatalf("entry %d: Fragments on the scoped volume failed: %v", entry, err)
		}
		got, err := absoluteFile.Fragments()
		if err != nil {
			t.Fatalf("entry %d: Fragments on the based volume failed: %v", entry, err)
		}

		if len(want) != len(got) {
			t.Fatalf("entry %d: fragment counts differ: %d and %d", entry, len(want), len(got))
		}
		if len(want) == 0 {
			t.Fatalf("entry %d: no fragments at all", entry)
		}
		if entry == baseOffsetFragmentedEntry && len(want) > 1 {
			sawFragmented = true
		}

		for j := range want {
			wantFrag, gotFrag := want[j], got[j]

			if wantFrag.Length != gotFrag.Length {
				t.Errorf("entry %d fragment %d: lengths differ: %d and %d",
					entry, j, wantFrag.Length, gotFrag.Length)
				continue
			}
			if wantFrag.FileOffset != gotFrag.FileOffset {
				t.Errorf("entry %d fragment %d: FileOffset is stream-relative and must not shift: %d and %d",
					entry, j, wantFrag.FileOffset, gotFrag.FileOffset)
			}
			if wantFrag.Resident {
				sawResident = true
			}
			if wantFrag.Sparse {
				sawSparse = true
				continue // covered by TestBaseOffsetSentinelsStayZero
			}

			if gotFrag.StartOffset != wantFrag.StartOffset+baseOffsetTestBase {
				t.Errorf("entry %d fragment %d: StartOffset = %d, want %d (%d plus the %d base)",
					entry, j, gotFrag.StartOffset, wantFrag.StartOffset+baseOffsetTestBase,
					wantFrag.StartOffset, baseOffsetTestBase)
				continue
			}
			if gotFrag.EndOffset != wantFrag.EndOffset+baseOffsetTestBase {
				t.Errorf("entry %d fragment %d: EndOffset = %d, want %d",
					entry, j, gotFrag.EndOffset, wantFrag.EndOffset+baseOffsetTestBase)
			}

			if wantFrag.StartOffset+wantFrag.Length > int64(len(image)) {
				t.Fatalf("entry %d fragment %d: the fixture puts this run past the end of the partition",
					entry, j)
			}
			fromPartition := image[wantFrag.StartOffset : wantFrag.StartOffset+wantFrag.Length]
			fromDisk := disk[gotFrag.StartOffset : gotFrag.StartOffset+gotFrag.Length]
			if !bytes.Equal(fromPartition, fromDisk) {
				t.Errorf("entry %d fragment %d: the two offsets name different bytes", entry, j)
			}
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("no allocated fragment was checked; the fixture proves nothing")
	}
	if !sawFragmented {
		t.Error("expected a file split across more than one run")
	}
	if !sawSparse {
		t.Error("expected a sparse run")
	}
	if !sawResident {
		t.Error("expected a resident stream")
	}
}

// TestBaseOffsetSentinelsStayZero covers the fragments that have no disk
// location at all.
//
// Adding the base unconditionally would turn "nowhere" into "the first byte of
// the image", which is a location, and a plausible-looking one.
func TestBaseOffsetSentinelsStayZero(t *testing.T) {
	_, _, _, absolute := openBaseOffsetPair(t)

	f, err := absolute.Open(baseOffsetSparseEntry)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	frags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}

	var checked bool
	for j, frag := range frags {
		if !frag.Sparse {
			continue
		}
		if frag.StartOffset != 0 || frag.EndOffset != 0 {
			t.Errorf("fragment %d: a hole reported a location (%d, %d), want the zero sentinel",
				j, frag.StartOffset, frag.EndOffset)
		}
		checked = true
	}
	if !checked {
		t.Fatal("no sparse fragment was found; the fixture proves nothing")
	}

	// An attribute merged from an extension record has bytes in no single MFT
	// record, so it gets the same sentinel. Build one directly: the merge path
	// that produces one is not reachable from this fixture.
	merged := &Attribute{
		Resident: &ResidentAttribute{Value: []byte("merged value")},
	}
	if merged.sourceKnown {
		t.Fatal("sourceKnown must default to false for this to test anything")
	}

	mergedFrags, err := absolute.AttributeFragments(merged)
	if err != nil {
		t.Fatalf("AttributeFragments failed: %v", err)
	}
	if len(mergedFrags) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(mergedFrags))
	}
	if mergedFrags[0].StartOffset != 0 || mergedFrags[0].EndOffset != 0 {
		t.Errorf("an attribute of unknown origin reported a location (%d, %d), want the zero sentinel",
			mergedFrags[0].StartOffset, mergedFrags[0].EndOffset)
	}
}

// TestBaseOffsetReadAtRoundTrip covers the reason Volume.ReadAt takes image
// offsets: so a reported offset can go straight back in.
func TestBaseOffsetReadAtRoundTrip(t *testing.T) {
	_, disk, _, absolute := openBaseOffsetPair(t)

	var checked int
	for _, entry := range []uint64{baseOffsetFragmentedEntry, baseOffsetSparseEntry} {
		f, err := absolute.Open(entry)
		if err != nil {
			t.Fatalf("entry %d: Open failed: %v", entry, err)
		}
		frags, err := f.Fragments()
		if err != nil {
			t.Fatalf("entry %d: Fragments failed: %v", entry, err)
		}

		for _, frag := range frags {
			if frag.Sparse || frag.Length == 0 {
				continue
			}
			buf := make([]byte, frag.Length)
			if _, err := absolute.ReadAt(buf, frag.StartOffset); err != nil {
				t.Fatalf("entry %d: ReadAt at the reported offset %d failed: %v",
					entry, frag.StartOffset, err)
			}
			if !bytes.Equal(buf, disk[frag.StartOffset:frag.StartOffset+frag.Length]) {
				t.Errorf("entry %d: ReadAt at %d returned different bytes than the image holds there",
					entry, frag.StartOffset)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no fragment was read back; the fixture proves nothing")
	}
}

// TestBaseOffsetRejectsOffsetsBelowTheBase covers the one offset a based volume
// cannot answer for: one landing before the volume begins.
func TestBaseOffsetRejectsOffsetsBelowTheBase(t *testing.T) {
	_, _, _, absolute := openBaseOffsetPair(t)

	buf := make([]byte, 512)
	for _, offset := range []int64{0, baseOffsetTestBase - 1, -1} {
		if _, err := absolute.ReadAt(buf, offset); !errors.Is(err, ErrInvalidOffset) {
			t.Errorf("ReadAt(%d) = %v, want ErrInvalidOffset", offset, err)
		}
	}

	// The read-only refusal must still come first. An offset check that ran
	// ahead of the write gate would report the wrong problem for a handle that
	// was never going to write at all.
	if _, err := absolute.WriteAt(buf, 0); !errors.Is(err, ErrReadOnlyMode) {
		t.Errorf("WriteAt on a read-only volume = %v, want ErrReadOnlyMode", err)
	}
}

// TestBaseOffsetReportHeader pins the span of the report itself, the only
// offset pair in the document not attached to a file.
func TestBaseOffsetReportHeader(t *testing.T) {
	_, _, scoped, absolute := openBaseOffsetPair(t)

	scopedReport, err := scoped.Report()
	if err != nil {
		t.Fatalf("Report on the scoped volume failed: %v", err)
	}
	absoluteReport, err := absolute.Report()
	if err != nil {
		t.Fatalf("Report on the based volume failed: %v", err)
	}

	if scopedReport.StartOffset != 0 {
		t.Errorf("scoped StartOffset = %d, want 0", scopedReport.StartOffset)
	}
	if absoluteReport.StartOffset != baseOffsetTestBase {
		t.Errorf("based StartOffset = %d, want %d", absoluteReport.StartOffset, baseOffsetTestBase)
	}

	wantEnd := baseOffsetTestBase + int64(absolute.VolumeSize())
	if absoluteReport.EndOffset != wantEnd {
		t.Errorf("based EndOffset = %d, want %d", absoluteReport.EndOffset, wantEnd)
	}
	if got := absoluteReport.EndOffset - absoluteReport.StartOffset; got != int64(absolute.VolumeSize()) {
		t.Errorf("the span covers %d bytes, want the volume size %d", got, absolute.VolumeSize())
	}

	// NTFSMeta.Offset locates the $MFT, not the volume, and shifts like
	// everything else.
	if absoluteReport.Filesystem.Offset != scopedReport.Filesystem.Offset+baseOffsetTestBase {
		t.Errorf("NTFSMeta.Offset = %d, want %d",
			absoluteReport.Filesystem.Offset, scopedReport.Filesystem.Offset+baseOffsetTestBase)
	}

	if absolute.BaseOffset() != baseOffsetTestBase {
		t.Errorf("BaseOffset() = %d, want %d", absolute.BaseOffset(), baseOffsetTestBase)
	}
	if scoped.BaseOffset() != 0 {
		t.Errorf("scoped BaseOffset() = %d, want 0", scoped.BaseOffset())
	}
}

// TestBaseOffsetClusterConversionsRoundTrip covers the exported conversions,
// which are the other half of the image-space surface.
func TestBaseOffsetClusterConversionsRoundTrip(t *testing.T) {
	_, _, _, absolute := openBaseOffsetPair(t)

	for _, cluster := range []uint64{0, 1, 4, 15} {
		offset := absolute.ClusterToOffset(cluster)
		wantOffset := baseOffsetTestBase + int64(cluster)*int64(absolute.BytesPerCluster())
		if offset != wantOffset {
			t.Errorf("ClusterToOffset(%d) = %d, want %d", cluster, offset, wantOffset)
		}
		if got := absolute.OffsetToCluster(offset); got != cluster {
			t.Errorf("OffsetToCluster(ClusterToOffset(%d)) = %d, want %d", cluster, got, cluster)
		}
	}
}

// TestOffsetToClusterClamps covers the inputs that would otherwise wrap.
//
// OffsetToCluster has no error to return, so a negative intermediate would come
// back as a cluster number near 2^64 rather than as a complaint.
func TestOffsetToClusterClamps(t *testing.T) {
	_, _, _, absolute := openBaseOffsetPair(t)

	for _, offset := range []int64{-1, 0, baseOffsetTestBase - 1} {
		if got := absolute.OffsetToCluster(offset); got != 0 {
			t.Errorf("OffsetToCluster(%d) = %d, want 0 for an offset below the base", offset, got)
		}
	}

	// A volume with no geometry must not divide by zero.
	empty := &Volume{}
	if got := empty.OffsetToCluster(4096); got != 0 {
		t.Errorf("OffsetToCluster on a volume with no cluster size = %d, want 0", got)
	}
}

// TestOpenRejectsMisalignedBaseOffset covers a partition that starts part-way
// through one of its own sectors.
//
// The case is only reachable when the volume's sector size is larger than the
// granularity the partition was placed at — a 4Kn volume starting at a
// 512-byte boundary — so the fixture declares 4096-byte sectors and plants the
// volume half a sector off. The boot sector still parses, which is the point:
// without the check the volume would open and every sector-aligned read would
// land off a sector boundary in the image.
func TestOpenRejectsMisalignedBaseOffset(t *testing.T) {
	image := buildBaseOffsetFixtureImage(t)

	// 4096-byte sectors, one per cluster, so the cluster size and every offset
	// in the image are unchanged and only the declared sector size differs.
	const totalClusters = 16
	WriteUint16LE(image, 11, 4096)
	image[13] = 1
	WriteUint64LE(image, 40, totalClusters)

	const misaligned = baseOffsetTestBase + 512
	disk := make([]byte, misaligned+len(image))
	copy(disk[misaligned:], image)

	// Aligned to the 512 the volume was planted at, but not to its own sector.
	if misaligned%4096 == 0 {
		t.Fatal("the fixture offset is meant to be misaligned")
	}

	_, err := OpenWithOptions(&mockReaderAt{data: disk}, Options{
		ReadOnly:   true,
		BaseOffset: misaligned,
	})
	if !errors.Is(err, ErrInvalidOffset) {
		t.Fatalf("a base half a sector off = %v, want ErrInvalidOffset", err)
	}

	// The same image at a sector boundary must still open, or the check is
	// rejecting something other than the misalignment.
	aligned := make([]byte, baseOffsetTestBase+len(image))
	copy(aligned[baseOffsetTestBase:], image)

	vol, err := OpenWithOptions(&mockReaderAt{data: aligned}, Options{
		ReadOnly:   true,
		BaseOffset: baseOffsetTestBase,
	})
	if err != nil {
		t.Fatalf("the same volume on a sector boundary failed to open: %v", err)
	}
	vol.Close()
}
