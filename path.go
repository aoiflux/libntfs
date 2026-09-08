package libntfs

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// NTFS stores the tree upwards: a file record names its parent, and nothing
// names its children except the directory index, which a deleted file has
// already fallen out of. Reconstructing a path therefore means walking parent
// references from the file towards the root, and it is the direction that
// matters forensically — [Volume.OpenPath] answers "what is at this path", but a
// report of what was found on a volume needs "where was this record".
//
// Two things make that walk fallible. A parent reference can point at a record
// that no longer parses, or that was reused for something else, which is routine
// for deleted files. And a corrupt tree can contain a cycle. Both end the walk
// at [OrphanDir] rather than failing it, because a file recorded under a
// synthetic parent is still evidence and a file dropped from the report is not.

// OrphanDir is the synthetic directory under which entries whose parent chain
// cannot be followed to the root are reported.
//
// It is not a real directory and no such name exists on the volume. A path below
// it is OrphanDir plus the entry's MFT record number, which keeps every reported
// path unique and traceable back to the record it came from.
const OrphanDir = "/$ORPHAN"

// maxPathDepth bounds a parent walk. NTFS itself permits deeper nesting than
// this in principle, but a chain longer than it on a real volume means a damaged
// tree rather than a deep one, and the bound is what stops a walk over corrupt
// records from running until the cycle check happens to catch it.
const maxPathDepth = 64

// orphanPath is the path reported for an entry whose parents could not be
// followed.
func orphanPath(entryNum uint64) string {
	return OrphanDir + "/" + strconv.FormatUint(entryNum, 10)
}

// PathFor reconstructs the absolute path of an MFT entry by following
// $FILE_NAME parent references to the root.
//
// This is the inverse of [Volume.OpenPath]: given a record, it answers where
// that record sat in the tree. It works for deleted entries and for entries no
// directory index still lists, which is the case that matters — a deleted file's
// record still names its parent long after the parent has stopped naming it.
//
// The root directory is "/". An entry whose parent chain cannot be followed to
// the root — a missing or unparsable ancestor, a nameless one, a cycle, or a
// chain longer than 64 links — is reported under [OrphanDir] with a nil error,
// since a partial answer is more useful than none. An error is returned only
// when the entry itself cannot be read or carries no name, and when the volume
// is closed.
//
// The reconstruction follows record numbers, not full file references: an
// ancestor whose record has since been reused by another directory yields a path
// that reads plausibly but describes where the record now sits rather than where
// this file was. Callers that need to detect that should compare the parent
// sequence numbers themselves — [Volume.WalkFiles] hands them to its callback,
// and [FileName.ParentSeqNum] carries one for a single entry.
//
// Resolving many paths this way re-reads ancestors once per entry. Use
// [Volume.WalkFiles] to path a whole volume; it builds the parent map once.
func (v *Volume) PathFor(entryNum uint64) (string, error) {
	if v.IsClosed() {
		return "", ErrVolumeClosed
	}

	if entryNum == MFTEntryRoot {
		return "/", nil
	}

	parts := make([]string, 0, 8)
	seen := make(map[uint64]struct{}, maxPathDepth)
	cur := entryNum

	for depth := 0; cur != MFTEntryRoot; depth++ {
		if depth >= maxPathDepth {
			return orphanPath(entryNum), nil
		}

		if _, cycle := seen[cur]; cycle {
			return orphanPath(entryNum), nil
		}
		seen[cur] = struct{}{}

		name, parent, err := v.parentLink(cur)
		if err != nil {
			if errors.Is(err, ErrVolumeClosed) {
				return "", err
			}
			// The entry the caller asked about is their problem to hear about;
			// a broken ancestor is what OrphanDir exists for.
			if cur == entryNum {
				return "", err
			}
			return orphanPath(entryNum), nil
		}

		parts = append(parts, name)
		cur = parent
	}

	return joinPathParts(parts), nil
}

// parentLink returns an entry's name and the record number of its parent.
func (v *Volume) parentLink(entryNum uint64) (string, uint64, error) {
	entry, err := v.GetMFTEntry(entryNum)
	if err != nil {
		return "", 0, err
	}

	fn, err := entry.GetFileName()
	if err != nil {
		return "", 0, err
	}

	if fn == nil || fn.Name == "" {
		return "", 0, wrapMFTError(entryNum, "path", ErrAttributeNotFound)
	}

	return fn.Name, fileReferenceRecordNumber(fn.ParentDirectory), nil
}

// joinPathParts turns a child-to-root list of names into an absolute path.
func joinPathParts(parts []string) string {
	for lo, hi := 0, len(parts)-1; lo < hi; lo, hi = lo+1, hi-1 {
		parts[lo], parts[hi] = parts[hi], parts[lo]
	}

	return "/" + strings.Join(parts, "/")
}

// pathMeta is what path reconstruction needs from one MFT entry.
type pathMeta struct {
	name      string
	parent    uint64 // Parent MFT record number
	parentSeq uint16 // Parent sequence number, for callers checking record reuse
	isDir     bool
}

// pathIndex is the whole volume's parent map, built once so a walk can path
// every entry without re-reading ancestors.
type pathIndex struct {
	meta map[uint64]pathMeta
}

// buildPathIndex reads every parsable record's name and parent reference.
//
// It is a full pass over the MFT. Nothing cheaper exists: a path cannot be
// resolved until its ancestors' names are known, and an ancestor may sit at any
// record number, so the map has to be complete before the first path is built.
func (v *Volume) buildPathIndex(ctx context.Context, cfg WalkConfig) (*pathIndex, error) {
	idx := &pathIndex{meta: make(map[uint64]pathMeta)}

	// WalkMFT delivers from one goroutine in entry order, so the map needs no
	// synchronisation of its own even when the parse pool is wide.
	err := v.WalkMFT(ctx, cfg, func(entryNum uint64, e *MFTEntry) error {
		fn, err := e.GetFileName()
		if err != nil || fn == nil || fn.Name == "" {
			return nil
		}

		idx.meta[entryNum] = pathMeta{
			name:      fn.Name,
			parent:    fileReferenceRecordNumber(fn.ParentDirectory),
			parentSeq: fn.ParentSeqNum,
			isDir:     e.IsDirectory(),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return idx, nil
}

// pathFor resolves one entry against the prebuilt map, with the same orphan and
// cycle rules as [Volume.PathFor].
func (idx *pathIndex) pathFor(entryNum uint64) string {
	if entryNum == MFTEntryRoot {
		return "/"
	}

	parts := make([]string, 0, 8)
	seen := make(map[uint64]struct{}, maxPathDepth)
	cur := entryNum

	for depth := 0; cur != MFTEntryRoot; depth++ {
		if depth >= maxPathDepth {
			return orphanPath(entryNum)
		}

		if _, cycle := seen[cur]; cycle {
			return orphanPath(entryNum)
		}
		seen[cur] = struct{}{}

		meta, ok := idx.meta[cur]
		if !ok {
			return orphanPath(entryNum)
		}

		parts = append(parts, meta.name)
		cur = meta.parent
	}

	return joinPathParts(parts)
}

// WalkFiles walks every parsable MFT record and hands the callback the record
// together with the path it sits at and the parent it names.
//
// It is [Volume.WalkMFT] with identity attached. WalkMFT yields entry numbers
// and records, which is everything except the two things a report needs most:
// where the file was, and which directory record claimed it. Deriving those per
// entry costs a lookup chain each time, and forgetting the parent sequence
// number is what causes a reused MFT record to be written down as a modified
// file.
//
// The arguments are the reconstructed absolute path, the parent's MFT record
// number, the parent's sequence number, and the record itself:
//
//   - path follows the rules in [Volume.PathFor], including the [OrphanDir]
//     fallback for entries whose parents cannot be followed.
//   - parentRef and parentSeq come straight from the entry's $FILE_NAME. Compare
//     parentSeq against the parent record's [MFTEntry.SequenceNum] to detect a
//     parent whose record has been reused, which makes path a reconstruction
//     rather than a fact.
//   - Records carrying no usable $FILE_NAME — $MFT extension records, and
//     damaged ones — are still delivered, under [OrphanDir] with a zero parent,
//     so a walk over a volume reports every record that parses.
//
// This costs two passes over the MFT: one to build the parent map, one to
// deliver. cfg applies to both, ctx cancels both, and delivery ordering and
// callback guarantees are exactly [Volume.WalkMFT]'s.
//
// The entries handed to fn come from the volume's cache and are shared with
// every other caller. Treat them as read-only.
func (v *Volume) WalkFiles(ctx context.Context, cfg WalkConfig,
	fn func(path string, parentRef uint64, parentSeq uint16, e *MFTEntry) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}

	idx, err := v.buildPathIndex(ctx, cfg)
	if err != nil {
		return err
	}

	return v.WalkMFT(ctx, cfg, func(entryNum uint64, e *MFTEntry) error {
		meta, ok := idx.meta[entryNum]
		if !ok {
			return fn(orphanPath(entryNum), 0, 0, e)
		}

		return fn(idx.pathFor(entryNum), meta.parent, meta.parentSeq, e)
	})
}
