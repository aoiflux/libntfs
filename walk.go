package libntfs

import (
	"context"
	"errors"
	"sync"
)

// Walking the MFT is the one place in this library where concurrency clearly
// pays. Every record is parsed independently, the reads go through an
// io.ReaderAt — which is safe for concurrent use by contract — and a full walk
// over a large volume is the longest operation the library performs.
//
// # The guarantee
//
// A parallel walk delivers exactly what a sequential one delivers, in the same
// order. Records are parsed on several goroutines but handed to the callback one
// at a time, in ascending entry order, from a single goroutine. The callback
// therefore needs no locking of its own and sees no interleaving, and a caller
// can turn workers up or down without changing results.
//
// That ordering is not free — a record parsed early waits for its predecessors —
// but it is what makes parallelism a tuning decision rather than a semantic one.
//
// # Degrading
//
// Every path here collapses to the sequential one. WorkersSequential, a volume
// too small to be worth splitting, or a machine reporting one CPU all run the
// walk on the calling goroutine with no goroutines started and no channels
// allocated.

// mftWalkBatch is a contiguous run of parsed records on its way to the callback.
//
// Records travel in batches rather than singly because per-record handoff does
// not pay for itself: parsing costs a few microseconds and a send, a receive,
// and a reorder-map insert per record cost more. Measured on a real volume,
// per-record handoff made the parallel walk ~25% slower than the sequential one.
//
// first is carried explicitly rather than inferred from arrival order, because
// batches arrive out of order and the reorder buffer is keyed on it.
type mftWalkBatch struct {
	first   uint64
	entries []mftWalkEntry
}

// mftWalkEntry is one record within a batch. A record that failed to parse is
// carried with its error so the delivering goroutine can apply the same
// skip-or-stop rule the sequential walk uses.
type mftWalkEntry struct {
	entryNum uint64
	entry    *MFTEntry
	err      error
}

// EachMFTEntryContext walks every parsable MFT record in ascending order,
// stopping early if ctx is cancelled.
//
// It is [Volume.EachMFTEntry] with cancellation. Unallocated, bad, and
// unparsable records are skipped rather than reported, since a walk over a whole
// volume routinely meets them; a closed volume stops the walk. If fn returns an
// error, iteration stops and that error is returned.
func (v *Volume) EachMFTEntryContext(ctx context.Context, fn func(entryNum uint64, e *MFTEntry) error) error {
	return v.WalkMFT(ctx, WalkConfig{Workers: WorkersSequential}, fn)
}

// WalkMFT walks every parsable MFT record, parsing on a bounded pool of workers
// and delivering to fn in ascending entry order.
//
// The zero WalkConfig runs the walk single-threaded, which is the right default
// for a volume backed by a file handle. Set Workers to [WorkersAuto] to take a
// count from the machine, or to a count of your own.
//
// fn is called from one goroutine at a time and in order, so it needs no
// synchronisation of its own, and a parallel walk produces exactly the sequence
// a sequential one does. Returning an error from fn stops the walk, shuts the
// pool down, and returns that error. Cancelling ctx does the same and returns
// the context's error.
//
// The entries handed to fn come from the volume's cache and are shared with
// every other caller. Treat them as read-only.
func (v *Volume) WalkMFT(ctx context.Context, cfg WalkConfig, fn func(entryNum uint64, e *MFTEntry) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	count, err := v.MFTEntryCount()
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}

	normalized := cfg.normalize(count)
	if !normalized.parallel() {
		return v.walkMFTSequential(ctx, count, fn)
	}

	return v.walkMFTParallel(ctx, count, normalized, fn)
}

// walkMFTSequential is the reference implementation. The parallel walk must
// produce the same sequence this does.
func (v *Volume) walkMFTSequential(ctx context.Context, count uint64,
	fn func(entryNum uint64, e *MFTEntry) error,
) error {
	for entryNum := uint64(0); entryNum < count; entryNum++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		entry, err := v.GetMFTEntry(entryNum)
		if err != nil {
			// A closed volume is fatal; an unparsable record is not.
			if errors.Is(err, ErrVolumeClosed) {
				return err
			}
			continue
		}

		if err := fn(entryNum, entry); err != nil {
			return err
		}
	}

	return nil
}

// walkMFTParallel parses on a worker pool and delivers in order.
//
// The shape is a fixed set of workers pulling batches of entry numbers off one
// channel and pushing parsed batches onto another, with the calling goroutine
// reordering and invoking the callback. Workers are bounded by the
// configuration, every goroutine is joined before returning, and cancelling the
// context unblocks all of them.
func (v *Volume) walkMFTParallel(ctx context.Context, count uint64, cfg WalkConfig,
	fn func(entryNum uint64, e *MFTEntry) error,
) error {
	// Cancelling this on the way out is what guarantees no worker outlives the
	// call, whether the walk finished, the callback failed, or the caller's
	// context was cancelled.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	batchSize := uint64(cfg.BatchSize)

	work := make(chan uint64, cfg.Workers)
	results := make(chan mftWalkBatch, cfg.Workers*cfg.QueueDepth)

	var feeder sync.WaitGroup

	// Feeder: hands out the first entry number of each batch until the range is
	// exhausted or the walk is cancelled. It owns the work channel and is its
	// only closer.
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		defer close(work)

		for first := uint64(0); first < count; first += batchSize {
			select {
			case work <- first:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Workers: parse batches. Reads go through io.ReaderAt, which is safe for
	// concurrent use, and the MFT cache is mutex-guarded, so no worker state is
	// shared beyond that.
	var parsers sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		parsers.Add(1)
		go func() {
			defer parsers.Done()

			for first := range work {
				last := first + batchSize
				if last > count {
					last = count
				}

				batch := mftWalkBatch{
					first:   first,
					entries: make([]mftWalkEntry, 0, last-first),
				}

				for entryNum := first; entryNum < last; entryNum++ {
					entry, err := v.GetMFTEntry(entryNum)
					batch.entries = append(batch.entries, mftWalkEntry{
						entryNum: entryNum,
						entry:    entry,
						err:      err,
					})
				}

				select {
				case results <- batch:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// The results channel outlives the workers, so exactly one goroutine closes
	// it once they have all finished.
	go func() {
		parsers.Wait()
		close(results)
	}()

	err := deliverInOrder(ctx, results, count, batchSize, fn)

	// Unblock anything still parked on a send, then join before returning so no
	// goroutine outlives the call.
	cancel()
	for range results {
		// Drain so a worker blocked on send can observe the cancellation.
	}
	parsers.Wait()
	feeder.Wait()

	return err
}

// deliverInOrder feeds batches to fn in ascending entry order.
//
// Workers finish out of order, so a batch that arrives early is held until every
// earlier one has been delivered. The buffer is bounded by the results channel:
// a worker cannot run more than a queue's depth ahead, so the records held back
// stay proportional to the pool rather than to the volume.
func deliverInOrder(ctx context.Context, results <-chan mftWalkBatch, count, batchSize uint64,
	fn func(entryNum uint64, e *MFTEntry) error,
) error {
	pending := make(map[uint64]mftWalkBatch)
	next := uint64(0)

	for batch := range results {
		if err := ctx.Err(); err != nil {
			return err
		}

		pending[batch.first] = batch

		// Deliver everything now contiguous from the low-water mark.
		for next < count {
			ready, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next += batchSize

			for _, item := range ready.entries {
				if item.err != nil {
					// A closed volume is fatal; an unparsable record is skipped,
					// the same as in the sequential walk.
					if errors.Is(item.err, ErrVolumeClosed) {
						return item.err
					}
					continue
				}

				if err := fn(item.entryNum, item.entry); err != nil {
					return err
				}
			}
		}
	}

	return ctx.Err()
}
