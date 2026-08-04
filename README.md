# libntfs

Thread-safe, production-focused NTFS parsing in pure Go.

libntfs reads NTFS volumes and disk images with strong validation, typed errors,
and an API designed for tooling, forensics workflows, and systems integration.

## Why libntfs

- Thread-safe public API suitable for concurrent reads
- Zero external dependencies (Go standard library only)
- Strong bounds checking and corruption-aware parsing
- Typed errors for robust caller-side handling
- Practical examples for listing, traversal, and extraction

## Installation

```bash
go get github.com/aoiflux/libntfs
```

## Go Version

- Requires Go 1.25+

## Quick Start

```go
package main

import (
    "fmt"
    "log"
    "os"

    "github.com/aoiflux/libntfs"
)

func main() {
    img, err := os.Open("disk.img") // or raw device path
    if err != nil {
        log.Fatal(err)
    }
    defer img.Close()

    vol, err := libntfs.Open(img)
    if err != nil {
        log.Fatal(err)
    }
    defer vol.Close()

    f, err := vol.GetRootDirectory()
    if err != nil {
        log.Fatal(err)
    }

    entries, err := f.ReadDir()
    if err != nil {
        log.Fatal(err)
    }

    for _, e := range entries {
        kind := "FILE"
        if e.IsDirectory {
            kind = "DIR "
        }
        fmt.Printf("[%s] %s (%d bytes)\n", kind, e.Name, e.Size)
    }
}
```

## Feature Support

Implemented:

- NTFS boot sector parsing and validation
- MFT parsing with cache-backed entry lookup
- Resident and non-resident attribute parsing
- Data run parsing and sparse run handling
- File and directory access by MFT entry and path
- Directory index parsing ($INDEX_ROOT and $INDEX_ALLOCATION)
- Deleted directory entry recovery from index slack (flagged via
  DirEntry.Deleted)
- Update sequence (fixup) validation
- Standalone $MFT record parsing without a volume
- File name namespace handling (DOS/Win32/WinDOS/POSIX)
- Typed error model with context wrappers (including path traversal errors)

Current limitations:

- Encrypted file data: detection only (returns ErrEncryptedData)
- Standalone $MFT records expose metadata and data run maps, but their content
  cannot be read: that requires cluster access, which only a Volume provides
- Write support is limited to in-place overwrite of non-resident, non-sparse,
  non-compressed, non-encrypted streams on writable handles (no resident writes
  and no file growth/truncation)

## API Highlights

Volume-level:

- Open(reader io.ReaderAt) (*Volume, error)
- (*Volume).Open(entryNum uint64) (*File, error)
- (*Volume).OpenPath(path string) (*File, error)
- (*Volume).GetRootDirectory() (*File, error)

MFT-level:

- ParseMFTRecord(buf []byte, entryNum uint64, recordSize uint32) (*MFTEntry, error)
- (*Volume).GetMFTEntry(entryNum uint64) (*MFTEntry, error)
- (*Volume).MFTEntryCount() (uint64, error)
- (*Volume).EachMFTEntry(fn func(entryNum uint64, e *MFTEntry) error) error

File-level:

- (*File).Read(p []byte) (int, error)
- (*File).ReadAt(p []byte, off int64) (int, error)
- (*File).Write(p []byte) (int, error)
- (*File).WriteAt(p []byte, off int64) (int, error)
- (*File).ReadAll() ([]byte, error)
- (*File).ReadDir() ([]DirEntry, error)
- (*File).ReadSupport() FileReadSupport

Attribute-level:

- (*Attribute).ReadSupport() FileReadSupport

`ReadDir` returns both allocated and recoverable deleted names. Use
`DirEntry.Deleted` to distinguish deleted entries.

Use `ReadSupport` to check whether the primary `$DATA` stream is readable, and
whether it is resident, sparse, compressed, or encrypted, before calling `Read`,
`ReadAt`, or `ReadAll`.

Use `(*Attribute).ReadSupport()` for alternate or named `$DATA` streams when you
are inspecting attributes directly.

## Parsing a Standalone $MFT

`ParseMFTRecord` parses one MFT record from a raw buffer with no volume, which
covers `$MFT` files extracted by tools such as KAPE, FTK Imager, or icat.
Unparsable records are normal in an extracted table, so skip them and continue.

```go
mft, err := os.Open("$MFT")
if err != nil {
    log.Fatal(err)
}
defer mft.Close()

buf := make([]byte, libntfs.DefaultMFTRecordSize)
for entryNum := uint64(0); ; entryNum++ {
    if _, err := io.ReadFull(mft, buf); err != nil {
        break // io.EOF or io.ErrUnexpectedEOF ends the table
    }

    entry, err := libntfs.ParseMFTRecord(buf, entryNum, libntfs.DefaultMFTRecordSize)
    if err != nil {
        continue // Unallocated, bad, or corrupt record
    }

    name, err := entry.GetFileName()
    if err != nil {
        continue
    }

    si, err := entry.GetStandardInformation()
    if err != nil {
        continue
    }

    fmt.Printf("%d\t%s\t%s\n", entryNum, si.ModifyTime.Format(time.RFC3339), name.Name)
}
```

`ParseMFTRecord` applies the update sequence fixups in place, so pass a copy if
you need the original bytes. Resident attributes parse fully; non-resident
attributes have their data runs decoded but not resolved, since reading content
needs the volume the records came from.

To walk the MFT of a live volume instead, use `MFTEntryCount` to get the bound
and tolerate per-entry errors, or let `EachMFTEntry` do both:

```go
err := vol.EachMFTEntry(func(entryNum uint64, e *libntfs.MFTEntry) error {
    name, err := e.GetFileName()
    if err != nil {
        return nil // Keep going: no usable $FILE_NAME on this record
    }
    fmt.Printf("%d\t%s\n", entryNum, name.Name)
    return nil
})
```

## Error Handling

libntfs uses standard wrapping semantics so errors.Is and errors.As work
reliably.

```go
import (
    "errors"
    "fmt"

    "github.com/aoiflux/libntfs"
)

f, err := vol.OpenPath("/Windows/System32/nope.dll")
if err != nil {
    if errors.Is(err, libntfs.ErrFileNotFound) {
        // caller-level not-found logic
    }

    var pErr *libntfs.PathError
    if errors.As(err, &pErr) {
        fmt.Printf("op=%s path=%s component=%s\n", pErr.Op, pErr.Path, pErr.Component)
    }
}

_ = f
```

## Platform Notes

Raw volume access usually requires elevated privileges.

Windows:

- Run terminal as Administrator
- Use paths like \\.\C: or \\.\PhysicalDrive0

Linux:

- Use block-device paths like /dev/sda1
- Prefer read-only/forensic-safe workflows

You can also use disk image files directly on all platforms.

## Examples

- examples/basic: open volume, show metadata, list root directory
- examples/traverse: recursive traversal and size statistics
- examples/extract: extract a file from NTFS to local output
- examples/windows_drive: Windows-focused raw-drive workflow
- examples/bench: end-to-end traversal and sampled read throughput benchmark
- examples/finfo: structured metadata for a specific file path (offsets, size,
  fragments, timestamps)
- examples/overwrite: guarded in-place overwrite at logical file offsets with
  user-supplied end-bound checks
- examples/mftdump: MFT record timeline from either a standalone $MFT file or a
  full volume

Run one example:

```bash
cd examples/basic
go run . <ntfs_volume_or_image>
```

## Performance Notes

- MFT cache reduces repeated metadata reads
- Internal buffer pooling reduces allocation pressure
- Concurrent readers are supported by design

## Development

Run checks:

```bash
go test ./...
go vet ./...
```

## Project Status

Beta. Core parsing functionality is implemented and tested, with ongoing
hardening around edge cases and malformed metadata.

## License

MIT. See LICENSE.
