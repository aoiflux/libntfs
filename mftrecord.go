package libntfs

import "fmt"

// DefaultMFTRecordSize is the MFT record size assumed by ParseMFTRecord when the
// caller passes 0. It matches the size used by every mainstream NTFS formatter.
const DefaultMFTRecordSize = 1024

// ParseMFTRecord parses a single MFT FILE record from a raw buffer, with no Volume.
// It is intended for standalone $MFT files, such as those produced by KAPE, FTK
// Imager, or icat, where the surrounding volume is unavailable.
//
// entryNum is the record's index within the $MFT; it is recorded on the returned
// entry as RecordNumber and used for error context. recordSize is the on-disk MFT
// record size, and defaults to DefaultMFTRecordSize when 0.
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
	if recordSize == 0 {
		recordSize = DefaultMFTRecordSize
	}

	if recordSize < MinMFTEntrySize || recordSize > MaxMFTEntrySize {
		return nil, fmt.Errorf("%w: record size %d out of range [%d, %d]",
			ErrInvalidMFTEntry, recordSize, MinMFTEntrySize, MaxMFTEntrySize)
	}

	if len(buf) < int(recordSize) {
		return nil, fmt.Errorf("%w: buffer holds %d bytes, need %d",
			ErrInvalidMFTEntry, len(buf), recordSize)
	}

	entry, err := parseMFTRecordCore(buf, entryNum, recordSize)
	if err != nil {
		return nil, err
	}

	entry.RecordNumber = uint32(entryNum)

	return entry, nil
}
