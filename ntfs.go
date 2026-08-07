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
//	vol, err := libntfs.Open(f)
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
// [Open] expects the NTFS boot sector at offset zero. For a partition inside a
// whole-disk image, wrap the file first:
//
//	vol, err := libntfs.Open(io.NewSectionReader(f, partitionOffset, partitionSize))
//
// # The API by area
//
// Volume — geometry and entry points. [Open], [Volume.Open],
// [Volume.OpenPath], [Volume.GetRootDirectory], [Volume.BytesPerCluster],
// [Volume.VolumeSize], [Volume.Report].
//
// Files and directories — [File.Read], [File.ReadAt], [File.ReadAll],
// [File.ReadDir], [File.GetMetadata] for the full MACB set including the MFT
// change time, and [File.ReadSupport] to ask whether a stream is readable before
// trying. [File.ReadDir] returns deleted entries alongside live ones; check
// [DirEntry.Deleted].
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
//
// # Working without a volume
//
// Several parsers take a byte buffer and need no [Volume] at all, for artifacts
// extracted from an image by another tool: [ParseMFTRecord], [ParseUSNRecord],
// [EachUSNRecord], [ParseLogRestartPage], [ParseLogRecord], [EachLogRecord],
// [ParseSecurityDescriptor], [ParseExtendedAttributes], [ParseReparsePoint],
// [ParseLinuxAttributes], and [ParseLinuxExtendedAttributes].
//
// # Concurrency
//
// A [Volume] is safe for concurrent use. Its geometry is written once during
// [Open] and read-only afterwards; the MFT cache, the security descriptor map,
// and the parent-link map are each guarded by their own lock, so contention on
// one does not block the others.
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
// [Volume.EachMFTEntryContext] is the sequential walk with cancellation.
//
// # What this library does not do
//
// Encrypted file content is detected, not decrypted: a read returns
// [ErrEncryptedData]. The transaction log is read, not replayed — records are
// decoded and handed over, but reconstructing file content from redo data and
// undoing to a consistent state are out of scope. Write support is limited to
// in-place overwrites of the same length, with no allocation, growth, or
// truncation. Reparse points are reported, never followed.
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
package libntfs
