# Changelog

All notable changes to libntfs are recorded here. Versions follow the git tags.

## v0.3.5

Follow-up fixes to v0.3.4.

### Fixed

- **NTFS-R1.** `Open` panicked with an integer divide by zero on some boot
  sectors: a 4096-byte sector with a sectors-per-cluster byte of 0xEC or 0xEB,
  or a 2048-byte sector with 0xEB. The cluster size was multiplied in 32 bits,
  so it wrapped to 0 at 2^32 before the volume size was divided by it. It is
  now computed in 64 bits and rejected with `ErrInvalidBootSector` when it is 0
  or above `MaxClusterSize`, before anything uses it. A sector size that is not
  a power of two from 256 to 4096 is rejected too.
- **NTFS-R2.** `TimeToFiletime` returned 0 instead of clamping to
  `math.MaxUint64` for a Unix time within 11644473600 seconds of
  `math.MaxInt64`, where adding the 1601 epoch offset overflowed.

### Changed

- **NTFS-R3.** `ReadDir` now decides whether an INDX block is live the way NTFS
  does: the B-tree must reach it from `$INDEX_ROOT` through sub-node VCNs, and
  `$BITMAP:$I30` must mark it in use. The entries of a block whose bit is set
  but which no node points at, as a merge can leave behind, now come back
  `Deleted`; they used to be listed as live, and only strict mode reported the
  block (`bitmap-unreachable`). Blocks are still scanned in physical order, so
  their names stay available for recovery. With no `$BITMAP`, reachability
  alone decides, so a directory whose root reaches no INDX block now lists that
  block's names as `Deleted` rather than live. When the walk cannot read part
  of the tree, a block it did not reach is judged by its bitmap bit alone, as
  before.
- **NTFS-R4.** The tolerant `ReadDir` takes either `0x10` or `0x10000000` in an
  index key as marking a directory, while strict mode's `directory-flag` check
  takes only `0x10000000` (`FileAttrDupFileNameIndexPresent`), which Windows and
  ntfs-3g write in a directory's `$FILE_NAME`. The difference is deliberate; it
  is now documented on both, and the strict error says `directory $FILE_NAME
  lacks 0x10000000`.

### Tests

- **NTFS-T1.** `WalkIndex` is now tested over a depth-2 tree (collation order,
  sub-nodes before their parent entry, depth and VCN of each entry), over a
  freed block that nothing reaches (skipped), and over a two-level `$R` view
  index in ULONGS order (raw 12-byte keys).
- Tests for R1 to R4, and the fuzz target `FuzzParseBootSector`.
- The fixtures behind `TestReadDirMergesNamesSplitAcrossRootAndAllocation`,
  `TestReadDirReportsPartialOnCorruptIndexRecord` and
  `TestRecursiveWalkSurvivesCorruptSubtree` had a leaf `$INDEX_ROOT` that never
  pointed at their INDX block; their roots now point at it.

## v0.3.4

This release readies libntfs to be the reader behind fsagen, which generates
NTFS images byte by byte and checks them by reading them back. libntfs stays a
parser: what it gains on the writing side are pure codecs over bytes (fixups,
runlists, FILETIME, reparse buffers, `$UpCase` collation), never builders of
records, attributes, indexes or volumes.

### Changed — breaking

- `FileName.FileAttributes` is now `uint32`, which is what `$FILE_NAME` stores
  at 0x38. Code assigning it to or from a `uint64` needs a conversion.
- `MaxClusterSize` is now 2 MiB, the largest cluster Windows formats.
- Entries of an INDX block that `$BITMAP:$I30` marks free now come back from
  `ReadDir` with `Deleted` set; they used to be listed as live.
- The unexported `parseDataRuns` and `parseFileNameAttribute` are now exported
  as `DecodeRunlist` and `ParseFileName`, and the constants
  `IndexRecordHeaderSize`, `ReparseHeaderSize` and `SymlinkFlagRelative` are
  exported under those names.

### Fixed

- FILETIME conversion covered only 1970 to 2262. `NTFSTimeToTime` returned the
  zero time for anything before the Unix epoch, and `TimeToNTFSTime` went
  through `UnixNano`, which returns 0 before 1970 and overflows after 2262. Both
  now cover the whole FILETIME range; 0 still means "not set".
- `$FILE_NAME` was read with a 64-bit `FileAttributes`, which folded the reparse
  tag (or packed EA size) at 0x3C into the attributes and never set
  `FileName.ReparseValue`. Both fields are now read as the 32-bit values they
  are.
- `ReadDir` ignored `$BITMAP:$I30` and listed every INDX block it found as live,
  including blocks the directory had released. Free blocks' names are now
  `Deleted`, and a free block that fails to parse no longer counts as skipped. A
  directory with no `$BITMAP` is read as before.
- Volumes with clusters larger than 64 KiB were refused. The boot sector encodes
  such a cluster as a negative power of two (0xF8 is 256 sectors), which the
  parser rejected as more than 128 sectors. Clusters up to 2 MiB now open, and
  index VCNs on them address 512-byte units as NTFS defines.
- `$ATTRIBUTE_LIST` resolution matched extension attributes by type and name
  only, so two `$FILE_NAME`s in one extension record both resolved to the first,
  and a resident extension attribute was dropped whenever the base record held
  one of the same type. A file with many hard links lost names. Attributes are
  now matched by attribute ID, and each resident extension attribute is kept.

### Added

- Codecs: `ApplyFixup` and `ProtectFixup` (update sequence arrays),
  `DecodeRunlist` and `EncodeRunlist` (mapping pairs, minimal signed widths as
  Windows writes them), `ParseFileName`, `FiletimeToTime` and `TimeToFiletime`.
- `$UpCase` and collation: the `UpCase` type with `Compare`, `ToUpper` and
  `Bytes`; `DefaultUpCase` (the table Windows 8+ and mkntfs write);
  `ParseUpCase`; `(*Volume).UpCase`; `CollateULong` and `CollateULongs`; and
  `ErrInvalidUpCase`.
- Reparse encoders: `EncodeSymlinkReparse` and `EncodeMountPointReparse`, which
  produce the buffers `ParseReparsePoint` decodes.
- `(*MFTEntry).FileNames` returns every `$FILE_NAME`, extension records
  included.
- `DirEntry` gained `MFTChangeTime`, `Namespace`, `ParentReference` and
  `FileName`.
- Index access: `(*File).ReadIndexRoot`, `ReadIndexBlock`, `IndexBitmap` and
  `WalkIndex`, over `$I30` and the view indexes alike, and the `IndexBlock`
  type. `IndexEntry` gained `Key` and `Data`.
- `Options.Strict`, `(*Volume).IsStrict`, `(*Volume).VerifyReparseIndex`,
  `ErrStrictViolation` and `StrictError`: opt-in checks that turn tolerated
  damage into errors naming the record, index and VCN. See the README's "Strict
  mode". The default stays tolerant.
- Constants: `RecordSize`, `IndexBlockSize`, `UpCaseSize`, `FirstUserEntry`,
  `SystemEntryCount`, `AttrTypeEnd`, `FileAttrViewIndex`, `MFTFlagIsExtend`,
  `MFTFlagViewIndex`, and the `$AttrDef` flags `AttrDefIndexable` to
  `AttrDefAlwaysLog`.
- Fuzz targets `FuzzRunlistRoundTrip`, `FuzzFixupRoundTrip` and
  `FuzzParseIndexNode`.

### Documented

- `ReadDir`'s times and sizes come from the directory index's copy of
  `$FILE_NAME`, not from the file's own record.
- `OpenPath` matches names with `strings.EqualFold`, not `$UpCase`.

## v0.3.3

### Changed — breaking for callers that set `Options.BaseOffset`

**Every disk offset the library reports is now image-absolute: it includes
`Options.BaseOffset`.** It used to be relative to the volume.

A volume opened at the start of its reader — `Open`, `OpenReadOnly`, or
`OpenWithOptions` with `BaseOffset` left at zero — is completely unaffected.
Every number it reports is the number it reported before. **If you have never
set `BaseOffset`, nothing in this section applies to you.**

For a volume opened with a non-zero `BaseOffset`, these now return a value
larger by exactly that base:

- `FileFragment.StartOffset` and `FileFragment.EndOffset`, from
  `(*File).Fragments` and `(*Volume).AttributeFragments`
- `NTFSReport.StartOffset`, `NTFSReport.EndOffset` and `NTFSMeta.Offset`
- `(*Volume).ClusterToOffset`
- `IOError.Offset`

and these now _expect_ an image-absolute offset rather than a volume-relative
one:

- `(*Volume).ReadAt` and `(*Volume).WriteAt`, which reject an offset below
  `BaseOffset` with `ErrInvalidOffset`
- `(*Volume).OffsetToCluster`

Stream-relative offsets are a separate axis and are unchanged: `(*File).ReadAt`,
`(*File).WriteAt`, `(*Volume).ReadDataRuns`, `FileFragment.FileOffset`, and the
`$LogFile` and `$UsnJrnl` walks.

**Why.** The old behaviour made `BaseOffset` a way to _find_ a volume; it is now
also a way to _describe_ where the volume's contents sit. An offset reported
relative to the volume cannot be intersected with a range expressed in
whole-disk coordinates, and nothing about the mismatch is visible at the point
of use — the intersection succeeds and is quietly wrong. This also aligns
libntfs with libext, libfat, libhfs, libxfat and libxfs, so a consumer joining
reports from several of them does not have to special-case this one.

**To migrate.** If you were adding the partition start to libntfs offsets
yourself, stop. If you need the volume-relative value, subtract the new
`(*Volume).BaseOffset`.

### Added

- `(*Volume).BaseOffset` returns where the volume begins inside the reader it
  was opened over, so the two coordinate spaces can be converted between.
- `Capabilities` and `(*Volume).Capabilities` report what NTFS can record, as
  distinct from what a particular volume happens to record. Every field's doc
  comment says which of the two it is.
- `(*Volume).HasChangeJournal` answers `Capabilities.ChangeJournal` with the
  error kept, so a deleted `$UsnJrnl` can be told apart from an `$Extend`
  directory too damaged to read.
- `NTFSReport` gained `SchemaVersion`, `LibraryVersion` and `Generated`
  (`schema_version`, `library_version`, `generated`), so a report stays readable
  by a tool built against a different version of this library.
- `Version` and `ReportSchemaVersion` constants. `ReportSchemaVersion` is 1, the
  first published schema, and also the schema in which the offsets above became
  image-absolute.

### Fixed

- `(*Volume).OffsetToCluster` no longer divides by zero on a volume with no
  geometry, and no longer wraps a below-base offset into a cluster number near
  2^64. Both now return zero.
- `OpenWithOptions` rejects a `BaseOffset` that is not a whole number of the
  volume's own sectors, which would otherwise place every sector-aligned read
  off a sector boundary in the image.

### Unchanged

The read-only guarantee introduced in v0.3.2 is untouched. `bindHandles` and the
handle wrappers were deliberately not modified, and `go test -run ReadOnly`
passes every subtest.
