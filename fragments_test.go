package libntfs

import (
	"bytes"
	"testing"
)

const (
	testClusterSize  = 4096
	testMFTCluster   = 10 // newTestMFTVolume places the $MFT run here
	testMFTRecordLen = DefaultMFTRecordSize
)

// buildTwoRunDataAttr builds an unnamed non-resident $DATA attribute spanning two
// separate allocated runs, which is the minimal fragmented file.
func buildTwoRunDataAttr(t testing.TB, attrID uint16,
	firstCluster, firstLength, secondCluster, secondLength uint64,
	clusterSize uint32, realSize uint64,
) []byte {
	t.Helper()

	delta := int64(secondCluster) - int64(firstCluster)
	if firstCluster > 0x7F || secondCluster > 0x7F ||
		firstLength > 0x7F || secondLength > 0x7F ||
		delta < -0x7F || delta > 0x7F {
		t.Fatalf("test data runs need single-byte fields")
	}

	runs := []byte{
		0x11, byte(firstLength), byte(firstCluster),
		0x11, byte(secondLength), byte(int8(delta)),
		0x00,
	}

	const dataRunOffset = NonResidentAttributeHeaderSize
	length := AlignUp(dataRunOffset+len(runs), 8)
	attr := make([]byte, length)

	totalClusters := firstLength + secondLength

	WriteUint32LE(attr, 0, AttrTypeData)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1 // Non-resident
	attr[9] = 0 // No attribute name
	WriteUint16LE(attr, 10, NonResidentAttributeHeaderSize)
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0)               // Starting VCN
	WriteUint64LE(attr, 24, totalClusters-1) // Last VCN
	WriteUint16LE(attr, 32, dataRunOffset)
	WriteUint64LE(attr, 40, totalClusters*uint64(clusterSize)) // Allocated size
	WriteUint64LE(attr, 48, realSize)                          // Real size
	WriteUint64LE(attr, 56, realSize)                          // Initialized size
	copy(attr[dataRunOffset:], runs)

	return attr
}

// openFragmentTestFile builds a one-record volume around the supplied attributes
// and opens entry 0.
func openFragmentTestFile(t testing.TB, name string, extra ...[]byte) (*Volume, *File) {
	t.Helper()

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       name,
		namespace:  NamespaceWin32,
		extraAttrs: extra,
	})

	vol := newTestMFTVolume(t, [][]byte{record})

	f, err := vol.Open(0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	return vol, f
}

// TestFragmentsClampLastRunToRealSize covers the slack a cluster-rounded run
// length would otherwise report as file content.
func TestFragmentsClampLastRunToRealSize(t *testing.T) {
	const realSize = 5000 // Two clusters allocated, 3192 bytes of it slack

	_, f := openFragmentTestFile(t, "short.bin",
		buildNamedNonResidentDataAttr(t, 2, "", 6, 2, testClusterSize, realSize))

	frags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}

	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment, got %d: %+v", len(frags), frags)
	}

	frag := frags[0]
	if frag.Length != realSize {
		t.Errorf("expected the run clamped to %d bytes, got %d", realSize, frag.Length)
	}

	wantStart := int64(6 * testClusterSize)
	if frag.StartOffset != wantStart {
		t.Errorf("expected StartOffset %d, got %d", wantStart, frag.StartOffset)
	}

	if frag.EndOffset != wantStart+realSize {
		t.Errorf("expected EndOffset %d, got %d", wantStart+realSize, frag.EndOffset)
	}

	if frag.Sparse || frag.Resident {
		t.Errorf("expected an allocated non-resident fragment, got %+v", frag)
	}
}

// TestFragmentsTileStreamAcrossSparseHole covers the mapping a dropped sparse run
// would break: every fragment's FileOffset must continue where the last ended.
func TestFragmentsTileStreamAcrossSparseHole(t *testing.T) {
	const realSize = 2 * testClusterSize

	_, f := openFragmentTestFile(t, "sparse.bin",
		buildSparseThenDataAttr(t, 2, "", 1, 6, 1, testClusterSize, realSize))

	frags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}

	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments, got %d: %+v", len(frags), frags)
	}

	if !frags[0].Sparse {
		t.Errorf("expected the first fragment to be the hole, got %+v", frags[0])
	}
	if frags[0].StartOffset != 0 || frags[0].EndOffset != 0 {
		t.Errorf("expected a hole to carry no volume location, got %+v", frags[0])
	}
	if frags[0].FileOffset != 0 || frags[0].Length != testClusterSize {
		t.Errorf("expected the hole to cover the first cluster, got %+v", frags[0])
	}

	if frags[1].Sparse {
		t.Errorf("expected the second fragment to be allocated, got %+v", frags[1])
	}
	if frags[1].FileOffset != testClusterSize {
		t.Errorf("expected the allocated run at stream offset %d, got %d",
			testClusterSize, frags[1].FileOffset)
	}
	if frags[1].StartOffset != 6*testClusterSize {
		t.Errorf("expected StartOffset %d, got %d", 6*testClusterSize, frags[1].StartOffset)
	}

	// Fragments must tile the stream with no gap and no overlap.
	var covered int64
	for _, frag := range frags {
		if frag.FileOffset != covered {
			t.Fatalf("fragment at FileOffset %d leaves a gap after %d", frag.FileOffset, covered)
		}
		covered += frag.Length
	}
	if covered != realSize {
		t.Errorf("expected fragments to cover %d bytes, covered %d", realSize, covered)
	}
}

// TestFragmentsResidentLocatesValueInRecord covers the resident case, which has
// no data runs at all and must still report where the bytes live.
func TestFragmentsResidentLocatesValueInRecord(t *testing.T) {
	content := []byte("resident bytes")

	vol, f := openFragmentTestFile(t, "r.txt",
		buildResidentAttr(AttrTypeData, 2, content))

	frags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}

	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment, got %d: %+v", len(frags), frags)
	}

	frag := frags[0]
	if !frag.Resident {
		t.Errorf("expected a resident fragment, got %+v", frag)
	}
	if frag.Length != int64(len(content)) {
		t.Errorf("expected Length %d, got %d", len(content), frag.Length)
	}

	recordStart := int64(testMFTCluster * testClusterSize)
	if frag.StartOffset < recordStart || frag.StartOffset >= recordStart+testMFTRecordLen {
		t.Fatalf("expected StartOffset inside record 0 at [%d,%d), got %d",
			recordStart, recordStart+testMFTRecordLen, frag.StartOffset)
	}

	// The value sits clear of a sector boundary here, so the volume bytes at
	// StartOffset are the content itself rather than an update sequence stamp.
	got := make([]byte, len(content))
	if _, err := vol.ReadAt(got, frag.StartOffset); err != nil {
		t.Fatalf("reading the located bytes failed: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("expected the fragment to locate %q, found %q", content, got)
	}
}

func TestFragmentsEmptyWithoutDataAttribute(t *testing.T) {
	_, f := openFragmentTestFile(t, "nodata.bin")

	frags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}
	if len(frags) != 0 {
		t.Errorf("expected no fragments without a $DATA attribute, got %+v", frags)
	}
}

func TestIsFragmented(t *testing.T) {
	tests := []struct {
		name string
		attr []byte
		want bool
	}{
		{
			name: "single run",
			attr: buildNonResidentDataAttr(t, 2, 6, 2, testClusterSize),
			want: false,
		},
		{
			name: "two allocated runs",
			attr: buildTwoRunDataAttr(t, 2, 6, 1, 9, 1, testClusterSize, 2*testClusterSize),
			want: true,
		},
		{
			// A hole is not fragmentation: the allocated part is still one run.
			name: "sparse hole then one run",
			attr: buildSparseThenDataAttr(t, 2, "", 1, 6, 1, testClusterSize, 2*testClusterSize),
			want: false,
		},
		{
			name: "resident",
			attr: buildResidentAttr(AttrTypeData, 2, []byte("inline")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, f := openFragmentTestFile(t, "sample.bin", tt.attr)

			if got := f.IsFragmented(); got != tt.want {
				t.Errorf("IsFragmented() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFragmentsFollowOpenedStream covers alternate data streams, whose layout
// must come from the stream the handle was opened on rather than the host file.
func TestFragmentsFollowOpenedStream(t *testing.T) {
	const adsRealSize = 1000

	_, f := openFragmentTestFile(t, "host.txt",
		buildNonResidentDataAttr(t, 2, 6, 1, testClusterSize),
		buildNamedNonResidentDataAttr(t, 3, "Zone.Identifier", 8, 1, testClusterSize, adsRealSize))

	hostFrags, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments on the host failed: %v", err)
	}
	if len(hostFrags) != 1 || hostFrags[0].StartOffset != 6*testClusterSize {
		t.Fatalf("expected the host stream at cluster 6, got %+v", hostFrags)
	}

	ads, err := f.OpenStream("Zone.Identifier")
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	adsFrags, err := ads.Fragments()
	if err != nil {
		t.Fatalf("Fragments on the stream failed: %v", err)
	}

	if len(adsFrags) != 1 {
		t.Fatalf("expected 1 fragment on the stream, got %d: %+v", len(adsFrags), adsFrags)
	}
	if adsFrags[0].StartOffset != 8*testClusterSize {
		t.Errorf("expected the stream at cluster 8, got StartOffset %d", adsFrags[0].StartOffset)
	}
	if adsFrags[0].Length != adsRealSize {
		t.Errorf("expected the stream clamped to %d bytes, got %d", adsRealSize, adsFrags[0].Length)
	}
}

// TestAllocatedFragmentsDropHoles pins the report's view, which describes
// physical regions and so has nothing to say about a hole.
func TestAllocatedFragmentsDropHoles(t *testing.T) {
	vol, f := openFragmentTestFile(t, "sparse.bin",
		buildSparseThenDataAttr(t, 2, "", 1, 6, 1, testClusterSize, 2*testClusterSize))

	all, err := f.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 fragments before filtering, got %d", len(all))
	}

	allocated := vol.allocatedFragments(f.dataAttr)
	if len(allocated) != 1 {
		t.Fatalf("expected 1 allocated fragment, got %d: %+v", len(allocated), allocated)
	}
	if allocated[0].Sparse {
		t.Errorf("expected holes dropped, got %+v", allocated[0])
	}
}

func TestAttributeFragmentsRejectsNilAndClosed(t *testing.T) {
	vol, f := openFragmentTestFile(t, "sample.bin",
		buildNonResidentDataAttr(t, 2, 6, 1, testClusterSize))

	if _, err := vol.AttributeFragments(nil); err == nil {
		t.Error("expected an error for a nil attribute")
	}

	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := f.Fragments(); err != ErrVolumeClosed {
		t.Errorf("expected ErrVolumeClosed, got %v", err)
	}
}
