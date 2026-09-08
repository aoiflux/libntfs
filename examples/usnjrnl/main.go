// Command usnjrnl prints the NTFS USN change journal as a timeline.
//
// It accepts either an extracted $J file, as produced by KAPE or FTK Imager, or
// a full NTFS volume or disk image, from which \$Extend\$UsnJrnl:$J is read
// directly. The journal records every create, delete, rename, and write, which
// makes it the densest activity timeline on a Windows volume.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aoiflux/libntfs"
)

// journalEntry is the -json shape, one object per line.
type journalEntry struct {
	USN       int64     `json:"usn"`
	Version   string    `json:"version"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	Entry     uint64    `json:"entry"`
	Parent    uint64    `json:"parent"`
	Name      string    `json:"name,omitempty"`
	Directory bool      `json:"directory"`
	Reasons   []string  `json:"reasons"`
}

var (
	jsonEnc *json.Encoder
	shown   int
	limit   int
)

// errLimit stops the walk once -limit records have been printed.
var errLimit = errors.New("limit reached")

func main() {
	flag.IntVar(&limit, "limit", 0, "stop after N records (0 = no limit)")
	reason := flag.String("reason", "", "only show records whose reasons contain this substring")
	name := flag.String("name", "", "only show records whose file name contains this substring")
	asJSON := flag.Bool("json", false, "emit one JSON object per record instead of a table")
	standalone := flag.Bool("standalone", false, "force treating the input as an extracted $J file")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	if *asJSON {
		jsonEnc = json.NewEncoder(os.Stdout)
	}

	path := flag.Arg(0)

	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("Failed to open %s: %v", path, err)
	}
	defer f.Close()

	emit := filtered(*reason, *name)

	if *standalone || !looksLikeVolume(f) {
		err = walkStandalone(f, emit)
	} else {
		err = walkVolume(f, emit)
	}

	if err != nil && !errors.Is(err, errLimit) {
		log.Fatalf("Failed to read %s: %v", path, err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <$J file | ntfs_volume_or_image>\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  ./usnjrnl ./\\$J                          # Extracted journal")
	fmt.Fprintln(os.Stderr, "  ./usnjrnl -reason Delete disk.img         # Deletions only")
	fmt.Fprintln(os.Stderr, "  ./usnjrnl -name .exe -limit 50 disk.img   # Executables")
	fmt.Fprintln(os.Stderr, "  ./usnjrnl -json disk.img | jq .           # JSON Lines")
	fmt.Fprintln(os.Stderr, "\nFlags:")
	flag.PrintDefaults()
}

// looksLikeVolume reports whether the input starts with an NTFS boot sector.
func looksLikeVolume(r io.ReaderAt) bool {
	oem := make([]byte, 8)
	if _, err := r.ReadAt(oem, 3); err != nil {
		return false
	}

	return string(oem) == libntfs.BootSectorOEMNameNTFS
}

// filtered wraps the printer with the -reason and -name filters.
func filtered(reason, name string) func(*libntfs.USNRecord) error {
	return func(r *libntfs.USNRecord) error {
		if name != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(name)) {
			return nil
		}

		reasons := r.ReasonNames()
		if reason != "" && !strings.Contains(strings.ToLower(strings.Join(reasons, ",")), strings.ToLower(reason)) {
			return nil
		}

		print(r, reasons)

		shown++
		if limit > 0 && shown >= limit {
			return errLimit
		}

		return nil
	}
}

// print writes one record in the selected output format.
func print(r *libntfs.USNRecord, reasons []string) {
	if jsonEnc != nil {
		if err := jsonEnc.Encode(journalEntry{
			USN:       r.USN,
			Version:   fmt.Sprintf("%d.%d", r.MajorVersion, r.MinorVersion),
			Timestamp: r.Timestamp,
			Entry:     r.FileReference,
			Parent:    r.ParentReference,
			Name:      r.Name,
			Directory: r.IsDirectory(),
			Reasons:   reasons,
		}); err != nil {
			log.Fatalf("Failed to encode record %d: %v", r.USN, err)
		}
		return
	}

	stamp := "-"
	if !r.Timestamp.IsZero() {
		stamp = r.Timestamp.Format(time.DateTime)
	}

	kind := "file"
	if r.IsDirectory() {
		kind = "dir"
	}

	name := r.Name
	if name == "" {
		name = "(no name)"
	}

	fmt.Printf("%-14d %-19s %-8d %-4s %-40s %s\n",
		r.USN, stamp, r.FileReference, kind, name, strings.Join(reasons, ","))
}

func printHeader() {
	if jsonEnc != nil {
		return
	}

	fmt.Printf("%-14s %-19s %-8s %-4s %-40s %s\n",
		"USN", "TIMESTAMP", "ENTRY", "TYPE", "NAME", "REASONS")
}

// walkStandalone parses an extracted $J file.
func walkStandalone(f *os.File, emit func(*libntfs.USNRecord) error) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}

	if info.Size() > 1<<30 {
		// The library streams a volume's journal in chunks, but an extracted
		// file is read whole here, so refuse rather than exhaust memory.
		return fmt.Errorf("extracted journal is %d bytes; extract a smaller range", info.Size())
	}

	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "# extracted $J, %d bytes\n", len(data))
	printHeader()

	return libntfs.EachUSNRecord(data, emit)
}

// walkVolume reads the journal from a mounted volume or image.
func walkVolume(f *os.File, emit func(*libntfs.USNRecord) error) error {
	vol, err := libntfs.OpenReadOnly(f)
	if err != nil {
		return err
	}
	defer vol.Close()

	journal, err := vol.OpenUSNJournal()
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "# volume journal, %d bytes\n", journal.Size())
	printHeader()

	return vol.EachUSNRecord(emit)
}
