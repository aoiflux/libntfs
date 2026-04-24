package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <ntfs_volume> <directory_path>\n", os.Args[0])
		fmt.Println("\nExample:")
		fmt.Println("  ./traverse /dev/sda1 /Windows")
		fmt.Println("  ./traverse disk.img /Users")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	dirPath := os.Args[2]

	// Open volume
	file, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("Failed to open volume: %v", err)
	}
	defer file.Close()

	volume, err := libntfs.Open(file)
	if err != nil {
		log.Fatalf("Failed to parse NTFS volume: %v", err)
	}
	defer volume.Close()

	// Open starting directory
	dir, err := volume.OpenPath(dirPath)
	if err != nil {
		log.Fatalf("Failed to open directory %s: %v", dirPath, err)
	}

	if !dir.IsDirectory() {
		log.Fatalf("%s is not a directory", dirPath)
	}

	fmt.Printf("Traversing: %s\n\n", dirPath)

	// Traverse recursively
	stats := &Stats{}
	if err := traverse(volume, dir, dirPath, 0, stats); err != nil {
		log.Fatalf("Traversal error: %v", err)
	}

	// Print statistics
	fmt.Println("\n=== Statistics ===")
	fmt.Printf("Directories: %d\n", stats.DirCount)
	fmt.Printf("Files: %d\n", stats.FileCount)
	fmt.Printf("Deleted Directories: %d\n", stats.DeletedDirCount)
	fmt.Printf("Deleted Files: %d\n", stats.DeletedFileCount)
	fmt.Printf("Total Size: %d bytes (%.2f MB)\n",
		stats.TotalSize, float64(stats.TotalSize)/(1<<20))
}

type Stats struct {
	DirCount         int
	FileCount        int
	DeletedDirCount  int
	DeletedFileCount int
	TotalSize        uint64
}

func traverse(volume *libntfs.Volume, dir *libntfs.File, path string, depth int, stats *Stats) error {
	const maxDepth = 5 // Limit recursion depth

	if depth > maxDepth {
		return nil
	}

	entries, err := dir.ReadDir()
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	indent := strings.Repeat("  ", depth)

	for _, entry := range entries {
		fullPath := filepath.Join(path, entry.Name)
		status := ""
		if entry.Deleted {
			status = " [DELETED]"
		}

		if entry.IsDirectory {
			if entry.Deleted {
				stats.DeletedDirCount++
			} else {
				stats.DirCount++
			}
			fmt.Printf("%s[DIR]  %s/%s\n", indent, entry.Name, status)

			if entry.Deleted {
				continue
			}

			// Open subdirectory
			subdir, err := volume.Open(entry.EntryNum)
			if err != nil {
				fmt.Printf("%s  (error opening: %v)\n", indent, err)
				continue
			}

			// Recurse
			if err := traverse(volume, subdir, fullPath, depth+1, stats); err != nil {
				fmt.Printf("%s  (error traversing: %v)\n", indent, err)
			}
		} else {
			if entry.Deleted {
				stats.DeletedFileCount++
			} else {
				stats.FileCount++
				stats.TotalSize += entry.Size
			}
			fmt.Printf("%s[FILE] %s (%d bytes)%s\n", indent, entry.Name, entry.Size, status)
		}
	}

	return nil
}
