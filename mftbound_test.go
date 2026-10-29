package libntfs

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// Nothing checked the $MFT's own $DATA run list against the volume up to
// v0.3.5, so one flipped byte in it could claim an MFT far larger than the
// disk. MFTEntryCount reported that, WalkMFT looped over it, and a caller
// sizing an allocation by it died (PAR-34).

// buildForgedMFTDataAttr builds a non-resident $DATA attribute carrying an
// arbitrary run list, so a test can describe an $MFT no volume could hold.
func buildForgedMFTDataAttr(t testing.TB, attrID uint16, runs []DataRun, clusterSize uint32) []byte {
	t.Helper()

	runlist := EncodeRunlist(runs)

	const dataRunOffset = NonResidentAttributeHeaderSize
	length := AlignUp(dataRunOffset+len(runlist), 8)
	attr := make([]byte, length)

	var clusters, lastVCN uint64
	for _, run := range runs {
		clusters += run.LengthClusters
	}
	if clusters > 0 {
		lastVCN = clusters - 1
	}
	// Deliberately consistent with the forged run list rather than with the
	// volume: a damaged run list is what this fixture is for.
	allocated := clusters * uint64(clusterSize)

	WriteUint32LE(attr, 0, AttrTypeData)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1 // Non-resident
	attr[9] = 0 // No attribute name
	WriteUint16LE(attr, 10, NonResidentAttributeHeaderSize)
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0) // Starting VCN
	WriteUint64LE(attr, 24, lastVCN)
	WriteUint16LE(attr, 32, dataRunOffset)
	WriteUint64LE(attr, 40, allocated)
	WriteUint64LE(attr, 48, allocated)
	WriteUint64LE(attr, 56, allocated)
	copy(attr[dataRunOffset:], runlist)

	return attr
}

// buildForgedMFTImage assembles an openable image whose $MFT record 0 carries
// the supplied $DATA run list. The record is fixup-stamped as NTFS writes it,
// so Open accepts the record and the damage is in the run list alone, which is
// what a flipped byte on a real volume looks like.
func buildForgedMFTImage(t testing.TB, runs []DataRun) []byte {
	t.Helper()

	g := defaultTestGeometry

	mftRecord := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "$MFT",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildForgedMFTDataAttr(t, 2, runs, uint32(g.clusterSize)),
		},
	})

	image := make([]byte, g.totalClusters*g.clusterSize)
	copy(image, buildTestBootSector(uint16(g.bytesPerSector), g.sectorsPerCluster,
		uint64(g.totalClusters*g.clusterSize/g.bytesPerSector), uint64(g.mftCluster)))
	copy(image[g.mftCluster*g.clusterSize:], mftRecord)

	return image
}

// soundMFTRuns is the run list buildForgedMFTImage's geometry really has: one
// cluster of MFT at cluster 4 of 16.
func soundMFTRuns() []DataRun {
	return []DataRun{{StartCluster: int64(defaultTestGeometry.mftCluster), LengthClusters: 1}}
}

// newBoundedTestVolume builds a volume whose geometry is sound and whose $MFT
// run list is whatever the caller supplies, without going through Open. It is
// the unit-level equivalent of buildForgedMFTImage.
func newBoundedTestVolume(runs []DataRun) *Volume {
	const (
		clusterSize   = uint32(4096)
		totalClusters = 16
	)

	vol := &Volume{
		reader:          &mockReaderAt{data: make([]byte, totalClusters*int(clusterSize))},
		bytesPerSector:  512,
		bytesPerCluster: clusterSize,
		clusterCount:    totalClusters,
		volumeSize:      totalClusters * uint64(clusterSize),
		mftRecordSize:   DefaultMFTRecordSize,
		mftCluster:      4,
		mftDataRuns:     runs,
		mftCache:        make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
	}
	vol.rebuildMFTRunIndex()

	return vol
}

// TestMFTEntryCountCapsAtVolumeSize checks the ceiling: the $MFT cannot hold
// more records than the volume has room for, whatever its run list claims.
func TestMFTEntryCountCapsAtVolumeSize(t *testing.T) {
	vol := newBoundedTestVolume([]DataRun{{StartCluster: 4, LengthClusters: 1 << 40}})

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}

	ceiling := vol.volumeSize / uint64(vol.mftRecordSize)
	if count > ceiling {
		t.Fatalf("MFTEntryCount = %d, which is past the %d records the %d byte volume holds",
			count, ceiling, vol.volumeSize)
	}
}

// TestMFTEntryCountDoesNotWrapOnOverflow is the other half: a run length whose
// product with the cluster size overflows a uint64 must not wrap to a small
// count, which would hide the whole table instead of over-reporting it.
func TestMFTEntryCountDoesNotWrapOnOverflow(t *testing.T) {
	// 1<<62 clusters of 4096 bytes is 2^74, which wraps to 1024 in 64 bits, so
	// the unfixed arithmetic reports exactly one record.
	vol := newBoundedTestVolume([]DataRun{{StartCluster: 4, LengthClusters: 1 << 62}})

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}

	ceiling := vol.volumeSize / uint64(vol.mftRecordSize)
	if count != ceiling {
		t.Fatalf("MFTEntryCount = %d, want the %d record ceiling; an overflowing "+
			"product must not wrap to a small count", count, ceiling)
	}
}

// TestMFTEntryCountSoundRunListUnchanged guards against the cap swallowing a
// legitimate count.
func TestMFTEntryCountSoundRunListUnchanged(t *testing.T) {
	vol := newBoundedTestVolume(soundMFTRuns())

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}

	if want := uint64(4096 / DefaultMFTRecordSize); count != want {
		t.Fatalf("MFTEntryCount = %d, want %d for a one-cluster $MFT", count, want)
	}
}

// TestWalkMFTFinishesOnForgedRunList is the hang: WalkMFT iterates [0, count),
// skipping every unreadable record, so an inflated count is an unbounded loop
// from the caller's point of view.
func TestWalkMFTFinishesOnForgedRunList(t *testing.T) {
	vol := newBoundedTestVolume([]DataRun{{StartCluster: 4, LengthClusters: 1 << 40}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- vol.WalkMFT(ctx, WalkConfig{}, func(uint64, *MFTEntry) error { return nil })
	}()

	select {
	case err := <-done:
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("WalkMFT ran until the deadline instead of finishing over a bounded count")
		}
		if err != nil {
			t.Fatalf("WalkMFT failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WalkMFT did not return even after its context expired")
	}
}

// TestEachMFTEntryFinishesOnForgedRunList is the same hang through the API that
// cannot be cancelled at all.
func TestEachMFTEntryFinishesOnForgedRunList(t *testing.T) {
	vol := newBoundedTestVolume([]DataRun{{StartCluster: 4, LengthClusters: 1 << 40}})

	done := make(chan error, 1)
	go func() {
		done <- vol.EachMFTEntry(func(uint64, *MFTEntry) error { return nil })
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EachMFTEntry failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EachMFTEntry did not finish; it has no context to cancel")
	}
}

// TestMFTDataRunsErrorReportsForgedRunList checks that a caller can tell a
// capped count from a real one.
func TestMFTDataRunsErrorReportsForgedRunList(t *testing.T) {
	t.Run("forged run list reported", func(t *testing.T) {
		vol, err := Open(&mockReaderAt{data: buildForgedMFTImage(t,
			[]DataRun{{StartCluster: 4, LengthClusters: 1 << 40}})})
		if err != nil {
			t.Fatalf("Open refused a damaged volume in tolerant mode: %v", err)
		}
		defer vol.Close()

		runsErr := vol.MFTDataRunsError()
		if !errors.Is(runsErr, ErrInvalidMFTRuns) {
			t.Fatalf("MFTDataRunsError = %v, want one matching ErrInvalidMFTRuns", runsErr)
		}
		if !errors.Is(runsErr, ErrInvalidDataRun) {
			t.Errorf("error %v does not match ErrInvalidDataRun, which it wraps", runsErr)
		}
		if !strings.Contains(runsErr.Error(), "$MFT") {
			t.Errorf("error %q does not name $MFT", runsErr)
		}

		count, err := vol.MFTEntryCount()
		if err != nil {
			t.Fatalf("MFTEntryCount failed: %v", err)
		}
		if ceiling := vol.VolumeSize() / uint64(vol.MFTRecordSize()); count > ceiling {
			t.Fatalf("MFTEntryCount = %d, past the %d record ceiling", count, ceiling)
		}
	})

	t.Run("run list outside the volume reported", func(t *testing.T) {
		// The length fits, but the run starts past the last cluster.
		vol, err := Open(&mockReaderAt{data: buildForgedMFTImage(t,
			[]DataRun{{StartCluster: 1 << 30, LengthClusters: 1}})})
		if err != nil {
			t.Fatalf("Open refused a damaged volume in tolerant mode: %v", err)
		}
		defer vol.Close()

		if runsErr := vol.MFTDataRunsError(); !errors.Is(runsErr, ErrInvalidMFTRuns) {
			t.Fatalf("MFTDataRunsError = %v, want one matching ErrInvalidMFTRuns", runsErr)
		}
	})

	t.Run("sound run list reports nothing", func(t *testing.T) {
		vol, err := Open(&mockReaderAt{data: buildForgedMFTImage(t, soundMFTRuns())})
		if err != nil {
			t.Fatalf("Open failed on a sound image: %v", err)
		}
		defer vol.Close()

		if runsErr := vol.MFTDataRunsError(); runsErr != nil {
			t.Fatalf("MFTDataRunsError = %v on a sound $MFT run list, want nil", runsErr)
		}
	})
}

// TestOpenStrictRejectsForgedMFTRunList checks the strict-mode half of the
// decision: tolerant mode caps and reports, strict mode refuses.
func TestOpenStrictRejectsForgedMFTRunList(t *testing.T) {
	image := buildForgedMFTImage(t, []DataRun{{StartCluster: 4, LengthClusters: 1 << 40}})

	vol, err := OpenWithOptions(&mockReaderAt{data: image}, Options{Strict: true})
	if err == nil {
		vol.Close()
		t.Fatal("strict mode opened a volume whose $MFT run list is outside it")
	}
	if !errors.Is(err, ErrInvalidMFTRuns) {
		t.Fatalf("error = %v, want one matching ErrInvalidMFTRuns", err)
	}
	var se *StrictError
	if !errors.As(err, &se) || se.Check != "mft-runs" || se.Record != MFTEntryMFT || !errors.Is(err, ErrStrictViolation) {
		t.Fatalf("error = %v, want a *StrictError with check mft-runs on record 0", err)
	}

	sound, err := OpenWithOptions(&mockReaderAt{data: buildForgedMFTImage(t, soundMFTRuns())},
		Options{Strict: true})
	if err != nil {
		t.Fatalf("strict mode refused a sound image: %v", err)
	}
	sound.Close()
}

// TestBuildRunEndsSaturatesOnOverflow covers the cumulative index every MFT
// lookup consults. A wrapped cumulative would alias a huge entry number onto a
// small, valid offset and read the wrong record.
func TestBuildRunEndsSaturatesOnOverflow(t *testing.T) {
	runs := []DataRun{
		{StartCluster: 4, LengthClusters: 1},
		{StartCluster: 5, LengthClusters: 1 << 62},
		{StartCluster: 6, LengthClusters: 1},
	}

	ends := buildRunEnds(runs, 4096)
	if len(ends) != len(runs) {
		t.Fatalf("buildRunEnds returned %d ends for %d runs", len(ends), len(runs))
	}

	if ends[0] != 4096 {
		t.Errorf("end of the first run = %d, want 4096", ends[0])
	}
	for i := 1; i < len(ends); i++ {
		if ends[i] < ends[i-1] {
			t.Fatalf("run end %d (%d) is below run end %d (%d); the cumulative wrapped",
				i, ends[i], i-1, ends[i-1])
		}
	}
	if ends[len(ends)-1] != math.MaxUint64 {
		t.Errorf("final run end = %d, want it saturated at %d",
			ends[len(ends)-1], uint64(math.MaxUint64))
	}
}

// TestOpenRejectsWrappingVolumeSize covers the boot-sector product the record
// ceiling divides by: total sectors times bytes per sector, taken in 64 bits.
func TestOpenRejectsWrappingVolumeSize(t *testing.T) {
	image := buildForgedMFTImage(t, soundMFTRuns())

	// 2^56 sectors of 512 bytes is 2^65, which wraps to zero.
	WriteUint64LE(image, 40, 1<<56)

	vol, err := Open(&mockReaderAt{data: image})
	if err == nil {
		vol.Close()
		t.Fatal("Open accepted a boot sector whose volume size overflows 64 bits")
	}
	if !errors.Is(err, ErrInvalidBootSector) {
		t.Fatalf("error = %v, want one matching ErrInvalidBootSector", err)
	}
}
