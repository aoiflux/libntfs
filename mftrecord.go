package libntfs

import "fmt"

// DefaultMFTRecordSize is the MFT record size assumed by ParseMFTRecord when the
// caller passes 0. It matches the size used by every mainstream NTFS formatter.
const DefaultMFTRecordSize = 1024

// ParseMFTRecord parses a single MFT FILE record from a raw buffer, with no Volume.
// It is intended for standalone $MFT files, such as those produced by KAPE, FTK
// Imager, or icat, where the surrounding volume is unavailable.
//
// entryNum is the record's index within the $MFT and is recorded on the returned
// entry as RecordNumber. recordSize is the on-disk MFT record size, and defaults
// to DefaultMFTRecordSize when 0.
//
// Resident attributes parse fully, so GetFileName, GetStandardInformation,
// IsDirectory, IsInUse and FindAttribute all work on the returned entry.
// Non-resident attributes have their data runs decoded but not resolved: reading
// content requires cluster access, which only a Volume provides.
//
// buf is modified in place by the update sequence (fixup) array. Callers that
// need the original bytes should pass a copy.
//
// Records that are unallocated, marked bad, or otherwise malformed return an
// error wrapping ErrInvalidMFTEntry, and records that fail fixup validation
// return an error wrapping ErrUpdateSequence. Callers streaming an entire $MFT
// should skip those and continue rather than stop.
func ParseMFTRecord(buf []byte, entryNum uint64, recordSize uint32) (*MFTEntry, error) {
	entry := &MFTEntry{}
	if err := ParseMFTRecordInto(entry, buf, entryNum, recordSize); err != nil {
		return nil, err
	}

	return entry, nil
}

// ParseMFTRecordInto is ParseMFTRecord with a caller-supplied destination, for
// sweeping a large $MFT without allocating a fresh MFTEntry per record. entry is
// reset before parsing, and its attribute slice keeps its capacity so repeated
// calls stop regrowing it.
//
// Reuse invalidates everything the previous call produced: the entry, its
// attributes, and any FileName or StandardInformation still referencing them.
// Callers that retain results across records must copy what they need, or use
// ParseMFTRecord instead. On error the contents of entry are undefined.
//
// Note that the per-attribute values are still allocated on every call, so this
// trims allocation pressure rather than eliminating it.
func ParseMFTRecordInto(entry *MFTEntry, buf []byte, entryNum uint64, recordSize uint32) error {
	if entry == nil {
		return fmt.Errorf("%w: nil destination entry", ErrInvalidMFTEntry)
	}

	if recordSize == 0 {
		recordSize = DefaultMFTRecordSize
	}

	if recordSize < MinMFTEntrySize || recordSize > MaxMFTEntrySize {
		return fmt.Errorf("%w: record size %d out of range [%d, %d]",
			ErrInvalidMFTEntry, recordSize, MinMFTEntrySize, MaxMFTEntrySize)
	}

	if len(buf) < int(recordSize) {
		return fmt.Errorf("%w: buffer holds %d bytes, need %d",
			ErrInvalidMFTEntry, len(buf), recordSize)
	}

	return parseMFTRecordCore(entry, buf, entryNum, recordSize)
}
