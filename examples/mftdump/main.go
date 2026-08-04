// Command mftdump lists MFT records as a flat timeline.
//
// It accepts either a standalone $MFT file, as extracted by KAPE, FTK Imager,
// or icat, or a full NTFS volume or disk image. The input type is detected from
// the first four bytes and can be forced with -standalone.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aoiflux/libntfs"
)

func main() {
	limit := flag.Int("limit", 0, "stop after N records (0 = no limit)")
	recordSize := flag.Uint("record-size", 0, "MFT record size for standalone mode (0 = detect)")
	standalone := flag.Bool("standalone", false, "force standalone $MFT parsing")
	showUnused := flag.Bool("deleted", false, "include records not marked in use")
	flag.Usage = usage
	flag.Parse()

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

	isMFT := *standalone
	if !isMFT {
		isMFT, err = looksLikeMFTFile(f)
		if err != nil {
			log.Fatalf("Failed to inspect %s: %v", path, err)
		}
	}

	if isMFT {
		err = dumpStandaloneMFT(f, uint32(*recordSize), *limit, *showUnused)
	} else {
		err = dumpVolume(f, *limit, *showUnused)
	}
	if err != nil {
		log.Fatalf("Failed to dump %s: %v", path, err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <$MFT file | ntfs_volume_or_image>\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  ./mftdump -limit 100 ./$MFT           # Extracted $MFT file")
	fmt.Fprintln(os.Stderr, "  ./mftdump -deleted ./$MFT             # Include unallocated records")
	fmt.Fprintln(os.Stderr, "  ./mftdump disk.img                    # Disk image")
	fmt.Fprintln(os.Stderr, "  mftdump.exe \\\\.\\C:                    # Windows (requires Administrator)")
	fmt.Fprintln(os.Stderr, "\nFlags:")
	flag.PrintDefaults()
}

// looksLikeMFTFile reports whether the input starts with a FILE record, which
// distinguishes an extracted $MFT from a volume starting with a boot sector.
func looksLikeMFTFile(r io.ReaderAt) (bool, error) {
	magic := make([]byte, 4)
	if _, err := r.ReadAt(magic, 0); err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}

	return libntfs.ReadUint32LE(magic, 0) == libntfs.MFTMagicFILE, nil
}

// detectRecordSize reads the allocated size out of the first record header,
// falling back to the default when it is not a sane record size.
func detectRecordSize(r io.ReaderAt) uint32 {
	header := make([]byte, libntfs.MFTEntryHeaderSize)
	if _, err := r.ReadAt(header, 0); err != nil {
		return libntfs.DefaultMFTRecordSize
	}

	allocated := libntfs.ReadUint32LE(header, 28)
	if allocated < libntfs.MinMFTEntrySize || allocated > libntfs.MaxMFTEntrySize {
		return libntfs.DefaultMFTRecordSize
	}

	return allocated
}

// dumpStandaloneMFT walks an extracted $MFT with no volume behind it.
func dumpStandaloneMFT(r io.ReaderAt, recordSize uint32, limit int, showUnused bool) error {
	if recordSize == 0 {
		recordSize = detectRecordSize(r)
	}
	fmt.Fprintf(os.Stderr, "# standalone $MFT, %d-byte records\n", recordSize)

	printHeader()

	buf := make([]byte, recordSize)
	shown := 0

	for entryNum := uint64(0); ; entryNum++ {
		offset := int64(entryNum) * int64(recordSize)
		if _, err := r.ReadAt(buf, offset); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // End of the table
			}
			return err
		}

		// ParseMFTRecord applies fixups in place, so buf is reusable only
		// because we overwrite it fully on every iteration.
		entry, err := libntfs.ParseMFTRecord(buf, entryNum, recordSize)
		if err != nil {
			continue // Unallocated, bad, or corrupt record
		}

		if printEntry(entryNum, entry, showUnused) {
			shown++
			if limit > 0 && shown >= limit {
				break
			}
		}
	}

	return nil
}

// dumpVolume walks the MFT of a live volume or image.
func dumpVolume(r io.ReaderAt, limit int, showUnused bool) error {
	vol, err := libntfs.Open(r)
	if err != nil {
		return err
	}
	defer vol.Close()

	count, err := vol.MFTEntryCount()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "# volume with %d addressable MFT records\n", count)

	printHeader()

	shown := 0
	errStop := errors.New("limit reached")

	err = vol.EachMFTEntry(func(entryNum uint64, entry *libntfs.MFTEntry) error {
		if printEntry(entryNum, entry, showUnused) {
			shown++
			if limit > 0 && shown >= limit {
				return errStop
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return err
	}

	return nil
}

func printHeader() {
	fmt.Printf("%-10s %-6s %-5s %12s  %-19s  %s\n",
		"ENTRY", "STATE", "TYPE", "SIZE", "MODIFIED", "NAME")
}

// printEntry writes one timeline row and reports whether it printed anything.
func printEntry(entryNum uint64, entry *libntfs.MFTEntry, showUnused bool) bool {
	if !entry.IsInUse() && !showUnused {
		return false
	}

	name, err := entry.GetFileName()
	if err != nil {
		return false // Extension records and $MFT metafiles without $FILE_NAME
	}

	state := "alloc"
	if !entry.IsInUse() {
		state = "del"
	}

	kind := "file"
	if entry.IsDirectory() {
		kind = "dir"
	}

	modified := "-"
	if si, err := entry.GetStandardInformation(); err == nil {
		modified = si.ModifyTime.Format(time.DateTime)
	}

	fmt.Printf("%-10d %-6s %-5s %12d  %-19s  %s\n",
		entryNum, state, kind, dataSize(entry), modified, name.Name)

	return true
}

// dataSize returns the size of the unnamed $DATA stream, which is 0 for
// directories and for records whose data lives in an extension record.
func dataSize(entry *libntfs.MFTEntry) uint64 {
	data := entry.FindAttribute(libntfs.AttrTypeData, "")
	if data == nil {
		return 0
	}

	if data.Resident != nil {
		return uint64(data.Resident.ValueLength)
	}
	if data.NonResident != nil {
		return data.NonResident.RealSize
	}

	return 0
}
