// Package libntfs provides a high-quality, thread-safe Go library for parsing
// NTFS (New Technology File System) file systems.
//
// This library offers complete NTFS parsing capabilities including boot sector
// parsing, MFT entry reading, attribute handling, and file/directory operations.
// All public APIs are thread-safe and designed for concurrent access.
//
// Example usage:
//
//	file, err := os.Open("/dev/sda1") // or disk image
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer file.Close()
//
//	volume, err := libntfs.Open(file)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer volume.Close()
//
//	// Read root directory
//	root, err := volume.GetRootDirectory()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	rootFile, err := volume.Open(root.RecordNumber)
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	entries, err := rootFile.ReadDir()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	for _, entry := range entries {
//		fmt.Printf("%s\n", entry.Name)
//	}
package libntfs

// Version information
const (
	// Version is the current library version
	Version = "0.1.0"

	// Author information
	Author = "libntfs contributors"
)
