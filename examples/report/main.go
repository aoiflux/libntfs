package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <ntfs_volume_or_image> [max_files]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./report disk.img")
		fmt.Println("  ./report disk.img 100      # limit output to first 100 files")
		fmt.Println("  ./report /dev/sda1")
		fmt.Println("  ./report \\\\.\\C:           # Windows (requires Administrator)")
		os.Exit(1)
	}

	volumePath := os.Args[1]

	maxFiles := -1 // -1 means no limit
	if len(os.Args) >= 3 {
		n, err := strconv.Atoi(os.Args[2])
		if err != nil || n < 0 {
			log.Fatalf("invalid max_files value: %s", os.Args[2])
		}
		maxFiles = n
	}

	f, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("failed to open volume: %v", err)
	}
	defer f.Close()

	volume, err := libntfs.Open(f)
	if err != nil {
		log.Fatalf("failed to parse NTFS volume: %v", err)
	}
	defer volume.Close()

	fmt.Fprintln(os.Stderr, "Building report (this may take a while on large volumes)...")

	report, err := volume.Report()
	if err != nil {
		log.Fatalf("failed to generate report: %v", err)
	}

	// Optionally cap the file list so the output stays readable in a terminal.
	if maxFiles >= 0 && len(report.Files) > maxFiles {
		report.Files = report.Files[:maxFiles]
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		log.Fatalf("failed to encode report: %v", err)
	}
}
