package libntfs

import "runtime"

// Tunables for concurrent work.
//
// These are the defaults [WalkConfig.normalize] applies; a caller who needs
// different behaviour sets the field rather than editing these.
const (
	// WorkersSequential runs the walk on the calling goroutine with no workers
	// at all. It is the zero value's meaning and what every concurrent path here
	// degrades to.
	WorkersSequential = 1

	// WorkersAuto asks for a worker count taken from the machine.
	//
	// It is deliberately not the zero value. Whether a pool helps depends
	// entirely on whether the volume's io.ReaderAt serves concurrent reads
	// concurrently, and the library cannot tell. Measured over the same volume:
	//
	//	reader                sequential   8 workers
	//	*os.File (Windows)       13.5 ms     22.9 ms
	//	in-memory                 4.5 ms      1.3 ms
	//
	// A file handle is the common case and it loses, because the read syscall
	// dominates and the kernel serialises concurrent reads against one handle.
	// A caller who knows their reader parallelises — memory, mmap, an object
	// store, a striped array — opts in and gets better than a 3x speedup.
	WorkersAuto = -1

	// MaxWalkWorkers bounds the pool however many CPUs are reported.
	//
	// The work is IO-bound against one io.ReaderAt, so past a modest count the
	// only thing more goroutines add is contention and memory: each worker holds
	// an MFT record buffer and the records it has parsed but not yet delivered.
	MaxWalkWorkers = 16

	// DefaultWalkQueueDepth is how many entries may sit parsed but undelivered
	// per worker.
	//
	// It bounds memory and provides the slack that keeps workers busy while the
	// delivering goroutine runs the callback. Too small and workers stall behind
	// a slow callback; too large and a walk over a big $MFT holds far more
	// parsed records than it needs.
	DefaultWalkQueueDepth = 4

	// DefaultWalkBatchSize is how many consecutive records a worker claims at a
	// time.
	//
	// Handing out one record per channel operation does not pay: parsing a
	// record takes a few microseconds, and a send, a receive, and a reorder-map
	// insert per record cost more than that. Measured on a real volume,
	// per-record handoff made the parallel walk ~25% slower than the sequential
	// one. Claiming a run of records amortises the handoff over the batch.
	DefaultWalkBatchSize = 64

	// minWorkForParallelWalk is the number of entries below which a walk runs
	// sequentially regardless of configuration.
	//
	// Starting a pool costs goroutines, channels, and a reorder buffer. Under
	// this many records that setup outweighs anything it can win back.
	minWorkForParallelWalk = 64
)

// WalkConfig configures a walk over the MFT.
//
// The zero value is valid and runs the walk single-threaded, which is the right
// default for the common case of a volume backed by a file handle. Set Workers
// to [WorkersAuto] or to a count to enable the pool; see WorkersAuto for when
// that is worth doing.
//
// Configuration is deliberately per-call rather than per-Volume. A Volume is
// shared between goroutines, and a knob on it would be shared mutable state of
// exactly the kind the concurrent paths here avoid.
type WalkConfig struct {
	// Workers is how many goroutines parse records concurrently.
	//
	// Zero and WorkersSequential both run the walk single-threaded. WorkersAuto,
	// or any negative value, takes a count from the machine. Values above
	// MaxWalkWorkers are clamped to it.
	Workers int

	// QueueDepth is how many parsed batches may be buffered per worker before it
	// blocks. Zero means DefaultWalkQueueDepth.
	QueueDepth int

	// BatchSize is how many consecutive records a worker claims per handoff.
	// Zero means DefaultWalkBatchSize.
	//
	// Larger batches cut coordination overhead but coarsen cancellation and hold
	// more parsed records in flight.
	BatchSize int
}

// normalize returns the configuration with defaults applied and every value
// clamped into range, so the walk itself never has to reason about a zero or a
// nonsensical field.
func (c WalkConfig) normalize(work uint64) WalkConfig {
	normalized := WalkConfig{
		Workers:    c.Workers,
		QueueDepth: c.QueueDepth,
		BatchSize:  c.BatchSize,
	}

	switch {
	case normalized.Workers == 0:
		// The zero value is the safe default rather than the fast one.
		normalized.Workers = WorkersSequential
	case normalized.Workers < 0:
		normalized.Workers = runtime.NumCPU()
	}
	if normalized.Workers > MaxWalkWorkers {
		normalized.Workers = MaxWalkWorkers
	}

	// Parallelism cannot pay for itself over a handful of records, and a pool
	// wider than the work merely starts goroutines with nothing to do.
	if work < minWorkForParallelWalk || work < uint64(normalized.Workers) {
		normalized.Workers = WorkersSequential
	}

	if normalized.QueueDepth <= 0 {
		normalized.QueueDepth = DefaultWalkQueueDepth
	}
	if normalized.BatchSize <= 0 {
		normalized.BatchSize = DefaultWalkBatchSize
	}

	return normalized
}

// parallel reports whether the configuration calls for a worker pool.
func (c WalkConfig) parallel() bool {
	return c.Workers > WorkersSequential
}
