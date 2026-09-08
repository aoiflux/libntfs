package libntfs

import (
	"context"
	"fmt"
	"io"
	"time"
)

// USN journal locations and limits.
const (
	USNJournalPath   = "$UsnJrnl" // Entry name inside the $Extend directory
	USNJournalStream = "$J"       // Named $DATA stream holding the records
	USNMaxStream     = "$Max"     // Named $DATA stream holding journal limits

	// USNRecordV2HeaderSize is the fixed portion of a V2 record, before the name.
	USNRecordV2HeaderSize = 60
	// USNRecordV3HeaderSize is the fixed portion of a V3 record, before the name.
	USNRecordV3HeaderSize = 76
	// USNRecordV4HeaderSize is the fixed portion of a V4 extent record.
	USNRecordV4HeaderSize = 64

	// USNMaxRecordLength bounds a single record, guarding against a corrupt
	// length field sending the walk off into nonsense.
	USNMaxRecordLength = 64 << 10

	// usnChunkSize is how much of $J is read per iteration while streaming.
	usnChunkSize = 1 << 20

	// usnAlignment is the boundary records are laid out on.
	usnAlignment = 8

	// usnCancelCheckInterval is how many loop steps pass between cancellation
	// checks while walking a buffer. A step over sparse space advances only 8
	// bytes, so checking every one would put a ctx.Err call on the hottest path
	// in the walk to no benefit; a few thousand steps is still sub-millisecond.
	usnCancelCheckInterval = 4096
)

// USN change reasons.
const (
	USNReasonDataOverwrite       = 0x00000001
	USNReasonDataExtend          = 0x00000002
	USNReasonDataTruncation      = 0x00000004
	USNReasonNamedDataOverwrite  = 0x00000010
	USNReasonNamedDataExtend     = 0x00000020
	USNReasonNamedDataTruncation = 0x00000040
	USNReasonFileCreate          = 0x00000100
	USNReasonFileDelete          = 0x00000200
	USNReasonEAChange            = 0x00000400
	USNReasonSecurityChange      = 0x00000800
	USNReasonRenameOldName       = 0x00001000
	USNReasonRenameNewName       = 0x00002000
	USNReasonIndexableChange     = 0x00004000
	USNReasonBasicInfoChange     = 0x00008000
	USNReasonHardLinkChange      = 0x00010000
	USNReasonCompressionChange   = 0x00020000
	USNReasonEncryptionChange    = 0x00040000
	USNReasonObjectIDChange      = 0x00080000
	USNReasonReparsePointChange  = 0x00100000
	USNReasonStreamChange        = 0x00200000
	USNReasonTransactedChange    = 0x00400000
	USNReasonIntegrityChange     = 0x00800000
	USNReasonDesiredStorageClass = 0x01000000
	USNReasonClose               = 0x80000000
)

// usnReasonNames pairs each reason bit with its name, ordered so the common
// lifecycle events read first.
var usnReasonNames = []struct {
	mask uint32
	name string
}{
	{USNReasonFileCreate, "FileCreate"},
	{USNReasonFileDelete, "FileDelete"},
	{USNReasonRenameOldName, "RenameOldName"},
	{USNReasonRenameNewName, "RenameNewName"},
	{USNReasonDataOverwrite, "DataOverwrite"},
	{USNReasonDataExtend, "DataExtend"},
	{USNReasonDataTruncation, "DataTruncation"},
	{USNReasonNamedDataOverwrite, "NamedDataOverwrite"},
	{USNReasonNamedDataExtend, "NamedDataExtend"},
	{USNReasonNamedDataTruncation, "NamedDataTruncation"},
	{USNReasonEAChange, "EAChange"},
	{USNReasonSecurityChange, "SecurityChange"},
	{USNReasonIndexableChange, "IndexableChange"},
	{USNReasonBasicInfoChange, "BasicInfoChange"},
	{USNReasonHardLinkChange, "HardLinkChange"},
	{USNReasonCompressionChange, "CompressionChange"},
	{USNReasonEncryptionChange, "EncryptionChange"},
	{USNReasonObjectIDChange, "ObjectIDChange"},
	{USNReasonReparsePointChange, "ReparsePointChange"},
	{USNReasonStreamChange, "StreamChange"},
	{USNReasonTransactedChange, "TransactedChange"},
	{USNReasonIntegrityChange, "IntegrityChange"},
	{USNReasonDesiredStorageClass, "DesiredStorageClassChange"},
	{USNReasonClose, "Close"},
}

// USNRecord is one entry from the USN change journal.
//
// Version 4 records track extents rather than name changes: they carry no
// timestamp and no file name, so those fields are zero for them.
type USNRecord struct {
	MajorVersion uint16 `json:"major_version"`
	MinorVersion uint16 `json:"minor_version"`
	USN          int64  `json:"usn"`

	// FileReference is the MFT record number of the changed file and
	// FileSequence that record's sequence number; ParentReference and
	// ParentSequence are the same pair for the directory containing it. The
	// sequence numbers are what let a journal entry be tied to the right
	// occupant of a reused record, and they match [NTFSFile.SequenceNumber] and
	// [NTFSFile.ParentSequence] in a volume report.
	FileReference   uint64 `json:"file_reference"`
	FileSequence    uint16 `json:"file_sequence"`
	ParentReference uint64 `json:"parent_reference"`
	ParentSequence  uint16 `json:"parent_sequence"`

	Timestamp      time.Time `json:"timestamp"`
	Reason         uint32    `json:"reason"`
	SourceInfo     uint32    `json:"source_info"`
	SecurityID     uint32    `json:"security_id"`
	FileAttributes uint32    `json:"file_attributes"`
	Name           string    `json:"name"`
}

// ReasonNames decodes the reason bitmask into readable names. Bits with no known
// name are reported as a trailing hex value so nothing is silently dropped.
func (r *USNRecord) ReasonNames() []string {
	var (
		names     []string
		remaining = r.Reason
	)

	for _, reason := range usnReasonNames {
		if r.Reason&reason.mask != 0 {
			names = append(names, reason.name)
			remaining &^= reason.mask
		}
	}

	if remaining != 0 {
		names = append(names, fmt.Sprintf("0x%08X", remaining))
	}

	return names
}

// IsDirectory reports whether the record describes a directory.
func (r *USNRecord) IsDirectory() bool {
	return r.FileAttributes&FileAttrDirectory != 0
}

// ParseUSNRecord parses a single USN record from the front of buf, returning the
// record and the number of bytes it occupies. It needs no Volume, so it can be
// pointed at an extracted $J file.
//
// Versions 2, 3, and 4 are decoded. A record length of zero means padding or
// sparse space rather than a record, and returns an error wrapping
// ErrInvalidUSNRecord. A record extending past the end of buf returns
// io.ErrUnexpectedEOF, which a chunked reader should treat as a signal to supply
// more data rather than as corruption.
func ParseUSNRecord(buf []byte) (*USNRecord, int, error) {
	if len(buf) < 4 {
		return nil, 0, io.ErrUnexpectedEOF
	}

	recordLength := int(ReadUint32LE(buf, 0))

	if recordLength == 0 {
		return nil, 0, fmt.Errorf("%w: zero record length", ErrInvalidUSNRecord)
	}

	if recordLength < USNRecordV4HeaderSize || recordLength > USNMaxRecordLength {
		return nil, 0, fmt.Errorf("%w: implausible record length %d",
			ErrInvalidUSNRecord, recordLength)
	}

	if recordLength > len(buf) {
		return nil, 0, io.ErrUnexpectedEOF
	}

	if len(buf) < 8 {
		return nil, 0, io.ErrUnexpectedEOF
	}

	record := &USNRecord{
		MajorVersion: ReadUint16LE(buf, 4),
		MinorVersion: ReadUint16LE(buf, 6),
	}

	var err error

	switch record.MajorVersion {
	case 2:
		err = parseUSNRecordV2(buf[:recordLength], record)
	case 3:
		err = parseUSNRecordV3(buf[:recordLength], record)
	case 4:
		err = parseUSNRecordV4(buf[:recordLength], record)
	default:
		return nil, recordLength, fmt.Errorf("%w: unsupported version %d.%d",
			ErrInvalidUSNRecord, record.MajorVersion, record.MinorVersion)
	}
	if err != nil {
		return nil, recordLength, err
	}

	return record, recordLength, nil
}

// parseUSNRecordV2 decodes a version 2 record, which uses 64-bit file references.
func parseUSNRecordV2(buf []byte, record *USNRecord) error {
	if len(buf) < USNRecordV2HeaderSize {
		return fmt.Errorf("%w: V2 record is %d bytes, need %d",
			ErrInvalidUSNRecord, len(buf), USNRecordV2HeaderSize)
	}

	record.FileReference = fileReferenceRecordNumber(ReadUint64LE(buf, 8))
	record.FileSequence = ReadUint16LE(buf, 14)
	record.ParentReference = fileReferenceRecordNumber(ReadUint64LE(buf, 16))
	record.ParentSequence = ReadUint16LE(buf, 22)
	record.USN = int64(ReadUint64LE(buf, 24))
	record.Timestamp = NTFSTimeToTime(ReadUint64LE(buf, 32))
	record.Reason = ReadUint32LE(buf, 40)
	record.SourceInfo = ReadUint32LE(buf, 44)
	record.SecurityID = ReadUint32LE(buf, 48)
	record.FileAttributes = ReadUint32LE(buf, 52)

	return readUSNName(buf, record, 56, 58)
}

// parseUSNRecordV3 decodes a version 3 record, which widens both file references
// to 128 bits. On NTFS the leading 8 bytes still hold the familiar reference.
func parseUSNRecordV3(buf []byte, record *USNRecord) error {
	if len(buf) < USNRecordV3HeaderSize {
		return fmt.Errorf("%w: V3 record is %d bytes, need %d",
			ErrInvalidUSNRecord, len(buf), USNRecordV3HeaderSize)
	}

	record.FileReference = fileReferenceRecordNumber(ReadUint64LE(buf, 8))
	record.FileSequence = ReadUint16LE(buf, 14)
	record.ParentReference = fileReferenceRecordNumber(ReadUint64LE(buf, 24))
	record.ParentSequence = ReadUint16LE(buf, 30)
	record.USN = int64(ReadUint64LE(buf, 40))
	record.Timestamp = NTFSTimeToTime(ReadUint64LE(buf, 48))
	record.Reason = ReadUint32LE(buf, 56)
	record.SourceInfo = ReadUint32LE(buf, 60)
	record.SecurityID = ReadUint32LE(buf, 64)
	record.FileAttributes = ReadUint32LE(buf, 68)

	return readUSNName(buf, record, 72, 74)
}

// parseUSNRecordV4 decodes a version 4 extent record. These track ranges written
// within a file and carry neither a timestamp nor a name.
func parseUSNRecordV4(buf []byte, record *USNRecord) error {
	if len(buf) < USNRecordV4HeaderSize {
		return fmt.Errorf("%w: V4 record is %d bytes, need %d",
			ErrInvalidUSNRecord, len(buf), USNRecordV4HeaderSize)
	}

	record.FileReference = fileReferenceRecordNumber(ReadUint64LE(buf, 8))
	record.FileSequence = ReadUint16LE(buf, 14)
	record.ParentReference = fileReferenceRecordNumber(ReadUint64LE(buf, 24))
	record.ParentSequence = ReadUint16LE(buf, 30)
	record.USN = int64(ReadUint64LE(buf, 40))
	record.Reason = ReadUint32LE(buf, 48)
	record.SourceInfo = ReadUint32LE(buf, 52)

	return nil
}

// readUSNName reads the record's file name using the length and offset fields at
// the given positions.
func readUSNName(buf []byte, record *USNRecord, lengthOffset, offsetOffset int) error {
	nameLength := int(ReadUint16LE(buf, lengthOffset))
	nameOffset := int(ReadUint16LE(buf, offsetOffset))

	if nameLength == 0 {
		return nil
	}

	if nameOffset < 0 || nameOffset+nameLength > len(buf) {
		return fmt.Errorf("%w: name at %d+%d exceeds the %d byte record",
			ErrInvalidUSNRecord, nameOffset, nameLength, len(buf))
	}

	r := NewBinaryReader(buf[nameOffset:])
	name, err := r.ReadUTF16String(nameLength / 2)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidUSNRecord, err)
	}

	record.Name = name

	return nil
}

// EachUSNRecord walks every record in a $J buffer, calling fn for each one.
//
// Zero-filled regions, which is how the sparse front of $J and inter-record
// padding read, are skipped. Malformed records are skipped rather than ending
// the walk, so damage in one region does not hide the records after it. If fn
// returns a non-nil error, iteration stops and that error is returned.
func EachUSNRecord(buf []byte, fn func(*USNRecord) error) error {
	_, err := walkUSNRecords(context.Background(), buf, true, fn)
	return err
}

// EachUSNRecordContext is [EachUSNRecord] with cancellation. Cancelling ctx
// stops the walk and returns the context's error.
func EachUSNRecordContext(ctx context.Context, buf []byte, fn func(*USNRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	_, err := walkUSNRecords(ctx, buf, true, fn)
	return err
}

// walkUSNRecords is the shared walker. When allowTail is false it stops at the
// first record that runs past the end of buf and reports how many bytes were
// consumed, so a chunked reader can carry the remainder into the next chunk.
func walkUSNRecords(ctx context.Context, buf []byte, allowTail bool, fn func(*USNRecord) error) (int, error) {
	offset := 0

	// Primed so the first iteration checks: a walk started under an already
	// cancelled context must stop, however short the buffer is.
	sinceCheck := usnCancelCheckInterval - 1

	for offset+4 <= len(buf) {
		// After that, cancellation is checked every so many steps rather than
		// every one: skipping a sparse region advances 8 bytes at a time, and a
		// ctx.Err call per step would cost more than the scan it guards.
		if sinceCheck++; sinceCheck >= usnCancelCheckInterval {
			sinceCheck = 0
			if err := ctx.Err(); err != nil {
				return offset, err
			}
		}

		// Skip zero-filled space. Records are 8-byte aligned, so stepping by the
		// alignment is both correct and much faster than a byte-at-a-time scan.
		if ReadUint32LE(buf, offset) == 0 {
			offset += usnAlignment
			continue
		}

		record, consumed, err := ParseUSNRecord(buf[offset:])
		if err == io.ErrUnexpectedEOF {
			if allowTail {
				break // Truncated final record: nothing more to read
			}
			return offset, nil // Carry the partial record into the next chunk
		}

		if err != nil {
			// Unusable record. Advance past it when its length was believable,
			// otherwise resynchronise on the next alignment boundary.
			if consumed >= USNRecordV4HeaderSize {
				offset += AlignUp(consumed, usnAlignment)
			} else {
				offset += usnAlignment
			}
			continue
		}

		if err := fn(record); err != nil {
			return offset, err
		}

		offset += AlignUp(consumed, usnAlignment)
	}

	return offset, nil
}

// OpenUSNJournal opens the $J stream of \$Extend\$UsnJrnl, which holds the
// change journal records.
func (v *Volume) OpenUSNJournal() (*File, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	extend, err := v.Open(MFTEntryExtend)
	if err != nil {
		return nil, fmt.Errorf("open $Extend: %w", err)
	}

	entries, err := extend.ReadDir()
	if err != nil {
		return nil, fmt.Errorf("read $Extend: %w", err)
	}

	for _, entry := range entries {
		if entry.Deleted || entry.Name != USNJournalPath {
			continue
		}

		journal, err := v.Open(entry.EntryNum)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", USNJournalPath, err)
		}

		return journal.OpenStream(USNJournalStream)
	}

	return nil, fmt.Errorf("%w: %s not found in $Extend",
		ErrFileNotFound, USNJournalPath)
}

// EachUSNRecord walks the volume's change journal, calling fn for every record.
//
// The journal is read in chunks rather than loaded whole, since $J routinely
// runs to gigabytes, and its sparse leading region is skipped outright using the
// stream's data runs. If fn returns a non-nil error, iteration stops and that
// error is returned.
func (v *Volume) EachUSNRecord(fn func(*USNRecord) error) error {
	return v.EachUSNRecordContext(context.Background(), fn)
}

// EachUSNRecordContext is [Volume.EachUSNRecord] with cancellation.
//
// $J routinely runs to gigabytes, so this is one of the long operations on a
// large volume. Cancelling ctx stops the walk between chunks and returns the
// context's error; records already handed to fn stay handed over.
func (v *Volume) EachUSNRecordContext(ctx context.Context, fn func(*USNRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	journal, err := v.OpenUSNJournal()
	if err != nil {
		return err
	}

	size := journal.Size()
	if size <= 0 {
		return nil
	}

	offset := usnFirstAllocatedOffset(journal, v.BytesPerCluster())
	if offset >= size {
		return nil
	}

	var (
		chunk = make([]byte, usnChunkSize)
		carry []byte
	)

	for offset < size {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, readErr := journal.ReadAt(chunk, offset)
		if n == 0 {
			break
		}

		data := chunk[:n]
		if len(carry) > 0 {
			data = append(carry, data...)
		}

		atEnd := readErr == io.EOF || offset+int64(n) >= size

		consumed, err := walkUSNRecords(ctx, data, atEnd, fn)
		if err != nil {
			return err
		}

		carry = append(carry[:0], data[consumed:]...)
		offset += int64(n)

		if atEnd {
			break
		}
	}

	return nil
}

// usnFirstAllocatedOffset returns the logical offset of the first allocated byte
// of the journal, skipping the sparse region that $J carries at its front.
func usnFirstAllocatedOffset(journal *File, bytesPerCluster uint32) int64 {
	attr := journal.dataAttr
	if attr == nil || attr.NonResident == nil || bytesPerCluster == 0 {
		return 0
	}

	var offset int64
	for _, run := range attr.NonResident.DataRuns {
		if !run.IsSparse {
			break
		}
		offset += int64(run.LengthClusters * uint64(bytesPerCluster))
	}

	return offset
}
