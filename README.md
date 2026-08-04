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
- Alternate data stream enumeration and reading
- Security descriptor parsing with $Secure lookup (owner, group, DACL, SACL)
- USN change journal parsing from a volume or an extracted $J file
- File name namespace handling (DOS/Win32/WinDOS/POSIX)
- Typed error model with context wrappers (including path traversal errors)

Current limitations:

- Encrypted file data: detection only (returns ErrEncryptedData)
- Standalone $MFT records expose metadata and data run maps, but their content
  cannot be read: that requires cluster access, which only a Volume provides
- Write support is limited to in-place overwrite on writable handles: no file
  growth or truncation, no sparse, compressed, or encrypted streams, and a
  resident value cannot change length

## API Highlights

Volume-level:

- Open(reader io.ReaderAt) (*Volume, error)
- (*Volume).Open(entryNum uint64) (*File, error)
- (*Volume).OpenPath(path string) (*File, error)
- (*Volume).GetRootDirectory() (*File, error)

MFT-level:

- ParseMFTRecord(buf []byte, entryNum uint64, recordSize uint32) (*MFTEntry, error)
- ParseMFTRecordInto(entry *MFTEntry, buf []byte, entryNum uint64, recordSize uint32) error
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

Stream-level:

- (*File).Streams() []StreamInfo
- (*File).OpenStream(name string) (*File, error)
- (*MFTEntry).DataStreams() []*Attribute

Journal-level:

- ParseUSNRecord(buf []byte) (*USNRecord, int, error)
- EachUSNRecord(buf []byte, fn func(*USNRecord) error) error
- (*Volume).OpenUSNJournal() (*File, error)
- (*Volume).EachUSNRecord(fn func(*USNRecord) error) error

Security-level:

- ParseSecurityDescriptor(buf []byte) (*SecurityDescriptor, error)
- (*File).SecurityDescriptor() (*SecurityDescriptor, error)
- (*Volume).SecurityDescriptorByID(securityID uint32) (*SecurityDescriptor, error)
- (*Volume).EachSecurityDescriptor(fn func(uint32, *SecurityDescriptor) error) error

Attribute-level:

- (*Attribute).ReadSupport() FileReadSupport

`ReadDir` returns both allocated and recoverable deleted names. Use
`DirEntry.Deleted` to distinguish deleted entries.

Use `ReadSupport` to check whether the primary `$DATA` stream is readable, and
whether it is resident, sparse, compressed, or encrypted, before calling `Read`,
`ReadAt`, or `ReadAll`.

`WriteAt` overwrites both non-resident and resident streams in place. A
non-resident stream is written to its clusters; a resident one, whose content
sits inside the MFT record, is patched by rewriting that record with its update
sequence reapplied and evicting the cached entry. Neither path changes a file's
length, and a resident value cannot grow, since that would mean resizing the
attribute and shifting every attribute after it in the record.

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

For a multi-million-record sweep, `ParseMFTRecordInto` parses into an entry you
own, so the loop stops allocating a fresh one per record:

```go
entry := &libntfs.MFTEntry{}
for entryNum := uint64(0); ; entryNum++ {
    if _, err := io.ReadFull(mft, buf); err != nil {
        break
    }
    if err := libntfs.ParseMFTRecordInto(entry, buf, entryNum, libntfs.DefaultMFTRecordSize); err != nil {
        continue
    }
    // Consume entry before the next iteration overwrites it.
}
```

Each call invalidates the previous one's results, so copy anything you intend to
keep.

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

## Alternate Data Streams and Access Control

Every `$DATA` stream on a file is reachable, not just the default one. Because
`OpenStream` hands back a `File`, the ordinary read API works on an alternate
stream, compression and sparse handling included.

```go
f, err := vol.OpenPath("/Users/bob/download.exe")
if err != nil {
    log.Fatal(err)
}

for _, s := range f.Streams() {
    name := s.Name
    if name == "" {
        name = "(unnamed)"
    }
    fmt.Printf("%-20s %d bytes\n", name, s.Size)
}

zone, err := f.OpenStream("Zone.Identifier")
if err == nil {
    data, _ := zone.ReadAll()
    fmt.Printf("%s", data)
}
```

Security descriptors resolve through `$Secure`, so a file answers who could
reach it:

```go
sd, err := f.SecurityDescriptor()
if err != nil {
    log.Fatal(err)
}

fmt.Printf("owner: %s\n", sd.Owner)
if sd.DACL == nil {
    // An absent DACL grants everyone full access; an empty one denies everyone.
    fmt.Println("dacl: absent (grants everyone)")
} else {
    for _, ace := range sd.DACL.ACEs {
        fmt.Printf("%s %s %v\n", ace.TypeName(), ace.Trustee, ace.RightsNames())
    }
}
```

`(*SID).WellKnownName` maps the common SIDs to names such as
`BUILTIN\Administrators`. `$Secure:$SDS` is read and indexed on first use and
cached for the life of the volume, so per-file lookups are cheap.

## USN Change Journal

The journal records every create, delete, rename, and write, which makes it the
densest activity timeline on a volume. Read it from a volume:

```go
err := vol.EachUSNRecord(func(r *libntfs.USNRecord) error {
    fmt.Printf("%d\t%s\t%s\t%v\n",
        r.USN, r.Timestamp.Format(time.RFC3339), r.Name, r.ReasonNames())
    return nil
})
```

`$J` is routinely gigabytes, so the volume walk reads it in chunks and skips the
sparse region at its front using the stream's data runs rather than scanning
through it.

An extracted `$J` file parses with no volume at all:

```go
data, err := os.ReadFile("$J")
if err != nil {
    log.Fatal(err)
}

err = libntfs.EachUSNRecord(data, func(r *libntfs.USNRecord) error {
    if r.Reason&libntfs.USNReasonFileDelete != 0 {
        fmt.Printf("deleted: %s (entry %d)\n", r.Name, r.FileReference)
    }
    return nil
})
```

Zero-filled and malformed regions are skipped rather than ending the walk, so a
partially damaged journal still yields the records around the damage. Version 4
extent records carry no timestamp or file name and report that rather than
inventing one.

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
- examples/streams: data streams and access control for a path or MFT entry,
  with alternate-stream hunting and extraction
- examples/usnjrnl: USN change journal timeline from a volume or an extracted $J

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
