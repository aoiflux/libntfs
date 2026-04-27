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
- File name namespace handling (DOS/Win32/WinDOS/POSIX)
- Typed error model with context wrappers (including path traversal errors)

Current limitations:

- Encrypted file data: detection only (returns ErrEncryptedData)

## API Highlights

Volume-level:

- Open(reader io.ReaderAt) (*Volume, error)
- (*Volume).Open(entryNum uint64) (*File, error)
- (*Volume).OpenPath(path string) (*File, error)
- (*Volume).GetRootDirectory() (*File, error)

File-level:

- (*File).Read(p []byte) (int, error)
- (*File).ReadAt(p []byte, off int64) (int, error)
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
