package libntfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

const (
	testLogPageSize  = 4096
	testLogDataStart = 0x40 // Past the record page header and its update sequence
)

// stampLogPageFixups stamps a log page's update sequence the way NTFS does:
// stash the real last two bytes of every sector in the array, then write the
// sequence number in their place.
func stampLogPageFixups(page []byte, usaOffset int) {
	const usn = 0x0C0D

	usaSize := len(page)/UpdateSequenceStride + 1

	WriteUint16LE(page, 4, uint16(usaOffset))
	WriteUint16LE(page, 6, uint16(usaSize))
	WriteUint16LE(page, usaOffset, usn)

	for i := 0; i < usaSize-1; i++ {
		sectorEnd := (i+1)*UpdateSequenceStride - 2
		WriteUint16LE(page, usaOffset+2+i*2, ReadUint16LE(page, sectorEnd))
		WriteUint16LE(page, sectorEnd, usn)
	}
}

// logRestartOpts describes a restart page fixture.
type logRestartOpts struct {
	magic      uint32 // 0 means LogRestartPageMagic
	currentLSN uint64
	chkdskLSN  uint64
	fileSize   uint64
	openCount  uint32
	clientName string
}

// buildLogRestartPage assembles a restart page with a single client entry.
func buildLogRestartPage(opts logRestartOpts) []byte {
	page := make([]byte, testLogPageSize)

	magic := opts.magic
	if magic == 0 {
		magic = LogRestartPageMagic
	}

	// The update sequence array sits at 0x1E and runs to 0x30 for a 4 KiB page,
	// so the restart area starts immediately after it.
	const restartOffset = 0x30

	WriteUint32LE(page, 0, magic)
	WriteUint64LE(page, 0x08, opts.chkdskLSN)
	WriteUint32LE(page, 0x10, testLogPageSize) // System page size
	WriteUint32LE(page, 0x14, testLogPageSize) // Log page size
	WriteUint16LE(page, 0x18, restartOffset)
	WriteUint16LE(page, 0x1A, 1) // Minor version
	WriteUint16LE(page, 0x1C, 1) // Major version

	area := page[restartOffset:]
	const clientArrayOffset = logRestartAreaSize

	WriteUint64LE(area, 0x00, opts.currentLSN)
	WriteUint16LE(area, 0x08, 1) // Log clients
	WriteUint16LE(area, 0x0E, 0) // Flags
	WriteUint32LE(area, 0x10, 45)
	WriteUint16LE(area, 0x14, logRestartAreaSize)
	WriteUint16LE(area, 0x16, clientArrayOffset)
	WriteUint64LE(area, 0x18, opts.fileSize)
	WriteUint32LE(area, 0x20, 64)
	WriteUint16LE(area, 0x24, LogRecordHeaderSize)
	WriteUint16LE(area, 0x26, testLogDataStart)
	WriteUint32LE(area, 0x28, opts.openCount)

	client := area[clientArrayOffset:]
	WriteUint64LE(client, 0x00, 100) // Oldest LSN
	WriteUint64LE(client, 0x08, 200) // Client restart LSN
	WriteUint16LE(client, 0x14, 7)   // Sequence number

	nameBytes := utf16LEFromString(opts.clientName)
	WriteUint32LE(client, 0x1C, uint32(len(nameBytes)))
	copy(client[0x20:], nameBytes)

	stampLogPageFixups(page, logRestartPageHeaderSize)

	return page
}

// buildLogRecordPage assembles one "RCRD" page carrying the supplied record
// bytes, which must fit in the page's data area.
func buildLogRecordPage(t testing.TB, lastLSN uint64, data []byte) []byte {
	t.Helper()

	if testLogDataStart+len(data) > testLogPageSize {
		t.Fatalf("record data of %d bytes does not fit in a page", len(data))
	}

	page := make([]byte, testLogPageSize)

	WriteUint32LE(page, 0, LogRecordPageMagic)
	WriteUint64LE(page, 0x08, lastLSN)
	WriteUint32LE(page, 0x10, 0) // Flags
	WriteUint16LE(page, 0x14, 1) // Page count
	WriteUint16LE(page, 0x16, 1) // Page position
	WriteUint16LE(page, 0x18, uint16(testLogDataStart+len(data)))
	WriteUint64LE(page, 0x20, lastLSN)

	copy(page[testLogDataStart:], data)

	stampLogPageFixups(page, logRecordPageHeaderSize)

	return page
}

// buildNTFSLogData assembles the NTFS client half of a log record.
func buildNTFSLogData(redoOp, undoOp uint16, targetVCN int64,
	lcns []uint64, redo, undo []byte,
) []byte {
	redoOffset := logNTFSRecordHeaderSize + len(lcns)*8
	undoOffset := redoOffset + len(redo)

	data := make([]byte, undoOffset+len(undo))

	WriteUint16LE(data, 0x00, redoOp)
	WriteUint16LE(data, 0x02, undoOp)
	WriteUint16LE(data, 0x04, uint16(redoOffset))
	WriteUint16LE(data, 0x06, uint16(len(redo)))
	WriteUint16LE(data, 0x08, uint16(undoOffset))
	WriteUint16LE(data, 0x0A, uint16(len(undo)))
	WriteUint16LE(data, 0x0C, 0x18) // Target attribute
	WriteUint16LE(data, 0x0E, uint16(len(lcns)))
	WriteUint16LE(data, 0x10, 0x38) // Record offset
	WriteUint16LE(data, 0x12, 0x30) // Attribute offset
	WriteUint16LE(data, 0x14, 2)    // MFT cluster index
	WriteUint64LE(data, 0x18, uint64(targetVCN))

	for i, lcn := range lcns {
		WriteUint64LE(data, logNTFSRecordHeaderSize+i*8, lcn)
	}

	copy(data[redoOffset:], redo)
	copy(data[undoOffset:], undo)

	return data
}

// buildLogRecord wraps client data in a log-file-service record header.
func buildLogRecord(lsn uint64, recordType uint32, clientData []byte) []byte {
	total := AlignUp(LogRecordHeaderSize+len(clientData), logAlignment)
	record := make([]byte, total)

	WriteUint64LE(record, 0x00, lsn)
	WriteUint64LE(record, 0x08, lsn-1) // Client previous LSN
	WriteUint64LE(record, 0x10, lsn-2) // Client undo next LSN
	WriteUint32LE(record, 0x18, uint32(len(clientData)))
	WriteUint16LE(record, 0x1C, 3) // Client sequence
	WriteUint16LE(record, 0x1E, 0) // Client index
	WriteUint32LE(record, 0x20, recordType)
	WriteUint32LE(record, 0x24, 0x2A) // Transaction ID
	WriteUint16LE(record, 0x28, 0)    // Flags

	copy(record[LogRecordHeaderSize:], clientData)

	return record
}

// buildLogFileImage assembles a whole $LogFile: two restart pages followed by
// record pages carrying the supplied record stream, chopped across pages exactly
// as NTFS does.
func buildLogFileImage(t testing.TB, records []byte) []byte {
	t.Helper()

	restart := buildLogRestartPage(logRestartOpts{
		currentLSN: 0x5000,
		fileSize:   1 << 20,
		openCount:  3,
		clientName: "NTFS",
	})

	image := make([]byte, 0, 2*testLogPageSize+len(records)+testLogPageSize)
	image = append(image, restart...)
	image = append(image, restart...)

	perPage := testLogPageSize - testLogDataStart
	lsn := uint64(0x1000)

	for offset := 0; offset < len(records); offset += perPage {
		end := min(offset+perPage, len(records))
		image = append(image, buildLogRecordPage(t, lsn, records[offset:end])...)
		lsn++
	}

	return image
}

func TestParseLogRestartPage(t *testing.T) {
	page := buildLogRestartPage(logRestartOpts{
		currentLSN: 0x1234_5678,
		chkdskLSN:  0x99,
		fileSize:   64 << 20,
		openCount:  12,
		clientName: "NTFS",
	})

	restart, err := ParseLogRestartPage(page)
	if err != nil {
		t.Fatalf("ParseLogRestartPage failed: %v", err)
	}

	if restart.CurrentLSN != 0x1234_5678 {
		t.Errorf("expected CurrentLSN 0x12345678, got 0x%X", restart.CurrentLSN)
	}
	if restart.ChkdskLSN != 0x99 {
		t.Errorf("expected ChkdskLSN 0x99, got 0x%X", restart.ChkdskLSN)
	}
	if restart.LogPageSize != testLogPageSize || restart.SystemPageSize != testLogPageSize {
		t.Errorf("unexpected page sizes: %+v", restart)
	}
	if restart.FileSize != 64<<20 {
		t.Errorf("expected FileSize %d, got %d", 64<<20, restart.FileSize)
	}
	if restart.RestartOpenCount != 12 {
		t.Errorf("expected RestartOpenCount 12, got %d", restart.RestartOpenCount)
	}
	if restart.LogPageDataOffset != testLogDataStart {
		t.Errorf("expected LogPageDataOffset 0x%X, got 0x%X",
			testLogDataStart, restart.LogPageDataOffset)
	}

	if len(restart.Clients) != 1 {
		t.Fatalf("expected 1 client, got %d: %+v", len(restart.Clients), restart.Clients)
	}
	if restart.Clients[0].Name != "NTFS" {
		t.Errorf("expected client name NTFS, got %q", restart.Clients[0].Name)
	}
	if restart.Clients[0].OldestLSN != 100 || restart.Clients[0].ClientRestartLSN != 200 {
		t.Errorf("unexpected client LSNs: %+v", restart.Clients[0])
	}
}

// TestParseLogRestartPageAcceptsChkdskMagic covers a restart page chkdsk has
// rewritten, whose contents are still a restart area.
func TestParseLogRestartPageAcceptsChkdskMagic(t *testing.T) {
	page := buildLogRestartPage(logRestartOpts{
		magic:      LogChkdskPageMagic,
		currentLSN: 7,
		clientName: "NTFS",
	})

	restart, err := ParseLogRestartPage(page)
	if err != nil {
		t.Fatalf("ParseLogRestartPage failed: %v", err)
	}
	if restart.CurrentLSN != 7 {
		t.Errorf("expected CurrentLSN 7, got %d", restart.CurrentLSN)
	}
}

func TestParseLogRestartPageRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		page []byte
	}{
		{name: "short", page: make([]byte, 8)},
		{name: "wrong magic", page: buildLogRestartPage(logRestartOpts{magic: 0xDEADBEEF})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseLogRestartPage(tt.page); !errors.Is(err, ErrInvalidLogRecord) {
				t.Errorf("expected ErrInvalidLogRecord, got %v", err)
			}
		})
	}
}

func TestParseLogRecord(t *testing.T) {
	redo := []byte("redo payload")
	undo := []byte("undo payload")
	lcns := []uint64{0x20, 0x21, 0x40}

	clientData := buildNTFSLogData(LogOpUpdateResidentValue, LogOpUpdateResidentValue,
		0x1234, lcns, redo, undo)
	raw := buildLogRecord(0xABCD, LogRecordTypeClient, clientData)

	record, consumed, err := ParseLogRecord(raw)
	if err != nil {
		t.Fatalf("ParseLogRecord failed: %v", err)
	}

	if consumed != len(raw) {
		t.Errorf("expected to consume %d bytes, consumed %d", len(raw), consumed)
	}

	if record.LSN != 0xABCD {
		t.Errorf("expected LSN 0xABCD, got 0x%X", record.LSN)
	}
	if !record.IsClientRecord() {
		t.Error("expected a client record")
	}
	if record.TransactionID != 0x2A {
		t.Errorf("expected transaction 0x2A, got 0x%X", record.TransactionID)
	}
	if record.RedoOperationName() != "UpdateResidentValue" {
		t.Errorf("unexpected redo operation %q", record.RedoOperationName())
	}
	if record.TargetVCN != 0x1234 {
		t.Errorf("expected TargetVCN 0x1234, got 0x%X", record.TargetVCN)
	}
	if record.MFTClusterIndex != 2 {
		t.Errorf("expected MFTClusterIndex 2, got %d", record.MFTClusterIndex)
	}

	if !bytes.Equal(record.RedoData, redo) {
		t.Errorf("expected redo data %q, got %q", redo, record.RedoData)
	}
	if !bytes.Equal(record.UndoData, undo) {
		t.Errorf("expected undo data %q, got %q", undo, record.UndoData)
	}

	if len(record.LCNs) != len(lcns) {
		t.Fatalf("expected %d LCNs, got %d: %v", len(lcns), len(record.LCNs), record.LCNs)
	}
	for i, lcn := range lcns {
		if record.LCNs[i] != lcn {
			t.Errorf("LCN %d: expected 0x%X, got 0x%X", i, lcn, record.LCNs[i])
		}
	}
}

// TestParseLogRecordRestartCarriesNoOperation covers a client restart record,
// which has no NTFS half to decode.
func TestParseLogRecordRestartCarriesNoOperation(t *testing.T) {
	raw := buildLogRecord(9, LogRecordTypeRestart, make([]byte, 64))

	record, _, err := ParseLogRecord(raw)
	if err != nil {
		t.Fatalf("ParseLogRecord failed: %v", err)
	}

	if record.IsClientRecord() {
		t.Error("expected a restart record")
	}
	if record.RedoOperation != 0 || record.RedoData != nil || record.LCNs != nil {
		t.Errorf("expected no NTFS fields decoded, got %+v", record)
	}
	if record.String() != "LSN 9 restart" {
		t.Errorf("unexpected String() %q", record.String())
	}
}

func TestParseLogRecordRejectsBadInput(t *testing.T) {
	oversize := buildLogRecord(1, LogRecordTypeClient, nil)
	WriteUint32LE(oversize, 0x18, LogMaxRecordDataLength+1)

	badType := buildLogRecord(1, 99, nil)

	tests := []struct {
		name   string
		record []byte
	}{
		{name: "oversize client data", record: oversize},
		{name: "unknown record type", record: badType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ParseLogRecord(tt.record); !errors.Is(err, ErrInvalidLogRecord) {
				t.Errorf("expected ErrInvalidLogRecord, got %v", err)
			}
		})
	}
}

// TestParseLogRecordReportsTruncation covers the signal a page-spanning record
// depends on: a record whose data runs past the buffer must say how much it
// needed rather than returning a short record.
func TestParseLogRecordReportsTruncation(t *testing.T) {
	raw := buildLogRecord(1, LogRecordTypeClient, make([]byte, 256))
	truncated := raw[:len(raw)-64]

	_, needed, err := ParseLogRecord(truncated)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if needed != len(raw) {
		t.Errorf("expected the full length %d reported, got %d", len(raw), needed)
	}

	if _, _, err := ParseLogRecord(raw[:8]); err != io.ErrUnexpectedEOF {
		t.Errorf("expected io.ErrUnexpectedEOF for a partial header, got %v", err)
	}
}

func TestEachLogRecordWalksPages(t *testing.T) {
	first := buildLogRecord(0x100, LogRecordTypeClient,
		buildNTFSLogData(LogOpCreateAttribute, LogOpDeleteAttribute, 1, nil, []byte("a"), nil))
	second := buildLogRecord(0x200, LogRecordTypeClient,
		buildNTFSLogData(LogOpAddIndexEntryRoot, LogOpDeleteIndexEntryRoot, 2, nil, []byte("b"), nil))

	image := buildLogFileImage(t, append(append([]byte{}, first...), second...))

	var seen []*LogRecord
	if err := EachLogRecord(image, func(r *LogRecord) error {
		seen = append(seen, r)
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 records, got %d", len(seen))
	}
	if seen[0].LSN != 0x100 || seen[1].LSN != 0x200 {
		t.Errorf("unexpected LSNs: 0x%X, 0x%X", seen[0].LSN, seen[1].LSN)
	}
	if seen[0].RedoOperationName() != "CreateAttribute" {
		t.Errorf("unexpected redo operation %q", seen[0].RedoOperationName())
	}
	if seen[1].UndoOperationName() != "DeleteIndexEntryRoot" {
		t.Errorf("unexpected undo operation %q", seen[1].UndoOperationName())
	}
}

// TestEachLogRecordSpansPageBoundary is the case page-at-a-time parsing gets
// wrong: a record that starts near the end of one page and finishes in the next.
// The log's data area is one logical byte stream chopped into pages, each with
// its own header and update sequence, so the record must be reassembled across
// the two rather than reported as truncated or skipped.
func TestEachLogRecordSpansPageBoundary(t *testing.T) {
	perPage := testLogPageSize - testLogDataStart

	// Client data large enough that the record cannot fit in one page's data
	// area, forcing a split.
	redo := bytes.Repeat([]byte{0xAB}, perPage)
	clientData := buildNTFSLogData(LogOpUpdateNonResidentValue, LogOpNoop, 0x77, nil, redo, nil)
	raw := buildLogRecord(0x777, LogRecordTypeClient, clientData)

	if len(raw) <= perPage {
		t.Fatalf("fixture does not span a page: record is %d bytes, page holds %d",
			len(raw), perPage)
	}

	image := buildLogFileImage(t, raw)

	var seen []*LogRecord
	if err := EachLogRecord(image, func(r *LogRecord) error {
		seen = append(seen, r)
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("expected the spanning record reassembled into 1 record, got %d", len(seen))
	}
	if seen[0].LSN != 0x777 {
		t.Errorf("expected LSN 0x777, got 0x%X", seen[0].LSN)
	}
	if !bytes.Equal(seen[0].RedoData, redo) {
		t.Errorf("expected %d bytes of redo data reassembled, got %d",
			len(redo), len(seen[0].RedoData))
	}
}

// TestEachLogRecordSkipsDamagedPage covers a corrupt page between two good ones,
// which must not hide the records that follow it.
func TestEachLogRecordSkipsDamagedPage(t *testing.T) {
	record := buildLogRecord(0x300, LogRecordTypeClient,
		buildNTFSLogData(LogOpCommitTransaction, LogOpNoop, 0, nil, nil, nil))

	restart := buildLogRestartPage(logRestartOpts{clientName: "NTFS"})
	damaged := buildLogRecordPage(t, 1, record)
	WriteUint32LE(damaged, 0, 0xFFFFFFFF) // Uninitialised page

	var image []byte
	image = append(image, restart...)
	image = append(image, restart...)
	image = append(image, damaged...)
	image = append(image, buildLogRecordPage(t, 2, record)...)

	count := 0
	if err := EachLogRecord(image, func(*LogRecord) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected the readable page's record found, got %d records", count)
	}
}

// TestEachLogRecordStopsOnCallbackError covers a caller that has found what it
// wants and does not want the rest walked.
func TestEachLogRecordStopsOnCallbackError(t *testing.T) {
	record := buildLogRecord(0x400, LogRecordTypeClient,
		buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil))

	image := buildLogFileImage(t, append(append([]byte{}, record...), record...))

	stop := errors.New("stop")
	seen := 0

	err := EachLogRecord(image, func(*LogRecord) error {
		seen++
		return stop
	})

	if !errors.Is(err, stop) {
		t.Errorf("expected the caller's error returned, got %v", err)
	}
	if seen != 1 {
		t.Errorf("expected the walk to stop after 1 record, saw %d", seen)
	}
}

func TestEachLogRecordEmpty(t *testing.T) {
	if err := EachLogRecord(nil, func(*LogRecord) error {
		t.Fatal("expected no records from an empty buffer")
		return nil
	}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestLogOperationName(t *testing.T) {
	if got := LogOperationName(LogOpForgetTransaction); got != "ForgetTransaction" {
		t.Errorf("expected ForgetTransaction, got %q", got)
	}
	if got := LogOperationName(0x9999); got != "0x9999" {
		t.Errorf("expected the raw value rendered, got %q", got)
	}
}

// newLogTestVolume builds an in-memory volume whose $LogFile at MFT entry 2 is
// backed by the supplied image.
func newLogTestVolume(t testing.TB, logImage []byte) *Volume {
	t.Helper()

	const (
		clusterSize = 4096
		recordSize  = DefaultMFTRecordSize
		mftCluster  = 2
		mftClusters = 4
		logCluster  = 8
	)

	clusters := (len(logImage) + clusterSize - 1) / clusterSize
	disk := make([]byte, (logCluster+clusters+1)*clusterSize)

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "$LogFile",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedNonResidentDataAttr(t, 2, "", logCluster, uint64(clusters),
				clusterSize, uint64(len(logImage))),
		},
	})

	copy(disk[mftCluster*clusterSize+MFTEntryLogFile*recordSize:], record)
	copy(disk[logCluster*clusterSize:], logImage)

	return &Volume{
		reader:          &mockReaderAt{data: disk},
		bytesPerCluster: clusterSize,
		mftRecordSize:   recordSize,
		mftDataRuns:     []DataRun{{StartCluster: mftCluster, LengthClusters: mftClusters}},
		mftCache:        make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
	}
}

func TestVolumeLogRestartArea(t *testing.T) {
	record := buildLogRecord(0x500, LogRecordTypeClient,
		buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil))
	vol := newLogTestVolume(t, buildLogFileImage(t, record))

	restart, err := vol.LogRestartArea()
	if err != nil {
		t.Fatalf("LogRestartArea failed: %v", err)
	}

	if restart.CurrentLSN != 0x5000 {
		t.Errorf("expected CurrentLSN 0x5000, got 0x%X", restart.CurrentLSN)
	}
	if len(restart.Clients) != 1 || restart.Clients[0].Name != "NTFS" {
		t.Errorf("expected the NTFS client, got %+v", restart.Clients)
	}
}

// TestVolumeLogRestartAreaFallsBackToSecondCopy covers why NTFS writes two: the
// second must be used when the first cannot be read.
func TestVolumeLogRestartAreaFallsBackToSecondCopy(t *testing.T) {
	record := buildLogRecord(0x500, LogRecordTypeClient,
		buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil))

	image := buildLogFileImage(t, record)
	WriteUint32LE(image, 0, 0xFFFFFFFF) // Tear the first copy

	vol := newLogTestVolume(t, image)

	restart, err := vol.LogRestartArea()
	if err != nil {
		t.Fatalf("expected the second restart copy used, got %v", err)
	}
	if restart.CurrentLSN != 0x5000 {
		t.Errorf("expected CurrentLSN 0x5000, got 0x%X", restart.CurrentLSN)
	}
}

func TestVolumeEachLogRecord(t *testing.T) {
	first := buildLogRecord(0x600, LogRecordTypeClient,
		buildNTFSLogData(LogOpInitializeFileRecordSegment, LogOpNoop, 5, nil, []byte("x"), nil))
	second := buildLogRecord(0x601, LogRecordTypeClient,
		buildNTFSLogData(LogOpForgetTransaction, LogOpCompensationLogRecord, 6, nil, nil, nil))

	vol := newLogTestVolume(t, buildLogFileImage(t,
		append(append([]byte{}, first...), second...)))

	var seen []uint64
	if err := vol.EachLogRecord(func(r *LogRecord) error {
		seen = append(seen, r.LSN)
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if len(seen) != 2 || seen[0] != 0x600 || seen[1] != 0x601 {
		t.Fatalf("unexpected LSNs: %v", seen)
	}
}

func TestVolumeEachLogRecordClosedVolume(t *testing.T) {
	record := buildLogRecord(1, LogRecordTypeClient,
		buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil))
	vol := newLogTestVolume(t, buildLogFileImage(t, record))

	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := vol.EachLogRecord(func(*LogRecord) error { return nil }); err != ErrVolumeClosed {
		t.Errorf("expected ErrVolumeClosed, got %v", err)
	}
	if _, err := vol.LogRestartArea(); err != ErrVolumeClosed {
		t.Errorf("expected ErrVolumeClosed, got %v", err)
	}
}
