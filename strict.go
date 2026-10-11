package libntfs

import (
	"context"
	"fmt"
	"maps"
	"math/bits"
	"slices"
)

// IsStrict reports whether the volume was opened with Options.Strict.
func (v *Volume) IsStrict() bool {
	return v.strict
}

// verifyDirectory runs the strict checks over a directory's $I30: the B-tree
// walk, then each live entry against the record it names.
func (f *File) verifyDirectory() error {
	type located struct {
		entry IndexEntry
		vcn   int64
	}
	var live []located
	err := f.walkIndex("$I30", true, func(e IndexEntry, _ int, vcn int64) error {
		live = append(live, located{e, vcn})
		return nil
	})
	if err != nil {
		return err
	}
	for _, l := range live {
		if err := f.verifyDirEntry(l.entry, l.vcn); err != nil {
			return err
		}
	}
	return nil
}

// verifyDirEntry checks one live $I30 entry against the record it names.
func (f *File) verifyDirEntry(e IndexEntry, vcn int64) error {
	fail := func(check string, format string, args ...any) error {
		return &StrictError{
			Record: f.entryNum, Index: "$I30", VCN: vcn, Entry: e.FileReference, Check: check,
			Err: fmt.Errorf("%w: "+format, append([]any{ErrCorruptIndex}, args...)...),
		}
	}

	key := e.FileName
	if key.ParentDirectory != f.entryNum || key.ParentSeqNum != f.entry.SequenceNum {
		return fail("parent", "entry names parent %d/%d, not this directory (%d/%d)",
			key.ParentDirectory, key.ParentSeqNum, f.entryNum, f.entry.SequenceNum)
	}

	target, err := f.volume.GetMFTEntry(e.FileReference)
	if err != nil {
		return fail("record", "%v", err)
	}
	if !target.IsInUse() {
		return fail("record-not-in-use", "record %d is not in use", e.FileReference)
	}
	if target.SequenceNum != e.SequenceNum {
		return fail("sequence", "reference sequence %d, record sequence %d", e.SequenceNum, target.SequenceNum)
	}

	names, _ := target.FileNames()
	if !slices.ContainsFunc(names, func(fn *FileName) bool {
		return fn.ParentDirectory == f.entryNum && fn.ParentSeqNum == f.entry.SequenceNum &&
			fn.Name == key.Name && fn.Namespace == key.Namespace
	}) {
		return fail("file-name", "record %d has no $FILE_NAME %q (namespace %d) in this directory",
			e.FileReference, key.Name, key.Namespace)
	}

	isDir := target.Flags&MFTFlagDirectory != 0
	hasI30 := target.FindAttribute(AttrTypeIndexRoot, "$I30") != nil
	keyDir := key.FileAttributes&FileAttrDupFileNameIndexPresent != 0
	if isDir != hasI30 || isDir != keyDir {
		return fail("directory-flag", "record directory flag %v, $I30 present %v, entry directory attribute %v",
			isDir, hasI30, keyDir)
	}

	if err := f.volume.verifyDataCoverage(e.FileReference, target); err != nil {
		return err
	}
	return nil
}

// verifyDataCoverage checks that the runs of every non-resident $DATA of entry
// cover exactly its allocated size, and that its real size fits inside that.
func (v *Volume) verifyDataCoverage(entryNum uint64, entry *MFTEntry) error {
	for _, attr := range entry.FindAllAttributes(AttrTypeData) {
		nr := attr.NonResident
		if nr == nil {
			continue
		}
		fail := func(format string, args ...any) error {
			return &StrictError{
				Record: entryNum, VCN: -1, Check: "data-coverage",
				Err: wrapAttributeError(AttrTypeData, attributeName(attr), "verify",
					fmt.Errorf("%w: "+format, append([]any{ErrInvalidDataRun}, args...)...)),
			}
		}

		var clusters uint64
		for _, run := range nr.DataRuns {
			sum, carry := bits.Add64(clusters, run.LengthClusters, 0)
			if carry != 0 {
				return fail("run lengths overflow")
			}
			clusters = sum
		}
		hi, covered := bits.Mul64(clusters, uint64(v.bytesPerCluster))
		if hi != 0 || covered != nr.AllocatedSize {
			return fail("runs cover %d clusters (%d bytes), allocated size is %d",
				clusters, covered, nr.AllocatedSize)
		}
		if nr.RealSize > nr.AllocatedSize {
			return fail("real size %d exceeds allocated size %d", nr.RealSize, nr.AllocatedSize)
		}
	}
	return nil
}

// reparseKeySize is the size of a $Reparse:$R key: the reparse tag, then the
// packed file reference of the record carrying the reparse point.
const reparseKeySize = 12

// VerifyReparseIndex checks $Extend\$Reparse:$R against the volume's reparse
// points, applying the strict checks whatever Options.Strict says.
//
// The $R B-tree has to pass the WalkIndex checks, with every key twelve bytes
// (tag, then file reference) in ULONGS order; and the keys have to name
// exactly the in-use records that carry a $REPARSE_POINT, each with the tag
// that record's reparse point holds. It walks the whole MFT to establish the
// second, so it costs a full volume read.
//
// The first discrepancy is returned as a [*StrictError] naming the record and
// check; nil means the index is exact.
func (v *Volume) VerifyReparseIndex() error {
	if v.IsClosed() {
		return ErrVolumeClosed
	}

	reparse, err := v.openExtendChild("$Reparse")
	if err != nil {
		return &StrictError{Record: MFTEntryExtend, Index: "$I30", VCN: -1, Check: "reparse-index-missing", Err: err}
	}
	fail := func(check string, entry uint64, format string, args ...any) error {
		return &StrictError{
			Record: reparse.entryNum, Index: "$R", VCN: -1, Entry: entry, Check: check,
			Err: fmt.Errorf("%w: "+format, append([]any{ErrCorruptIndex}, args...)...),
		}
	}

	root, err := reparse.ReadIndexRoot("$R")
	if err != nil {
		return &StrictError{Record: reparse.entryNum, Index: "$R", VCN: -1, Check: "root", Err: err}
	}
	if root.CollationRule != CollationULongs {
		return fail("collation-rule", 0, "$R collates by rule 0x%X, not ULONGS", root.CollationRule)
	}

	indexed := make(map[uint64]uint32)
	err = reparse.walkIndex("$R", true, func(e IndexEntry, _ int, vcn int64) error {
		if len(e.Key) != reparseKeySize {
			return &StrictError{
				Record: reparse.entryNum, Index: "$R", VCN: vcn, Check: "reparse-key",
				Err: fmt.Errorf("%w: key is %d bytes, want %d", ErrCorruptIndex, len(e.Key), reparseKeySize),
			}
		}
		ref := ReadUint64LE(e.Key, 4)
		if _, dup := indexed[ref]; dup {
			return fail("reparse-duplicate", FileReferenceEntry(ref), "record listed twice")
		}
		indexed[ref] = ReadUint32LE(e.Key, 0)
		return nil
	})
	if err != nil {
		return err
	}

	onDisk := make(map[uint64]uint32)
	err = v.WalkMFT(context.Background(), WalkConfig{}, func(entryNum uint64, e *MFTEntry) error {
		if !e.IsInUse() || e.BaseRecordRef != 0 {
			return nil
		}
		attr := e.FindAttribute(AttrTypeReparsePoint, "")
		if attr == nil {
			return nil
		}
		value, err := v.readAttributeValue(attr, ReparseHeaderSize)
		if err != nil || len(value) < 4 {
			return &StrictError{Record: entryNum, VCN: -1, Check: "reparse-point",
				Err: fmt.Errorf("%w: unreadable $REPARSE_POINT: %v", ErrInvalidReparsePoint, err)}
		}
		onDisk[MakeFileReference(entryNum, e.SequenceNum)] = ReadUint32LE(value, 0)
		return nil
	})
	if err != nil {
		return err
	}

	for _, ref := range slices.Sorted(maps.Keys(onDisk)) {
		tag, ok := indexed[ref]
		switch {
		case !ok:
			return fail("reparse-missing", FileReferenceEntry(ref),
				"record %d carries reparse tag 0x%08X but $R does not list it", FileReferenceEntry(ref), onDisk[ref])
		case tag != onDisk[ref]:
			return fail("reparse-tag", FileReferenceEntry(ref),
				"$R lists tag 0x%08X, the record carries 0x%08X", tag, onDisk[ref])
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(indexed)) {
		if _, ok := onDisk[ref]; !ok {
			return fail("reparse-extra", FileReferenceEntry(ref),
				"$R lists reference %#x, which carries no reparse point", ref)
		}
	}
	return nil
}

// openExtendChild opens the $Extend entry called name.
func (v *Volume) openExtendChild(name string) (*File, error) {
	extend, err := v.Open(MFTEntryExtend)
	if err != nil {
		return nil, err
	}
	entries, err := extend.ReadDir()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Deleted && e.Name == name {
			return v.Open(e.EntryNum)
		}
	}
	return nil, fmt.Errorf("%w: $Extend\\%s", ErrFileNotFound, name)
}
