package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aoiflux/libntfs"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Printf("Usage: %s <ntfs_volume> <file_path> <output_file>\n", os.Args[0])
		fmt.Println("\nExample:")
		fmt.Println("  ./extract /dev/sda1 /Windows/System32/notepad.exe notepad.exe")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	filePath := os.Args[2]
	outputPath := os.Args[3]

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

	// Open file
	fmt.Printf("Opening: %s\n", filePath)
	ntfsFile, err := volume.OpenPath(filePath)
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}

	if ntfsFile.IsDirectory() {
		log.Fatalf("%s is a directory, not a file", filePath)
	}

	fmt.Printf("File size: %d bytes\n", ntfsFile.Size())

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer outFile.Close()

	// Copy data
	fmt.Printf("Extracting to: %s\n", outputPath)

	buf := make([]byte, 64*1024) // 64 KB buffer
	bytesWritten := int64(0)

	for {
		n, err := ntfsFile.Read(buf)
		if n > 0 {
			if _, writeErr := outFile.Write(buf[:n]); writeErr != nil {
				log.Fatalf("Failed to write to output file: %v", writeErr)
			}
			bytesWritten += int64(n)
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("Failed to read file: %v", err)
		}
	}

	fmt.Printf("Successfully extracted %d bytes\n", bytesWritten)
}
