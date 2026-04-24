//go:build windows
// +build windows

package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: windows_drive <drive_letter>")
		fmt.Println("\nExample:")
		fmt.Println("  windows_drive C")
		fmt.Println("  windows_drive D")
		fmt.Println("\nNote: Requires administrator privileges")
		os.Exit(1)
	}

	driveLetter := strings.ToUpper(os.Args[1])
	if len(driveLetter) > 1 {
		driveLetter = string(driveLetter[0])
	}

	// Windows requires the \\.\X: format for raw drive access
	volumePath := fmt.Sprintf("\\\\.\\%s:", driveLetter)

	fmt.Printf("Opening drive %s: (path: %s)\n", driveLetter, volumePath)
	fmt.Println("Note: This requires administrator privileges")
	fmt.Println()

	// Open the volume
	// On Windows, you MUST run as administrator to access raw drives
	file, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("Failed to open volume: %v\n\nMake sure to:\n1. Run as Administrator\n2. Use format: %s\n", err, volumePath)
	}
	defer file.Close()

	// Parse NTFS volume
	volume, err := libntfs.Open(file)
	if err != nil {
		log.Fatalf("Failed to parse NTFS volume: %v", err)
	}
	defer volume.Close()

	// Print volume information
	fmt.Println("=== NTFS Volume Information ===")
	fmt.Printf("Volume Size: %d bytes (%.2f GB)\n",
		volume.VolumeSize(), float64(volume.VolumeSize())/(1<<30))
	fmt.Printf("Cluster Size: %d bytes\n", volume.BytesPerCluster())
	fmt.Printf("Sector Size: %d bytes\n", volume.BytesPerSector())
	fmt.Printf("MFT Record Size: %d bytes\n", volume.MFTRecordSize())
	fmt.Printf("Serial Number: 0x%X\n", volume.VolumeSerialNumber())

	bs := volume.GetBootSector()
	fmt.Printf("OEM Name: %s\n", strings.TrimSpace(string(bs.OEMName[:])))
	fmt.Println()

	// Get root directory
	root, err := volume.GetRootDirectory()
	if err != nil {
		log.Fatalf("Failed to open root directory: %v", err)
	}

	// List root directory contents
	fmt.Println("=== Root Directory Contents ===")
	entries, err := root.ReadDir()
	if err != nil {
		log.Fatalf("Failed to read root directory: %v", err)
	}

	fmt.Printf("Total entries: %d\n\n", len(entries))
	var deletedCount int
	for _, entry := range entries {
		if entry.Deleted {
			deletedCount++
		}
	}
	fmt.Printf("Deleted entries: %d\n\n", deletedCount)

	// Print first 30 entries
	limit := len(entries)
	if limit > 30 {
		limit = 30
	}

	for i := 0; i < limit; i++ {
		entry := entries[i]
		typeStr := "[FILE]"
		if entry.IsDirectory {
			typeStr = "[DIR] "
		}
		if entry.Deleted {
			typeStr = "[DEL] "
		}
		fmt.Printf("%s %-40s %10d bytes\n", typeStr, entry.Name, entry.Size)
	}

	if len(entries) > 30 {
		fmt.Printf("\n... and %d more entries\n", len(entries)-30)
	}
}
