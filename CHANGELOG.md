# Changelog

All notable changes to the libntfs project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.5]

### Added
- Limited write support for in-place overwrites: `(*File).Write`,
  `(*File).WriteAt`, `(*Volume).WriteAt`, `(*Volume).WriteDataRuns`, and
  `(*Volume).IsWritable`. Restricted to non-resident, non-sparse,
  non-compressed, non-encrypted streams on writable handles. No resident
  writes, no file growth, no truncation.
- `ErrReadOnlyVolume`, `ErrWriteUnsupported`, and `ErrWriteOutOfRange`.
- `examples/overwrite`, a guarded in-place overwrite at logical file offsets
  with caller-supplied end-bound checks. On Windows raw volume paths it
  attempts `FSCTL_LOCK_VOLUME` before writing and unlocks on exit.
- `ParseMFTRecord` parses a single MFT FILE record from a raw buffer with no
  Volume, for standalone `$MFT` files extracted with KAPE, FTK Imager, or icat.
  Resident attributes parse fully; non-resident data runs are decoded but not
  resolved, since reading content requires cluster access.
- `(*Volume).MFTEntryCount` reports the number of addressable MFT records, so
  callers can iterate the table with a known bound and skip per-entry errors
  rather than stopping at the first one.
- `(*Volume).EachMFTEntry` walks every parsable record in order, skipping
  unallocated and corrupt entries and stopping when the callback returns an
  error.
- `ParseMFTRecordInto` parses into a caller-supplied `MFTEntry` so a sweep over a
  large `$MFT` stops allocating one per record. Roughly 9 allocations per record
  instead of 13. Each call invalidates the previous call's results.
- `DefaultMFTRecordSize` and `MinMFTEntrySize` constants.
- `examples/mftdump`, which lists MFT records as a timeline from either a
  standalone `$MFT` file or a full volume, with a `-json` mode emitting one JSON
  object per record for piping into `jq` or a timeline tool.

### Changed
- `MFTEntry.RecordNumber` is now populated with the entry number by every parse
  path, including `(*Volume).GetMFTEntry`, which previously always left it 0.
- The MFT record parser is now volume-independent internally. Record parsing,
  fixup application, and attribute parsing moved from `*Volume` methods to
  package-level functions, and `(*Volume).parseMFTEntry` delegates to the shared
  core. No exported API changed and volume-backed parsing behaves identically.

### Fixed
- The `Version` constant had been left at `0.1.1` while 0.1.2, 0.1.3, and 0.1.4
  shipped. It now tracks the release.
- The package documentation example called `volume.Open(root.RecordNumber)` on
  the `*File` returned by `GetRootDirectory`, which has no such field.

## [0.1.4]

### Added
- `(*Volume).Report` and the supporting `report.go`, producing a structured
  volume report rather than ad-hoc printed output.
- `examples/report` and `examples/finfo`, the latter showing per-file metadata
  including size, start and end byte offsets, fragmentation, and timestamps.

## [0.1.3]

### Added
- `(*Volume).SetMFTParentFallback` to opt into the expensive full-MFT
  parent-link fallback during directory reads.
- `examples/bench`, an end-to-end traversal and sampled read throughput
  benchmark.

### Changed
- MFT entry lookup now uses a cached index of cumulative data run ends with
  binary search instead of a linear scan, which matters on fragmented volumes.
- Reworked file extraction and data run reading.

## [0.1.2]

### Added
- `(*MFTEntry).FindPrimaryDataAttribute` and
  `(*MFTEntry).FindPrimaryNonResidentDataAttribute`, which prefer the unnamed
  `$DATA` stream so alternate data streams are not selected by accident.

### Changed
- `$ATTRIBUTE_LIST` resolution now merges attributes from MFT extension records
  and stitches split data runs in VCN order, so large fragmented streams present
  as a single run list.
- Improved file, attribute, and file size detection parity with The Sleuth Kit.
- Reworked the traverse example.

## [0.1.1]

### Added
- Compressed file support in `compression.go`, decompressing NTFS compression
  units so compressed streams can be read and extracted.
- Stream capability inspection: `(*File).ReadSupport`, `(*Attribute).ReadSupport`,
  `(*File).HasData`, `(*File).IsCompressed`, `(*File).IsEncrypted`, and
  `(*File).IsSparse`, so callers can check whether a stream is readable and how
  it is stored before reading.

## [0.1.0]

### Added
- Initial release of libntfs
- Core NTFS parsing functionality
  - Boot sector parsing with full validation
  - MFT entry reading and caching
  - Resident and non-resident attribute parsing
  - Data run parsing and resolution
  - Update sequence array (fixup) validation
- File system operations
  - Volume opening and initialization
  - File reading via path or MFT entry number
  - Directory listing and traversal
  - Metadata access (standard information, file names)
- Thread-safe API design
  - RWMutex for concurrent access
  - Thread-safe MFT caching
  - Buffer pooling for performance
- Comprehensive error handling
  - Typed error structures
  - Error wrapping with context
  - Detailed error messages
- Data structure support
  - $STANDARD_INFORMATION
  - $FILE_NAME (all namespaces)
  - $DATA (resident and non-resident)
  - $INDEX_ROOT
  - $INDEX_ALLOCATION
  - $ATTRIBUTE_LIST
  - $VOLUME_INFORMATION
  - $OBJECT_ID
  - $BITMAP
  - $REPARSE_POINT (detection)
- Utility functions
  - NTFS time conversion
  - UTF-16 string handling
  - Little-endian binary parsing
  - Variable-length integer parsing
- Documentation
  - Comprehensive README
  - Developer guide (DEVELOPER.md)
  - Project summary (PROJECT_SUMMARY.md)
  - Code examples (3 working examples)
  - API documentation via godoc
- Testing
  - Unit tests for core functionality
  - Benchmark tests
  - Test coverage for critical paths
- Examples
  - Basic volume access and directory listing
  - Recursive directory traversal
  - File extraction

### Features by Component

#### volume.go
- Thread-safe volume opening
- Boot sector validation
- MFT initialization
- Cluster/sector calculations
- Buffer pooling

#### mft.go
- MFT entry parsing
- Update sequence application
- Entry caching (1000 entries default)
- Data run offset calculation
- Attribute enumeration

#### attributes.go
- Resident attribute parsing
- Non-resident attribute parsing
- Data run parsing with sparse support
- Attribute-specific parsers:
  - StandardInformation
  - FileName
  - IndexRoot
  - VolumeInformation
  - ObjectID

#### file.go
- File opening by path or entry number
- Read/ReadAt/ReadAll operations
- Directory listing (ReadDir)
- Metadata retrieval
- Index allocation parsing

#### binary.go
- Little-endian integer reading (8/16/32/64-bit)
- File reference parsing (48-bit)
- NTFS time conversion
- UTF-16 to UTF-8 string conversion
- Alignment utilities

#### errors.go
- VolumeError
- MFTError
- AttributeError
- ParseError
- IOError
- Common error constants

#### constants.go
- NTFS magic values
- Attribute type constants
- File attribute flags
- MFT entry constants
- Boot sector constants
- Namespace constants
- Human-readable name mappings

### Known Limitations
- Compressed data not supported (detection only)
- Encrypted data not supported (detection only)
- USN Journal not fully implemented
- Security descriptors not parsed
- Alternate data streams not enumerated
- Reparse points detected but not fully parsed

### Technical Details
- Pure Go implementation (no CGO)
- Zero external dependencies
- Go 1.21+ required
- Cross-platform support
- Thread-safe by design
- Production-ready error handling

### Documentation Files
- README.md - User guide and quick start
- LICENSE - MIT License
- DEVELOPER.md - Development guidelines
- PROJECT_SUMMARY.md - Technical overview
- CHANGELOG.md - This file
- .gitignore - Git ignore rules

### Examples Provided
1. **basic** - Volume info and root directory listing
2. **traverse** - Recursive directory traversal with statistics
3. **extract** - Extract a file from NTFS volume

### Testing Coverage
- Binary parsing utilities
- Time conversion functions
- Data run parsing
- Attribute type lookups
- Error wrapping
- Buffer boundary checks
- UTF-16 string conversion
- File reference parsing
- Variable integer parsing

---

## Roadmap

### Features
- [x] Compression support (read path, delivered in 0.1.1)
- [x] Write support (limited to in-place overwrite, delivered in 0.1.5)
- [ ] Encryption support (EFS) — currently detection only
- [ ] Resident writes, file growth, and truncation
- [ ] USN Journal full support
- [ ] Security descriptor parsing
- [ ] Alternate data stream enumeration
- [ ] Enhanced caching strategies
- [ ] Memory-mapped I/O option

### Improvements
- [x] Integration tests against a synthetic volume image (delivered in 0.1.5)
- [ ] More comprehensive test coverage
- [ ] Performance profiling and optimization
- [ ] Additional examples
- [ ] Extended documentation
- [ ] API stability guarantees for v1.0

---

## Version History

- **0.1.5** - Limited write support, volume-free MFT record parsing, bounded MFT
  iteration
- **0.1.4** - Structured volume reporting
- **0.1.3** - MFT lookup speedups and parent-link fallback control
- **0.1.2** - $ATTRIBUTE_LIST resolution and Sleuth Kit parity
- **0.1.1** - Compressed file support and stream capability checks
- **0.1.0** - Initial release

## Links

- [GitHub Repository](https://github.com/aoiflux/libntfs)
- [Issue Tracker](https://github.com/aoiflux/libntfs/issues)
- [Documentation](https://pkg.go.dev/github.com/aoiflux/libntfs)
