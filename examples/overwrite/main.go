package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libntfs"
)

func usage() {
	fmt.Printf("Usage: %s <ntfs_volume_or_image> <file_path> <offset> <data> [end_offset]\n", os.Args[0])
	fmt.Println()
	fmt.Println("Arguments:")
	fmt.Println("  ntfs_volume_or_image  Path to NTFS image/device (must be writable)")
	fmt.Println("  file_path             NTFS path, e.g. /Windows/System32/drivers/etc/hosts")
	fmt.Println("  offset                Logical file offset in bytes (supports 0x...)")
	fmt.Println("  data                  Raw string bytes to write")
	fmt.Println("  end_offset            Optional logical end boundary (exclusive, supports 0x...)")
	fmt.Println()
	fmt.Println("Safety guard rails:")
	fmt.Println("  1) offset must be within file size")
	fmt.Println("  2) offset+len(data) must be <= end_offset")
	fmt.Println("  3) end_offset must be <= file size")
	fmt.Println("  4) write cannot extend the file")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  ./overwrite disk.img /notes.txt 0x10 HELLO 0x40")
	fmt.Println("  overwrite.exe \\\\.\\C: /notes.txt 0x10 HELLO 0x40   # auto-locks raw Windows volume")
}

func parseOffset(argName, raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", argName, raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must be >= 0", argName)
	}
	return v, nil
}

func main() {
	if len(os.Args) < 5 || len(os.Args) > 6 {
		usage()
		os.Exit(1)
	}

	volumePath := os.Args[1]
	filePath := os.Args[2]
	offsetRaw := os.Args[3]
	dataRaw := os.Args[4]

	offset, err := parseOffset("offset", offsetRaw)
	if err != nil {
		log.Fatal(err)
	}

	data := []byte(dataRaw)
	if len(data) == 0 {
		log.Fatal("data must not be empty")
	}

	fh, err := os.OpenFile(volumePath, os.O_RDWR, 0)
	if err != nil {
		log.Fatalf("failed to open volume for read/write: %v", err)
	}
	defer fh.Close()

	unlockVolume, err := tryLockVolumeForWrite(volumePath, fh)
	if err != nil {
		log.Fatalf("failed to lock volume for write: %v", err)
	}
	defer unlockVolume()

	vol, err := libntfs.Open(fh)
	if err != nil {
		log.Fatalf("failed to parse NTFS volume: %v", err)
	}
	defer vol.Close()

	f, err := vol.OpenPath(filePath)
	if err != nil {
		log.Fatalf("failed to open NTFS path %q: %v", filePath, err)
	}

	if f.IsDirectory() {
		log.Fatalf("path %q is a directory", filePath)
	}

	size := f.Size()
	if offset >= size {
		log.Fatalf("offset %d is outside file size %d", offset, size)
	}

	endOffset := size
	if len(os.Args) == 6 {
		endOffset, err = parseOffset("end_offset", os.Args[5])
		if err != nil {
			log.Fatal(err)
		}
	}

	if endOffset <= offset {
		log.Fatalf("end_offset (%d) must be greater than offset (%d)", endOffset, offset)
	}
	if endOffset > size {
		log.Fatalf("end_offset %d exceeds file size %d", endOffset, size)
	}

	writeEnd := offset + int64(len(data))
	if writeEnd > endOffset {
		log.Fatalf("refusing write: [%d,%d) exceeds allowed range ending at %d", offset, writeEnd, endOffset)
	}
	if writeEnd > size {
		log.Fatalf("refusing write: [%d,%d) exceeds file size %d", offset, writeEnd, size)
	}

	n, err := f.WriteAt(data, offset)
	if err != nil {
		if errors.Is(err, libntfs.ErrWriteUnsupported) {
			log.Fatalf("write unsupported for this file stream: %v", err)
		}
		log.Fatalf("write failed: %v", err)
	}
	if n != len(data) {
		log.Fatalf("short write: wrote %d of %d bytes", n, len(data))
	}

	verify := make([]byte, len(data))
	vn, rerr := f.ReadAt(verify, offset)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		log.Fatalf("verify read failed: %v", rerr)
	}
	if vn != len(data) {
		log.Fatalf("verify read short: read %d of %d bytes", vn, len(data))
	}
	if string(verify) != string(data) {
		log.Fatalf("verify mismatch: wrote %q but read back %q", string(data), string(verify))
	}

	fmt.Printf("write ok: path=%q offset=%d bytes=%d allowed_end=%d\n", filePath, offset, len(data), endOffset)
}
