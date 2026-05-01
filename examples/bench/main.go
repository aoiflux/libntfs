package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aoiflux/libntfs"
)

type sampledFile struct {
	entryNum uint64
	fullPath string
	size     uint64
}

type walkStats struct {
	directories      int
	files            int
	deletedDirs      int
	deletedFiles     int
	totalFileBytes   uint64
	sampledFiles     []sampledFile
	readErrors       int
	sampledBytesRead int64
}

type stackItem struct {
	dir  *libntfs.File
	path string
}

func main() {
	sampleFiles := flag.Int("sample-files", 200, "Max number of files to sample for read throughput")
	readBytesPerFile := flag.Int64("read-bytes", 1<<20, "Max bytes to read from each sampled file")
	readBufferSize := flag.Int("read-buffer", 256*1024, "Read buffer size in bytes for sampled reads")
	enableParentFallback := flag.Bool("mft-parent-fallback", false, "Enable expensive full-MFT parent fallback during ReadDir")
	includeDeleted := flag.Bool("include-deleted", false, "Include deleted files in sampled reads")
	flag.Parse()

	if *sampleFiles < 0 {
		log.Fatalf("invalid -sample-files value: %d", *sampleFiles)
	}
	if *readBytesPerFile < 0 {
		log.Fatalf("invalid -read-bytes value: %d", *readBytesPerFile)
	}
	if *readBufferSize <= 0 {
		log.Fatalf("invalid -read-buffer value: %d", *readBufferSize)
	}

	args := flag.Args()
	if len(args) < 1 || len(args) > 2 {
		fmt.Printf("Usage: %s [flags] <ntfs_volume> [start_path]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Printf("  %s disk.img /Windows\n", os.Args[0])
		fmt.Printf("  %s -sample-files=500 -read-bytes=2097152 \\\\.\\C: /\n", os.Args[0])
		os.Exit(1)
	}

	volumePath := args[0]
	startPath := "/"
	if len(args) == 2 {
		startPath = normalizePath(args[1])
	}

	img, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("Failed to open volume: %v", err)
	}
	defer img.Close()

	openStart := time.Now()
	vol, err := libntfs.Open(img)
	if err != nil {
		log.Fatalf("Failed to parse NTFS volume: %v", err)
	}
	defer vol.Close()
	openDur := time.Since(openStart)

	vol.SetMFTParentFallback(*enableParentFallback)

	root, err := vol.OpenPath(startPath)
	if err != nil {
		log.Fatalf("Failed to open start path %q: %v", startPath, err)
	}
	if !root.IsDirectory() {
		log.Fatalf("Start path %q is not a directory", startPath)
	}

	fmt.Println("=== Benchmark Configuration ===")
	fmt.Printf("Volume:                %s\n", volumePath)
	fmt.Printf("Start path:            %s\n", startPath)
	fmt.Printf("MFT parent fallback:   %v\n", *enableParentFallback)
	fmt.Printf("Sample files:          %d\n", *sampleFiles)
	fmt.Printf("Read bytes/file:       %d\n", *readBytesPerFile)
	fmt.Printf("Read buffer size:      %d\n", *readBufferSize)
	fmt.Printf("Include deleted files: %v\n", *includeDeleted)
	fmt.Println()

	walkStart := time.Now()
	stats, err := walkAndSample(vol, root, startPath, *sampleFiles, *includeDeleted)
	if err != nil {
		log.Fatalf("Traversal failed: %v", err)
	}
	walkDur := time.Since(walkStart)

	readDur := time.Duration(0)
	if *sampleFiles > 0 && *readBytesPerFile > 0 && len(stats.sampledFiles) > 0 {
		readStart := time.Now()
		for _, s := range stats.sampledFiles {
			n, readErr := readSampledFile(vol, s.entryNum, *readBytesPerFile, *readBufferSize)
			stats.sampledBytesRead += n
			if readErr != nil && readErr != io.EOF {
				stats.readErrors++
			}
		}
		readDur = time.Since(readStart)
	}

	fmt.Println("=== Results ===")
	fmt.Printf("Open/parse time:       %s\n", openDur)
	fmt.Printf("Traversal time:        %s\n", walkDur)
	fmt.Printf("Directories visited:   %d\n", stats.directories)
	fmt.Printf("Files discovered:      %d\n", stats.files)
	fmt.Printf("Deleted dirs seen:     %d\n", stats.deletedDirs)
	fmt.Printf("Deleted files seen:    %d\n", stats.deletedFiles)
	fmt.Printf("Total file bytes:      %d\n", stats.totalFileBytes)

	totalEntries := stats.directories + stats.files
	if walkDur > 0 {
		fmt.Printf("Traversal rate:        %.2f entries/s\n", float64(totalEntries)/walkDur.Seconds())
	}

	fmt.Printf("Sampled files read:    %d\n", len(stats.sampledFiles))
	fmt.Printf("Read errors:           %d\n", stats.readErrors)
	fmt.Printf("Sampled bytes read:    %d\n", stats.sampledBytesRead)
	fmt.Printf("Read phase time:       %s\n", readDur)
	if readDur > 0 {
		mb := float64(stats.sampledBytesRead) / (1 << 20)
		fmt.Printf("Read throughput:       %.2f MiB/s\n", mb/readDur.Seconds())
	}
}

func normalizePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "/"
	}
	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func walkAndSample(volume *libntfs.Volume, root *libntfs.File, rootPath string, sampleLimit int, includeDeleted bool) (*walkStats, error) {
	stats := &walkStats{}
	visited := map[uint64]bool{root.EntryNumber(): true}
	stack := []stackItem{{dir: root, path: rootPath}}

	for len(stack) > 0 {
		i := len(stack) - 1
		item := stack[i]
		stack = stack[:i]

		entries, err := item.dir.ReadDir()
		if err != nil {
			return nil, fmt.Errorf("readdir %s: %w", item.path, err)
		}

		for _, entry := range entries {
			fullPath := strings.ReplaceAll(item.path+"/"+entry.Name, "//", "/")

			if entry.IsDirectory {
				if entry.Deleted {
					stats.deletedDirs++
					continue
				}

				stats.directories++
				if visited[entry.EntryNum] {
					continue
				}
				subdir, openErr := volume.Open(entry.EntryNum)
				if openErr != nil {
					continue
				}
				visited[entry.EntryNum] = true
				stack = append(stack, stackItem{dir: subdir, path: fullPath})
				continue
			}

			if entry.Deleted {
				stats.deletedFiles++
				if !includeDeleted {
					continue
				}
			}

			stats.files++
			stats.totalFileBytes += entry.Size

			if sampleLimit <= 0 || len(stats.sampledFiles) >= sampleLimit {
				continue
			}
			stats.sampledFiles = append(stats.sampledFiles, sampledFile{
				entryNum: entry.EntryNum,
				fullPath: fullPath,
				size:     entry.Size,
			})
		}
	}

	return stats, nil
}

func readSampledFile(volume *libntfs.Volume, entryNum uint64, maxBytes int64, bufSize int) (int64, error) {
	f, err := volume.Open(entryNum)
	if err != nil {
		return 0, err
	}
	if f.IsDirectory() {
		return 0, nil
	}

	buf := make([]byte, bufSize)
	remaining := maxBytes
	var total int64

	for remaining > 0 {
		chunk := len(buf)
		if int64(chunk) > remaining {
			chunk = int(remaining)
		}
		n, readErr := f.Read(buf[:chunk])
		total += int64(n)
		remaining -= int64(n)

		if readErr != nil {
			return total, readErr
		}
		if n == 0 {
			return total, io.EOF
		}
	}

	return total, nil
}
