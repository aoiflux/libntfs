package libntfs

import "fmt"

// The open attribute table is what turns a log record's TargetAttribute from a
// number into a file and a stream.
//
// NTFS does not repeat the file reference on every record. It opens an attribute
// once, records that in the open attribute table, and every later record naming
// that attribute carries only its offset in the table. Reading the log without
// the table therefore yields operations with no subject: "UpdateResidentValue on
// attribute 0x18" rather than "UpdateResidentValue on $MFT's $DATA".
//
// NTFS periodically dumps the table into the log so a recovery pass can rebuild
// it, which is what makes this recoverable from the log alone.
//
// # How this layout was established
//
// The entry layout below is not from a published specification. It was derived
// from a real volume and then cross-checked until each field had an independent
// confirmation:
//
//   - The attribute type at 0x08 reads 0x80, 0xA0, and 0xB0 — $DATA,
//     $INDEX_ALLOCATION, and $BITMAP — and never anything else.
//   - The file reference at 0x10 resolves to $MFT, $AttrDef, the root directory,
//     $Bitmap, and $Extend, each with the sequence number those records
//     actually carry, and each paired with an attribute type that file really
//     has.
//   - The LSN at 0x18 falls inside the range of LSNs the same log holds.
//   - Every name in the AttributeNamesDump resolves through
//     (offset - headerSize) / entrySize to an entry whose type is
//     $INDEX_ALLOCATION, which is the only attribute type on those files that
//     carries a name: "$I30" on directories, "$Q" on $Quota, "$O" on $ObjId,
//     "$SDS", "$SII", and "$SDH" on $Secure.
//   - The dirty page table's LCN for a given VCN of $MFT agrees with the VCN to
//     LCN mapping carried by unrelated records elsewhere in the same log.
//
// Two fields in each entry are not identified and are deliberately not exposed:
// the word at 0x04, which reads 0x1000 for index allocations and zero
// otherwise, and the word at 0x0C. The last eight bytes hold what looks like a
// kernel pointer, which has no meaning on disk. Raw carries the whole entry for
// anyone who wants to go further.
const (
	// logOpenAttributeTypeOffset is where the attribute type sits in an entry.
	logOpenAttributeTypeOffset = 0x08
	// logOpenAttributeFileRefOffset is where the file reference sits.
	logOpenAttributeFileRefOffset = 0x10
	// logOpenAttributeLSNOffset is where the LSN of the open record sits.
	logOpenAttributeLSNOffset = 0x18
	// logOpenAttributeMinSize is the smallest entry these offsets can be read
	// from.
	logOpenAttributeMinSize = 0x20

	// logAttributeNameHeaderSize is the index and length preceding each name in
	// an AttributeNamesDump.
	logAttributeNameHeaderSize = 4

	// logAttributeNameMaxBytes bounds one name, which is an NTFS attribute name
	// and so cannot exceed 255 UTF-16 characters.
	logAttributeNameMaxBytes = 255 * 2

	// logAttributeEntryInUse marks an allocated entry; anything else is the
	// index of the next free one.
	logAttributeEntryInUse = 0xFFFFFFFF
)

// LogOpenAttributeEntry is one attribute NTFS had open, recovered from an
// OpenAttributeTableDump record.
//
// Offset is the value a log record's TargetAttribute carries, so it is the key
// to look an entry up by rather than Index.
type LogOpenAttributeEntry struct {
	Index  int    // Position in the table
	Offset uint32 // Byte offset within the dump, which TargetAttribute names
	InUse  bool   // False when the slot is on the free list

	AttributeType  uint32 // $DATA, $INDEX_ALLOCATION, $BITMAP, and so on
	FileReference  uint64 // Whole reference, record number and sequence
	RecordNumber   uint64 // MFT entry the attribute belongs to
	SequenceNumber uint16 // Sequence of that entry, for detecting reuse
	OpenLSN        uint64 // LSN of the record that opened the attribute

	// Name is the attribute name, filled in from an AttributeNamesDump record.
	// It is empty for an unnamed attribute, which is the common case.
	Name string

	// Raw is the whole entry, for fields libntfs does not identify.
	Raw []byte
}

// AttributeTypeName returns the attribute type as a name, such as "$DATA".
func (e LogOpenAttributeEntry) AttributeTypeName() string {
	return GetAttributeTypeName(e.AttributeType)
}

// String renders the entry as the file and stream it names.
func (e LogOpenAttributeEntry) String() string {
	if e.Name != "" {
		return fmt.Sprintf("entry %d:%s%s", e.RecordNumber, e.AttributeTypeName(), ":"+e.Name)
	}
	return fmt.Sprintf("entry %d:%s", e.RecordNumber, e.AttributeTypeName())
}

// ParseLogOpenAttributeTable decodes the open attribute table carried in an
// OpenAttributeTableDump record's redo data.
//
// Entries on the free list are returned with InUse false rather than dropped,
// since a record from before the slot was freed still refers to it and an
// examiner reading a historic record wants to see what was there.
func ParseLogOpenAttributeTable(buf []byte) ([]LogOpenAttributeEntry, error) {
	table, err := ParseLogRestartTable(buf)
	if err != nil {
		return nil, err
	}

	if int(table.EntrySize) < logOpenAttributeMinSize {
		return nil, fmt.Errorf("%w: open attribute entries are %d bytes, need %d",
			ErrInvalidLogRecord, table.EntrySize, logOpenAttributeMinSize)
	}

	entries := make([]LogOpenAttributeEntry, 0, len(table.Entries))

	for i, raw := range table.Entries {
		fileReference := ReadUint64LE(raw, logOpenAttributeFileRefOffset)

		entries = append(entries, LogOpenAttributeEntry{
			Index:          i,
			Offset:         uint32(logRestartTableHeaderSize + i*int(table.EntrySize)),
			InUse:          ReadUint32LE(raw, 0) == logAttributeEntryInUse,
			AttributeType:  ReadUint32LE(raw, logOpenAttributeTypeOffset),
			FileReference:  fileReference,
			RecordNumber:   fileReferenceRecordNumber(fileReference),
			SequenceNumber: uint16(fileReference >> 48),
			OpenLSN:        ReadUint64LE(raw, logOpenAttributeLSNOffset),
			Raw:            raw,
		})
	}

	return entries, nil
}

// ParseLogAttributeNames decodes an AttributeNamesDump record's redo data into
// the attribute names it carries, keyed by the table offset each belongs to.
//
// This is not a restart table despite arriving in the same family of records:
// the data is a bare run of entries, each an index, a length in bytes, the name
// in UTF-16, and a terminator.
func ParseLogAttributeNames(buf []byte) (map[uint32]string, error) {
	names := make(map[uint32]string)
	offset := 0

	for offset+logAttributeNameHeaderSize <= len(buf) {
		index := uint32(ReadUint16LE(buf, offset))
		length := int(ReadUint16LE(buf, offset+2))

		// A zeroed header is the end of the run rather than a nameless entry.
		if index == 0 && length == 0 {
			return names, nil
		}

		if length == 0 || length%2 != 0 || length > logAttributeNameMaxBytes {
			return names, fmt.Errorf("%w: attribute name at %d declares %d bytes",
				ErrInvalidLogRecord, offset, length)
		}

		start := offset + logAttributeNameHeaderSize
		end := start + length
		if end > len(buf) {
			return names, fmt.Errorf("%w: attribute name at %d needs %d bytes, only %d remain",
				ErrInvalidLogRecord, offset, end-offset, len(buf)-offset)
		}

		reader := NewBinaryReader(buf[start:end])
		name, err := reader.ReadUTF16String(length / 2)
		if err != nil {
			return names, fmt.Errorf("%w: attribute name at %d: %v",
				ErrInvalidLogRecord, offset, err)
		}

		names[index] = name

		// The name is followed by a UTF-16 terminator.
		offset = end + 2
	}

	return names, nil
}

// LogAttributeTable resolves a log record's TargetAttribute to the file and
// stream it names.
//
// Build one with [Volume.LogAttributeTable], which scans the log for the dump
// records that carry it.
type LogAttributeTable struct {
	byOffset map[uint32]LogOpenAttributeEntry
}

// Len reports how many attributes the table holds.
func (t *LogAttributeTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byOffset)
}

// Entries returns every attribute in the table, in table order.
func (t *LogAttributeTable) Entries() []LogOpenAttributeEntry {
	if t == nil {
		return nil
	}

	entries := make([]LogOpenAttributeEntry, 0, len(t.byOffset))
	for _, entry := range t.byOffset {
		entries = append(entries, entry)
	}

	sortLogAttributeEntries(entries)

	return entries
}

// Lookup resolves a log record's TargetAttribute.
//
// It reports false when the log's window holds no dump covering that offset,
// which for a log that wrapped since the attribute was opened is the normal case
// rather than an error.
func (t *LogAttributeTable) Lookup(targetAttribute uint16) (LogOpenAttributeEntry, bool) {
	if t == nil {
		return LogOpenAttributeEntry{}, false
	}

	entry, ok := t.byOffset[uint32(targetAttribute)]

	return entry, ok
}

// Resolve returns the attribute a record operated on.
//
// A record that names no attribute, or one the table does not cover, reports
// false.
func (t *LogAttributeTable) Resolve(record *LogRecord) (LogOpenAttributeEntry, bool) {
	if record == nil || !record.IsClientRecord() {
		return LogOpenAttributeEntry{}, false
	}

	return t.Lookup(record.TargetAttribute)
}

// LogAttributeTable builds the open attribute table from the dump records in the
// volume's $LogFile.
//
// Later dumps win over earlier ones, since the table they describe is the more
// recent state. Names from AttributeNamesDump records are merged in, so a
// resolved entry carries the stream name where it has one.
//
// The log is circular, so a volume whose log has wrapped past its last dump
// returns an empty table rather than an error: there is nothing wrong, the
// information has simply been overwritten.
func (v *Volume) LogAttributeTable() (*LogAttributeTable, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	table := &LogAttributeTable{byOffset: make(map[uint32]LogOpenAttributeEntry)}
	names := make(map[uint32]string)

	// Dumps are applied in the order the log holds them, so the newest wins.
	err := v.EachLogRecord(func(record *LogRecord) error {
		switch record.RedoOperation {
		case LogOpOpenAttributeTableDump:
			entries, err := ParseLogOpenAttributeTable(record.RedoData)
			if err != nil {
				// A dump clipped by the circular log is expected. Take what
				// parsed and carry on.
				return nil
			}
			for _, entry := range entries {
				table.byOffset[entry.Offset] = entry
			}

		case LogOpAttributeNamesDump:
			dumped, err := ParseLogAttributeNames(record.RedoData)
			if err != nil && len(dumped) == 0 {
				return nil
			}
			for offset, name := range dumped {
				names[offset] = name
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	for offset, name := range names {
		if entry, ok := table.byOffset[offset]; ok {
			entry.Name = name
			table.byOffset[offset] = entry
		}
	}

	return table, nil
}

// sortLogAttributeEntries orders entries by their table offset.
func sortLogAttributeEntries(entries []LogOpenAttributeEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Offset < entries[j-1].Offset; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
