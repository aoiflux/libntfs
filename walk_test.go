package libntfs

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// The property that matters for the parallel walk is that it is not observably
// parallel: same entries, same order, same result, whatever the worker count.
// Everything below is a way of checking that, or of checking that the goroutines
// it starts all end.

func TestWalkConfigNormalize(t *testing.T) {
	const plenty = 1 << 20

	tests := []struct {
		name       string
		cfg        WalkConfig
		work       uint64
		wantWorker int
		wantQueue  int
	}{
		{
			// The zero value is the safe default, not the fast one: a pool
			// loses against a file handle, which is the common case.
			name:       "zero value is sequential",
			cfg:        WalkConfig{},
			work:       plenty,
			wantWorker: WorkersSequential,
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			name:       "auto picks from the machine",
			cfg:        WalkConfig{Workers: WorkersAuto},
			work:       plenty,
			wantWorker: min(runtime.NumCPU(), MaxWalkWorkers),
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			name:       "explicit sequential is honoured",
			cfg:        WalkConfig{Workers: WorkersSequential},
			work:       plenty,
			wantWorker: WorkersSequential,
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			name:       "oversized pool is clamped",
			cfg:        WalkConfig{Workers: 1000},
			work:       plenty,
			wantWorker: MaxWalkWorkers,
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			name:       "any negative value is treated as auto",
			cfg:        WalkConfig{Workers: -4},
			work:       plenty,
			wantWorker: min(runtime.NumCPU(), MaxWalkWorkers),
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			// Starting a pool costs more than it can win back here.
			name:       "too little work falls back to sequential",
			cfg:        WalkConfig{Workers: 8},
			work:       minWorkForParallelWalk - 1,
			wantWorker: WorkersSequential,
			wantQueue:  DefaultWalkQueueDepth,
		},
		{
			name:       "queue depth is configurable",
			cfg:        WalkConfig{Workers: 4, QueueDepth: 32},
			work:       plenty,
			wantWorker: 4,
			wantQueue:  32,
		},
		{
			name:       "negative queue depth falls back to the default",
			cfg:        WalkConfig{Workers: 4, QueueDepth: -1},
			work:       plenty,
			wantWorker: 4,
			wantQueue:  DefaultWalkQueueDepth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.normalize(tt.work)

			if got.Workers != tt.wantWorker {
				t.Errorf("Workers = %d, want %d", got.Workers, tt.wantWorker)
			}
			if got.QueueDepth != tt.wantQueue {
				t.Errorf("QueueDepth = %d, want %d", got.QueueDepth, tt.wantQueue)
			}
			if got.parallel() != (tt.wantWorker > WorkersSequential) {
				t.Errorf("parallel() = %v for %d workers", got.parallel(), got.Workers)
			}
		})
	}
}

// TestWalkMFTMatchesSequential is the guarantee the whole design exists to
// provide. Whatever the worker count, the walk must deliver the same entries in
// the same order as the single-threaded one.
func TestWalkMFTMatchesSequential(t *testing.T) {
	vol := newWalkTestVolume(t, 300)

	collect := func(cfg WalkConfig) []uint64 {
		t.Helper()

		var seen []uint64
		if err := vol.WalkMFT(context.Background(), cfg, func(entryNum uint64, _ *MFTEntry) error {
			seen = append(seen, entryNum)
			return nil
		}); err != nil {
			t.Fatalf("WalkMFT(%+v) failed: %v", cfg, err)
		}

		return seen
	}

	want := collect(WalkConfig{Workers: WorkersSequential})
	if len(want) == 0 {
		t.Fatal("expected the sequential walk to deliver entries")
	}

	// Ascending order is the contract, so check it directly rather than only
	// against the other runs.
	for i := 1; i < len(want); i++ {
		if want[i] <= want[i-1] {
			t.Fatalf("sequential walk delivered %d after %d", want[i], want[i-1])
		}
	}

	for _, workers := range []int{2, 3, 4, 8, MaxWalkWorkers} {
		t.Run(workerName(workers), func(t *testing.T) {
			got := collect(WalkConfig{Workers: workers})

			if len(got) != len(want) {
				t.Fatalf("%d workers delivered %d entries, sequential delivered %d",
					workers, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%d workers: entry %d at position %d, sequential had %d",
						workers, got[i], i, want[i])
				}
			}
		})
	}
}

// TestWalkMFTCallbackIsSerialised checks that fn never runs on two goroutines at
// once, which is what lets a caller write one without locking.
func TestWalkMFTCallbackIsSerialised(t *testing.T) {
	vol := newWalkTestVolume(t, 400)

	var (
		inCallback atomic.Int32
		overlaps   atomic.Int32
		calls      atomic.Int32
	)

	err := vol.WalkMFT(context.Background(), WalkConfig{Workers: 8},
		func(uint64, *MFTEntry) error {
			if inCallback.Add(1) != 1 {
				overlaps.Add(1)
			}
			calls.Add(1)

			// Widen the window an overlapping call would land in.
			runtime.Gosched()

			inCallback.Add(-1)
			return nil
		})
	if err != nil {
		t.Fatalf("WalkMFT failed: %v", err)
	}

	if calls.Load() == 0 {
		t.Fatal("expected the callback to run")
	}
	if got := overlaps.Load(); got != 0 {
		t.Errorf("the callback ran concurrently %d times; it must be serialised", got)
	}
}

// TestWalkMFTStopsOnCallbackError covers a caller that has found what it wants.
// The walk must stop and return that error rather than running to completion.
func TestWalkMFTStopsOnCallbackError(t *testing.T) {
	vol := newWalkTestVolume(t, 400)

	stop := errors.New("stop")

	for _, workers := range []int{WorkersSequential, 4} {
		t.Run(workerName(workers), func(t *testing.T) {
			var calls atomic.Int32

			err := vol.WalkMFT(context.Background(), WalkConfig{Workers: workers},
				func(uint64, *MFTEntry) error {
					if calls.Add(1) >= 5 {
						return stop
					}
					return nil
				})

			if !errors.Is(err, stop) {
				t.Fatalf("expected the callback's error, got %v", err)
			}
			if got := calls.Load(); got > 64 {
				t.Errorf("the walk ran on for %d calls after the callback stopped it", got)
			}
		})
	}
}

// TestWalkMFTHonoursCancellation covers the context path, including a context
// already cancelled before the walk starts.
func TestWalkMFTHonoursCancellation(t *testing.T) {
	vol := newWalkTestVolume(t, 400)

	for _, workers := range []int{WorkersSequential, 4} {
		t.Run(workerName(workers), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())

			var calls atomic.Int32
			err := vol.WalkMFT(ctx, WalkConfig{Workers: workers},
				func(uint64, *MFTEntry) error {
					if calls.Add(1) == 3 {
						cancel()
					}
					return nil
				})
			cancel()

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got %v", err)
			}
		})
	}

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var calls atomic.Int32
		err := vol.WalkMFT(ctx, WalkConfig{Workers: 4}, func(uint64, *MFTEntry) error {
			calls.Add(1)
			return nil
		})

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if got := calls.Load(); got > 0 {
			t.Errorf("expected no callbacks on an already-cancelled context, got %d", got)
		}
	})
}

// TestWalkMFTLeavesNoGoroutines checks the pool is fully joined. A walk that
// stops early is the case most likely to strand a worker blocked on a send.
func TestWalkMFTLeavesNoGoroutines(t *testing.T) {
	vol := newWalkTestVolume(t, 500)

	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			runtime.GC()
		}
		return runtime.NumGoroutine()
	}

	before := settle()

	stop := errors.New("stop")
	for i := 0; i < 20; i++ {
		_ = vol.WalkMFT(context.Background(), WalkConfig{Workers: 8},
			func(uint64, *MFTEntry) error { return stop })

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = vol.WalkMFT(ctx, WalkConfig{Workers: 8},
			func(uint64, *MFTEntry) error { return nil })

		_ = vol.WalkMFT(context.Background(), WalkConfig{Workers: 8},
			func(uint64, *MFTEntry) error { return nil })
	}

	after := settle()

	// A small drift is normal; a leak scales with the number of walks.
	if after > before+8 {
		t.Errorf("goroutines grew from %d to %d across 60 walks, which suggests "+
			"workers are not being joined", before, after)
	}
}

// TestWalkMFTNilContext covers a caller passing nil rather than
// context.Background(), which should behave as an uncancelled walk rather than
// panicking.
func TestWalkMFTNilContext(t *testing.T) {
	vol := newWalkTestVolume(t, 200)

	//nolint:staticcheck // Passing nil is exactly what this test covers.
	err := vol.WalkMFT(nil, WalkConfig{Workers: 4}, func(uint64, *MFTEntry) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected a nil context to be treated as Background, got %v", err)
	}
}

// TestEachMFTEntryContextMatchesEachMFTEntry checks the cancellable wrapper is
// otherwise identical to the original.
func TestEachMFTEntryContextMatchesEachMFTEntry(t *testing.T) {
	vol := newWalkTestVolume(t, 200)

	var original []uint64
	if err := vol.EachMFTEntry(func(entryNum uint64, _ *MFTEntry) error {
		original = append(original, entryNum)
		return nil
	}); err != nil {
		t.Fatalf("EachMFTEntry failed: %v", err)
	}

	var withContext []uint64
	if err := vol.EachMFTEntryContext(context.Background(),
		func(entryNum uint64, _ *MFTEntry) error {
			withContext = append(withContext, entryNum)
			return nil
		}); err != nil {
		t.Fatalf("EachMFTEntryContext failed: %v", err)
	}

	if len(original) != len(withContext) {
		t.Fatalf("EachMFTEntry delivered %d entries, EachMFTEntryContext delivered %d",
			len(original), len(withContext))
	}
	for i := range original {
		if original[i] != withContext[i] {
			t.Fatalf("entry %d at position %d, want %d", withContext[i], i, original[i])
		}
	}
}

// TestWalkMFTClosedVolume covers the one error a walk treats as fatal rather
// than as a record to skip.
func TestWalkMFTClosedVolume(t *testing.T) {
	vol := newWalkTestVolume(t, 200)
	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	for _, workers := range []int{WorkersSequential, 4} {
		t.Run(workerName(workers), func(t *testing.T) {
			err := vol.WalkMFT(context.Background(), WalkConfig{Workers: workers},
				func(uint64, *MFTEntry) error { return nil })

			if !errors.Is(err, ErrVolumeClosed) {
				t.Errorf("expected ErrVolumeClosed, got %v", err)
			}
		})
	}
}

// TestWalkMFTConcurrentWalks runs several walks over one volume at once, which
// is what a caller with independent workers of its own would do.
func TestWalkMFTConcurrentWalks(t *testing.T) {
	vol := newWalkTestVolume(t, 300)

	var reference []uint64
	if err := vol.WalkMFT(context.Background(), WalkConfig{Workers: WorkersSequential},
		func(entryNum uint64, _ *MFTEntry) error {
			reference = append(reference, entryNum)
			return nil
		}); err != nil {
		t.Fatalf("reference walk failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var seen []uint64
			if err := vol.WalkMFT(context.Background(), WalkConfig{Workers: 4},
				func(entryNum uint64, _ *MFTEntry) error {
					seen = append(seen, entryNum)
					return nil
				}); err != nil {
				t.Errorf("concurrent walk failed: %v", err)
				return
			}

			if len(seen) != len(reference) {
				t.Errorf("concurrent walk delivered %d entries, reference had %d",
					len(seen), len(reference))
				return
			}
			for j := range reference {
				if seen[j] != reference[j] {
					t.Errorf("concurrent walk diverged at position %d", j)
					return
				}
			}
		}()
	}

	wg.Wait()
}

// newWalkTestVolume builds a volume with enough records that a parallel walk is
// actually taken rather than falling back to sequential.
func newWalkTestVolume(t testing.TB, records int) *Volume {
	t.Helper()

	const (
		clusterSize = 4096
		recordSize  = DefaultMFTRecordSize
		mftCluster  = 2
	)

	recordsPerCluster := clusterSize / recordSize
	clusters := (records + recordsPerCluster - 1) / recordsPerCluster

	disk := make([]byte, (mftCluster+clusters+1)*clusterSize)

	for i := 0; i < records; i++ {
		record := buildTestMFTRecord(t, testRecordOpts{
			flags:     MFTFlagInUse,
			name:      "entry.bin",
			namespace: NamespaceWin32,
			extraAttrs: [][]byte{
				buildResidentAttr(AttrTypeData, 2, []byte("content")),
			},
		})
		copy(disk[mftCluster*clusterSize+i*recordSize:], record)
	}

	return &Volume{
		reader:          &mockReaderAt{data: disk},
		bytesPerCluster: clusterSize,
		mftRecordSize:   recordSize,
		mftDataRuns:     []DataRun{{StartCluster: mftCluster, LengthClusters: uint64(clusters)}},
		mftDataRunEnds:  []uint64{uint64(clusters) * clusterSize},
		mftCache:        make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
	}
}

func workerName(workers int) string {
	if workers == WorkersSequential {
		return "sequential"
	}
	return "workers=" + itoa(workers)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}

// TestCorpusWalkMFTIsDeterministic is the determinism check against a real
// volume, where records vary in size and parse cost so workers genuinely finish
// out of order.
func TestCorpusWalkMFTIsDeterministic(t *testing.T) {
	vol := openCorpus(t)

	type observed struct {
		entryNum uint64
		inUse    bool
		isDir    bool
		attrs    int
	}

	collect := func(cfg WalkConfig) []observed {
		t.Helper()

		var seen []observed
		if err := vol.WalkMFT(context.Background(), cfg,
			func(entryNum uint64, entry *MFTEntry) error {
				seen = append(seen, observed{
					entryNum: entryNum,
					inUse:    entry.IsInUse(),
					isDir:    entry.IsDirectory(),
					attrs:    len(entry.Attributes),
				})
				return nil
			}); err != nil {
			t.Fatalf("WalkMFT(%+v) failed: %v", cfg, err)
		}

		return seen
	}

	want := collect(WalkConfig{Workers: WorkersSequential})
	if len(want) == 0 {
		t.Skip("no parsable records on this volume")
	}

	for _, workers := range []int{2, 4, 8, MaxWalkWorkers} {
		got := collect(WalkConfig{Workers: workers})

		if len(got) != len(want) {
			t.Fatalf("%d workers delivered %d records, sequential delivered %d",
				workers, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%d workers: position %d is %+v, sequential had %+v",
					workers, i, got[i], want[i])
			}
		}
	}

	t.Logf("%d records identical across sequential and 2, 4, 8, %d workers",
		len(want), MaxWalkWorkers)
}

// BenchmarkWalkMFT measures whether the pool earns its complexity. Run against a
// real volume with LIBNTFS_TEST_IMAGE for a meaningful answer; the synthetic
// volume is small enough that setup dominates.
func BenchmarkWalkMFT(b *testing.B) {
	vol := openCorpusForBenchmark(b)

	for _, workers := range []int{WorkersSequential, 2, 4, 8} {
		b.Run(workerName(workers), func(b *testing.B) {
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				// Each iteration starts cold, so the benchmark measures parsing
				// rather than the MFT cache.
				vol.mftCacheMu.Lock()
				vol.mftCache = make(map[uint64]*MFTEntry, DefaultMFTCacheSize)
				vol.mftCacheMu.Unlock()

				var records int
				err := vol.WalkMFT(context.Background(), WalkConfig{Workers: workers},
					func(uint64, *MFTEntry) error {
						records++
						return nil
					})
				if err != nil {
					b.Fatalf("WalkMFT failed: %v", err)
				}
			}
		})
	}
}

// openCorpusForBenchmark mirrors openCorpus for a *testing.B.
func openCorpusForBenchmark(b *testing.B) *Volume {
	b.Helper()

	path := os.Getenv(corpusEnv)
	if path == "" {
		b.Skipf("set %s to a raw NTFS volume image to benchmark against real data", corpusEnv)
	}

	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("opening %s failed: %v", path, err)
	}
	b.Cleanup(func() { f.Close() })

	vol, err := Open(f)
	if err != nil {
		b.Fatalf("opening %s as an NTFS volume failed: %v", path, err)
	}
	b.Cleanup(func() { vol.Close() })

	return vol
}

// BenchmarkWalkMFTInMemory is the control for BenchmarkWalkMFT.
//
// A walk against a file handle is dominated by the read syscall — on Windows,
// concurrent ReadAt against one handle serialises in the kernel, so workers add
// contention rather than throughput. Serving the same volume from memory removes
// that, leaving parsing as the cost, and shows what the pool is worth when the
// underlying ReadAt genuinely runs in parallel.
func BenchmarkWalkMFTInMemory(b *testing.B) {
	path := os.Getenv(corpusEnv)
	if path == "" {
		b.Skipf("set %s to a raw NTFS volume image", corpusEnv)
	}

	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("opening %s failed: %v", path, err)
	}
	defer f.Close()

	// The whole image has to be resident: the $MFT sits wherever the volume put
	// it, which on a real disk is nowhere near the front.
	const maxResident = 4 << 30
	info, err := f.Stat()
	if err != nil {
		b.Fatalf("stat failed: %v", err)
	}
	if info.Size() > maxResident {
		b.Skipf("%s is %d bytes, too large to hold in memory for this control",
			path, info.Size())
	}

	data := make([]byte, info.Size())
	if _, err := f.ReadAt(data, 0); err != nil {
		b.Fatalf("reading the image into memory failed: %v", err)
	}

	vol, err := Open(&mockReaderAt{data: data})
	if err != nil {
		b.Fatalf("opening the in-memory volume failed: %v", err)
	}
	defer vol.Close()

	for _, workers := range []int{WorkersSequential, 2, 4, 8} {
		b.Run(workerName(workers), func(b *testing.B) {
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				vol.mftCacheMu.Lock()
				vol.mftCache = make(map[uint64]*MFTEntry, DefaultMFTCacheSize)
				vol.mftCacheFull.Store(false)
				vol.mftCacheMu.Unlock()

				err := vol.WalkMFT(context.Background(), WalkConfig{Workers: workers},
					func(uint64, *MFTEntry) error { return nil })
				if err != nil {
					b.Fatalf("WalkMFT failed: %v", err)
				}
			}
		})
	}
}
