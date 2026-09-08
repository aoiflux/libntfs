// Command logdump reads $LogFile, the NTFS transaction log, from either a full
// volume or a standalone $LogFile extracted with KAPE or FTK Imager.
//
// $LogFile records the metadata operations NTFS was about to perform, which
// makes it the finest-grained record of activity on a volume — finer than the
// USN journal, since it captures the individual attribute writes behind each
// change. It is a circular log holding only the most recent operations, so it
// covers a short window in far more detail than anything else on disk.
//
// This tool reports what the log says. It does not replay it: reconstructing
// file content from redo data is a different problem, and libntfs deliberately
// stops at handing over the decoded records.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/aoiflux/libntfs"
)

// recordReport is one log record rendered for output.
type recordReport struct {
	LSN             uint64   `json:"lsn"`
	PreviousLSN     uint64   `json:"client_previous_lsn"`
	UndoNextLSN     uint64   `json:"client_undo_next_lsn"`
	TransactionID   uint32   `json:"transaction_id"`
	RecordType      string   `json:"record_type"`
	RedoOperation   string   `json:"redo_operation"`
	UndoOperation   string   `json:"undo_operation"`
	TargetAttribute uint16   `json:"target_attribute"`
	TargetVCN       int64    `json:"target_vcn"`
	MFTClusterIndex uint16   `json:"mft_cluster_index"`
	Target          string   `json:"target,omitempty"` // Resolved file and stream
	TargetEntry     uint64   `json:"target_entry,omitempty"`
	RedoLength      int      `json:"redo_length"`
	UndoLength      int      `json:"undo_length"`
	LCNs            []uint64 `json:"lcns,omitempty"`
	RedoData        string   `json:"redo_data,omitempty"` // Hex, with -data
	UndoData        string   `json:"undo_data,omitempty"`
}

func main() {
	asJSON := flag.Bool("json", false, "emit one JSON object per record")
	sorted := flag.Bool("sort", false, "sort by LSN before printing, giving a timeline")
	withData := flag.Bool("data", false, "include the redo and undo payloads as hex")
	opFilter := flag.String("op", "", "only report records whose redo operation contains this text")
	restartOnly := flag.Bool("restart", false, "report the restart area and exit")
	transactions := flag.Bool("transactions", false, "group records into transactions instead of listing them")
	resolve := flag.Bool("resolve", false, "resolve each record's target attribute to a file and stream")
	limit := flag.Int("n", 0, "stop after this many records (0 means no limit)")
	flag.Usage = usage
	flag.Parse()

	mode := outputMode{
		json:         *asJSON,
		sorted:       *sorted,
		withData:     *withData,
		transactions: *transactions,
		resolve:      *resolve,
	}

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	path := flag.Arg(0)

	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("Failed to open %s: %v", path, err)
	}
	defer f.Close()

	// A full volume parses as one; anything else is treated as a standalone
	// $LogFile, which is how KAPE and FTK Imager hand it over.
	vol, volErr := libntfs.OpenReadOnly(f)
	if volErr == nil {
		defer vol.Close()

		if *restartOnly {
			restart, err := vol.LogRestartArea()
			if err != nil {
				log.Fatalf("Failed to read the restart area: %v", err)
			}
			printRestart(restart, mode.json)
			return
		}

		if mode.resolve {
			table, err := vol.LogAttributeTable()
			if err != nil {
				log.Fatalf("Failed to build the open attribute table: %v", err)
			}
			if table.Len() == 0 {
				fmt.Fprintln(os.Stderr,
					"warning: the log holds no open attribute table dump, so nothing resolves")
			}
			mode.table = table
		}

		emit(collect(vol.EachLogRecord, *opFilter, *limit), mode)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Failed to read %s: %v", path, err)
	}

	if *restartOnly {
		restart, err := libntfs.ParseLogRestartPage(data)
		if err != nil {
			log.Fatalf("Not an NTFS volume (%v) and no readable restart page: %v", volErr, err)
		}
		printRestart(restart, mode.json)
		return
	}

	records := collect(func(fn func(*libntfs.LogRecord) error) error {
		return libntfs.EachLogRecord(data, fn)
	}, *opFilter, *limit)

	emit(records, mode)
}

// outputMode carries the presentation flags, which every path needs and none of
// which change how the log is read.
type outputMode struct {
	json         bool
	sorted       bool
	withData     bool
	transactions bool
	resolve      bool

	// table resolves a record's target attribute to the file and stream it
	// names. NTFS records the file once, when it opens an attribute, and every
	// later record carries only an offset into the open attribute table — so
	// without this a log reads as operations with no subject.
	table *libntfs.LogAttributeTable
}

// errLimitReached stops the walk once enough records have been gathered.
var errLimitReached = fmt.Errorf("limit reached")

// collect gathers records from a walker, applying the operation filter and the
// record limit.
func collect(walk func(func(*libntfs.LogRecord) error) error,
	opFilter string, limit int,
) []*libntfs.LogRecord {
	var records []*libntfs.LogRecord

	err := walk(func(r *libntfs.LogRecord) error {
		if opFilter != "" && !matchesOp(r, opFilter) {
			return nil
		}

		records = append(records, r)

		if limit > 0 && len(records) >= limit {
			return errLimitReached
		}

		return nil
	})

	if err != nil && err != errLimitReached {
		log.Fatalf("Failed to walk $LogFile: %v", err)
	}

	return records
}

func matchesOp(r *libntfs.LogRecord, filter string) bool {
	return containsFold(r.RedoOperationName(), filter) ||
		containsFold(r.UndoOperationName(), filter)
}

// containsFold is a case-insensitive substring test, so "-op index" matches
// AddIndexEntryRoot without the caller getting the capitalisation right.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}

	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}

	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if lower(haystack[i+j]) != lower(needle[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}

	return false
}

func emit(records []*libntfs.LogRecord, mode outputMode) {
	if mode.sorted {
		// The log is circular, so on-disk order is wherever the ring currently
		// wraps. Sorting by LSN turns it back into a timeline.
		sort.Slice(records, func(i, j int) bool {
			return records[i].LSN < records[j].LSN
		})
	}

	if mode.transactions {
		emitTransactions(libntfs.GroupLogTransactions(records), mode.json)
		return
	}

	if mode.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		reports := make([]recordReport, 0, len(records))
		for _, r := range records {
			reports = append(reports, describe(r, mode))
		}
		if err := enc.Encode(reports); err != nil {
			log.Fatalf("Failed to encode output: %v", err)
		}
		return
	}

	for _, r := range records {
		if !r.IsClientRecord() {
			fmt.Printf("%-12d  restart\n", r.LSN)
			continue
		}

		// NTFS names the file once, when it opens an attribute; every later
		// record carries only an offset into the open attribute table. Without
		// resolving that, a log reads as operations with no subject.
		target := ""
		if entry, ok := mode.table.Resolve(r); ok {
			target = "  -> " + entry.String()
		}

		fmt.Printf("%-12d  txn %-8d  %-32s %-32s  vcn %-8d%s\n",
			r.LSN, r.TransactionID, r.RedoOperationName(), r.UndoOperationName(),
			r.TargetVCN, target)

		if mode.withData && len(r.RedoData) > 0 {
			fmt.Printf("%14sredo %s\n", "", hex.EncodeToString(r.RedoData))
		}
		if mode.withData && len(r.UndoData) > 0 {
			fmt.Printf("%14sundo %s\n", "", hex.EncodeToString(r.UndoData))
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d records\n", len(records))
}

// transactionReport is one grouped transaction rendered for output.
type transactionReport struct {
	ID          uint32   `json:"transaction_id"`
	FirstLSN    uint64   `json:"first_lsn"`
	LastLSN     uint64   `json:"last_lsn"`
	RecordCount int      `json:"record_count"`
	Committed   bool     `json:"committed"`
	Forgotten   bool     `json:"forgotten"`
	Operations  []string `json:"operations"`
}

// emitTransactions prints records grouped by transaction, which is how a single
// user-visible action — creating a file, renaming one — reads as one thing
// rather than as the handful of records NTFS actually wrote for it.
func emitTransactions(transactions []libntfs.LogTransaction, asJSON bool) {
	if asJSON {
		reports := make([]transactionReport, 0, len(transactions))
		for _, t := range transactions {
			reports = append(reports, transactionReport{
				ID:          t.ID,
				FirstLSN:    t.FirstLSN,
				LastLSN:     t.LastLSN,
				RecordCount: len(t.Records),
				Committed:   t.Committed,
				Forgotten:   t.Forgotten,
				Operations:  t.Operations(),
			})
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			log.Fatalf("Failed to encode output: %v", err)
		}
		return
	}

	for _, t := range transactions {
		// Neither outcome set means the log window does not contain the end of
		// this transaction, which for the oldest entries in a circular log is
		// normal rather than a sign of interrupted work.
		outcome := "open"
		switch {
		case t.Forgotten:
			outcome = "forgotten"
		case t.Committed:
			outcome = "committed"
		}

		fmt.Printf("txn %-10d  lsn %d..%d  %-10s  %d records\n",
			t.ID, t.FirstLSN, t.LastLSN, outcome, len(t.Records))

		for _, op := range t.Operations() {
			fmt.Printf("%16s%s\n", "", op)
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d transactions\n", len(transactions))
}

func describe(r *libntfs.LogRecord, mode outputMode) recordReport {
	recordType := "restart"
	if r.IsClientRecord() {
		recordType = "client"
	}

	report := recordReport{
		LSN:             r.LSN,
		PreviousLSN:     r.ClientPreviousLSN,
		UndoNextLSN:     r.ClientUndoNextLSN,
		TransactionID:   r.TransactionID,
		RecordType:      recordType,
		RedoOperation:   r.RedoOperationName(),
		UndoOperation:   r.UndoOperationName(),
		TargetAttribute: r.TargetAttribute,
		TargetVCN:       r.TargetVCN,
		MFTClusterIndex: r.MFTClusterIndex,
		RedoLength:      len(r.RedoData),
		UndoLength:      len(r.UndoData),
		LCNs:            r.LCNs,
	}

	if entry, ok := mode.table.Resolve(r); ok {
		report.Target = entry.String()
		report.TargetEntry = entry.RecordNumber
	}

	if mode.withData {
		report.RedoData = hex.EncodeToString(r.RedoData)
		report.UndoData = hex.EncodeToString(r.UndoData)
	}

	return report
}

func printRestart(restart *libntfs.LogRestartArea, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(restart); err != nil {
			log.Fatalf("Failed to encode output: %v", err)
		}
		return
	}

	fmt.Printf("version           %d.%d\n", restart.MajorVersion, restart.MinorVersion)
	fmt.Printf("system page size  %d\n", restart.SystemPageSize)
	fmt.Printf("log page size     %d\n", restart.LogPageSize)
	fmt.Printf("log file size     %d\n", restart.FileSize)
	fmt.Printf("current LSN       %d\n", restart.CurrentLSN)
	fmt.Printf("chkdsk LSN        %d\n", restart.ChkdskLSN)
	fmt.Printf("open count        %d\n", restart.RestartOpenCount)

	for _, client := range restart.Clients {
		fmt.Printf("client            %q oldest LSN %d, restart LSN %d\n",
			client.Name, client.OldestLSN, client.ClientRestartLSN)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: logdump [flags] <image_or_$LogFile>

Reads the NTFS transaction log from a volume or a standalone $LogFile.

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Examples:
  logdump -restart disk.img              # log geometry and how recent it is
  logdump -sort -n 200 disk.img          # the 200 most recent operations
  logdump -op index disk.img             # directory index changes only
  logdump -transactions -sort disk.img   # grouped, so one action reads as one thing
  logdump -resolve -op index disk.img    # index changes, named by file and stream
  logdump -json -data '$LogFile'         # an extracted log, payloads included

$LogFile is circular: it holds the most recent operations and overwrites the
oldest, so use -sort to read it as a timeline.
`)
}
