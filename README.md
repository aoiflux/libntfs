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
- Fragment mapping on both axes: where a region sits in the stream and where its
  bytes sit on the volume, holes included
- Extended attribute parsing ($EA and $EA_INFORMATION), including WSL metadata
- Reparse point parsing with target resolution for symlinks, junctions, and WSL
  symlinks
- WSL POSIX metadata decoding (mode, owner, group, POSIX timestamps) from both
  the WSL2 LXATTRB layout and WSL1's separate attributes, plus POSIX extended
  attributes from LXXATTR
- Security descriptor parsing with $Secure lookup (owner, group, DACL, SACL)
- USN change journal parsing from a volume or an extracted $J file
- $LogFile transaction log parsing, including records split across page
  boundaries, from a volume or an extracted $LogFile
- File name namespace handling (DOS/Win32/WinDOS/POSIX)
- Typed error model with context wrappers (including path traversal errors)

Current limitations:

- Encrypted file data: detection only (returns ErrEncryptedData)
- $LogFile is read, not replayed: records are decoded and handed over, but
  reconstructing file content from redo data, undoing to a consistent state, and
  replaying transactions are out of scope
- $LogFile dirty page and transaction table entries are exposed raw rather than
  decoded; the open attribute table is decoded, so LogRecord.TargetAttribute
  resolves to a file and stream through (*Volume).LogAttributeTable
- Log operations above 0x22 are undocumented and reported as hex. One (0x25)
  occurs on real volumes and is described in logfile.go, but not named
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

Layout-level:

- (*File).Fragments() ([]FileFragment, error)
- (*File).IsFragmented() bool
- (*Volume).AttributeFragments(attr *Attribute) ([]FileFragment, error)

Metadata-level:

- ParseExtendedAttributes(buf []byte) ([]ExtendedAttribute, error)
- EachExtendedAttribute(buf []byte, fn func(ExtendedAttribute) error) error
- ParseEAInformation(buf []byte) (*EAInfo, error)
- (*File).ExtendedAttributes() ([]ExtendedAttribute, error)
- (*File).EAInformation() (*EAInfo, error)
- ParseReparsePoint(buf []byte) (*ReparsePoint, error)
- (*File).ReparsePoint() (*ReparsePoint, error)
- (*ReparsePoint).Target() (substituteName, printName string, ok bool)
- ParseLinuxAttributes(buf []byte) (*LinuxAttributes, error)
- (*File).LinuxAttributes() (*LinuxAttributes, error)
- ParseLinuxExtendedAttributes(buf []byte) ([]LinuxExtendedAttribute, error)
- (*File).LinuxExtendedAttributes() ([]LinuxExtendedAttribute, error)

Journal-level:

- ParseUSNRecord(buf []byte) (*USNRecord, int, error)
- EachUSNRecord(buf []byte, fn func(*USNRecord) error) error
- (*Volume).OpenUSNJournal() (*File, error)
- (*Volume).EachUSNRecord(fn func(*USNRecord) error) error

Log-level:

- ParseLogRestartPage(buf []byte) (*LogRestartArea, error)
- ParseLogRecord(buf []byte) (*LogRecord, int, error)
- EachLogRecord(buf []byte, fn func(*LogRecord) error) error
- (*Volume).OpenLogFile() (*File, error)
- (*Volume).LogRestartArea() (*LogRestartArea, error)
- (*Volume).EachLogRecord(fn func(*LogRecord) error) error
- GroupLogTransactions(records []*LogRecord) []LogTransaction
- (*Volume).LogAttributeTable() (*LogAttributeTable, error)
- (*LogAttributeTable).Resolve(record *LogRecord) (LogOpenAttributeEntry, bool)
- ParseLogOpenAttributeTable(buf []byte) ([]LogOpenAttributeEntry, error)
- ParseLogAttributeNames(buf []byte) (map[uint32]string, error)
- ParseLogRestartTable(buf []byte) (*LogRestartTable, error)
- (*LogRecord).RestartTable() (*LogRestartTable, error)

Security-level:

- ParseSecurityDescriptor(buf []byte) (*SecurityDescriptor, error)
- (*File).SecurityDescriptor() (*SecurityDescriptor, error)
- (*Volume).SecurityDescriptorByID(securityID uint32) (*SecurityDescriptor, error)
- (*Volume).EachSecurityDescriptor(fn func(uint32, *SecurityDescriptor) error) error

Attribute-level:

- (*Attribute).ReadSupport() FileReadSupport

`ReadDir` returns both allocated and recoverable deleted names. Use
`DirEntry.Deleted` to distinguish deleted entries.

On a damaged volume `ReadDir` returns the entries it could parse *together with*
a `*PartialDirError`, so both return values can be non-nil. A walk over a whole
volume can then record the loss and descend into what survived, instead of
losing the subtree:

```go
entries, err := dir.ReadDir()

var partial *libntfs.PartialDirError
if errors.As(err, &partial) {
    log.Printf("directory %d: %d names recovered, %d index records lost",
        partial.EntryNum, partial.Recovered, partial.Skipped)
} else if err != nil {
    return err // Nothing could be read at all
}

for _, entry := range entries {
    // ...
}
```

A nil slice with a non-nil error means nothing could be read; the error wraps
`ErrCorruptIndex` or `ErrNotDirectory`. Code that only wants complete listings
can keep testing `err != nil` and ignore the entries. `ListFiles` and
`ListDirectories` follow the same contract.

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

## File Layout and Fragmentation

`Fragments` maps a stream onto the volume, reporting both where each region sits
in the stream and where its bytes sit on disk:

```go
frags, err := file.Fragments()
if err != nil {
    log.Fatal(err)
}

for _, f := range frags {
    if f.Sparse {
        fmt.Printf("hole   stream %d..%d\n", f.FileOffset, f.FileOffset+f.Length)
        continue
    }
    fmt.Printf("extent stream %d..%d -> volume %d..%d\n",
        f.FileOffset, f.FileOffset+f.Length, f.StartOffset, f.EndOffset)
}
```

Fragments tile the stream with no gaps, so a byte range of the file maps onto the
volume by walking them. Sparse holes are reported rather than dropped, because
dropping one would silently shift every later fragment's position in the stream.
The last fragment is clamped to the stream's real size, so allocation slack at
the tail of the final cluster is not reported as content.

Because the layout comes from the stream the handle was opened on, an alternate
data stream maps with no extra work:

```go
ads, err := file.OpenStream("Zone.Identifier")
if err != nil {
    log.Fatal(err)
}
frags, err := ads.Fragments()
```

A resident stream, whose content lives inside the MFT record, reports a single
fragment with `Resident` set and `StartOffset` pointing at those bytes. Read it
through `ReadAt` rather than off the volume directly: the record carries update
sequence fixups, so the last two bytes of each sector hold the record's sequence
number rather than content.

`IsFragmented` reports whether the stream is split across more than one region of
the volume. A hole is not fragmentation — a file stored in one run with a hole
punched in it is not scattered across the disk.

For a compressed stream the data runs describe compression units rather than
decompressed bytes, so the fragments describe physical allocation only. Gate on
`IsCompressed` before treating fragment offsets as stream offsets.

`(*Volume).AttributeFragments` does the same for any attribute, which is how to
map streams that are not reachable as a `File` — `$Bitmap`, `$Secure:$SDS`,
`$UsnJrnl:$J`.

## Extended Attributes and Reparse Points

Extended attributes are where the Windows Subsystem for Linux keeps POSIX
metadata that no Windows tool surfaces: `LXATTRB` holds mode, owner, and the
POSIX timestamps, `LXXATTR` holds user extended attributes, and `$LXUID`,
`$LXGID`, and `$LXMOD` carry those fields for files created by WSL1.

```go
eas, err := file.ExtendedAttributes()
if err != nil {
    log.Fatal(err)
}

for _, ea := range eas {
    fmt.Printf("%s: %d bytes (need-EA: %v)\n", ea.Name, len(ea.Value), ea.NeedEA())
}
```

A `$EA` is frequently non-resident, and is read through its data runs when it is.
`ParseExtendedAttributes` and `ParseEAInformation` need no volume, so attribute
bytes carved out of a standalone `$MFT` parse the same way.

A reparse point turns an ordinary directory entry into a redirection. `Target`
resolves the ones that name a path — symbolic links, mount points (junctions),
and WSL symlinks — and reports `ok == false` for the tags that do not, such as
deduplication, WOF-compressed files, and cloud placeholders:

```go
reparse, err := file.ReparsePoint()
if err != nil {
    log.Fatal(err)
}

if reparse != nil {
    if target, _, ok := reparse.Target(); ok {
        fmt.Printf("%s -> %s\n", reparse.TagName(), target)
    } else {
        fmt.Printf("%s (%d bytes of opaque data)\n", reparse.TagName(), len(reparse.Data))
    }
}
```

Both buffer shapes are handled: Microsoft-owned tags, whose data follows the
header directly, and third-party tags, which carry a 16-byte owner GUID first.
`Data` holds the tag-specific bytes with the header and any GUID stripped.

libntfs does not follow reparse points. A directory carrying one is reported as a
directory, and `ReadDir` lists its own index rather than the target's.

### WSL POSIX metadata

`LinuxAttributes` decodes what WSL stores in those extended attributes into
typed fields:

```go
attrs, err := file.LinuxAttributes()
if err != nil {
    log.Fatal(err)
}

if attrs != nil {
    fmt.Printf("%s %d:%d  %s\n",
        attrs.ModeString(), attrs.UID, attrs.GID, attrs.ModifyTime)
}
```

This is metadata no Windows tool surfaces and no NTFS structure otherwise
records. The POSIX timestamps are tracked separately from the NTFS ones in
`$STANDARD_INFORMATION` and can disagree with them, which is occasionally the
interesting part.

WSL2 writes one `LXATTRB` attribute; WSL1 wrote the same fields as separate
`$LXUID`, `$LXGID`, `$LXMOD`, and `$LXDEV` attributes. Both are read, and
`FromLegacyAttributes` reports which. A POSIX zero timestamp reads as the zero
time rather than 1970, so an absent timestamp does not become a 1970 entry in a
timeline.

POSIX extended attributes — the `user.*` and `security.*` names `getfattr` shows
— live in a separate `LXXATTR` attribute:

```go
xattrs, err := file.LinuxExtendedAttributes()
if err != nil {
    log.Fatal(err)
}

for _, x := range xattrs {
    fmt.Printf("%s = %q\n", x.Name, x.Value)
}
```

A file commonly has `LXATTRB` and not `LXXATTR`, so an empty result is the normal
case rather than a failure.

`LXXATTR` is worth one caveat: it is not a documented structure. The layout
libntfs decodes matches what WSL writes, but Microsoft publishes no specification
for it. Every field is bounds-checked against the buffer, so a layout change
surfaces as a parse error rather than as plausible nonsense — but validate
against your own corpus before relying on it.

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

## Transaction Log ($LogFile)

`$LogFile` records the metadata operations NTFS was about to perform. It is finer
grained than the USN journal — it captures the individual attribute writes behind
each change — and being circular, it covers a short window in far more detail
than anything else on the volume.

Start with the restart area, which says how recent the log's contents are:

```go
restart, err := vol.LogRestartArea()
if err != nil {
    log.Fatal(err)
}

fmt.Printf("current LSN %d, %d bytes of log, opened %d times\n",
    restart.CurrentLSN, restart.FileSize, restart.RestartOpenCount)
```

Two copies are written, in the first two pages. The second is used when the first
cannot be read, which is exactly the redundancy NTFS keeps them for.

Then walk the records:

```go
err := vol.EachLogRecord(func(r *libntfs.LogRecord) error {
    if !r.IsClientRecord() {
        return nil // A client restart marker, not an operation
    }
    fmt.Printf("LSN %d  txn %d  %s / %s  vcn %d\n",
        r.LSN, r.TransactionID,
        r.RedoOperationName(), r.UndoOperationName(), r.TargetVCN)
    return nil
})
```

An extracted `$LogFile` parses with no volume at all, the same way `$J` does:

```go
data, err := os.ReadFile("$LogFile")
if err != nil {
    log.Fatal(err)
}

err = libntfs.EachLogRecord(data, func(r *libntfs.LogRecord) error { ... })
```

Two things are worth knowing before building on this.

**The log is circular.** It holds the most recent operations and overwrites the
oldest, so records arrive in whatever order the ring currently holds rather than
oldest-first. Sort by `LSN` for a timeline.

**libntfs reads the log; it does not replay it.** `RedoData` and `UndoData` are
handed over as NTFS wrote them — for an `UpdateResidentValue`, the attribute
bytes after and before the change. Interpreting them depends entirely on the
operation, and reconstructing file content from redo data, undoing to a
consistent state, and replaying transactions are deliberately out of scope.

Records split across a page boundary are reassembled. The log's data area is one
logical byte stream chopped into fixed-size pages, each with its own header and
update sequence, so a record can start near the end of one page and finish in the
next. Damaged and uninitialised pages are skipped rather than ending the walk.

### Transactions

A single user-visible action is several log records sharing a transaction ID, and
reading them individually loses the fact that they belong together.
`GroupLogTransactions` restores it:

```go
var records []*libntfs.LogRecord
err := vol.EachLogRecord(func(r *libntfs.LogRecord) error {
    records = append(records, r)
    return nil
})

for _, t := range libntfs.GroupLogTransactions(records) {
    fmt.Printf("txn %d  lsn %d..%d  committed=%v  %v\n",
        t.ID, t.FirstLSN, t.LastLSN, t.Committed, t.Operations())
}
```

`Committed` and `Forgotten` say how a transaction ended. Neither being set means
the log's window does not contain the end of it, which for the oldest
transactions in a circular log is the normal case rather than a sign of
interrupted work.

### Naming what a record touched

A log record does not carry the file it operated on. NTFS names a file once, when
it opens an attribute, records that in the open attribute table, and every later
record carries only an offset into that table. Read without it, a log is a list
of operations with no subject: `UpdateResidentValue on attribute 0x18`.

NTFS periodically dumps that table into the log so recovery can rebuild it, which
is what makes this recoverable from the log alone:

```go
table, err := vol.LogAttributeTable()
if err != nil {
    log.Fatal(err)
}

err = vol.EachLogRecord(func(r *libntfs.LogRecord) error {
    entry, ok := table.Resolve(r)
    if !ok {
        return nil // The log wrapped past the dump covering this attribute
    }
    fmt.Printf("LSN %d  %s  ->  MFT entry %d %s
",
        r.LSN, r.RedoOperationName(), entry.RecordNumber, entry.AttributeTypeName())
    return nil
})
```

`entry.Name` carries the stream name where there is one — `$I30` on a directory
index, `$SDS` on `$Secure`, `$Q` on `$Quota` — merged in from the separate
`AttributeNamesDump` records. On a real volume this resolved 98% of the records
that name an attribute.

The entry layout is not from a published specification. It was derived from a
real volume and cross-checked field by field; the reasoning is recorded in
[logtable.go](logtable.go) so it can be argued with. Two fields per entry remain
unidentified and are deliberately not exposed — `Raw` carries the whole entry for
anyone who wants to go further.

`ParseLogRestartTable` gives the same treatment to the dirty page and transaction
tables, whose per-entry layouts libntfs does not decode: the header is parsed and
the entries handed over raw, with `EntrySize` saying how to slice them.

One shape to know about: an `AttributeNamesDump` is a table dump but *not* a
restart table — its data is a bare run of names with no table header. `IsTableDump`
covers both; `IsRestartTableDump` and `RestartTable` exclude it, and
`AttributeNames` reads it.


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

Damaged structures are reported rather than fatal. `ErrCorruptIndex` and
`ErrTruncatedRecord` both wrap `ErrInvalidIndex`, so a caller can match the
specific cause or the whole family:

```go
entries, err := dir.ReadDir()
if errors.Is(err, libntfs.ErrInvalidIndex) {
    // entries still holds whatever survived; see PartialDirError above.
}
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
- examples/attrinfo: extended attributes and reparse point for a path or MFT
  entry, with a recursive hunt for WSL metadata and links
- examples/overwrite: guarded in-place overwrite at logical file offsets with
  user-supplied end-bound checks
- examples/mftdump: MFT record timeline from either a standalone $MFT file or a
  full volume
- examples/streams: data streams and access control for a path or MFT entry,
  with alternate-stream hunting and extraction
- examples/usnjrnl: USN change journal timeline from a volume or an extracted $J
- examples/logdump: $LogFile transaction log from a volume or an extracted
  $LogFile, with operation filtering, LSN sorting, and transaction grouping

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

The attribute parsers are fed bytes lifted straight off a disk image, which in a
forensic context is untrusted input by definition. Each has a fuzz target:

```bash
go test -run '^$' -fuzz FuzzParseExtendedAttributes -fuzztime 60s
go test -run '^$' -fuzz FuzzParseReparsePoint -fuzztime 60s
go test -run '^$' -fuzz FuzzParseDataRuns -fuzztime 60s
```

Eighteen targets cover every parser fed image bytes; `go test -list 'Fuzz.*' .`
lists them. CI runs a 20-second smoke pass of each on every change and ten
minutes per target nightly, carrying the generated corpus between runs.

`FuzzDirEntryAccumulator` is the odd one out: it takes already-parsed index
entries rather than image bytes, and asserts that the positions the directory
listing records for deduplication always address a live entry. That invariant
is not reachable from any parser input, which is exactly why it needs a target
of its own.

`Report()` is covered by a golden-file test, since its JSON is the library's
widest output surface:

```bash
go test -run TestReportGolden           # compare against testdata/report.golden.json
go test -run TestReportGolden -update   # rewrite it after an intended change
```

Most tests build NTFS structures byte by byte, which proves the parsers against
bytes libntfs itself wrote. That is not enough on its own: a parser and its
fixtures can share a wrong assumption indefinitely and agree with each other the
whole time. Two bugs found this way were invisible to the synthetic suite —
every directory on a real volume was reported as a file, and index-slack recovery
emitted phantom deleted files with mojibake names.

So point the tests at a real volume image. Any raw NTFS image will do, since
these tests assert invariants rather than filenames:

```bash
LIBNTFS_TEST_IMAGE=/path/to/ntfs.dd go test -run TestCorpus ./...
```

They check that `ReadDir`'s directory flag agrees with the MFT record for every
entry, that every file's fragments tile its stream exactly and land inside the
volume, that the first and last byte of every file read back and one past the end
reports EOF, that `Report` and the directory walk agree on what exists, that
`$Secure` resolves, and that walking every MFT record is clean.

For the fixture-specific tests — named files carrying one of each shape,
including WSL metadata — generate the small purpose-built volume instead, from an
**elevated** PowerShell prompt:

```powershell
.\testdata\make_corpus.ps1
```

The script creates a VHD, formats it NTFS, writes the fixtures — fragmented,
sparse, compressed, and resident files, an alternate data stream, a symlink, a
junction, a deleted entry, and WSL metadata if a distribution is installed —
and copies the raw volume out to `testdata/ntfs-corpus.img.gz`. On Linux,
`mkntfs` produces an equivalent image without elevation, but that is ntfs-3g
writing the structures rather than Windows, which is the point of the fixture.

## Project Status

Beta. Core parsing functionality is implemented and tested, with ongoing
hardening around edge cases and malformed metadata.

## License

MIT. See LICENSE.
