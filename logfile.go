package libntfs

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// $LogFile page magics and layout constants.
const (
	// LogRestartPageMagic marks a restart page, "RSTR".
	LogRestartPageMagic = 0x52545352
	// LogRecordPageMagic marks a log record page, "RCRD".
	LogRecordPageMagic = 0x44524352
	// LogChkdskPageMagic marks a page chkdsk has rewritten, "CHKD".
	LogChkdskPageMagic = 0x444B4843

	// DefaultLogPageSize is the page size $LogFile uses when the restart area
	// cannot be read to confirm it.
	DefaultLogPageSize = 4096

	// logRestartPageHeaderSize is the fixed header before a restart page's
	// update sequence array.
	logRestartPageHeaderSize = 0x1E
	// logRestartAreaSize is the restart area proper, found at RestartOffset.
	logRestartAreaSize = 0x30
	// logClientRecordSize is one entry of the restart area's client array.
	logClientRecordSize = 0x60
	// logClientNameMaxBytes bounds the UTF-16 client name inside that entry.
	logClientNameMaxBytes = 64

	// logRecordPageHeaderSize is the fixed header before a record page's update
	// sequence array.
	logRecordPageHeaderSize = 0x28

	// LogRecordHeaderSize is the log-file-service header every record starts
	// with, before the client's own data.
	LogRecordHeaderSize = 0x30
	// logNTFSRecordHeaderSize is the fixed part of NTFS's client data, before
	// the LCN list.
	logNTFSRecordHeaderSize = 0x20

	// LogMaxRecordDataLength bounds one record's client data, guarding against a
	// corrupt length field sending the walk off into nonsense.
	LogMaxRecordDataLength = 1 << 20

	// logMaxClientIndex bounds a record's client index.
	//
	// The log file service addresses its clients through a small array in the
	// restart area, and NTFS is the only client of its own log, so a real record
	// carries index 0. This is the loose bound applied when the restart area is
	// not to hand; the walkers apply the exact client count instead.
	//
	// It matters more than it looks. Resynchronising after damage means testing
	// arbitrary bytes as a record header, and record type plus data length alone
	// let roughly one in sixty pass: on a real 5 MB log that produced ~489 false
	// records out of 32,594, with client indices in the thousands.
	logMaxClientIndex = 64

	// logAlignment is the boundary records are laid out on.
	logAlignment = 8

	// logChunkPages is how many pages are read per iteration while streaming.
	logChunkPages = 256

	// maxLogPageSize bounds a page size read out of a restart area, so a corrupt
	// field cannot drive the allocation.
	maxLogPageSize = 1 << 20
)

// Log record types.
const (
	// LogRecordTypeClient is an ordinary client record carrying NTFS log data.
	LogRecordTypeClient = 1
	// LogRecordTypeRestart is a client restart record, which carries no NTFS
	// operation.
	LogRecordTypeRestart = 2
)

// LogRecordMultiPage marks a record whose data continues on the following page.
const LogRecordMultiPage = 0x0001

// NTFS log operations, used for both the redo and undo halves of a record.
const (
	LogOpNoop                         = 0x00
	LogOpCompensationLogRecord        = 0x01
	LogOpInitializeFileRecordSegment  = 0x02
	LogOpDeallocateFileRecordSegment  = 0x03
	LogOpWriteEndOfFileRecordSegment  = 0x04
	LogOpCreateAttribute              = 0x05
	LogOpDeleteAttribute              = 0x06
	LogOpUpdateResidentValue          = 0x07
	LogOpUpdateNonResidentValue       = 0x08
	LogOpUpdateMappingPairs           = 0x09
	LogOpDeleteDirtyClusters          = 0x0A
	LogOpSetNewAttributeSizes         = 0x0B
	LogOpAddIndexEntryRoot            = 0x0C
	LogOpDeleteIndexEntryRoot         = 0x0D
	LogOpAddIndexEntryAllocation      = 0x0E
	LogOpDeleteIndexEntryAllocation   = 0x0F
	LogOpWriteEndOfIndexBuffer        = 0x10
	LogOpSetIndexEntryVCNRoot         = 0x11
	LogOpSetIndexEntryVCNAllocation   = 0x12
	LogOpUpdateFileNameRoot           = 0x13
	LogOpUpdateFileNameAllocation     = 0x14
	LogOpSetBitsInNonResidentBitMap   = 0x15
	LogOpClearBitsInNonResidentBitMap = 0x16
	LogOpHotFix                       = 0x17
	LogOpEndTopLevelAction            = 0x18
	LogOpPrepareTransaction           = 0x19
	LogOpCommitTransaction            = 0x1A
	LogOpForgetTransaction            = 0x1B
	LogOpOpenNonResidentAttribute     = 0x1C
	LogOpOpenAttributeTableDump       = 0x1D
	LogOpAttributeNamesDump           = 0x1E
	LogOpDirtyPageTableDump           = 0x1F
	LogOpTransactionTableDump         = 0x20
	LogOpUpdateRecordDataRoot         = 0x21
	LogOpUpdateRecordDataAllocation   = 0x22
)

// Operations above LogOpUpdateRecordDataAllocation are not documented and are
// deliberately not named here.
//
// They do occur: a real Windows volume produced 376 records carrying operation
// 0x25, all with the same shape — one LCN, no redo or undo payload, and a target
// VCN that maps to that LCN by a constant offset, which is the $MFT's own
// allocation. Enough to see it tracks a single cluster's mapping; not enough to
// name it. LogOperationName reports the hex value rather than inventing one,
// since a wrong name in a forensic report is worse than a number.

// logOperationNames maps each operation to its name. Operations outside this
// range are reported as a hex value rather than silently dropped.
var logOperationNames = map[uint16]string{
	LogOpNoop:                         "Noop",
	LogOpCompensationLogRecord:        "CompensationLogRecord",
	LogOpInitializeFileRecordSegment:  "InitializeFileRecordSegment",
	LogOpDeallocateFileRecordSegment:  "DeallocateFileRecordSegment",
	LogOpWriteEndOfFileRecordSegment:  "WriteEndOfFileRecordSegment",
	LogOpCreateAttribute:              "CreateAttribute",
	LogOpDeleteAttribute:              "DeleteAttribute",
	LogOpUpdateResidentValue:          "UpdateResidentValue",
	LogOpUpdateNonResidentValue:       "UpdateNonResidentValue",
	LogOpUpdateMappingPairs:           "UpdateMappingPairs",
	LogOpDeleteDirtyClusters:          "DeleteDirtyClusters",
	LogOpSetNewAttributeSizes:         "SetNewAttributeSizes",
	LogOpAddIndexEntryRoot:            "AddIndexEntryRoot",
	LogOpDeleteIndexEntryRoot:         "DeleteIndexEntryRoot",
	LogOpAddIndexEntryAllocation:      "AddIndexEntryAllocation",
	LogOpDeleteIndexEntryAllocation:   "DeleteIndexEntryAllocation",
	LogOpWriteEndOfIndexBuffer:        "WriteEndOfIndexBuffer",
	LogOpSetIndexEntryVCNRoot:         "SetIndexEntryVcnRoot",
	LogOpSetIndexEntryVCNAllocation:   "SetIndexEntryVcnAllocation",
	LogOpUpdateFileNameRoot:           "UpdateFileNameRoot",
	LogOpUpdateFileNameAllocation:     "UpdateFileNameAllocation",
	LogOpSetBitsInNonResidentBitMap:   "SetBitsInNonResidentBitMap",
	LogOpClearBitsInNonResidentBitMap: "ClearBitsInNonResidentBitMap",
	LogOpHotFix:                       "HotFix",
	LogOpEndTopLevelAction:            "EndTopLevelAction",
	LogOpPrepareTransaction:           "PrepareTransaction",
	LogOpCommitTransaction:            "CommitTransaction",
	LogOpForgetTransaction:            "ForgetTransaction",
	LogOpOpenNonResidentAttribute:     "OpenNonResidentAttribute",
	LogOpOpenAttributeTableDump:       "OpenAttributeTableDump",
	LogOpAttributeNamesDump:           "AttributeNamesDump",
	LogOpDirtyPageTableDump:           "DirtyPageTableDump",
	LogOpTransactionTableDump:         "TransactionTableDump",
	LogOpUpdateRecordDataRoot:         "UpdateRecordDataRoot",
	LogOpUpdateRecordDataAllocation:   "UpdateRecordDataAllocation",
}

// LogOperationName returns a readable name for a log operation, or its hex value
// when the operation is not one libntfs recognises.
func LogOperationName(op uint16) string {
	if name, ok := logOperationNames[op]; ok {
		return name
	}
	return fmt.Sprintf("0x%04X", op)
}

// logRestartTableHeaderSize is the fixed header before a restart table's
// entries.
const logRestartTableHeaderSize = 0x18

// maxLogRestartTableEntries bounds how many entries are sliced out of a table,
// so a corrupt count cannot drive the allocation.
const maxLogRestartTableEntries = 1 << 16

// LogRestartTable is one of the tables NTFS periodically dumps into the log so a
// recovery pass can rebuild its state: the open attribute table, the dirty page
// table, and the transaction table.
//
// The table header is decoded. Entries are handed over as raw fixed-size records
// rather than decoded structures, because their layout depends on the NTFS
// version that wrote them and libntfs has no way to confirm which it is looking
// at. Reporting a wrongly-decoded MFT reference would be worse than reporting
// none: it would attribute an operation to the wrong file.
//
// EntrySize is what makes the raw entries usable — it says how to slice them,
// and a caller that knows its target version can decode them itself.
type LogRestartTable struct {
	EntrySize      uint16 // Bytes per entry
	EntryCount     uint16 // Entries in use
	AllocatedCount uint16 // Entries the table has room for
	FreeGoal       uint32
	FirstFree      uint32
	LastFree       uint32
	Entries        [][]byte // Raw entries, EntrySize bytes each
}

// ParseLogRestartTable decodes the table header carried in a dump record's redo
// data and slices out its entries.
func ParseLogRestartTable(buf []byte) (*LogRestartTable, error) {
	if len(buf) < logRestartTableHeaderSize {
		return nil, fmt.Errorf("%w: restart table is %d bytes, need %d for a header",
			ErrInvalidLogRecord, len(buf), logRestartTableHeaderSize)
	}

	table := &LogRestartTable{
		EntrySize:      ReadUint16LE(buf, 0x00),
		EntryCount:     ReadUint16LE(buf, 0x02),
		AllocatedCount: ReadUint16LE(buf, 0x04),
		FreeGoal:       ReadUint32LE(buf, 0x0C),
		FirstFree:      ReadUint32LE(buf, 0x10),
		LastFree:       ReadUint32LE(buf, 0x14),
	}

	if table.EntrySize == 0 {
		return nil, fmt.Errorf("%w: restart table declares a zero entry size",
			ErrInvalidLogRecord)
	}

	// Slice whatever the buffer actually holds rather than what the header
	// claims, so a truncated dump still yields the entries that are present.
	available := (len(buf) - logRestartTableHeaderSize) / int(table.EntrySize)
	count := int(table.AllocatedCount)
	if count > available {
		count = available
	}
	if count > maxLogRestartTableEntries {
		count = maxLogRestartTableEntries
	}

	table.Entries = make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		start := logRestartTableHeaderSize + i*int(table.EntrySize)
		entry := make([]byte, table.EntrySize)
		copy(entry, buf[start:start+int(table.EntrySize)])
		table.Entries = append(table.Entries, entry)
	}

	return table, nil
}

// IsTableDump reports whether the record carries one of the periodic dumps NTFS
// writes so a recovery pass can rebuild its state, rather than a file system
// operation.
func (r *LogRecord) IsTableDump() bool {
	if r == nil || !r.IsClientRecord() {
		return false
	}

	switch r.RedoOperation {
	case LogOpOpenAttributeTableDump, LogOpAttributeNamesDump,
		LogOpDirtyPageTableDump, LogOpTransactionTableDump:
		return true
	default:
		return false
	}
}

// IsRestartTableDump reports whether the record carries a restart table
// specifically.
//
// AttributeNamesDump is a table dump but not a restart table: its data is a bare
// run of names with no table header, so reading one as a restart table produces
// an entry size and count out of the first name's bytes. Use
// [ParseLogAttributeNames] for it.
func (r *LogRecord) IsRestartTableDump() bool {
	if r == nil || !r.IsClientRecord() {
		return false
	}

	switch r.RedoOperation {
	case LogOpOpenAttributeTableDump, LogOpDirtyPageTableDump,
		LogOpTransactionTableDump:
		return true
	default:
		return false
	}
}

// RestartTable decodes the restart table a dump record carries, or returns nil
// when the record carries none.
//
// An AttributeNamesDump returns nil rather than a table decoded out of its
// names; [ParseLogAttributeNames] reads that one.
func (r *LogRecord) RestartTable() (*LogRestartTable, error) {
	if !r.IsRestartTableDump() {
		return nil, nil
	}

	return ParseLogRestartTable(r.RedoData)
}

// AttributeNames decodes the attribute names an AttributeNamesDump record
// carries, keyed by their offset in the open attribute table, or returns nil
// when the record carries none.
func (r *LogRecord) AttributeNames() (map[uint32]string, error) {
	if r == nil || !r.IsClientRecord() || r.RedoOperation != LogOpAttributeNamesDump {
		return nil, nil
	}

	return ParseLogAttributeNames(r.RedoData)
}

// OpenAttributeTable decodes the open attribute table an OpenAttributeTableDump
// record carries, or returns nil when the record carries none.
func (r *LogRecord) OpenAttributeTable() ([]LogOpenAttributeEntry, error) {
	if r == nil || !r.IsClientRecord() || r.RedoOperation != LogOpOpenAttributeTableDump {
		return nil, nil
	}

	return ParseLogOpenAttributeTable(r.RedoData)
}

// LogTransaction groups the records belonging to one NTFS transaction.
//
// A single user-visible action — creating a file, renaming one — is several log
// records under one transaction ID, and reading them individually loses the fact
// that they belong together. Grouping restores it.
//
// Committed and Forgotten say how the transaction ended. In practice NTFS ends
// almost every transaction with ForgetTransaction rather than
// CommitTransaction, so Forgotten is the outcome to expect; a log holding tens
// of thousands of records routinely contains no CommitTransaction at all.
//
// Neither being set means the log's window does not contain the end of the
// transaction, which for the oldest entries in a circular log is the normal case
// rather than a sign of interrupted work.
type LogTransaction struct {
	ID        uint32
	FirstLSN  uint64
	LastLSN   uint64
	Records   []*LogRecord
	Committed bool // A CommitTransaction record was seen
	Forgotten bool // A ForgetTransaction record was seen
}

// Operations returns the distinct redo operation names in the transaction, in
// the order they first appear, which reads as a summary of what it did.
func (t LogTransaction) Operations() []string {
	seen := make(map[string]bool, len(t.Records))
	names := make([]string, 0, len(t.Records))

	for _, record := range t.Records {
		name := record.RedoOperationName()
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}

	return names
}

// GroupLogTransactions groups records into transactions, ordered by the LSN each
// began at.
//
// TransactionID is an index into NTFS's transaction table, not a unique
// identifier: the table is small and its slots are reused constantly, so the
// same ID reappears for thousands of unrelated transactions over the life of a
// log. Grouping on the ID alone therefore collapses a whole log into a handful
// of enormous pseudo-transactions — on a real volume, 32,000 records into five.
//
// A transaction is closed instead when its end record appears, and the next
// record carrying that ID starts a new one. That is what makes the grouping
// track actual transactions rather than table slots.
//
// Restart records carry no transaction and are skipped. Because the log is
// circular, a transaction near either end of the window will be missing records
// that have been overwritten; the grouping reports what is present rather than
// pretending otherwise.
func GroupLogTransactions(records []*LogRecord) []LogTransaction {
	var transactions []LogTransaction

	// Only transactions still awaiting their end record live here. Once one is
	// closed it moves to the result, freeing the slot for the next reuse of the
	// same ID.
	open := make(map[uint32]*LogTransaction)

	closeTransaction := func(id uint32) {
		transaction, ok := open[id]
		if !ok {
			return
		}
		transactions = append(transactions, *transaction)
		delete(open, id)
	}

	for _, record := range records {
		if record == nil || !record.IsClientRecord() {
			continue
		}

		transaction, ok := open[record.TransactionID]
		if !ok {
			transaction = &LogTransaction{
				ID:       record.TransactionID,
				FirstLSN: record.LSN,
				LastLSN:  record.LSN,
			}
			open[record.TransactionID] = transaction
		}

		transaction.Records = append(transaction.Records, record)

		if record.LSN < transaction.FirstLSN {
			transaction.FirstLSN = record.LSN
		}
		if record.LSN > transaction.LastLSN {
			transaction.LastLSN = record.LSN
		}

		switch record.RedoOperation {
		case LogOpCommitTransaction:
			transaction.Committed = true
			closeTransaction(record.TransactionID)
		case LogOpForgetTransaction:
			transaction.Forgotten = true
			closeTransaction(record.TransactionID)
		}
	}

	// Whatever is still open ran past the end of the window.
	for id := range open {
		transactions = append(transactions, *open[id])
	}

	sort.Slice(transactions, func(i, j int) bool {
		if transactions[i].FirstLSN != transactions[j].FirstLSN {
			return transactions[i].FirstLSN < transactions[j].FirstLSN
		}
		return transactions[i].ID < transactions[j].ID
	})

	return transactions
}

// LogClient is one entry of a restart area's client array. NTFS is the only
// client of its own log, so in practice there is exactly one, named "NTFS".
type LogClient struct {
	OldestLSN        uint64 // Oldest LSN this client still needs
	ClientRestartLSN uint64 // Where recovery would start for this client
	SequenceNumber   uint16
	Name             string
}

// LogRestartArea is the header $LogFile keeps at its front, describing the log's
// geometry and where recovery would begin.
//
// Two copies are written, in the first two pages, so the second survives a torn
// write to the first. CurrentLSN is the most useful field for a timeline: it
// bounds how recent the log's contents are.
type LogRestartArea struct {
	SystemPageSize    uint32 // Size of the restart pages themselves
	LogPageSize       uint32 // Size of the record pages that follow
	MajorVersion      int16
	MinorVersion      int16
	ChkdskLSN         uint64 // LSN at the last chkdsk, 0 if never run
	CurrentLSN        uint64 // Most recent LSN allocated
	LogClientCount    uint16
	Flags             uint16
	SeqNumberBits     uint32
	FileSize          uint64 // Usable size of $LogFile in bytes
	LastLSNDataLength uint32
	RecordLength      uint16
	LogPageDataOffset uint16 // Where record data starts within a record page
	RestartOpenCount  uint32 // Times the log has been opened, a rough mount count
	Clients           []LogClient
}

// LogRecord is one entry from $LogFile.
//
// The log-file-service header — LSN, the previous and undo-next links, the
// transaction, and the record type — is present on every record. The NTFS half,
// from RedoOperation onward, is present only when RecordType is
// LogRecordTypeClient; a restart record carries none of it and reports zeros.
//
// RedoData and UndoData are the operation's payload as NTFS wrote it: for an
// UpdateResidentValue that is the attribute bytes before and after the change.
// libntfs decodes where they are and hands them over; interpreting them belongs
// to the caller, since their meaning depends entirely on the operation.
type LogRecord struct {
	LSN               uint64
	ClientPreviousLSN uint64
	ClientUndoNextLSN uint64
	ClientDataLength  uint32
	ClientSequence    uint16
	ClientIndex       uint16
	RecordType        uint32
	TransactionID     uint32
	Flags             uint16

	RedoOperation   uint16
	UndoOperation   uint16
	TargetAttribute uint16 // Offset into the open attribute table
	MFTClusterIndex uint16
	RecordOffset    uint16
	AttributeOffset uint16
	TargetVCN       int64
	LCNs            []uint64 // Clusters the operation touches
	RedoData        []byte
	UndoData        []byte
}

// IsClientRecord reports whether the record carries an NTFS operation rather
// than being a client restart marker.
func (r *LogRecord) IsClientRecord() bool {
	return r != nil && r.RecordType == LogRecordTypeClient
}

// RedoOperationName returns a readable name for the redo half of the record.
func (r *LogRecord) RedoOperationName() string {
	if r == nil {
		return ""
	}
	return LogOperationName(r.RedoOperation)
}

// UndoOperationName returns a readable name for the undo half of the record.
func (r *LogRecord) UndoOperationName() string {
	if r == nil {
		return ""
	}
	return LogOperationName(r.UndoOperation)
}

// String renders the record as its LSN and the operation pair.
func (r *LogRecord) String() string {
	if r == nil {
		return ""
	}
	if !r.IsClientRecord() {
		return fmt.Sprintf("LSN %d restart", r.LSN)
	}
	return fmt.Sprintf("LSN %d %s/%s", r.LSN, r.RedoOperationName(), r.UndoOperationName())
}

// ParseLogRestartPage decodes a restart page, applying its update sequence.
//
// It needs no Volume, so it works on a $LogFile extracted with KAPE or FTK
// Imager as readily as on a mounted image. Pass the page bytes; the buffer is
// not modified.
//
// A page chkdsk has rewritten carries the magic "CHKD" rather than "RSTR" and is
// accepted, since its contents are still a restart area.
func ParseLogRestartPage(buf []byte) (*LogRestartArea, error) {
	if len(buf) < logRestartPageHeaderSize {
		return nil, fmt.Errorf("%w: restart page is %d bytes, need %d for a header",
			ErrInvalidLogRecord, len(buf), logRestartPageHeaderSize)
	}

	magic := ReadUint32LE(buf, 0)
	if magic != LogRestartPageMagic && magic != LogChkdskPageMagic {
		return nil, fmt.Errorf("%w: restart page has magic 0x%08X",
			ErrInvalidLogRecord, magic)
	}

	page, err := applyLogPageFixups(buf)
	if err != nil {
		return nil, err
	}

	restart := &LogRestartArea{
		ChkdskLSN:      ReadUint64LE(page, 0x08),
		SystemPageSize: ReadUint32LE(page, 0x10),
		LogPageSize:    ReadUint32LE(page, 0x14),
		MinorVersion:   int16(ReadUint16LE(page, 0x1A)),
		MajorVersion:   int16(ReadUint16LE(page, 0x1C)),
	}

	restartOffset := int(ReadUint16LE(page, 0x18))
	if restartOffset < logRestartPageHeaderSize ||
		restartOffset+logRestartAreaSize > len(page) {
		return nil, fmt.Errorf("%w: restart area at %d is outside the %d byte page",
			ErrInvalidLogRecord, restartOffset, len(page))
	}

	area := page[restartOffset:]
	restart.CurrentLSN = ReadUint64LE(area, 0x00)
	restart.LogClientCount = ReadUint16LE(area, 0x08)
	restart.Flags = ReadUint16LE(area, 0x0E)
	restart.SeqNumberBits = ReadUint32LE(area, 0x10)
	restart.FileSize = ReadUint64LE(area, 0x18)
	restart.LastLSNDataLength = ReadUint32LE(area, 0x20)
	restart.RecordLength = ReadUint16LE(area, 0x24)
	restart.LogPageDataOffset = ReadUint16LE(area, 0x26)
	restart.RestartOpenCount = ReadUint32LE(area, 0x28)

	clientArrayOffset := int(ReadUint16LE(area, 0x16))
	restart.Clients = parseLogClients(area, clientArrayOffset, restart.LogClientCount)

	return restart, nil
}

// parseLogClients reads the restart area's client array, stopping at the first
// entry that does not fit rather than rejecting the whole restart area. The
// geometry fields are the useful part; a truncated client list should not hide
// them.
func parseLogClients(area []byte, offset int, count uint16) []LogClient {
	if offset <= 0 || count == 0 {
		return nil
	}

	clients := make([]LogClient, 0, count)

	for i := 0; i < int(count); i++ {
		start := offset + i*logClientRecordSize
		if start < 0 || start+logClientRecordSize > len(area) {
			break
		}

		entry := area[start : start+logClientRecordSize]
		client := LogClient{
			OldestLSN:        ReadUint64LE(entry, 0x00),
			ClientRestartLSN: ReadUint64LE(entry, 0x08),
			SequenceNumber:   ReadUint16LE(entry, 0x14),
		}

		nameLength := int(ReadUint32LE(entry, 0x1C))
		if nameLength > 0 && nameLength <= logClientNameMaxBytes {
			reader := NewBinaryReader(entry[0x20 : 0x20+nameLength])
			if name, err := reader.ReadUTF16String(nameLength / 2); err == nil {
				client.Name = name
			}
		}

		clients = append(clients, client)
	}

	return clients
}

// applyLogPageFixups returns a copy of a log page with its update sequence
// applied, so the two bytes the sequence stamped over each sector tail are back
// where they belong.
func applyLogPageFixups(buf []byte) ([]byte, error) {
	page := make([]byte, len(buf))
	copy(page, buf)

	updateSeqOffset := ReadUint16LE(page, 4)
	updateSeqSize := ReadUint16LE(page, 6)

	if err := applyUpdateSequence(page, updateSeqOffset, updateSeqSize); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidLogRecord, err)
	}

	return page, nil
}

// logPageDataStart returns where a page's records begin: past the header and the
// update sequence array, rounded up to the record alignment.
func logPageDataStart(page []byte) int {
	updateSeqOffset := int(ReadUint16LE(page, 4))
	updateSeqSize := int(ReadUint16LE(page, 6))

	end := updateSeqOffset + updateSeqSize*2
	if end < logRecordPageHeaderSize {
		end = logRecordPageHeaderSize
	}

	return AlignUp(end, logAlignment)
}

// ParseLogRecord decodes one log record from the front of buf.
//
// It returns the record and how many bytes it consumed, which is the aligned
// total of the header and the client data. A record whose client data runs past
// the end of buf returns io.ErrUnexpectedEOF along with the byte count it would
// have needed, so a caller assembling a record split across pages knows how much
// more to gather.
func ParseLogRecord(buf []byte) (*LogRecord, int, error) {
	if len(buf) < LogRecordHeaderSize {
		return nil, 0, io.ErrUnexpectedEOF
	}

	record := &LogRecord{
		LSN:               ReadUint64LE(buf, 0x00),
		ClientPreviousLSN: ReadUint64LE(buf, 0x08),
		ClientUndoNextLSN: ReadUint64LE(buf, 0x10),
		ClientDataLength:  ReadUint32LE(buf, 0x18),
		ClientSequence:    ReadUint16LE(buf, 0x1C),
		ClientIndex:       ReadUint16LE(buf, 0x1E),
		RecordType:        ReadUint32LE(buf, 0x20),
		TransactionID:     ReadUint32LE(buf, 0x24),
		Flags:             ReadUint16LE(buf, 0x28),
	}

	if record.ClientDataLength > LogMaxRecordDataLength {
		return nil, 0, fmt.Errorf("%w: client data length %d exceeds the %d byte bound",
			ErrInvalidLogRecord, record.ClientDataLength, LogMaxRecordDataLength)
	}

	if record.RecordType != LogRecordTypeClient && record.RecordType != LogRecordTypeRestart {
		return nil, 0, fmt.Errorf("%w: record type %d is not a client or restart record",
			ErrInvalidLogRecord, record.RecordType)
	}

	if record.ClientIndex > logMaxClientIndex {
		return nil, 0, fmt.Errorf("%w: client index %d is beyond the %d the log file service addresses",
			ErrInvalidLogRecord, record.ClientIndex, logMaxClientIndex)
	}

	total := AlignUp(LogRecordHeaderSize+int(record.ClientDataLength), logAlignment)
	if LogRecordHeaderSize+int(record.ClientDataLength) > len(buf) {
		return nil, total, io.ErrUnexpectedEOF
	}

	// A restart record carries no NTFS operation, so there is nothing further to
	// decode from it.
	if record.RecordType != LogRecordTypeClient {
		return record, total, nil
	}

	clientData := buf[LogRecordHeaderSize : LogRecordHeaderSize+int(record.ClientDataLength)]
	parseNTFSLogData(record, clientData)

	return record, total, nil
}

// parseNTFSLogData decodes the NTFS half of a client record. Every field is
// bounds-checked against the client data rather than trusted, so a record whose
// offsets point outside itself yields the fields that are readable instead of
// failing the whole walk.
func parseNTFSLogData(record *LogRecord, data []byte) {
	if len(data) < logNTFSRecordHeaderSize {
		return
	}

	record.RedoOperation = ReadUint16LE(data, 0x00)
	record.UndoOperation = ReadUint16LE(data, 0x02)
	redoOffset := int(ReadUint16LE(data, 0x04))
	redoLength := int(ReadUint16LE(data, 0x06))
	undoOffset := int(ReadUint16LE(data, 0x08))
	undoLength := int(ReadUint16LE(data, 0x0A))
	record.TargetAttribute = ReadUint16LE(data, 0x0C)
	lcnsToFollow := int(ReadUint16LE(data, 0x0E))
	record.RecordOffset = ReadUint16LE(data, 0x10)
	record.AttributeOffset = ReadUint16LE(data, 0x12)
	record.MFTClusterIndex = ReadUint16LE(data, 0x14)
	record.TargetVCN = int64(ReadUint64LE(data, 0x18))

	record.RedoData = sliceLogPayload(data, redoOffset, redoLength)
	record.UndoData = sliceLogPayload(data, undoOffset, undoLength)

	for i := 0; i < lcnsToFollow; i++ {
		start := logNTFSRecordHeaderSize + i*8
		if start+8 > len(data) {
			break
		}
		record.LCNs = append(record.LCNs, ReadUint64LE(data, start))
	}
}

// sliceLogPayload copies length bytes at offset out of a record's client data,
// returning nil when the range does not fit inside it.
func sliceLogPayload(data []byte, offset, length int) []byte {
	if length <= 0 || offset < 0 || offset+length > len(data) {
		return nil
	}

	out := make([]byte, length)
	copy(out, data[offset:offset+length])

	return out
}

// EachLogRecord walks the records in a whole $LogFile buffer, calling fn for
// each one.
//
// It needs no Volume, so it works on a $LogFile extracted with KAPE or FTK
// Imager. Restart pages are read for the log's geometry and then skipped;
// records are read out of the "RCRD" pages that follow.
//
// Damage is skipped rather than ending the walk, so a corrupt region does not
// hide the records after it. If fn returns a non-nil error, iteration stops and
// that error is returned.
func EachLogRecord(buf []byte, fn func(*LogRecord) error) error {
	return EachLogRecordContext(context.Background(), buf, fn)
}

// EachLogRecordContext is [EachLogRecord] with cancellation. Cancelling ctx
// stops the walk at the next page boundary and returns the context's error.
func EachLogRecordContext(ctx context.Context, buf []byte, fn func(*LogRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	pageSize, bounds := logGeometryFrom(buf)
	walker := &logPageWalker{fn: fn, bounds: bounds}

	for offset := 0; offset+pageSize <= len(buf); offset += pageSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := walker.feed(buf[offset : offset+pageSize]); err != nil {
			return err
		}
	}

	// Anything still buffered is a record the log never finished writing.
	return nil
}

// logPageWalker feeds record pages in order, assembling records that span a page
// boundary across the two pages holding them.
//
// A record spanning pages is the whole difficulty of reading $LogFile: the log's
// data area is one logical byte stream chopped into fixed-size pages, each with
// its own header and update sequence, so a record can start near the end of one
// page and finish at the start of the next. Carrying the partial record forward
// is what makes that transparent to the caller.
type logPageWalker struct {
	carry []byte
	fn    func(*LogRecord) error

	// bounds are what the restart area says about the log. A zero value means
	// it could not be read, and only the loose checks in ParseLogRecord apply.
	bounds logBounds
}

// logBounds are the limits a restart area places on what can be a real record.
//
// Resynchronising after damage means testing arbitrary bytes as a record header,
// and the header's own fields are too weak to reject all of it: record type and
// data length alone let a surprising amount through. These come from the log's
// own description of itself, so they reject what the header cannot.
type logBounds struct {
	// clientCount is how many clients the log serves — one, for NTFS. A record
	// naming a client beyond that is not a record.
	clientCount uint16

	// sequenceBits is how many low bits of an LSN hold the offset within the
	// log, leaving the high bits as a wrap count.
	sequenceBits uint32

	// maxSequence is the highest wrap count a record may carry.
	maxSequence uint64
}

// logBoundsFrom derives the limits from a restart area.
//
// The sequence allowance is the restart area's own wrap count plus one, because
// CurrentLSN is written at a checkpoint and the log runs on past it: on a real
// volume, 8,686 of 32,594 records sat above CurrentLSN, so treating it as a hard
// ceiling would discard a quarter of the log. The wrap count moving on by more
// than one between checkpoints is not something a log this size does.
func logBoundsFrom(restart *LogRestartArea) logBounds {
	bounds := logBounds{clientCount: restart.LogClientCount}

	// A sequence field wider than the LSN leaves nothing to compare.
	if restart.SeqNumberBits == 0 || restart.SeqNumberBits >= 64 {
		return bounds
	}

	bounds.sequenceBits = restart.SeqNumberBits
	bounds.maxSequence = (restart.CurrentLSN >> restart.SeqNumberBits) + 1

	return bounds
}

// permits reports whether a record is within what the restart area allows.
func (b logBounds) permits(record *LogRecord) bool {
	if b.clientCount > 0 && record.ClientIndex >= b.clientCount {
		return false
	}

	if b.sequenceBits > 0 && (record.LSN>>b.sequenceBits) > b.maxSequence {
		return false
	}

	return true
}

// feed processes one page. Anything that is not a usable record page discards
// whatever was being assembled, since a record cannot continue across a gap.
func (w *logPageWalker) feed(page []byte) error {
	if len(page) < logRecordPageHeaderSize || ReadUint32LE(page, 0) != LogRecordPageMagic {
		// A restart page, an uninitialised page, or damage.
		w.carry = w.carry[:0]
		return nil
	}

	fixed, err := applyLogPageFixups(page)
	if err != nil {
		w.carry = w.carry[:0]
		return nil
	}

	dataStart := logPageDataStart(fixed)
	if dataStart >= len(fixed) {
		w.carry = w.carry[:0]
		return nil
	}

	w.carry = append(w.carry, fixed[dataStart:]...)

	consumed, err := walkLogRecords(w.carry, w.bounds, w.fn)
	if err != nil {
		return err
	}

	w.carry = append(w.carry[:0], w.carry[consumed:]...)

	return nil
}

// walkLogRecords consumes whole records from the front of buf, leaving a
// trailing partial record for the caller to complete from the next page. It
// returns how many bytes it consumed.
func walkLogRecords(buf []byte, bounds logBounds, fn func(*LogRecord) error) (int, error) {
	offset := 0

	for offset+LogRecordHeaderSize <= len(buf) {
		// Skip zero-filled space. Records are aligned, so stepping by the
		// alignment is both correct and much faster than a byte-at-a-time scan.
		if ReadUint64LE(buf, offset) == 0 {
			offset += logAlignment
			continue
		}

		record, total, err := ParseLogRecord(buf[offset:])
		if err == io.ErrUnexpectedEOF {
			// The record continues on the next page. Leave it for the caller to
			// complete rather than reporting a truncated one.
			return offset, nil
		}

		if err != nil {
			// Unusable record. Resynchronise on the next alignment boundary
			// rather than trusting a length field that just failed validation.
			offset += logAlignment
			continue
		}

		// Bytes the resynchronisation walked into can satisfy the header's own
		// fields; the log's description of itself is what rejects them.
		if !bounds.permits(record) {
			offset += logAlignment
			continue
		}

		if err := fn(record); err != nil {
			return offset, err
		}

		if total <= 0 {
			offset += logAlignment
			continue
		}

		offset += total
	}

	return offset, nil
}

// logGeometryFrom reads the page size and client count out of a $LogFile's
// restart area, falling back to the default page size and an unknown client
// count when no restart page can be read.
func logGeometryFrom(buf []byte) (pageSize int, bounds logBounds) {
	restart, err := ParseLogRestartPage(buf)
	if err != nil {
		return DefaultLogPageSize, logBounds{}
	}

	return logPageSizeOrDefault(restart.LogPageSize), logBoundsFrom(restart)
}

// logPageSizeOrDefault accepts a page size only when it is a plausible one.
func logPageSizeOrDefault(size uint32) int {
	if size == 0 || size > maxLogPageSize || size%logAlignment != 0 {
		return DefaultLogPageSize
	}
	return int(size)
}

// OpenLogFile opens $LogFile, the transaction log at MFT entry 2.
func (v *Volume) OpenLogFile() (*File, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	logFile, err := v.Open(MFTEntryLogFile)
	if err != nil {
		return nil, fmt.Errorf("open $LogFile: %w", err)
	}

	return logFile, nil
}

// LogRestartArea returns $LogFile's restart area, describing the log's geometry
// and how recent its contents are.
//
// Two copies are written, in the first two pages. The first is preferred and the
// second is used when the first cannot be read, which is exactly the redundancy
// NTFS keeps them for.
func (v *Volume) LogRestartArea() (*LogRestartArea, error) {
	logFile, err := v.OpenLogFile()
	if err != nil {
		return nil, err
	}

	page := make([]byte, DefaultLogPageSize)

	var firstErr error
	for copyIndex := 0; copyIndex < 2; copyIndex++ {
		offset := int64(copyIndex) * int64(len(page))
		if offset >= logFile.Size() {
			break
		}

		n, err := logFile.ReadAt(page, offset)
		if n == 0 && err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		restart, err := ParseLogRestartPage(page[:n])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		return restart, nil
	}

	if firstErr != nil {
		return nil, fmt.Errorf("read $LogFile restart area: %w", firstErr)
	}

	return nil, fmt.Errorf("%w: $LogFile has no readable restart area", ErrInvalidLogRecord)
}

// EachLogRecord walks the volume's $LogFile, calling fn for every record.
//
// The log is read a chunk of pages at a time rather than loaded whole, and a
// record split across a page boundary is assembled across the two. Damage is
// skipped rather than ending the walk. If fn returns a non-nil error, iteration
// stops and that error is returned.
//
// $LogFile is a circular log: it holds the most recent operations and overwrites
// the oldest, so records appear in whatever order the ring currently holds
// rather than oldest-first. Sort by LSN for a timeline.
func (v *Volume) EachLogRecord(fn func(*LogRecord) error) error {
	return v.EachLogRecordContext(context.Background(), fn)
}

// EachLogRecordContext is [Volume.EachLogRecord] with cancellation.
//
// $LogFile is commonly 64 MB and can be far larger, and every page in it is
// parsed. Cancelling ctx stops the walk at the next page boundary and returns
// the context's error; records already handed to fn stay handed over.
func (v *Volume) EachLogRecordContext(ctx context.Context, fn func(*LogRecord) error) error {
	if ctx == nil {
		ctx = context.Background()
	}

	logFile, err := v.OpenLogFile()
	if err != nil {
		return err
	}

	size := logFile.Size()
	if size <= 0 {
		return nil
	}

	pageSize := DefaultLogPageSize
	var bounds logBounds
	if restart, err := v.LogRestartArea(); err == nil {
		pageSize = logPageSizeOrDefault(restart.LogPageSize)
		bounds = logBoundsFrom(restart)
	}

	chunk := make([]byte, pageSize*logChunkPages)
	walker := &logPageWalker{fn: fn, bounds: bounds}

	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, readErr := logFile.ReadAt(chunk, offset)
		if n == 0 {
			break
		}

		for start := 0; start+pageSize <= n; start += pageSize {
			if err := walker.feed(chunk[start : start+pageSize]); err != nil {
				return err
			}
		}

		offset += int64(n)

		if readErr == io.EOF {
			break
		}
	}

	return nil
}
