package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <ntfs_volume> <directory_path>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./traverse disk.img /")
		fmt.Println("  ./traverse /dev/sda1 /Windows")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	dirPath := os.Args[2]

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

	dir, err := volume.OpenPath(dirPath)
	if err != nil {
		log.Fatalf("Failed to open directory %s: %v", dirPath, err)
	}

	if !dir.IsDirectory() {
		log.Fatalf("%s is not a directory", dirPath)
	}

	fmt.Printf("Traversing: %s\n\n", dirPath)

	stats := &Stats{}
	traverse(volume, dir, dirPath, stats)

	fmt.Println("\n=== Statistics ===")
	fmt.Printf("Directories:         %d\n", stats.DirCount)
	fmt.Printf("Files:               %d\n", stats.FileCount)
	fmt.Printf("Deleted Directories: %d\n", stats.DeletedDirCount)
	fmt.Printf("Deleted Files:       %d\n", stats.DeletedFileCount)
	fmt.Printf("Total Size:          %d bytes (%.2f MB)\n",
		stats.TotalSize, float64(stats.TotalSize)/(1<<20))
}

// Stats accumulates traversal counters.
type Stats struct {
	DirCount         int
	FileCount        int
	DeletedDirCount  int
	DeletedFileCount int
	TotalSize        uint64
}

// stackItem holds a directory and the path prefix used for display.
type stackItem struct {
	dir    *libntfs.File
	path   string
	indent string
}

// traverse walks all files and directories reachable from dir using an explicit
// stack (DFS, LIFO).  No recursion is used, so tree depth is unlimited.
// A visited-set keyed on MFT entry number prevents loops from hard links or
// corrupt parent pointers.
func traverse(volume *libntfs.Volume, dir *libntfs.File, rootPath string, stats *Stats) {
	visited := map[uint64]bool{dir.EntryNumber(): true}
	stack := []stackItem{{dir: dir, path: rootPath, indent: ""}}

	for len(stack) > 0 {
		// Pop from the top of the stack.
		top := len(stack) - 1
		item := stack[top]
		stack = stack[:top]

		entries, err := item.dir.ReadDir()
		if err != nil {
			fmt.Printf("%s(error reading %s: %v)\n", item.indent, item.path, err)
			continue
		}

		childIndent := item.indent + "  "

		for _, entry := range entries {
			fullPath := item.path + "/" + entry.Name
			// Normalise double-slash when root path is "/".
			fullPath = strings.ReplaceAll(fullPath, "//", "/")

			deleted := ""
			if entry.Deleted {
				deleted = " [DELETED]"
			}

			if entry.IsDirectory {
				if entry.Deleted {
					stats.DeletedDirCount++
					fmt.Printf("%s[DIR]  %s%s\n", childIndent, entry.Name, deleted)
					continue
				}

				stats.DirCount++
				fmt.Printf("%s[DIR]  %s\n", childIndent, entry.Name)

				if visited[entry.EntryNum] {
					continue
				}

				subdir, err := volume.Open(entry.EntryNum)
				if err != nil {
					fmt.Printf("%s  (error opening: %v)\n", childIndent, err)
					continue
				}

				visited[entry.EntryNum] = true
				stack = append(stack, stackItem{
					dir:    subdir,
					path:   fullPath,
					indent: childIndent,
				})
			} else {
				if entry.Deleted {
					stats.DeletedFileCount++
				} else {
					stats.FileCount++
					stats.TotalSize += entry.Size
				}
				fmt.Printf("%s[FILE] %s (%d bytes)%s\n", childIndent, entry.Name, entry.Size, deleted)
			}
		}
	}
}
