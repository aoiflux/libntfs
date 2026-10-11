package libntfs

import (
	"bytes"
	"fmt"
	"math"
)

// maxIndexBitmapBytes bounds how much of an index $BITMAP is read: one bit per
// block of the largest $INDEX_ALLOCATION readIndexAllocation accepts, at the
// smallest block size.
const maxIndexBitmapBytes = (1 << 30) / MinIndexRecordSize / 8

// maxIndexDepth bounds how deep WalkIndex descends. A B-tree over the largest
// index NTFS can hold is a handful of levels deep; anything deeper is a cycle
// or damage.
const maxIndexDepth = 32

// IndexBlock is one INDX block of an index's $INDEX_ALLOCATION: a node of the
// index B-tree below $INDEX_ROOT.
type IndexBlock struct {
	VCN        int64           // VCN recorded in the block's own header
	LSN        uint64          // $LogFile sequence number of the block's last change
	IsNode     bool            // The block's entries point at sub-nodes
	NodeHeader IndexNodeHeader // Entry list bounds, relative to the node header at byte 24
	Entries    []IndexEntry    // In-use entries, ending with the end entry
	Raw        []byte          // The whole block, with its update sequence applied
}

// indexBlockSize returns the INDX block size for an index.
//
// $INDEX_ROOT records the size the index was created with, which is what
// Windows uses; the boot sector's value is the fallback when the root's is not
// a power of two in [MinIndexRecordSize, MaxIndexRecordSize].
func (v *Volume) indexBlockSize(root *IndexRoot) int {
	if root != nil {
		size := root.IndexBlockSize
		if size >= MinIndexRecordSize && size <= MaxIndexRecordSize && size&(size-1) == 0 {
			return int(size)
		}
	}
	return int(v.IndexRecordSize())
}

// indexVCNUnit returns the number of bytes one index VCN addresses.
//
// An index VCN counts clusters when a block is at least a cluster long, and
// 512-byte units otherwise, so that a block smaller than a cluster still has an
// address of its own. This is the rule Windows and libntfs-3g apply.
func (v *Volume) indexVCNUnit(blockSize int) int64 {
	if int64(blockSize) >= int64(v.bytesPerCluster) {
		return int64(v.bytesPerCluster)
	}
	return MinIndexRecordSize
}

// loadIndexBitmap returns the $BITMAP named name, which holds one bit per
// block of the matching $INDEX_ALLOCATION. It returns nil and no error when
// the record has no such attribute.
func (f *File) loadIndexBitmap(name string) ([]byte, error) {
	attr := f.entry.FindAttribute(AttrTypeBitmap, name)
	if attr == nil {
		return nil, nil
	}
	bits, err := f.volume.readAttributeValue(attr, maxIndexBitmapBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read $BITMAP:%s: %w", ErrCorruptIndex, name, err)
	}
	return bits, nil
}

// bitmapBit reports whether bit i of bits is set. A bit past the end of the
// bitmap is clear.
func bitmapBit(bits []byte, i int) bool {
	if i < 0 || i/8 >= len(bits) {
		return false
	}
	return bits[i/8]&(1<<(i%8)) != 0
}

// ReadIndexRoot parses the $INDEX_ROOT named name: "$I30" for a directory,
// "$R" for $Extend\$Reparse, "$SII" and "$SDH" for $Secure, and so on.
//
// Entries holds the root node's in-use entries, ending with the end entry. For
// $I30 each entry carries its FileReference and parsed FileName; for a view
// index, its Key and Data.
func (f *File) ReadIndexRoot(name string) (*IndexRoot, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}
	attr := f.entry.FindAttribute(AttrTypeIndexRoot, name)
	if attr == nil {
		return nil, fmt.Errorf("%w: record %d has no $INDEX_ROOT:%s", ErrAttributeNotFound, f.entryNum, name)
	}
	if attr.Resident == nil {
		return nil, fmt.Errorf("%w: $INDEX_ROOT:%s is not resident", ErrCorruptIndex, name)
	}
	return parseIndexRootValue(attr.Resident.Value)
}

// ReadIndexBlock reads and parses the INDX block at vcn of the index named
// name.
//
// The block is located from vcn alone, as the B-tree's sub-node pointers
// locate it; the VCN the block records in its own header is returned in the
// block's VCN field for the caller to compare. The block must carry the INDX
// magic and a valid update sequence.
func (f *File) ReadIndexBlock(name string, vcn int64) (*IndexBlock, error) {
	root, err := f.ReadIndexRoot(name)
	if err != nil {
		return nil, err
	}
	alloc, err := f.indexAllocation(name)
	if err != nil {
		return nil, err
	}
	if alloc == nil {
		return nil, fmt.Errorf("%w: record %d has no $INDEX_ALLOCATION:%s", ErrAttributeNotFound, f.entryNum, name)
	}
	return f.readIndexBlock(alloc, root.AttributeType, f.volume.indexBlockSize(root), vcn)
}

// IndexBitmap returns the $BITMAP of the index named name: bit i is set when
// block i of its $INDEX_ALLOCATION is in use, block i being the one at byte
// offset i times the index block size. It returns an error wrapping
// ErrAttributeNotFound when the index has no $BITMAP, which only an index
// without an $INDEX_ALLOCATION may lack.
func (f *File) IndexBitmap(name string) ([]byte, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}
	bits, err := f.loadIndexBitmap(name)
	if err != nil {
		return nil, err
	}
	if bits == nil {
		return nil, fmt.Errorf("%w: record %d has no $BITMAP:%s", ErrAttributeNotFound, f.entryNum, name)
	}
	return bits, nil
}

// WalkIndex visits every entry of the index named name in collation order,
// descending from $INDEX_ROOT through the INDX blocks its entries point at.
//
// fn is called for each entry other than the end entries, with the entry's
// depth below the root (0 for the root itself) and the VCN of the block holding
// it (-1 for the root). A sub-node is visited before the entry that points at
// it, and the end entry's sub-node after every other entry of its node, so keys
// come out in ascending order. Returning an error from fn stops the walk and
// returns that error.
//
// Only blocks the index's $BITMAP marks in use are visited, each at most once
// and no deeper than 32 levels. On a damaged index the walk skips what it
// cannot read and carries on, then returns an error wrapping ErrCorruptIndex
// naming how many blocks were skipped. With Options.Strict it instead stops at
// the first fault with a [*StrictError]; see [Options] for the checks.
func (f *File) WalkIndex(name string, fn func(e IndexEntry, depth int, vcn int64) error) error {
	return f.walkIndex(name, f.volume.strict, fn)
}

// indexAllocation returns the $INDEX_ALLOCATION named name, or nil when the
// index has none.
func (f *File) indexAllocation(name string) (*NonResidentAttribute, error) {
	attr := f.entry.FindAttribute(AttrTypeIndexAllocation, name)
	if attr == nil {
		return nil, nil
	}
	if attr.NonResident == nil {
		return nil, fmt.Errorf("%w: $INDEX_ALLOCATION:%s is resident", ErrCorruptIndex, name)
	}
	return attr.NonResident, nil
}

// indexBlockOffset returns the byte offset within the allocation of the block
// at vcn.
func (v *Volume) indexBlockOffset(vcn int64, blockSize int) (int64, error) {
	unit := v.indexVCNUnit(blockSize)
	if vcn < 0 || vcn > math.MaxInt64/unit {
		return 0, fmt.Errorf("%w: index VCN %d", ErrInvalidOffset, vcn)
	}
	return vcn * unit, nil
}

// readIndexBlock reads the block at vcn from alloc and parses it.
func (f *File) readIndexBlock(alloc *NonResidentAttribute, indexedType uint32, blockSize int, vcn int64) (*IndexBlock, error) {
	offset, err := f.volume.indexBlockOffset(vcn, blockSize)
	if err != nil {
		return nil, err
	}
	if offset+int64(blockSize) > int64(alloc.RealSize) || int64(alloc.RealSize) < 0 {
		return nil, fmt.Errorf("%w: VCN %d is past the end of the %d byte allocation",
			ErrCorruptIndex, vcn, alloc.RealSize)
	}
	buf := make([]byte, blockSize)
	n, err := f.volume.ReadDataRuns(alloc.DataRuns, offset, buf)
	if n < blockSize {
		if err == nil {
			err = ErrTruncatedRecord
		}
		return nil, fmt.Errorf("%w: read index block at VCN %d: %w", ErrCorruptIndex, vcn, err)
	}
	return parseIndexBlock(buf, indexedType)
}

// parseIndexBlock validates an INDX block, applies its update sequence in
// place, and parses its entries.
func parseIndexBlock(buf []byte, indexedType uint32) (*IndexBlock, error) {
	const nodeHeader = IndexRecordHeaderSize
	if len(buf) < nodeHeader+IndexNodeHeaderSize {
		return nil, fmt.Errorf("%w: index block is %d bytes", ErrTruncatedRecord, len(buf))
	}
	if ReadUint32LE(buf, 0) != IndexAllocationMagic {
		return nil, fmt.Errorf("%w: block has magic 0x%08X, not INDX", ErrCorruptIndex, ReadUint32LE(buf, 0))
	}
	if err := ApplyFixup(buf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptIndex, err)
	}

	block := &IndexBlock{
		LSN: ReadUint64LE(buf, 8),
		VCN: int64(ReadUint64LE(buf, 16)),
		NodeHeader: IndexNodeHeader{
			EntryListOffset: ReadUint32LE(buf, nodeHeader),
			EntryListEnd:    ReadUint32LE(buf, nodeHeader+4),
			EntryListAlloc:  ReadUint32LE(buf, nodeHeader+8),
			Flags:           ReadUint32LE(buf, nodeHeader+12),
		},
		Raw: buf,
	}
	block.IsNode = block.NodeHeader.Flags&IndexNodeNode != 0

	entries, err := parseIndexNode(buf[nodeHeader:], block.NodeHeader, indexedType)
	if err != nil {
		return nil, err
	}
	block.Entries = entries
	return block, nil
}

// parseIndexRootValue parses an $INDEX_ROOT value with parseIndexNode.
func parseIndexRootValue(value []byte) (*IndexRoot, error) {
	if len(value) < IndexRootHeaderSize+IndexNodeHeaderSize {
		return nil, fmt.Errorf("%w: $INDEX_ROOT is %d bytes", ErrCorruptIndex, len(value))
	}
	root := &IndexRoot{
		AttributeType:    ReadUint32LE(value, 0),
		CollationRule:    ReadUint32LE(value, 4),
		IndexBlockSize:   ReadUint32LE(value, 8),
		ClustersPerIndex: value[12],
		NodeHeader: IndexNodeHeader{
			EntryListOffset: ReadUint32LE(value, 16),
			EntryListEnd:    ReadUint32LE(value, 20),
			EntryListAlloc:  ReadUint32LE(value, 24),
			Flags:           ReadUint32LE(value, 28),
		},
	}
	copy(root.Reserved[:], value[13:16])

	entries, err := parseIndexNode(value[IndexRootHeaderSize:], root.NodeHeader, root.AttributeType)
	if err != nil {
		return nil, err
	}
	root.Entries = entries
	return root, nil
}

// parseIndexNode parses the in-use entries of an index node. node starts at
// the node header; the header's offsets are relative to it.
//
// Every entry is bounds-checked: its length is a multiple of eight and inside
// the entry list, its key and sub-node pointer fit inside it, and the list ends
// with an end entry. The entries' own flags decide whether a sub-node VCN is
// read; the walk checks they agree with the node header.
func parseIndexNode(node []byte, header IndexNodeHeader, indexedType uint32) ([]IndexEntry, error) {
	start, end := uint64(header.EntryListOffset), uint64(header.EntryListEnd)
	if start < IndexNodeHeaderSize || end < start || end > uint64(header.EntryListAlloc) || end > uint64(len(node)) {
		return nil, fmt.Errorf("%w: invalid index node bounds (offset=%d end=%d alloc=%d size=%d)",
			ErrCorruptIndex, header.EntryListOffset, header.EntryListEnd, header.EntryListAlloc, len(node))
	}
	list := node[start:end]

	var entries []IndexEntry
	for off := 0; ; {
		if off+IndexEntryHeaderSize > len(list) {
			return nil, fmt.Errorf("%w: entry list ends without an end entry", ErrCorruptIndex)
		}
		entryLen := int(ReadUint16LE(list, off+8))
		keyLen := int(ReadUint16LE(list, off+10))
		flags := list[off+12]
		if entryLen < IndexEntryHeaderSize || entryLen%8 != 0 || off+entryLen > len(list) {
			return nil, fmt.Errorf("%w: entry at %d has length %d", ErrCorruptIndex, off, entryLen)
		}
		raw := list[off : off+entryLen]

		vcnBytes := 0
		if flags&IndexFlagNode != 0 {
			vcnBytes = 8
		}
		if IndexEntryHeaderSize+keyLen+vcnBytes > entryLen {
			return nil, fmt.Errorf("%w: entry at %d: %d byte key does not fit a %d byte entry",
				ErrCorruptIndex, off, keyLen, entryLen)
		}

		e := IndexEntry{
			EntryLength:  uint16(entryLen),
			StreamLength: uint16(keyLen),
			Flags:        flags,
		}
		copy(e.Reserved[:], raw[13:16])
		if keyLen > 0 {
			e.Key = append([]byte(nil), raw[IndexEntryHeaderSize:IndexEntryHeaderSize+keyLen]...)
			e.Stream = e.Key
		}
		if vcnBytes > 0 {
			e.SubNodeVCN = ReadUint64LE(raw, entryLen-8)
		}

		last := flags&IndexFlagLast != 0
		if indexedType == AttrTypeFileName {
			e.FileReference = ReadUint64LE(raw, 0) & fileReferenceRecordMask
			e.SequenceNum = ReadUint16LE(raw, 6)
			if !last {
				fn, err := ParseFileName(e.Key)
				if err != nil {
					return nil, fmt.Errorf("%w: entry at %d: %w", ErrCorruptIndex, off, err)
				}
				e.FileName = fn
			}
		} else if dataLen := int(ReadUint16LE(raw, 2)); dataLen > 0 {
			dataOff := int(ReadUint16LE(raw, 0))
			if dataOff < IndexEntryHeaderSize || dataOff+dataLen > entryLen-vcnBytes {
				return nil, fmt.Errorf("%w: entry at %d: data at %d+%d is outside the entry",
					ErrCorruptIndex, off, dataOff, dataLen)
			}
			e.Data = append([]byte(nil), raw[dataOff:dataOff+dataLen]...)
		}

		entries = append(entries, e)
		if last {
			return entries, nil
		}
		off += entryLen
	}
}

// indexWalker carries the state of one WalkIndex call.
type indexWalker struct {
	f      *File
	name   string
	strict bool
	fn     func(e IndexEntry, depth int, vcn int64) error

	root      *IndexRoot
	alloc     *NonResidentAttribute
	bitmap    []byte
	blockSize int
	collate   func(a, b []byte) int // nil when the collation is not checked

	data      []byte // the whole $INDEX_ALLOCATION, already read; nil reads each block from disk
	visited   map[int64]bool
	prev      []byte
	havePrev  bool
	skipped   int
	delivered int
}

// walkIndex is WalkIndex with the strict setting explicit, so
// VerifyReparseIndex can walk strictly on a tolerant volume.
func (f *File) walkIndex(name string, strict bool, fn func(e IndexEntry, depth int, vcn int64) error) error {
	if f.volume.IsClosed() {
		return ErrVolumeClosed
	}
	w := &indexWalker{f: f, name: name, strict: strict, fn: fn, visited: make(map[int64]bool)}
	return w.run()
}

// run walks the whole index from its root.
func (w *indexWalker) run() error {
	f, name, strict := w.f, w.name, w.strict

	root, err := f.ReadIndexRoot(name)
	if err != nil {
		return w.fault("root", -1, 0, err, true)
	}
	w.root = root
	w.blockSize = f.volume.indexBlockSize(root)

	if w.alloc, err = f.indexAllocation(name); err != nil {
		return w.fault("allocation", -1, 0, err, true)
	}
	if w.bitmap, err = f.loadIndexBitmap(name); err != nil {
		return w.fault("bitmap", -1, 0, err, true)
	}
	if strict {
		if w.collate, err = f.indexCollation(root.CollationRule); err != nil {
			return w.fault("upcase", -1, 0, err, true)
		}
	}

	if err := w.walkNode(root.Entries, root.NodeHeader.Flags&IndexNodeNode != 0, 0, -1); err != nil {
		return err
	}

	if strict {
		if err := w.checkBitmapCoverage(); err != nil {
			return err
		}
	}
	if w.skipped > 0 {
		return fmt.Errorf("%w: record %d index %s: skipped %d unreadable index blocks after %d entries",
			ErrCorruptIndex, f.entryNum, name, w.skipped, w.delivered)
	}
	return nil
}

// fault reports a damaged structure: as a StrictError when the walk is
// strict, and otherwise by counting a skipped block and carrying on. fatal
// faults stop even a tolerant walk, since there is nothing left to walk.
func (w *indexWalker) fault(check string, vcn int64, entry uint64, err error, fatal bool) error {
	if w.strict {
		return &StrictError{Record: w.f.entryNum, Index: w.name, VCN: vcn, Entry: entry, Check: check, Err: err}
	}
	if fatal {
		return err
	}
	w.skipped++
	return nil
}

// walkNode visits a node's entries in order, descending into each sub-node
// before the entry that points at it.
func (w *indexWalker) walkNode(entries []IndexEntry, isNode bool, depth int, vcn int64) error {
	for _, e := range entries {
		hasSub := e.Flags&IndexFlagNode != 0
		if w.strict && hasSub != isNode {
			return w.fault("node-flag", vcn, e.FileReference,
				fmt.Errorf("%w: entry sub-node flag %v in a node whose header says %v", ErrCorruptIndex, hasSub, isNode), false)
		}
		if hasSub {
			if err := w.descend(int64(e.SubNodeVCN), depth+1); err != nil {
				return err
			}
		}
		if e.Flags&IndexFlagLast != 0 {
			return nil
		}
		if w.collate != nil {
			if w.havePrev && w.collate(w.prev, e.Key) >= 0 {
				return w.fault("collation", vcn, e.FileReference,
					fmt.Errorf("%w: key does not sort after the one before it", ErrCorruptIndex), false)
			}
			w.prev, w.havePrev = e.Key, true
		}
		w.delivered++
		if err := w.fn(e, depth, vcn); err != nil {
			return err
		}
	}
	return nil
}

// descend reads the block at vcn and walks it.
func (w *indexWalker) descend(vcn int64, depth int) error {
	if depth > maxIndexDepth {
		return w.fault("depth", vcn, 0, fmt.Errorf("%w: index is more than %d levels deep", ErrCorruptIndex, maxIndexDepth), false)
	}
	if w.alloc == nil {
		return w.fault("allocation", vcn, 0, fmt.Errorf("%w: sub-node with no $INDEX_ALLOCATION", ErrCorruptIndex), false)
	}
	if w.visited[vcn] {
		return w.fault("revisit", vcn, 0, fmt.Errorf("%w: block reached twice", ErrCorruptIndex), false)
	}
	w.visited[vcn] = true

	offset, err := w.f.volume.indexBlockOffset(vcn, w.blockSize)
	if err != nil {
		return w.fault("slot", vcn, 0, err, false)
	}
	if offset%int64(w.blockSize) != 0 {
		return w.fault("slot", vcn, 0, fmt.Errorf("%w: VCN %d is not on a block boundary", ErrCorruptIndex, vcn), false)
	}
	if w.bitmap != nil && !bitmapBit(w.bitmap, int(offset/int64(w.blockSize))) {
		return w.fault("bitmap-clear", vcn, 0, fmt.Errorf("%w: reachable block is free in $BITMAP", ErrCorruptIndex), false)
	}

	block, err := w.readBlock(vcn, offset)
	if err != nil {
		return w.fault("block", vcn, 0, err, false)
	}
	if w.strict && block.VCN != vcn {
		return w.fault("vcn", vcn, 0, fmt.Errorf("%w: block at VCN %d records VCN %d", ErrCorruptIndex, vcn, block.VCN), false)
	}
	return w.walkNode(block.Entries, block.IsNode, depth, vcn)
}

// readBlock reads and parses the block at vcn, which sits at offset in the
// allocation: from w.data when the caller has already read the allocation,
// and from disk otherwise.
func (w *indexWalker) readBlock(vcn, offset int64) (*IndexBlock, error) {
	if w.data == nil {
		return w.f.readIndexBlock(w.alloc, w.root.AttributeType, w.blockSize, vcn)
	}
	if offset > int64(len(w.data)-w.blockSize) {
		return nil, fmt.Errorf("%w: VCN %d is past the end of the %d byte allocation",
			ErrCorruptIndex, vcn, len(w.data))
	}
	return parseIndexBlock(bytes.Clone(w.data[offset:offset+int64(w.blockSize)]), w.root.AttributeType)
}

// reachableIndexBlocks walks the index named name tolerantly over data, its
// $INDEX_ALLOCATION as already read, and returns the numbers of the blocks the
// B-tree's sub-node pointers reach, block i being the one at byte offset i
// times blockSize. A block the tree points at counts as reached even when it
// is unreadable or free in $BITMAP.
//
// complete is false when the walk could not cover the whole tree -- an
// unreadable root, an unreadable or free block, a cycle -- since a block it
// did not reach may then sit below the damage rather than outside the tree.
func (f *File) reachableIndexBlocks(name string, data []byte, blockSize int) (reached map[int]bool, complete bool) {
	w := &indexWalker{
		f: f, name: name, data: data, visited: make(map[int64]bool),
		fn: func(IndexEntry, int, int64) error { return nil },
	}
	err := w.run()
	reached = make(map[int]bool, len(w.visited))
	for vcn := range w.visited {
		offset, oerr := f.volume.indexBlockOffset(vcn, blockSize)
		if oerr == nil && offset%int64(blockSize) == 0 && offset/int64(blockSize) <= math.MaxInt32 {
			reached[int(offset/int64(blockSize))] = true
		}
	}
	return reached, err == nil
}

// checkBitmapCoverage checks that the blocks $BITMAP marks in use are exactly
// those the walk reached.
func (w *indexWalker) checkBitmapCoverage() error {
	if w.alloc == nil {
		return nil
	}
	if w.bitmap == nil {
		return w.fault("bitmap-missing", -1, 0, fmt.Errorf("%w: $INDEX_ALLOCATION with no $BITMAP", ErrCorruptIndex), false)
	}
	unit := w.f.volume.indexVCNUnit(w.blockSize)
	for i := 0; i < len(w.bitmap)*8; i++ {
		if !bitmapBit(w.bitmap, i) {
			continue
		}
		vcn := int64(i) * int64(w.blockSize) / unit
		if !w.visited[vcn] {
			return w.fault("bitmap-unreachable", vcn, 0,
				fmt.Errorf("%w: block %d is in use in $BITMAP but no entry points at it", ErrCorruptIndex, i), false)
		}
	}
	return nil
}

// indexCollation returns the comparison an index's keys must be ordered by,
// or nil for a collation the strict checks do not verify.
func (f *File) indexCollation(rule uint32) (func(a, b []byte) int, error) {
	switch rule {
	case CollationFileName:
		upcase, err := f.volume.UpCase()
		if err != nil {
			return nil, err
		}
		return func(a, b []byte) int { return collateFileNameKeys(upcase, a, b) }, nil
	case CollationULong:
		return CollateULong, nil
	case CollationULongs, CollationSecurityHash:
		// $SDH's key is a hash then a security ID, which Windows orders as
		// two ULONGs.
		return CollateULongs, nil
	case CollationBinary:
		return bytes.Compare, nil
	}
	return nil, nil
}

// collateFileNameKeys orders two $I30 keys by name as NTFS does: through
// $UpCase, and then, for names equal but for case, by the units themselves.
func collateFileNameKeys(upcase UpCase, a, b []byte) int {
	ua, ub := fileNameKeyUnits(a), fileNameKeyUnits(b)
	if c := upcase.compareUnits(ua, ub); c != 0 {
		return c
	}
	return UpCase(nil).compareUnits(ua, ub)
}

// fileNameKeyUnits returns the raw UTF-16 name of a $FILE_NAME key.
func fileNameKeyUnits(key []byte) []uint16 {
	if len(key) < 66 {
		return nil
	}
	n := int(key[64])
	if 66+2*n > len(key) {
		n = (len(key) - 66) / 2
	}
	units := make([]uint16, n)
	for i := range units {
		units[i] = ReadUint16LE(key, 66+2*i)
	}
	return units
}
