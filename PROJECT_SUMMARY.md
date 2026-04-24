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
- Directory index parsing ($INDEX_ROOT and $INDEX_ALLOCATION)
- Update sequence (fixup) validation
- File name namespace handling (DOS, Win32, WinDOS, POSIX)
- Typed error model with context wrappers (including path traversal context)

Current limitations:

- Compressed data: detected but not decoded (ErrCompressedData)
- Encrypted data: detected but not decoded (ErrEncryptedData)

## Architecture Snapshot

The implementation is organized in layers:

- Binary utilities: binary.go
- NTFS structure/types/constants: types.go, constants.go
- Volume lifecycle and metadata: volume.go
- MFT and data-run resolution: mft.go
- Attribute parsing: attributes.go
- File and directory API surface: file.go
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
|- README.md
|- PROJECT_SUMMARY.md
|- DEVELOPER.md
|- go.mod
|- ntfs.go
|- types.go
|- constants.go
|- errors.go
|- binary.go
|- volume.go
|- mft.go
|- attributes.go
|- file.go
|- ntfs_test.go
|- examples/
|  |- basic/
|  |- traverse/
|  |- extract/
|  \- windows_drive/
\- cppcode/
```

## Examples Included

- basic: open a volume/image, print volume metadata, list root directory
- traverse: recursively walk a directory subtree with summary stats
- extract: copy a file from NTFS to a local output path
- windows_drive: Windows-specific raw-drive workflow

## Validation and Quality

- Unit tests and benchmarks in ntfs_test.go
- Continuous local checks expected via:
  - go test ./...
  - go vet ./...
- Ongoing hardening against malformed on-disk structures

## Roadmap Themes

Potential future directions:

- Compression decoding support
- Encryption-related metadata support
- Alternate data stream (ADS) enumeration
- Security descriptor parsing improvements
- Additional NTFS metadata stream support

## References

- The Sleuth Kit (TSK) NTFS implementation (reference model)
- Microsoft NTFS documentation

## License

MIT.
