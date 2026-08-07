package libntfs

import (
	"errors"
	"testing"
)

// buildOpenAttributeEntry assembles one open attribute table entry.
//
// The offsets here are the ones derived from a real volume; the fields libntfs
// does not identify are left zero, which is what the parser ignores anyway.
func buildOpenAttributeEntry(inUse bool, attrType uint32,
	recordNumber uint64, sequence uint16, openLSN uint64,
) []byte {
	entry := make([]byte, 0x28)

	if inUse {
		WriteUint32LE(entry, 0, logAttributeEntryInUse)
	} else {
		WriteUint32LE(entry, 0, 7) // Index of the next free entry
	}

	WriteUint32LE(entry, logOpenAttributeTypeOffset, attrType)
	WriteUint64LE(entry, logOpenAttributeFileRefOffset,
		recordNumber|uint64(sequence)<<48)
	WriteUint64LE(entry, logOpenAttributeLSNOffset, openLSN)

	return entry
}

// buildOpenAttributeTableDump wraps entries in a restart table header.
func buildOpenAttributeTableDump(entries ...[]byte) []byte {
	const entrySize = 0x28

	buf := make([]byte, logRestartTableHeaderSize+entrySize*len(entries))

	WriteUint16LE(buf, 0x00, entrySize)
	WriteUint16LE(buf, 0x02, uint16(len(entries)))
	WriteUint16LE(buf, 0x04, uint16(len(entries)))
	WriteUint32LE(buf, 0x0C, 0xFFFFFFFF)

	for i, entry := range entries {
		copy(buf[logRestartTableHeaderSize+i*entrySize:], entry)
	}

	return buf
}

// buildAttributeNamesDump assembles the bare run of names an
// AttributeNamesDump carries: index, byte length, UTF-16 name, terminator.
func buildAttributeNamesDump(names map[uint32]string, order ...uint32) []byte {
	var buf []byte

	for _, index := range order {
		name := names[index]
		encoded := utf16LEFromString(name)

		header := make([]byte, logAttributeNameHeaderSize)
		WriteUint16LE(header, 0, uint16(index))
		WriteUint16LE(header, 2, uint16(len(encoded)))

		buf = append(buf, header...)
		buf = append(buf, encoded...)
		buf = append(buf, 0, 0) // UTF-16 terminator
	}

	return buf
}

func TestParseLogOpenAttributeTable(t *testing.T) {
	dump := buildOpenAttributeTableDump(
		buildOpenAttributeEntry(true, AttrTypeData, 0, 1, 2114056),
		buildOpenAttributeEntry(true, AttrTypeIndexAllocation, 5, 5, 2114200),
		buildOpenAttributeEntry(false, AttrTypeBitmap, 6, 6, 0),
	)

	entries, err := ParseLogOpenAttributeTable(dump)
	if err != nil {
		t.Fatalf("ParseLogOpenAttributeTable failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	first := entries[0]
	if !first.InUse {
		t.Error("expected the first entry in use")
	}
	if first.AttributeType != AttrTypeData || first.AttributeTypeName() != "$DATA" {
		t.Errorf("expected $DATA, got %s", first.AttributeTypeName())
	}
	if first.RecordNumber != 0 || first.SequenceNumber != 1 {
		t.Errorf("expected entry 0 sequence 1, got %d/%d",
			first.RecordNumber, first.SequenceNumber)
	}
	if first.OpenLSN != 2114056 {
		t.Errorf("expected open LSN 2114056, got %d", first.OpenLSN)
	}

	// Offset is what a record's TargetAttribute carries, so it must be the byte
	// position of the entry within the dump rather than its index.
	if first.Offset != logRestartTableHeaderSize {
		t.Errorf("expected offset %d, got %d", logRestartTableHeaderSize, first.Offset)
	}
	if entries[1].Offset != logRestartTableHeaderSize+0x28 {
		t.Errorf("expected offset %d, got %d", logRestartTableHeaderSize+0x28, entries[1].Offset)
	}

	if entries[1].RecordNumber != 5 || entries[1].AttributeType != AttrTypeIndexAllocation {
		t.Errorf("unexpected second entry: %+v", entries[1])
	}

	// A free slot is reported rather than dropped, since a historic record still
	// refers to what was there.
	if entries[2].InUse {
		t.Error("expected the third entry to be on the free list")
	}
}

func TestParseLogOpenAttributeTableRejectsShortEntries(t *testing.T) {
	// A table whose entries are too small to hold the fields libntfs reads.
	buf := make([]byte, logRestartTableHeaderSize+8)
	WriteUint16LE(buf, 0x00, 8) // Entry size
	WriteUint16LE(buf, 0x02, 1)
	WriteUint16LE(buf, 0x04, 1)

	if _, err := ParseLogOpenAttributeTable(buf); !errors.Is(err, ErrInvalidLogRecord) {
		t.Errorf("expected ErrInvalidLogRecord, got %v", err)
	}
}

// TestParseLogAttributeNames covers the layout that is not a restart table: a
// bare run of index, length, UTF-16 name, terminator.
func TestParseLogAttributeNames(t *testing.T) {
	want := map[uint32]string{
		0x0068: "$I30",
		0x0130: "$Q",
		0x0180: "$SDS",
		0x01A8: "$SII",
	}

	dump := buildAttributeNamesDump(want, 0x0068, 0x0130, 0x0180, 0x01A8)

	got, err := ParseLogAttributeNames(dump)
	if err != nil {
		t.Fatalf("ParseLogAttributeNames failed: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d names, got %d: %v", len(want), len(got), got)
	}
	for offset, name := range want {
		if got[offset] != name {
			t.Errorf("offset 0x%04X: expected %q, got %q", offset, name, got[offset])
		}
	}
}

func TestParseLogAttributeNamesStopsAtPadding(t *testing.T) {
	dump := buildAttributeNamesDump(map[uint32]string{0x68: "$I30"}, 0x68)
	dump = append(dump, make([]byte, 16)...)

	names, err := ParseLogAttributeNames(dump)
	if err != nil {
		t.Fatalf("ParseLogAttributeNames failed: %v", err)
	}
	if len(names) != 1 {
		t.Errorf("expected 1 name, got %d: %v", len(names), names)
	}
}

func TestParseLogAttributeNamesRejectsBadLength(t *testing.T) {
	dump := buildAttributeNamesDump(map[uint32]string{0x68: "$I30"}, 0x68)
	WriteUint16LE(dump, 2, 9) // Odd length cannot be UTF-16

	if _, err := ParseLogAttributeNames(dump); !errors.Is(err, ErrInvalidLogRecord) {
		t.Errorf("expected ErrInvalidLogRecord, got %v", err)
	}

	truncated := buildAttributeNamesDump(map[uint32]string{0x68: "$I30"}, 0x68)
	WriteUint16LE(truncated, 2, 64) // More than the buffer holds

	if _, err := ParseLogAttributeNames(truncated); !errors.Is(err, ErrInvalidLogRecord) {
		t.Errorf("expected ErrInvalidLogRecord for a truncated name, got %v", err)
	}
}

// TestLogRecordTableAccessorsSeparateTheTwoShapes covers the distinction that
// caused AttributeNamesDump to be read as a restart table: it is a table dump
// but not a restart table, so reading it as one decodes an entry size out of the
// first name's bytes.
func TestLogRecordTableAccessorsSeparateTheTwoShapes(t *testing.T) {
	names := buildAttributeNamesDump(map[uint32]string{0x68: "$I30"}, 0x68)

	raw := buildLogRecord(1, LogRecordTypeClient,
		buildNTFSLogData(LogOpAttributeNamesDump, LogOpNoop, 0, nil, names, nil))

	record, _, err := ParseLogRecord(raw)
	if err != nil {
		t.Fatalf("ParseLogRecord failed: %v", err)
	}

	if !record.IsTableDump() {
		t.Error("expected an AttributeNamesDump to be a table dump")
	}
	if record.IsRestartTableDump() {
		t.Error("expected an AttributeNamesDump not to be a restart table dump")
	}

	table, err := record.RestartTable()
	if err != nil {
		t.Fatalf("RestartTable failed: %v", err)
	}
	if table != nil {
		t.Errorf("expected no restart table decoded from names, got %+v", table)
	}

	decoded, err := record.AttributeNames()
	if err != nil {
		t.Fatalf("AttributeNames failed: %v", err)
	}
	if decoded[0x68] != "$I30" {
		t.Errorf("expected $I30 at 0x68, got %v", decoded)
	}
}

func TestLogRecordOpenAttributeTable(t *testing.T) {
	dump := buildOpenAttributeTableDump(
		buildOpenAttributeEntry(true, AttrTypeData, 0, 1, 100),
	)

	raw := buildLogRecord(1, LogRecordTypeClient,
		buildNTFSLogData(LogOpOpenAttributeTableDump, LogOpNoop, 0, nil, dump, nil))

	record, _, err := ParseLogRecord(raw)
	if err != nil {
		t.Fatalf("ParseLogRecord failed: %v", err)
	}

	entries, err := record.OpenAttributeTable()
	if err != nil {
		t.Fatalf("OpenAttributeTable failed: %v", err)
	}
	if len(entries) != 1 || entries[0].RecordNumber != 0 {
		t.Fatalf("unexpected entries: %+v", entries)
	}

	// An ordinary operation carries no table.
	plain := buildLogRecord(2, LogRecordTypeClient,
		buildNTFSLogData(LogOpUpdateResidentValue, LogOpNoop, 0, nil, []byte("x"), nil))
	other, _, err := ParseLogRecord(plain)
	if err != nil {
		t.Fatalf("ParseLogRecord failed: %v", err)
	}
	if got, err := other.OpenAttributeTable(); err != nil || got != nil {
		t.Errorf("expected no table on an ordinary record, got %+v (%v)", got, err)
	}
}

// TestLogAttributeTableResolves covers the whole point of the table: turning a
// record's TargetAttribute from a number into a file and a stream.
func TestLogAttributeTableResolves(t *testing.T) {
	dump := buildOpenAttributeTableDump(
		buildOpenAttributeEntry(true, AttrTypeData, 0, 1, 100),            // 0x18
		buildOpenAttributeEntry(true, AttrTypeIndexAllocation, 5, 5, 200), // 0x40
	)

	entries, err := ParseLogOpenAttributeTable(dump)
	if err != nil {
		t.Fatalf("ParseLogOpenAttributeTable failed: %v", err)
	}

	table := &LogAttributeTable{byOffset: make(map[uint32]LogOpenAttributeEntry)}
	for _, entry := range entries {
		table.byOffset[entry.Offset] = entry
	}

	// Merge a name in, the way LogAttributeTable does.
	if entry, ok := table.byOffset[0x40]; ok {
		entry.Name = "$I30"
		table.byOffset[0x40] = entry
	}

	if table.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", table.Len())
	}

	record := &LogRecord{
		RecordType:      LogRecordTypeClient,
		TargetAttribute: 0x18,
	}

	resolved, ok := table.Resolve(record)
	if !ok {
		t.Fatal("expected TargetAttribute 0x18 to resolve")
	}
	if resolved.RecordNumber != 0 || resolved.AttributeTypeName() != "$DATA" {
		t.Errorf("expected $MFT's $DATA, got %+v", resolved)
	}
	if resolved.String() != "entry 0:$DATA" {
		t.Errorf("unexpected String() %q", resolved.String())
	}

	named, ok := table.Lookup(0x40)
	if !ok {
		t.Fatal("expected TargetAttribute 0x40 to resolve")
	}
	if named.Name != "$I30" || named.String() != "entry 5:$INDEX_ALLOCATION:$I30" {
		t.Errorf("unexpected named entry: %+v (%s)", named, named.String())
	}

	// An offset the log's window does not cover reports false rather than an
	// arbitrary entry.
	if _, ok := table.Lookup(0x9999); ok {
		t.Error("expected an uncovered offset not to resolve")
	}

	// Entries come back in table order.
	ordered := table.Entries()
	if len(ordered) != 2 || ordered[0].Offset > ordered[1].Offset {
		t.Errorf("expected entries ordered by offset, got %+v", ordered)
	}
}

func TestLogAttributeTableNilAndRestartRecords(t *testing.T) {
	var table *LogAttributeTable

	if table.Len() != 0 || table.Entries() != nil {
		t.Error("expected a nil table to report nothing")
	}
	if _, ok := table.Lookup(0x18); ok {
		t.Error("expected a nil table to resolve nothing")
	}
	if _, ok := table.Resolve(&LogRecord{RecordType: LogRecordTypeClient}); ok {
		t.Error("expected a nil table to resolve nothing")
	}

	real := &LogAttributeTable{byOffset: map[uint32]LogOpenAttributeEntry{0x18: {}}}
	if _, ok := real.Resolve(&LogRecord{RecordType: LogRecordTypeRestart}); ok {
		t.Error("expected a restart record to name no attribute")
	}
	if _, ok := real.Resolve(nil); ok {
		t.Error("expected a nil record to name no attribute")
	}
}

func TestVolumeLogAttributeTable(t *testing.T) {
	dump := buildOpenAttributeTableDump(
		buildOpenAttributeEntry(true, AttrTypeData, 0, 1, 100),
		buildOpenAttributeEntry(true, AttrTypeIndexAllocation, 5, 5, 200),
	)
	names := buildAttributeNamesDump(map[uint32]string{0x40: "$I30"}, 0x40)

	records := append(
		buildLogRecord(0x100, LogRecordTypeClient,
			buildNTFSLogData(LogOpOpenAttributeTableDump, LogOpNoop, 0, nil, dump, nil)),
		buildLogRecord(0x101, LogRecordTypeClient,
			buildNTFSLogData(LogOpAttributeNamesDump, LogOpNoop, 0, nil, names, nil))...,
	)

	vol := newLogTestVolume(t, buildLogFileImage(t, records))

	table, err := vol.LogAttributeTable()
	if err != nil {
		t.Fatalf("LogAttributeTable failed: %v", err)
	}

	if table.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", table.Len())
	}

	entry, ok := table.Lookup(0x18)
	if !ok || entry.RecordNumber != 0 || entry.AttributeType != AttrTypeData {
		t.Errorf("expected $MFT's $DATA at 0x18, got %+v (ok=%v)", entry, ok)
	}

	// The name from the separate dump must be merged onto the table entry.
	named, ok := table.Lookup(0x40)
	if !ok || named.Name != "$I30" {
		t.Errorf("expected the $I30 name merged in, got %+v (ok=%v)", named, ok)
	}
}

func TestVolumeLogAttributeTableClosedVolume(t *testing.T) {
	vol := newLogTestVolume(t, buildLogFileImage(t,
		buildLogRecord(1, LogRecordTypeClient,
			buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil))))

	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := vol.LogAttributeTable(); err != ErrVolumeClosed {
		t.Errorf("expected ErrVolumeClosed, got %v", err)
	}
}
