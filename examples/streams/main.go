// Command streams reports the data streams and security descriptor of a path
// on an NTFS volume, or walks a directory reporting every file that carries an
// alternate data stream.
//
// Alternate data streams are a common hiding place, and the security descriptor
// answers who could reach the file, so the two are reported together.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aoiflux/libntfs"
)

func main() {
	adsOnly := flag.Bool("ads-only", false, "only report files carrying alternate data streams")
	recurse := flag.Bool("r", false, "walk the directory tree beneath the path")
	dump := flag.String("dump", "", "write the named stream's contents to stdout and exit")
	entry := flag.Int64("entry", -1, "target an MFT entry number instead of a path")
	flag.Usage = usage
	flag.Parse()

	byEntry := *entry >= 0
	if (byEntry && flag.NArg() != 1) || (!byEntry && flag.NArg() != 2) {
		flag.Usage()
		os.Exit(1)
	}

	image := flag.Arg(0)

	f, err := os.Open(image)
	if err != nil {
		log.Fatalf("Failed to open %s: %v", image, err)
	}
	defer f.Close()

	vol, err := libntfs.OpenReadOnly(f)
	if err != nil {
		log.Fatalf("Failed to parse NTFS volume: %v", err)
	}
	defer vol.Close()

	var (
		file   *libntfs.File
		target string
	)

	if byEntry {
		target = fmt.Sprintf("MFT entry %d", *entry)
		file, err = vol.Open(uint64(*entry))
	} else {
		target = flag.Arg(1)
		file, err = vol.OpenPath(target)
	}
	if err != nil {
		log.Fatalf("Failed to open %s: %v", target, err)
	}

	if *dump != "" {
		dumpStream(file, *dump)
		return
	}

	if *recurse {
		walk(vol, file, target, *adsOnly)
		return
	}

	report(file, target, *adsOnly)
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <ntfs_volume_or_image> <path>\n\n", os.Args[0])
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  ./streams disk.img /Users/bob/download.exe")
	fmt.Fprintln(os.Stderr, "  ./streams -r -ads-only disk.img /Users            # hunt for ADS")
	fmt.Fprintln(os.Stderr, "  ./streams -dump Zone.Identifier disk.img /a.exe   # stream contents")
	fmt.Fprintln(os.Stderr, "  ./streams -entry 42 disk.img                      # target by MFT entry")
	fmt.Fprintln(os.Stderr, "\nFlags:")
	flag.PrintDefaults()
}

// dumpStream writes one stream's contents to stdout.
func dumpStream(file *libntfs.File, name string) {
	stream, err := file.OpenStream(name)
	if err != nil {
		log.Fatalf("Failed to open stream %q: %v", name, err)
	}

	data, err := stream.ReadAll()
	if err != nil {
		log.Fatalf("Failed to read stream %q: %v", name, err)
	}

	if _, err := os.Stdout.Write(data); err != nil {
		log.Fatalf("Failed to write stream contents: %v", err)
	}
}

// report prints the streams and security descriptor for one file.
func report(file *libntfs.File, path string, adsOnly bool) {
	streams := file.Streams()

	alternates := 0
	for _, s := range streams {
		if s.IsAlternate() {
			alternates++
		}
	}

	if adsOnly && alternates == 0 {
		return
	}

	fmt.Printf("%s\n", path)

	for _, s := range streams {
		name := s.Name
		kind := "alternate"
		if name == "" {
			name = "(unnamed)"
			kind = "default"
		}

		storage := "non-resident"
		if s.Resident {
			storage = "resident"
		}

		fmt.Printf("  stream  %-24s %-10s %-13s %10d bytes", name, kind, storage, s.Size)
		if !s.Support.Readable {
			fmt.Printf("  [unreadable: %v]", s.Support.BlockingError)
		}
		fmt.Println()
	}

	printSecurity(file)
	fmt.Println()
}

// printSecurity prints the file's owner, group, and DACL.
func printSecurity(file *libntfs.File) {
	sd, err := file.SecurityDescriptor()
	if err != nil {
		if !errors.Is(err, libntfs.ErrSecurityDescriptorNotFound) {
			fmt.Printf("  security  unavailable: %v\n", err)
		}
		return
	}

	fmt.Printf("  owner   %s\n", describeSID(sd.Owner))
	if sd.Group != nil {
		fmt.Printf("  group   %s\n", describeSID(sd.Group))
	}

	if sd.DACL == nil {
		// No DACL is not the same as an empty one: it grants everyone access.
		fmt.Println("  dacl    absent (grants everyone full access)")
		return
	}

	if len(sd.DACL.ACEs) == 0 {
		fmt.Println("  dacl    present but empty (denies everyone)")
		return
	}

	for _, ace := range sd.DACL.ACEs {
		inherited := ""
		if ace.IsInherited() {
			inherited = " (inherited)"
		}
		fmt.Printf("  ace     %-16s %-46s %v%s\n",
			ace.TypeName(), describeSID(ace.Trustee), ace.RightsNames(), inherited)
	}
}

// describeSID renders a SID with its well-known account name when it has one.
func describeSID(sid *libntfs.SID) string {
	if sid == nil {
		return "(none)"
	}

	if name, ok := sid.WellKnownName(); ok {
		return fmt.Sprintf("%s [%s]", sid.String(), name)
	}

	return sid.String()
}

// walk reports every entry beneath dir, recursing into subdirectories.
func walk(vol *libntfs.Volume, dir *libntfs.File, path string, adsOnly bool) {
	entries, err := dir.ReadDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "# skipping %s: %v\n", path, err)
		return
	}

	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." || entry.Deleted {
			continue
		}

		child, err := vol.Open(entry.EntryNum)
		if err != nil {
			continue
		}

		childPath := path + "/" + entry.Name
		report(child, childPath, adsOnly)

		if entry.IsDirectory {
			walk(vol, child, childPath, adsOnly)
		}
	}
}
