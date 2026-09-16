# Changelog

All notable changes to libntfs are recorded here. Versions follow the git tags.

## v0.3.3

### Changed — breaking for callers that set `Options.BaseOffset`

**Every disk offset the library reports is now image-absolute: it includes
`Options.BaseOffset`.** It used to be relative to the volume.

A volume opened at the start of its reader — `Open`, `OpenReadOnly`, or
`OpenWithOptions` with `BaseOffset` left at zero — is completely unaffected. Every
number it reports is the number it reported before. **If you have never set
`BaseOffset`, nothing in this section applies to you.**

For a volume opened with a non-zero `BaseOffset`, these now return a value larger
by exactly that base:

- `FileFragment.StartOffset` and `FileFragment.EndOffset`, from
  `(*File).Fragments` and `(*Volume).AttributeFragments`
- `NTFSReport.StartOffset`, `NTFSReport.EndOffset` and `NTFSMeta.Offset`
- `(*Volume).ClusterToOffset`
- `IOError.Offset`

and these now *expect* an image-absolute offset rather than a volume-relative one:

- `(*Volume).ReadAt` and `(*Volume).WriteAt`, which reject an offset below
  `BaseOffset` with `ErrInvalidOffset`
- `(*Volume).OffsetToCluster`

Stream-relative offsets are a separate axis and are unchanged: `(*File).ReadAt`,
`(*File).WriteAt`, `(*Volume).ReadDataRuns`, `FileFragment.FileOffset`, and the
`$LogFile` and `$UsnJrnl` walks.

**Why.** The old behaviour made `BaseOffset` a way to *find* a volume; it is now
also a way to *describe* where the volume's contents sit. An offset reported
relative to the volume cannot be intersected with a range expressed in whole-disk
coordinates, and nothing about the mismatch is visible at the point of use — the
intersection succeeds and is quietly wrong. This also aligns libntfs with libext,
libfat, libhfs, libxfat and libxfs, so a consumer joining reports from several of
them does not have to special-case this one.

**To migrate.** If you were adding the partition start to libntfs offsets
yourself, stop. If you need the volume-relative value, subtract the new
`(*Volume).BaseOffset`.

### Added

- `(*Volume).BaseOffset` returns where the volume begins inside the reader it was
  opened over, so the two coordinate spaces can be converted between.
- `Capabilities` and `(*Volume).Capabilities` report what NTFS can record, as
  distinct from what a particular volume happens to record. Every field's doc
  comment says which of the two it is.
- `(*Volume).HasChangeJournal` answers `Capabilities.ChangeJournal` with the error
  kept, so a deleted `$UsnJrnl` can be told apart from an `$Extend` directory too
  damaged to read.
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
  volume's own sectors, which would otherwise place every sector-aligned read off
  a sector boundary in the image.

### Unchanged

The read-only guarantee introduced in v0.3.2 is untouched. `bindHandles` and the
handle wrappers were deliberately not modified, and `go test -run ReadOnly` passes
every subtest.
