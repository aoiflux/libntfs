package libntfs

import (
	"sync"
	"testing"
)

// These tests exist to be run under -race. They drive the access patterns a
// concurrent caller actually produces, which is the only way to tell a lock that
// works from a lock that is merely present.

// TestConcurrentGetMFTEntry hammers the MFT cache from many goroutines, mixing
// hits and misses on the same entries so the read path and the fill path overlap.
func TestConcurrentGetMFTEntry(t *testing.T) {
	vol := newConcurrencyTestVolume(t)

	const (
		goroutines = 16
		iterations = 200
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for i := 0; i < iterations; i++ {
				entryNum := uint64((i + id) % 3)

				entry, err := vol.GetMFTEntry(entryNum)
				if err != nil {
					continue
				}

				// Read through the entry the way a caller would, which is what
				// turns a mutation elsewhere into a race rather than a
				// theoretical one.
				_ = entry.IsDirectory()
				_ = entry.IsInUse()
				for _, attr := range entry.Attributes {
					_ = attr.GetAttributeName()
					_ = attr.ReadSupport()
				}
			}
		}(g)
	}

	wg.Wait()
}

// TestConcurrentOpenAndRead opens and reads the same files from many goroutines.
// Each File carries its own offset, but they share the cached MFTEntry behind it.
func TestConcurrentOpenAndRead(t *testing.T) {
	vol := newConcurrencyTestVolume(t)

	const goroutines = 16

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for i := 0; i < 100; i++ {
				file, err := vol.Open(0)
				if err != nil {
					continue
				}

				_ = file.Name()
				_ = file.Size()
				_ = file.IsDirectory()
				_ = file.ReadSupport()
				_ = file.Streams()
				_, _ = file.Fragments()

				buf := make([]byte, 8)
				_, _ = file.ReadAt(buf, 0)
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentVolumeAccessors drives the accessors a caller reaches for while
// other goroutines are doing real work.
func TestConcurrentVolumeAccessors(t *testing.T) {
	vol := newConcurrencyTestVolume(t)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for i := 0; i < 300; i++ {
				_ = vol.BytesPerCluster()
				_ = vol.BytesPerSector()
				_ = vol.MFTRecordSize()
				_ = vol.VolumeSize()
				_ = vol.ClusterCount()
				_ = vol.IsClosed()
				_ = vol.IsWritable()
				_, _ = vol.MFTEntryCount()
			}
		}()
	}

	// The parent-map fallback flag is the one piece of volume state a caller can
	// write while others read.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			vol.SetMFTParentFallback(i%2 == 0)
		}
	}()

	wg.Wait()
}

// TestConcurrentBufferPool exercises the pooled buffers from several goroutines,
// since a pool handing the same buffer to two callers would be invisible until
// they both wrote to it.
func TestConcurrentBufferPool(t *testing.T) {
	vol := newConcurrencyTestVolume(t)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for i := 0; i < 200; i++ {
				buf := vol.GetBuffer()
				if buf == nil {
					continue
				}

				// Write a goroutine-specific pattern and read it back. Two
				// goroutines holding the same buffer would corrupt each other.
				want := byte(id + 1)
				for j := range *buf {
					(*buf)[j] = want
				}
				for j := range *buf {
					if (*buf)[j] != want {
						t.Errorf("buffer handed to two goroutines: byte %d is %d, want %d",
							j, (*buf)[j], want)
						break
					}
				}

				vol.PutBuffer(buf)
			}
		}(g)
	}

	wg.Wait()
}

// newConcurrencyTestVolume builds a small volume with a few records to contend
// over.
func newConcurrencyTestVolume(t testing.TB) *Volume {
	t.Helper()

	records := [][]byte{
		buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "first.txt",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildResidentAttr(AttrTypeData, 2, []byte("shared content")),
			},
		}),
		buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "second.bin",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildNonResidentDataAttr(t, 2, 6, 1, 4096),
			},
		}),
		buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse | MFTFlagDirectory,
			name:      "dir",
			namespace: NamespaceWin32,
		}),
	}

	return newTestMFTVolume(t, records)
}

// TestConcurrentCorpusWalk drives a real volume from many goroutines. The
// synthetic volumes have a single-run $MFT, so the run-index lookup they
// exercise is trivial; a real volume's is not.
func TestConcurrentCorpusWalk(t *testing.T) {
	vol := openCorpus(t)

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}
	if count == 0 {
		t.Skip("no MFT entries to walk")
	}

	const goroutines = 12

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Each goroutine walks a different stride through the table, so the
			// cache sees a mix of hits and misses rather than one shared front.
			for entryNum := uint64(id); entryNum < count; entryNum += goroutines {
				entry, err := vol.GetMFTEntry(entryNum)
				if err != nil {
					continue
				}

				_ = entry.IsDirectory()
				for _, attr := range entry.Attributes {
					_ = attr.ReadSupport()
				}

				file, err := vol.Open(entryNum)
				if err != nil {
					continue
				}

				_ = file.Streams()
				_, _ = file.Fragments()
				_, _ = file.SecurityDescriptor()

				if !file.IsDirectory() && file.Size() > 0 {
					buf := make([]byte, 512)
					_, _ = file.ReadAt(buf, 0)
				}
			}
		}(g)
	}

	wg.Wait()
}

// TestConcurrentCorpusReadSameFile has every goroutine read the same file at
// the same time, which is where a shared read offset or a shared scratch buffer
// would corrupt results rather than merely race.
func TestConcurrentCorpusReadSameFile(t *testing.T) {
	vol := openCorpus(t)

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	entries, err := root.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	// Pick the largest non-resident file, so reads span several clusters.
	var target uint64
	var largest int64
	for _, entry := range entries {
		file, err := vol.Open(entry.EntryNum)
		if err != nil || file.IsDirectory() || !file.ReadSupport().NonResident {
			continue
		}
		if file.Size() > largest {
			largest, target = file.Size(), entry.EntryNum
		}
	}

	if largest == 0 {
		t.Skip("no non-resident file to read concurrently")
	}

	// Read the reference copy single-threaded first.
	reference, err := vol.Open(target)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	want := make([]byte, 64*1024)
	if _, err := reference.ReadAt(want, 0); err != nil {
		t.Fatalf("reference read failed: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			file, err := vol.Open(target)
			if err != nil {
				t.Errorf("Open failed: %v", err)
				return
			}

			for i := 0; i < 20; i++ {
				got := make([]byte, len(want))
				if _, err := file.ReadAt(got, 0); err != nil {
					t.Errorf("concurrent read failed: %v", err)
					return
				}
				for j := range want {
					if got[j] != want[j] {
						t.Errorf("concurrent read differs from the single-threaded read at byte %d", j)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
}
