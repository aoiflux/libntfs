package main

import (
	"fmt"
	"log"
	"os"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <ntfs_volume_or_image>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./basic /dev/sda1         # Linux")
		fmt.Println("  ./basic \\\\.\\C:           # Windows (requires Administrator)")
		fmt.Println("  ./basic \\\\.\\PhysicalDrive0  # Windows physical drive (Administrator)")
		fmt.Println("  ./basic disk.img          # Disk image file")
		fmt.Println("\nNote: On Windows, accessing raw drives requires running as Administrator")
		os.Exit(1)
	}

	volumePath := os.Args[1]

	// Open the volume
	file, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("Failed to open volume: %v", err)
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

	// Separate directories and files
	var dirs, files []libntfs.DirEntry
	for _, entry := range entries {
		if entry.IsDirectory {
			dirs = append(dirs, entry)
		} else {
			files = append(files, entry)
		}
	}

	// Print directories
	if len(dirs) > 0 {
		fmt.Printf("Directories (%d):\n", len(dirs))
		for _, dir := range dirs {
			fmt.Printf("  [DIR]  %-40s  Modified: %s\n",
				dir.Name, dir.ModifyTime.Format("2006-01-02 15:04:05"))
		}
		fmt.Println()
	}

	// Print files (limited to first 20)
	if len(files) > 0 {
		fmt.Printf("Files (%d):\n", len(files))
		limit := len(files)
		if limit > 20 {
			limit = 20
		}
		for i := 0; i < limit; i++ {
			file := files[i]
			fmt.Printf("  [FILE] %-40s %10d bytes  Modified: %s\n",
				file.Name, file.Size, file.ModifyTime.Format("2006-01-02 15:04:05"))
		}
		if len(files) > 20 {
			fmt.Printf("  ... and %d more files\n", len(files)-20)
		}
		fmt.Println()
	}
}
