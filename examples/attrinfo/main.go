// Command attrinfo reports the extended attributes and reparse point of a path
// on an NTFS volume, or walks a directory listing every file that carries
// either.
//
// Both are places metadata hides in plain sight. Extended attributes are where
// the Windows Subsystem for Linux keeps POSIX ownership, mode, and timestamps
// that no Windows tool surfaces. A reparse point is what turns an ordinary
// directory entry into a redirection — a symlink, a junction, a Store app stub,
// or a cloud placeholder that has no local content at all.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"unicode"

	"github.com/aoiflux/libntfs"
)

// attrReport is one file's extended attributes and reparse point.
type attrReport struct {
	Path         string         `json:"path"`
	EntryNumber  uint64         `json:"entry_number"`
	IsDirectory  bool           `json:"is_directory"`
	EAInfo       *eaInfoReport  `json:"ea_information,omitempty"`
	EAs          []eaReport     `json:"extended_attributes,omitempty"`
	ReparsePoint *reparseReport `json:"reparse_point,omitempty"`
}

type eaInfoReport struct {
	PackedSize   uint16 `json:"packed_size"`
	NeedEACount  uint16 `json:"need_ea_count"`
	UnpackedSize uint32 `json:"unpacked_size"`
}

type eaReport struct {
	Name   string `json:"name"`
	Size   int    `json:"size"`
	NeedEA bool   `json:"need_ea"`
	Value  string `json:"value"` // Printable text, or hex when the bytes are binary
}

type reparseReport struct {
	Tag            string `json:"tag"`
	TagValue       string `json:"tag_value"`
	Microsoft      bool   `json:"microsoft"`
	DataLength     uint16 `json:"data_length"`
	SubstituteName string `json:"substitute_name,omitempty"`
	PrintName      string `json:"print_name,omitempty"`
	Relative       bool   `json:"relative,omitempty"`
}

func main() {
	recurse := flag.Bool("r", false, "walk the directory tree beneath the path")
	only := flag.Bool("only", false, "report only files carrying extended attributes or a reparse point")
	asJSON := flag.Bool("json", false, "emit JSON instead of text")
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

	vol, err := libntfs.Open(f)
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

	var reports []attrReport
	if *recurse {
		reports = walk(vol, file, target, *only)
	} else {
		report, err := describe(file, target)
		if err != nil {
			log.Fatalf("Failed to read %s: %v", target, err)
		}
		if !*only || report.interesting() {
			reports = append(reports, *report)
		}
	}

	if *asJSON {
		emitJSON(reports)
		return
	}

	emitText(reports)
}

// interesting reports whether the file carries anything this tool exists to show.
func (r attrReport) interesting() bool {
	return len(r.EAs) > 0 || r.ReparsePoint != nil
}

// describe collects one file's extended attributes and reparse point.
func describe(file *libntfs.File, path string) (*attrReport, error) {
	report := &attrReport{
		Path:        path,
		EntryNumber: file.EntryNumber(),
		IsDirectory: file.IsDirectory(),
	}

	eas, err := file.ExtendedAttributes()
	if err != nil {
		// A damaged $EA still yields the entries that parsed, so report them
		// alongside the problem rather than dropping both.
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
	}
	for _, ea := range eas {
		report.EAs = append(report.EAs, eaReport{
			Name:   ea.Name,
			Size:   len(ea.Value),
			NeedEA: ea.NeedEA(),
			Value:  renderValue(ea.Value),
		})
	}

	info, err := file.EAInformation()
	if err != nil {
		return nil, err
	}
	if info != nil {
		report.EAInfo = &eaInfoReport{
			PackedSize:   info.PackedSize,
			NeedEACount:  info.NeedEACount,
			UnpackedSize: info.UnpackedSize,
		}
	}

	reparse, err := file.ReparsePoint()
	if err != nil {
		return nil, err
	}
	if reparse != nil {
		substitute, print, _ := reparse.Target()
		report.ReparsePoint = &reparseReport{
			Tag:            reparse.TagName(),
			TagValue:       fmt.Sprintf("0x%08X", reparse.ReparseType),
			Microsoft:      reparse.IsMicrosoft(),
			DataLength:     reparse.DataLength,
			SubstituteName: substitute,
			PrintName:      print,
			Relative:       reparse.IsRelative(),
		}
	}

	return report, nil
}

// walk reports every entry beneath a directory.
func walk(vol *libntfs.Volume, dir *libntfs.File, path string, only bool) []attrReport {
	if !dir.IsDirectory() {
		report, err := describe(dir, path)
		if err != nil {
			log.Fatalf("Failed to read %s: %v", path, err)
		}
		if !only || report.interesting() {
			return []attrReport{*report}
		}
		return nil
	}

	entries, err := dir.ReadDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
		return nil
	}

	var reports []attrReport
	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}

		child, err := vol.Open(entry.EntryNum)
		if err != nil {
			continue
		}

		childPath := strings.TrimSuffix(path, "/") + "/" + entry.Name

		// A directory carrying a reparse point is a junction. Reporting it is the
		// point of this tool, but descending into it is not: the link is not the
		// target, and following one risks walking in a circle.
		if entry.IsDirectory && !child.IsReparsePoint() {
			reports = append(reports, walk(vol, child, childPath, only)...)
			continue
		}

		report, err := describe(child, childPath)
		if err != nil {
			continue
		}
		if !only || report.interesting() {
			reports = append(reports, *report)
		}
	}

	return reports
}

// renderValue shows a value as text when it reads as text, and as hex when it
// does not. Extended attribute values are opaque bytes, and WSL's are binary.
func renderValue(value []byte) string {
	if len(value) == 0 {
		return ""
	}

	for _, b := range value {
		if b > unicode.MaxASCII || (!unicode.IsPrint(rune(b)) && b != '\t') {
			return hex.EncodeToString(value)
		}
	}

	return string(value)
}

func emitJSON(reports []attrReport) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(reports); err != nil {
		log.Fatalf("Failed to encode output: %v", err)
	}
}

func emitText(reports []attrReport) {
	for _, report := range reports {
		kind := "file"
		if report.IsDirectory {
			kind = "directory"
		}
		fmt.Printf("%s  (%s, MFT entry %d)\n", report.Path, kind, report.EntryNumber)

		if report.ReparsePoint != nil {
			r := report.ReparsePoint
			fmt.Printf("  reparse   %s (%s, %d bytes)\n", r.Tag, r.TagValue, r.DataLength)
			if r.SubstituteName != "" {
				scope := "absolute"
				if r.Relative {
					scope = "relative"
				}
				fmt.Printf("            -> %s [%s]\n", r.SubstituteName, scope)
			}
			if r.PrintName != "" && r.PrintName != r.SubstituteName {
				fmt.Printf("            shown as %s\n", r.PrintName)
			}
		}

		if report.EAInfo != nil {
			fmt.Printf("  ea info   packed %d, unpacked %d, need-EA %d\n",
				report.EAInfo.PackedSize, report.EAInfo.UnpackedSize, report.EAInfo.NeedEACount)
		}

		for _, ea := range report.EAs {
			marker := " "
			if ea.NeedEA {
				marker = "!"
			}
			fmt.Printf("  ea      %s %-16s %5d bytes  %s\n", marker, ea.Name, ea.Size, ea.Value)
		}

		if !report.interesting() && report.EAInfo == nil {
			fmt.Println("  no extended attributes, no reparse point")
		}

		fmt.Println()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: attrinfo [flags] <image> <path>
       attrinfo [flags] -entry <n> <image>

Reports the extended attributes and reparse point of a path on an NTFS volume.

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Examples:
  attrinfo disk.img /Users/Alice/wsl-file.txt
  attrinfo -r -only disk.img /Users            # hunt for WSL metadata and links
  attrinfo -json disk.img /ProgramData/link
  attrinfo -entry 5 disk.img                   # the root directory
`)
}
