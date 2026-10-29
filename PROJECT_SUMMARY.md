# libntfs - Project Summary

## Overview

libntfs is a thread-safe, production-focused Go library for parsing NTFS (New
Technology File System) volumes and disk images. It emphasizes robust
validation, typed error handling, and practical APIs for tooling and
forensic-style workflows.

## Current State

- Status: Beta
- Go version: 1.25+
- Dependencies: standard library only
- Platform support: Windows, Linux, macOS

## Core Capabilities

Implemented:

- NTFS boot sector parsing with validation
- MFT parsing and cache-backed entry access
- Resident and non-resident attribute parsing
- Data run parsing with sparse run support
- Path-based and entry-based open operations
- Directory index parsing ($INDEX_ROOT and $INDEX_ALLOCATION), honouring
  B-tree reachability and $BITMAP:$I30
- Index B-tree access for $I30 and view indexes, with $UpCase collation
- LZNT1-compressed stream decoding
- $MFT run list checked against the volume; record count capped when damaged
- Opt-in strict mode (Options.Strict) and $Reparse:$R verification
- Pure codecs: fixups, runlists, FILETIME, reparse buffers, $UpCase
- Sectors from 256 to 4096 bytes, clusters up to 2 MiB
- Update sequence (fixup) validation
- File name namespace handling (DOS, Win32, WinDOS, POSIX)
- Typed error model with context wrappers (including path traversal context)

Current limitations:

- WOF-compacted (CompactOS) content: detected but not decoded
  (ErrWOFCompressed)
- Encrypted data: detected but not decoded (ErrEncryptedData)

## Architecture Snapshot

The implementation is organized in layers:

- Binary utilities: binary.go
- NTFS structure/types/constants: types.go, constants.go
- Volume lifecycle and metadata: volume.go
- MFT and data-run resolution: mft.go
- Volume-free MFT record parsing: mftrecord.go
- Data stream access including alternates: streams.go
- Security descriptors and $Secure lookup: security.go, secure.go
- USN change journal parsing: usn.go
- MFT record write-back and resident writes: mftwrite.go
- Attribute parsing and runlist codec: attributes.go
- File and directory API surface: file.go
- Index B-tree access: index.go
- $UpCase table and collation: upcase.go
- Reparse point parsing and encoding: reparse.go
- LZNT1 decompression: compression.go
- WOF (CompactOS) detection: wof.go
- Strict-mode checks: strict.go
- Error model: errors.go

## Thread-Safety Model

- Public APIs are designed for concurrent reads
- MFT cache is protected with synchronization primitives
- Buffer reuse uses sync.Pool to reduce allocation overhead
- Internal state transitions (such as close) are guarded explicitly

## Error Model

libntfs uses wrapped, typed errors so callers can reliably use errors.Is and
errors.As.

- Sentinel errors for common categories (for example: not found, not directory,
  invalid structures)
- Context wrappers for volume, MFT, attribute, parse, and I/O failures
- PathError for path traversal failures with operation/path/component detail

## Performance Characteristics

- Cache-backed MFT lookups reduce repeated metadata I/O
- Buffer pooling lowers allocation pressure
- Read paths are concurrency-friendly
- Parser hardening focuses on malformed metadata safety and bounds checks

## Repository Layout

```text
libntfs/
|- README.md, CHANGELOG.md, PROJECT_SUMMARY.md, DEVELOPER.md
|- go.mod                 (no dependencies, no go.sum)
|- *.go                   (one file per area; see Architecture Snapshot)
|- *_test.go              (unit tests, fuzz targets, benchmarks)
|- testdata/              (fuzz corpus, report golden file, corpus builder)
|- examples/              (13 example programs; see README)
\- .github/workflows/     (ci.yml, fuzz.yml)
```

## Examples Included

The README's Examples section lists all 13: basic, traverse, extract,
windows_drive, bench, finfo, attrinfo, overwrite, mftdump, streams, usnjrnl,
logdump and report.

## Validation and Quality

- Unit tests, fuzz targets and benchmarks in the *_test.go files
- Continuous local checks expected via:
  - go test ./...
  - go vet ./...
- Ongoing hardening against malformed on-disk structures

## Roadmap Themes

Potential future directions:

- Encryption-related metadata support
- Security descriptor parsing improvements
- Additional NTFS metadata stream support

## References

- The Sleuth Kit (TSK) NTFS implementation (reference model)
- Microsoft NTFS documentation

## License

MIT.
