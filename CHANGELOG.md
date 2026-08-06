# Changelog

All notable changes to the libntfs project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0]

### Added
- `$LogFile` parsing. The transaction log is the finest-grained record of
  activity on a volume — finer than the USN journal, since it captures the
  individual attribute writes behind each change — and it was the last major
  forensic artifact libntfs could not read.
- `ParseLogRestartPage` decodes a restart page into a `LogRestartArea`: the log's
  geometry, its current LSN, the chkdsk LSN, the open count, and the client
  array. `"CHKD"` pages are accepted alongside `"RSTR"`, since a page chkdsk
  rewrote still holds a restart area.
- `ParseLogRecord` decodes one record, and `EachLogRecord` walks a whole
  `$LogFile` buffer with no Volume, for a log extracted with KAPE or FTK Imager.
  Records split across a page boundary are reassembled: the log's data area is
  one logical byte stream chopped into pages, each with its own header and
  update sequence, so a record can start near the end of one page and finish in
  the next.
- `(*Volume).OpenLogFile`, `(*Volume).LogRestartArea`, and
  `(*Volume).EachLogRecord`. The restart area falls back to the second copy when
  the first cannot be read, which is exactly the redundancy NTFS writes two for.
  The log is read a chunk of pages at a time rather than loaded whole.
- `LogRecord` exposes both halves of a record: the log-file-service header — LSN,
  the previous and undo-next links, the transaction, the record type — and NTFS's
  own fields, including the redo and undo operations, the target attribute and
  VCN, the LCN list, and the redo and undo payloads. Payload interpretation is
  left to the caller, since its meaning depends entirely on the operation.
- 35 `LogOp*` constants and `LogOperationName`, plus
  `(*LogRecord).RedoOperationName`, `.UndoOperationName`, and `.IsClientRecord`.
- `ErrInvalidLogRecord`.
- WSL POSIX metadata decoding on top of the extended attribute parser.
  `(*File).LinuxAttributes` returns the mode, owner, group, device, and POSIX
  timestamps WSL stores in `LXATTRB` — metadata no Windows tool surfaces and no
  NTFS structure otherwise records. `ParseLinuxAttributes` decodes the value with
  no Volume.
- WSL1's older layout is supported too: when `LXATTRB` is absent, the separate
  `$LXUID`, `$LXGID`, `$LXMOD`, and `$LXDEV` attributes are assembled into the
  same shape, with `FromLegacyAttributes` set so a caller can tell which it got.
  Names are matched case-insensitively and with or without the leading `$`, since
  the two WSL generations disagree about the prefix.
- `(*LinuxAttributes).ModeString` renders the mode the way `ls` does, setuid,
  setgid, and sticky bits included. `.Permissions`, `.FileType`, and `.IsSymlink`
  round it out. A POSIX zero timestamp reads as the zero time rather than 1970,
  so an absent timestamp does not become a 1970 entry in a timeline.
- `EANameLXATTRB` and the other WSL attribute name constants.
- `examples/logdump`, reading `$LogFile` from a volume or a standalone file, with
  `-restart` for the geometry, `-sort` to turn the circular log back into a
  timeline, `-op` to filter by operation, `-data` for the payloads, and `-json`.
- `TestReportGolden`, a golden-file test over `Report()`. Its output is the
  library's widest surface and had changed twice with no test watching it. Run
  `go test -run TestReportGolden -update` to rewrite the golden file after an
  intended change.
- A real-image test corpus. `testdata/make_corpus.ps1` builds a small NTFS volume
  that Windows formats and populates — fragmented, sparse, compressed, and
  resident files, an alternate data stream, a symlink, a junction, a deleted
  entry, and WSL metadata — and `corpus_test.go` runs thirteen tests against it.
  Every other test in the package builds NTFS structures byte by byte, which
  proves the parsers against bytes libntfs itself wrote; these prove them against
  bytes Windows wrote. The tests skip with instructions when the image has not
  been generated, since generating it needs an elevated Windows prompt.
- Fuzz targets `FuzzParseLogRestartPage`, `FuzzParseLogRecord`, and
  `FuzzEachLogRecord`, the last driving the page walker so the carry buffer that
  reassembles page-spanning records is exercised too.
- Transaction grouping. `GroupLogTransactions` collects log records by
  transaction ID, ordered by the LSN each began at, with `Committed` and
  `Forgotten` reporting how each ended and `(LogTransaction).Operations`
  summarising what it did. A single user-visible action — creating a file,
  renaming one — is several log records under one transaction ID, and reading
  them individually loses the fact that they belong together.
- `ParseLogRestartTable` and `(*LogRecord).RestartTable` decode the periodic
  table dumps NTFS writes into the log, plus `(*LogRecord).IsTableDump`.
  The table header is decoded; entries are handed over as raw fixed-size records
  rather than decoded structures, because their layout depends on the NTFS
  version that wrote them and libntfs cannot confirm which it is looking at.
  `EntrySize` is what makes them usable — a caller who knows its target version
  can decode them. Reporting a wrongly-decoded MFT reference would attribute an
  operation to the wrong file, which is worse than reporting none.
- POSIX extended attribute decoding. `(*File).LinuxExtendedAttributes` and
  `ParseLinuxExtendedAttributes` unpack the `user.*` and `security.*` attributes
  WSL stores inside `LXXATTR` — capabilities on a binary, SELinux labels, and
  whatever an application hung off a file. On Windows these are one opaque blob
  inside a single NTFS extended attribute, so nothing surfaces them.
- `examples/logdump` gained `-transactions`, grouping records so one action reads
  as one thing.
- Fuzz targets `FuzzParseLogRestartTable`, `FuzzParseLinuxAttributes`, and
  `FuzzParseLinuxExtendedAttributes`, bringing the set to twelve.
- GitHub Actions workflows. `ci.yml` builds, vets, and race-tests on Linux,
  Windows, and macOS, plus a 20-second smoke run of every fuzz target. `fuzz.yml`
  runs each target for ten minutes nightly, caching the generated corpus between
  runs so a night starts from what previous nights found, and uploading any
  crasher as an artifact.

### Notes
- `$LogFile` is read, not replayed. Reconstructing file content from redo data,
  undoing to a consistent state, and replaying transactions are deliberately out
  of scope: that is a different problem, and libntfs stops at handing over the
  decoded records.
- `LogRecord.TargetAttribute` is still an index into the open attribute table
  rather than a resolved file and stream. Resolving it needs the per-entry
  layout of that table, which differs between NTFS versions; guessing it would
  produce confident, wrong attribution. `ParseLogRestartTable` exposes the
  entries and their size so a caller with a known target version can do it.
- `LXXATTR` is not a documented structure. The layout decoded here matches what
  WSL writes, but Microsoft publishes no specification and a future version could
  change it. Every field is bounds-checked, so a layout change surfaces as a
  parse failure rather than as plausible nonsense — but validate against your own
  corpus before relying on it.
- The log is circular, holding the most recent operations and overwriting the
  oldest, so records arrive in whatever order the ring currently holds. Sort by
  LSN for a timeline.

## [0.2.1]

### Added
- Fragment mapping. `(*File).Fragments` returns a stream's on-disk layout with
  both axes a forensic caller needs: where each region sits inside the stream
  (`FileOffset`, `Length`) and where its bytes sit on the volume (`StartOffset`,
  `EndOffset`). Fragments tile the stream with no gaps, so a byte range of a file
  maps onto the volume by walking them. Every example that hand-rolled this
  calculation over `NonResident.DataRuns` can now call it instead.
- `(*File).IsFragmented` reports whether a stream is split across more than one
  region of the volume. A sparse hole is not counted: a file stored in one run
  with a hole punched in it is not scattered across the disk.
- `(*Volume).AttributeFragments` maps any attribute, which is how to reach the
  streams that are not `File`s — `$Bitmap`, `$Secure:$SDS`, `$UsnJrnl:$J`.
- Fragments follow the stream a handle was opened on, so
  `OpenStream(name).Fragments()` maps an alternate data stream with no extra
  work. A resident stream reports one fragment with `Resident` set, locating its
  bytes inside the MFT record.
- Extended attribute parsing. `ParseExtendedAttributes`, `EachExtendedAttribute`,
  and `ParseEAInformation` decode `$EA` and `$EA_INFORMATION` from a raw buffer
  with no Volume, for attribute bytes carved out of a standalone `$MFT`.
  `(*File).ExtendedAttributes`, `(*File).EAInformation`, and
  `(*File).HasExtendedAttributes` read them from a volume, following data runs
  when `$EA` is non-resident — which is how the Windows Subsystem for Linux's
  POSIX metadata is usually stored.
- `EAInfo`, `ExtendedAttribute`, `(ExtendedAttribute).NeedEA`, `EAFlagNeedEA`,
  and `ErrInvalidExtendedAttribute`.
- Reparse point parsing. `ParseReparsePoint` decodes both buffer shapes:
  Microsoft-owned tags, whose data follows the header directly, and third-party
  tags, which carry a 16-byte owner GUID first. `Data` holds the tag-specific
  bytes with the header and any GUID stripped, so a caller reading it needs no
  knowledge of which shape it came from.
- `(*ReparsePoint).Target` resolves the tags that name a path — symbolic links,
  mount points, and WSL symlinks — and reports `ok == false` for the tags that do
  not, such as deduplication, WOF, and cloud placeholders, leaving `Data` to the
  caller. `(*ReparsePoint).TagName`, `.IsMicrosoft`, and `.IsRelative` render the
  rest. `(*File).ReparsePoint` and `(*File).IsReparsePoint` read them.
- `ReparsePoint.GUID`, twelve further `ReparsePoint*` tag constants covering WOF,
  deduplication, cloud placeholders, app execution aliases, and the WSL types,
  and `ErrInvalidReparsePoint`.
- `examples/attrinfo`, reporting the extended attributes and reparse point of a
  path or MFT entry, with `-r` to walk a tree, `-only` to hunt for WSL metadata
  and links, and a `-json` mode. It reports junctions rather than descending
  through them, since the link is not the target.
- Fuzz targets for the parsers fed untrusted image bytes:
  `FuzzParseExtendedAttributes`, `FuzzParseEAInformation`,
  `FuzzParseReparsePoint`, `FuzzParseDataRuns`, `FuzzParseSecurityDescriptor`,
  and `FuzzParseUSNRecord`.

### Changed
- `FileFragment` gained `FileOffset`, `Length`, `Sparse`, and `Resident`, so
  `Report()`'s JSON now carries four more keys per fragment. Existing keys are
  unchanged. Callers constructing a `FileFragment` with an unkeyed literal need
  to switch to a keyed one.
- `examples/finfo` reports fragments through `(*File).Fragments` instead of
  walking data runs itself, and now emits each fragment's stream offset and
  whether it is sparse or resident.

### Fixed
- The last fragment of a file no longer overshoots its content. Run lengths are
  cluster-granular, and the final run was reported at its full allocated length,
  claiming up to one cluster of slack as file data. It is now clamped to the
  stream's real size. This affected `Report()`, `examples/finfo`, and anything
  else deriving byte ranges from data runs.
- Sparse runs are no longer dropped from a stream's map. Dropping a hole left
  every later fragment at a stream position it did not occupy, so a byte range of
  a sparse file could not be mapped back to disk. `(*File).Fragments` reports
  holes; `Report()` still filters them out, since it describes physical regions.

## [0.2.0]

### Added
- Alternate data stream access. `(*File).Streams` lists every `$DATA` stream on a
  file with its size, storage, and readability; `(*File).OpenStream` returns a
  `File` reading a named stream, so the whole existing read API — including
  compressed and sparse handling — works on alternate streams unchanged.
  `(*MFTEntry).DataStreams` enumerates streams with no Volume, composing with
  `ParseMFTRecord` for standalone `$MFT` analysis.
- Security descriptor parsing. `ParseSecurityDescriptor` decodes a self-relative
  descriptor into owner and group `SID`s and DACL/SACL `ACE`s, with
  `(*SID).String`, `(*SID).WellKnownName`, `(ACE).TypeName`, and
  `(ACE).RightsNames` rendering them readably.
- `$Secure` lookup. `(*Volume).SecurityDescriptorByID` and
  `(*Volume).EachSecurityDescriptor` read and index `$Secure:$SDS` once per
  volume, and `(*File).SecurityDescriptor` resolves a file's descriptor through
  its own `$SECURITY_DESCRIPTOR` attribute when present, otherwise through
  `StandardInformation.SecurityID`.
- `ErrInvalidSecurityDescriptor` and `ErrSecurityDescriptorNotFound`.
- `examples/streams`, reporting the streams and access control of a path or MFT
  entry, with `-ads-only` to hunt for alternate data streams and `-dump` to
  extract one.
- USN change journal parsing. `ParseUSNRecord` and `EachUSNRecord` decode records
  from a raw buffer with no Volume, for `$J` files extracted with KAPE or FTK
  Imager. Versions 2, 3, and 4 are decoded; V4 extent records carry no timestamp
  or name, and report that rather than mis-parsing one.
- `(*Volume).OpenUSNJournal` resolves `\$Extend\$UsnJrnl:$J`, and
  `(*Volume).EachUSNRecord` walks it. The journal is read in ~1 MiB chunks rather
  than loaded whole, since `$J` routinely runs to gigabytes, and its sparse
  leading region is skipped outright using the stream's data runs.
- `(*USNRecord).ReasonNames` decodes the change reason bitmask, and
  `(*USNRecord).IsDirectory` reports the record's target type.
- `USNReason*` constants covering the documented change reasons, plus
  `ErrInvalidUSNRecord`.
- `examples/usnjrnl`, printing the journal as a timeline from either an
  extracted `$J` or a full volume, with `-reason` and `-name` filters and a
  `-json` mode.
- Resident write support. `(*File).WriteAt` and `(*File).Write` now overwrite
  resident `$DATA` streams, whose content lives inside the MFT record, by
  rewriting that record with its update sequence reapplied. The MFT cache entry
  is evicted so later reads see the new bytes. Existing callers need no changes:
  `examples/overwrite` handles resident files with no modification.
- `AttributeHeader.RecordOffset` reports where an attribute sits within its MFT
  record, which locates it for in-place edits and for forensic reporting.

### Changed
- `(*File).WriteAt` no longer returns `ErrWriteUnsupported` for every resident
  stream. It still refuses writes that would change a resident value's length,
  since growing one would require resizing the attribute and shifting every
  later attribute in the record, and refuses attributes merged in from an
  extension record, whose bytes belong to a different MFT record.
- Zero-filled and malformed regions of `$J` are skipped rather than ending the
  walk, so damage in one region does not hide the records after it.

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
- [x] Security descriptor parsing (delivered in 0.2.0)
- [x] Alternate data stream enumeration (delivered in 0.2.0)
- [x] USN Journal support (delivered in 0.2.0)
- [x] Resident writes (same-length overwrite, delivered in 0.2.0)
- [ ] Encryption support (EFS) — currently detection only
- [ ] File growth, truncation, and resident-to-non-resident conversion
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

- **0.2.0** - Alternate data streams, security descriptors, USN change journal,
  and resident stream writes
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
