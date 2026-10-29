// Package libntfs reads NTFS volumes from disk images and raw devices.
//
// It is a forensic reader: it parses what is on disk and reports it, including
// the parts a mounted file system hides — deleted directory entries recoverable
// from index slack, alternate data streams, the change journal, and the
// transaction log. It never mounts anything and never repairs anything.
//
// # Getting started
//
// Everything begins with a volume, which needs only an [io.ReaderAt]:
//
//	f, err := os.Open("disk.img") // or \\.\C: on Windows, /dev/sda1 on Linux
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer f.Close()
//
//	vol, err := libntfs.OpenReadOnly(f)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer vol.Close()
//
//	root, err := vol.GetRootDirectory()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	entries, err := root.ReadDir()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	for _, entry := range entries {
//		fmt.Println(entry.Name, entry.Deleted)
//	}
//
// A volume expects the NTFS boot sector at offset zero. For a partition inside
// a whole-disk image, say where it starts:
//
//	vol, err := libntfs.OpenWithOptions(f, libntfs.Options{
//		ReadOnly:   true,
//		BaseOffset: partitionOffset,
//	})
//
// BaseOffset does two things. It shifts every read, so the library finds the
// volume; and it is added to every disk offset the library reports, so those
// offsets locate bytes in the image rather than within the partition. A
// fragment offset can then be intersected with a range in whole-disk
// coordinates, or handed straight back to [Volume.ReadAt].
//
// Wrapping the file in an [io.SectionReader] and calling [Open] still works,
// but it is not equivalent: a section reader cannot say where the partition
// sits, so the offsets reported are relative to the section. It is also
// read-only by accident rather than by declaration, since a section reader over
// an *os.File is not an [io.WriterAt]. Prefer BaseOffset, and reach for a
// section reader when the volume needs its length bounded too.
//
// # Reading and writing
//
// This is a reader. The read API is the library; writing exists only to patch
// bytes back into an image the caller owns, and it is off unless deliberately
// switched on. There are three states, and they are distinguishable:
//
//   - [OpenReadOnly], or [OpenWithOptions] with Options.ReadOnly — every write
//     is refused with [ErrReadOnlyMode] before the handle is touched, and the
//     handle the volume keeps cannot be asserted back to an [io.WriterAt], so
//     no code path can recover the ability to write. [Volume.IsReadOnly]
//     reports true.
//   - [Open] over a reader that is not an [io.WriterAt] — writes are refused
//     with [ErrReadOnlyVolume]. The volume is not writable, but nobody asked
//     for that; it is a property of the handle.
//   - [Open] over a reader that does implement [io.WriterAt] — writable, and
//     [Volume.IsWritable] says so.
//
// ErrReadOnlyMode wraps ErrReadOnlyVolume, so a caller matching the general
// case keeps matching and one that needs to tell "I asked for evidence safety"
// apart from "this handle happens not to write" can.
//
// Use [OpenReadOnly] on evidence. [Open] promotes any [io.WriterAt] it is
// given, and an *os.File opened for writing is one, so a volume over a live
// device or an acquired image is otherwise mutable with no opt-in.
//
// Every method that can modify a volume lives in write.go and passes through a
// single gate; nothing in the read API reaches it. See [Volume.WriteAt] for
// what writing does and does not support.
//
// # The API by area
//
// Volume — geometry and entry points. [OpenReadOnly], [OpenWithOptions],
// [Open], [Volume.Open], [Volume.OpenPath], [Volume.GetRootDirectory],
// [Volume.BytesPerCluster], [Volume.VolumeSize], [Volume.IsReadOnly],
// [Volume.IsWritable].
//
// Files and directories — [File.Read], [File.ReadAt], [File.ReadAll],
// [File.ReadDir], [File.GetMetadata] for the full MACB set including the MFT
// change time, and [File.ReadSupport] to ask whether a stream is readable before
// trying. [File.ReadDir] returns deleted entries alongside live ones; check
// [DirEntry.Deleted].
//
// Identity — [File.EntryNumber], [File.SequenceNumber], and
// [File.FileReference] for the packed 48:16 reference NTFS itself stores.
// [MakeFileReference], [FileReferenceEntry], and [FileReferenceSequence] pack
// and unpack it. The sequence number is not optional detail: NTFS reuses MFT
// records, and comparing two observations on the record number alone reports a
// deleted file whose record was reused as a modified one.
//
// Paths and walks — [Volume.PathFor] reconstructs where a record sat in the
// tree, which is the direction [Volume.OpenPath] cannot go and the one a report
// needs. [Volume.WalkFiles] walks the whole MFT handing the callback each
// record together with its path and the parent it names. [Volume.WalkMFT] and
// [Volume.EachMFTEntry] are the same walk without paths. [Volume.Report] and
// [Volume.ReportContext] produce a JSON-ready listing of the volume, identity
// and timestamps included.
//
// Streams — [File.Streams] lists a file's $DATA streams and [File.OpenStream]
// opens one by name, returning a [File] so the whole read API works on alternate
// data streams unchanged.
//
// Layout — [File.Fragments] maps a stream onto the volume with both axes:
// where each region sits in the stream and where its bytes sit on disk.
// [File.IsFragmented] and [Volume.AttributeFragments] round it out.
//
// The MFT — [Volume.GetMFTEntry], [Volume.EachMFTEntry], [Volume.MFTEntryCount],
// and [ParseMFTRecord] for a standalone $MFT extracted with KAPE or FTK Imager.
// [Volume.MFTDataRunsError] says when a damaged $MFT run list made
// MFTEntryCount a ceiling rather than the figure on disk.
// [MFTEntry.FileNames] returns every hard link's $FILE_NAME.
//
// Indexes — [File.ReadIndexRoot], [File.ReadIndexBlock], [File.IndexBitmap]
// and [File.WalkIndex] read any index B-tree node by node: $I30 directories and
// the view indexes such as $Reparse:$R and $Secure:$SII. [Volume.UpCase] and
// [UpCase.Compare] give NTFS's own file name collation.
//
// Metadata — [File.ExtendedAttributes] and [File.EAInformation] for $EA,
// [File.ReparsePoint] for symlinks and junctions, [File.SecurityDescriptor] for
// ownership and access control, and [File.LinuxAttributes] and
// [File.LinuxExtendedAttributes] for the POSIX metadata the Windows Subsystem
// for Linux stores in extended attributes.
//
// Timelines — [Volume.EachUSNRecord] walks the change journal and
// [Volume.EachLogRecord] walks the transaction log, which is finer grained.
// [GroupLogTransactions] turns log records into the actions that produced them.
// Both walks have [Volume.EachUSNRecordContext] and
// [Volume.EachLogRecordContext] variants; $J routinely runs to gigabytes, so on
// a real volume these are the operations worth being able to cancel.
//
// # Working without a volume
//
// Several parsers take a byte buffer and need no [Volume] at all, for artifacts
// extracted from an image by another tool: [ParseMFTRecord], [ParseUSNRecord],
// [EachUSNRecord], [ParseLogRestartPage], [ParseLogRecord], [EachLogRecord],
// [ParseSecurityDescriptor], [ParseExtendedAttributes], [ParseReparsePoint],
// [ParseLinuxAttributes], [ParseLinuxExtendedAttributes], [ParseWOFExternalInfo],
// and [ParseFileName].
//
// A few pure codecs run in both directions, for tools that generate images and
// check them with this reader: [ApplyFixup] and [ProtectFixup],
// [DecodeRunlist] and [EncodeRunlist], [FiletimeToTime] and [TimeToFiletime],
// [EncodeSymlinkReparse] and [EncodeMountPointReparse], and [DefaultUpCase]
// with [CollateULong] and [CollateULongs]. They encode values, never records,
// attributes, indexes or volumes.
//
// # Concurrency
//
// A [Volume] is safe for concurrent use. Its geometry is written once during
// [Open] and read-only afterwards; the MFT cache, the security descriptor map,
// the parent-link map and the $UpCase table are each guarded by their own lock,
// so contention on one does not block the others.
//
// A [File] carries its own read offset, so each goroutine should open its own
// rather than sharing one. Opening is cheap: the MFT record behind it is cached
// on the volume. [File.ReadAt] takes no offset from the handle and is safe to
// call concurrently on the same [File]; [File.Read] is not, since it advances a
// shared offset.
//
// The [MFTEntry] values handed out by [Volume.GetMFTEntry] and carried inside a
// [File] come from that cache and are shared between goroutines. Treat them as
// read-only.
//
// # Parallel walks
//
// [Volume.WalkMFT] parses records on a bounded worker pool and delivers them to
// the callback one at a time in ascending entry order, so a parallel walk
// returns exactly what a sequential one returns. The callback needs no locking
// and the worker count is a tuning decision rather than a semantic one.
//
// It is off by default. Whether a pool helps depends on whether the volume's
// [io.ReaderAt] serves concurrent reads concurrently, which the library cannot
// determine. Walking the same volume: backed by an *os.File on Windows,
// sequential took 13.5 ms and eight workers 22.9 ms, because the read syscall
// dominates and the kernel serialises concurrent reads against one handle;
// backed by memory, sequential took 4.5 ms and eight workers 1.3 ms. Callers
// whose reader parallelises should opt in with [WorkersAuto].
//
// [Volume.EachMFTEntryContext] is the sequential walk with cancellation, and
// [Volume.WalkFiles] is the parallel one with paths attached.
//
// # What this library does not do
//
// Encrypted file content is detected, not decrypted: a read returns
// [ErrEncryptedData]. Windows Overlay Filter (CompactOS) content is detected,
// not decoded: a read of the unnamed stream returns [ErrWOFCompressed], and
// the compressed bytes stay readable through the WofCompressedData stream.
// The transaction log is read, not replayed — records are
// decoded and handed over, but reconstructing file content from redo data and
// undoing to a consistent state are out of scope. Nothing here mounts, repairs,
// or allocates: write support is in-place overwrites of the same length and
// nothing more, as described in write.go. Reparse points are reported, never
// followed.
//
// # Damaged input
//
// Every parser here is fed bytes lifted straight off a disk image, which in a
// forensic context is untrusted by definition. Damage is skipped rather than
// treated as fatal wherever a partial result is still useful: a corrupt region
// of the change journal does not hide the records after it, a torn restart page
// falls back to its second copy, and a walk over the MFT reports the records
// that parse rather than stopping at the first that does not.
//
// Where a structure cannot be validated, this library prefers reporting nothing
// to reporting something plausible. Recovering a deleted name from index slack
// rejects candidates that do not look like real names, because a phantom file in
// a forensic report is worse than a missing one.
//
// A caller that needs the opposite — to know a volume is exactly well formed,
// such as a tool checking images it generated — sets Options.Strict. Damage
// the default reader reads past then fails the call with a [*StrictError]
// naming the record, index and VCN; [Volume.VerifyReparseIndex] applies the
// same discipline to $Extend\$Reparse.
package libntfs

// Version is the libntfs release this build was cut from.
//
// It is maintained by hand and is only as accurate as the last person to bump
// it; nothing here can check it against the git tag. It is recorded on
// [NTFSReport] so a document can say what produced it.
const Version = "v0.3.6"

// ReportSchemaVersion is the schema [NTFSReport] documents are written against,
// and the value of their SchemaVersion field.
//
// Increment it when a field is removed, renamed, or changes meaning. Adding a
// field does not, since a consumer that ignores unknown keys is unaffected by
// one. Nothing in the test suite can detect a forgotten increment, so it is on
// whoever changes the shape.
//
// 1 is the first published schema. It is also the schema in which StartOffset,
// EndOffset and [FileFragment] offsets became image-absolute rather than
// volume-relative.
const ReportSchemaVersion = 1
