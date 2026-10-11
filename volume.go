package libntfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
)

// Volume represents an NTFS volume with thread-safe access.
type Volume struct {
	reader io.ReaderAt // Underlying storage (disk, image file, etc.)
	writer io.WriterAt // Optional writable storage for in-place updates

	// Boot sector information
	bootSector        *BootSector
	bytesPerSector    uint32
	sectorsPerCluster uint32
	bytesPerCluster   uint32
	clusterCount      uint64
	mftCluster        uint64
	mftRecordSize     uint32
	indexRecordSize   uint32
	volumeSize        uint64

	// MFT information
	mftDataRuns    []DataRun
	mftDataRunEnds []uint64 // cumulative byte ends for mftDataRuns

	// Caching and synchronization
	mu         sync.RWMutex         // Protects the entire volume
	mftCache   map[uint64]*MFTEntry // Cached MFT entries
	mftCacheMu sync.RWMutex         // Protects MFT cache

	// mftCacheFull is set once the cache reaches DefaultMFTCacheSize.
	//
	// After that every insert is a no-op, and taking the exclusive lock to
	// discover that is pure contention: a profile of an eight-worker walk put
	// 48% of all lock delay in GetMFTEntry doing exactly that. Reading it
	// atomically lets a saturated cache skip the write lock entirely.
	mftCacheFull         atomic.Bool
	bufferPool           *sync.Pool                         // Pool for reusable buffers
	mftParentMap         map[uint64]map[uint16][]IndexEntry // parent MFT -> parent sequence -> child entries
	mftParentMapMu       sync.RWMutex                       // Protects mftParentMap state
	mftParentMapInit     bool                               // True when mftParentMap has been populated
	useMFTParentFallback bool                               // Enable expensive full-MFT parent-link fallback in ReadDir

	securityMap  map[uint32]*SecurityDescriptor // $Secure:$SDS descriptors keyed by security ID
	securityMu   sync.RWMutex                   // Protects securityMap state
	securityInit bool                           // True when securityMap has been populated
	securityErr  error                          // Sticky error from the $SDS load

	upcase     UpCase     // $UpCase, loaded on first use
	upcaseMu   sync.Mutex // Protects upcase state
	upcaseInit bool       // True when upcase has been loaded
	upcaseErr  error      // Sticky error from the $UpCase load

	// changeJournal caches whether $UsnJrnl is present and carrying records on
	// this volume, since answering costs a directory read of $Extend and
	// Capabilities has no error to return. changeJournalInit is deliberately not
	// set when the lookup failed because the volume was closed, so a probe on a
	// closed handle does not pin the answer for a volume that was fine.
	changeJournal     bool
	changeJournalMu   sync.RWMutex
	changeJournalInit bool
	changeJournalErr  error

	// readOnly records that the caller asked for a read-only volume. It is set
	// once during Open and never mutated, so it needs no lock: every write entry
	// point consults it before touching the handle.
	readOnly bool

	// strict records Options.Strict. Like readOnly it is set once during Open.
	strict bool

	// baseOffset is where the volume begins inside the reader handed to Open.
	//
	// It is the single translation constant between the two coordinate spaces
	// this library uses: image = volume + baseOffset. The reader wrappers built
	// by bindHandles already apply it to every read, so everything unexported
	// here speaks volume space; the exported surface speaks image space.
	baseOffset int64

	// State
	closed  bool
	closeMu sync.RWMutex
}

// Options configures how a volume is opened.
//
// The zero value reproduces [Open]: the boot sector is expected at offset zero,
// and a reader that also implements [io.WriterAt] yields a writable volume.
type Options struct {
	// ReadOnly opens the volume for reading only.
	//
	// A read-only volume never writes, whatever the reader turns out to support.
	// The [io.WriterAt] promotion [Open] performs is skipped, the handle is
	// wrapped so it cannot be recovered by a type assertion, [Volume.IsWritable]
	// reports false, and every write entry point returns [ErrReadOnlyMode]
	// without touching the underlying handle.
	//
	// This is the mode to use on evidence. It costs nothing and removes the
	// possibility of an accidental write entirely.
	ReadOnly bool

	// BaseOffset is the byte offset within reader at which the NTFS boot sector
	// begins, for a partition inside a whole-disk image.
	//
	// It does two jobs. Every read and write is shifted by this amount, so the
	// library finds the volume at all; and it is added to every absolute disk
	// offset the library reports, so those offsets are image-absolute —
	// [FileFragment] locations, the [NTFSReport] span, [Volume.ClusterToOffset].
	// Zero means the volume starts at the beginning of the reader, and the two
	// spaces coincide.
	//
	// That second job is the one worth stating plainly. An offset reported
	// relative to the volume cannot be intersected with a range expressed in
	// whole-disk coordinates, and nothing about the two makes the mismatch
	// visible: the intersection succeeds and is wrong. Reporting image-absolute
	// offsets removes that failure rather than documenting it.
	// [Volume.ReadAt] and [Volume.WriteAt] accept image offsets to match, so a
	// fragment offset can be handed straight back to them.
	//
	// It is an alternative to wrapping the reader in [io.SectionReader], and no
	// longer an equivalent one: a section reader cannot say where the partition
	// sits, so a volume opened over one reports offsets relative to the section.
	// Prefer this field, and reach for a section reader when the volume's length
	// also needs bounding.
	//
	// It must be a whole number of sectors. A partition never begins mid-sector,
	// and allowing one that did would misalign the read-modify-write underneath
	// [Volume.WriteAt].
	BaseOffset int64

	// Strict turns the structural damage the library otherwise tolerates into
	// errors, for a caller that needs to know a volume is exactly well formed
	// rather than to recover what it can from it.
	//
	// By default libntfs reads past damage: a directory with an unreadable INDX
	// block yields its other names with a [PartialDirError], and a freed block's
	// names come back marked Deleted. With Strict set, every check below fails
	// the call with a [*StrictError] naming the record, index and VCN:
	//
	//   - [File.ReadDir] and [File.WalkIndex] check the index B-tree: every
	//     block is reached at most once from the root, at most 32 levels down,
	//     on a block boundary, and carries a valid INDX header, update sequence
	//     and its own VCN; the $BITMAP exists and marks in use exactly the
	//     blocks reached; every entry of a node, and none of a leaf, points at a
	//     sub-node; and the keys come out in collation order ($UpCase for
	//     $I30, ULONG or ULONGS for the view indexes; SID and Unicode
	//     collations are not checked). A partial listing is an error.
	//   - [File.ReadDir] also checks each live entry against the record it
	//     names: the record is in use, its sequence number matches the
	//     reference, it holds a $FILE_NAME with the entry's parent and name, and
	//     its directory flag, its $I30 index and the entry's directory attribute
	//     agree.
	//   - [Volume.Open] checks that the runs of every non-resident $DATA cover
	//     exactly its allocated size, and that its real size fits inside it.
	//
	// [Volume.VerifyReparseIndex] applies the same checks to $Reparse:$R
	// whatever this setting.
	Strict bool
}

// Open opens an NTFS volume from the given reader.
// The reader should provide access to the raw volume data (e.g., a disk or disk image).
//
// If reader also implements [io.WriterAt], the volume is writable. Callers
// working on evidence should use [OpenReadOnly] instead, which refuses writes
// whatever the reader supports.
func Open(reader io.ReaderAt) (*Volume, error) {
	return OpenWithOptions(reader, Options{})
}

// OpenReadOnly opens an NTFS volume that can never be written to.
//
// It is [Open] with Options.ReadOnly set: the [io.WriterAt] promotion is
// skipped, so a reader that happens to support writes — an *os.File opened
// O_RDWR, a memory-backed image, a device handle — cannot be used to modify the
// volume through this library. Every write entry point returns [ErrReadOnlyMode].
//
// Prefer it for forensic work, where a mutable handle on evidence is a hazard
// rather than a feature.
func OpenReadOnly(reader io.ReaderAt) (*Volume, error) {
	return OpenWithOptions(reader, Options{ReadOnly: true})
}

// OpenWithOptions opens an NTFS volume with explicit options.
// See [Options] for what each one does.
func OpenWithOptions(reader io.ReaderAt, opts Options) (*Volume, error) {
	if reader == nil {
		return nil, wrapVolumeError("open", fmt.Errorf("reader is nil"))
	}

	if opts.BaseOffset < 0 {
		return nil, wrapVolumeError("open",
			fmt.Errorf("%w: negative base offset %d", ErrInvalidOffset, opts.BaseOffset))
	}

	// If the reader can be stat'ed, reject directory inputs explicitly. This runs
	// before any wrapping, which would hide the Stat method.
	type statReader interface {
		Stat() (fs.FileInfo, error)
	}
	if sr, ok := reader.(statReader); ok {
		if info, err := sr.Stat(); err == nil && info.IsDir() {
			return nil, wrapVolumeError("open", fmt.Errorf("%w: %s", ErrInputIsDirectory, info.Name()))
		}
	}

	v := &Volume{
		readOnly:   opts.ReadOnly,
		strict:     opts.Strict,
		baseOffset: opts.BaseOffset,
		mftCache:   make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
		bufferPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, DefaultBufferSize)
				return &buf
			},
		},
	}

	v.reader, v.writer = bindHandles(reader, opts)

	// Parse boot sector
	if err := v.parseBootSector(); err != nil {
		return nil, wrapVolumeError("open", err)
	}

	// The sector size is only known once the boot sector has parsed, so the
	// alignment of BaseOffset can only be checked here. A base that is not a
	// whole number of sectors would misalign readAtSectorAligned and, worse,
	// the read-modify-write in writeAtSectorAligned.
	if v.bytesPerSector > 0 && opts.BaseOffset%int64(v.bytesPerSector) != 0 {
		return nil, wrapVolumeError("open",
			fmt.Errorf("%w: base offset %d is not a multiple of the %d-byte sector",
				ErrInvalidOffset, opts.BaseOffset, v.bytesPerSector))
	}

	// Initialize MFT
	if err := v.initializeMFT(); err != nil {
		return nil, wrapVolumeError("open", err)
	}

	return v, nil
}

// bindHandles decides what the volume reads through and whether it can write at
// all.
//
// A read-only volume gets its reader wrapped in a type that exposes nothing but
// ReadAt, so the io.WriterAt promotion cannot fire here and the writable handle
// cannot be recovered later by a type assertion on the stored reader. That is
// belt and braces — nothing in the library asserts on v.reader — but the whole
// point of the mode is that no code path can reach a write, and a wrapper is a
// cheaper guarantee than a review.
func bindHandles(reader io.ReaderAt, opts Options) (io.ReaderAt, io.WriterAt) {
	if opts.ReadOnly {
		return readOnlyReaderAt{r: reader, base: opts.BaseOffset}, nil
	}

	writer, _ := reader.(io.WriterAt)

	if opts.BaseOffset == 0 {
		return reader, writer
	}

	shifted := &offsetHandle{r: reader, w: writer, base: opts.BaseOffset}
	if writer == nil {
		// Returning the struct as an io.WriterAt would hand back a non-nil
		// interface holding a nil-writing value, and IsWritable would lie.
		return shifted, nil
	}

	return shifted, shifted
}

// readOnlyReaderAt exposes ReadAt and nothing else, optionally shifted by base.
//
// It is deliberately not a pointer type and carries no writer: an assertion to
// io.WriterAt on it cannot succeed.
type readOnlyReaderAt struct {
	r    io.ReaderAt
	base int64
}

func (ro readOnlyReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	return ro.r.ReadAt(p, ro.base+off)
}

// offsetHandle shifts a read/write handle so volume offset zero lands at base.
type offsetHandle struct {
	r    io.ReaderAt
	w    io.WriterAt
	base int64
}

func (o *offsetHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	return o.r.ReadAt(p, o.base+off)
}

func (o *offsetHandle) WriteAt(p []byte, off int64) (int, error) {
	if o.w == nil {
		return 0, ErrReadOnlyVolume
	}
	if off < 0 {
		return 0, ErrInvalidOffset
	}
	return o.w.WriteAt(p, o.base+off)
}

// Close closes the volume and releases resources.
func (v *Volume) Close() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()

	if v.closed {
		return ErrVolumeClosed
	}

	v.closed = true

	// Clear caches
	v.mftCacheMu.Lock()
	v.mftCache = nil
	v.mftCacheMu.Unlock()

	return nil
}

// IsClosed returns true if the volume has been closed.
func (v *Volume) IsClosed() bool {
	v.closeMu.RLock()
	defer v.closeMu.RUnlock()
	return v.closed
}

// BaseOffset returns where this volume begins inside the reader it was opened
// over, as given by Options.BaseOffset.
//
// It is the constant relating the two coordinate spaces in this package: an
// offset the library reports, minus this, is the offset within the volume
// itself. A caller comparing reports taken from a whole-disk image and from the
// same partition acquired on its own needs it to line them up.
func (v *Volume) BaseOffset() int64 {
	return v.baseOffset
}

// IsReadOnly reports whether the volume was opened read-only.
//
// It answers a different question from [Volume.IsWritable]: a volume opened
// normally over a reader that cannot write is not writable but is also not in
// read-only mode, and one opened with [OpenReadOnly] is both.
func (v *Volume) IsReadOnly() bool {
	return v.readOnly
}

// parseBootSector reads and parses the NTFS boot sector.
func (v *Volume) parseBootSector() error {
	// Read boot sector (sector 0)
	buf := make([]byte, BootSectorSize)
	if _, err := v.reader.ReadAt(buf, 0); err != nil {
		return v.ioErrorAt("read", 0, BootSectorSize, err)
	}

	r := NewBinaryReader(buf)
	bs := &BootSector{}

	// Parse boot sector fields according to NTFS specification
	// Offset 0: Jump instruction (3 bytes)
	jump, _ := r.ReadBytes(3)
	copy(bs.Jump[:], jump)

	// Offset 3: OEM Name (8 bytes)
	oemName, _ := r.ReadBytes(8)
	copy(bs.OEMName[:], oemName)

	// Offset 11: Bytes per sector (2 bytes)
	bs.BytesPerSector, _ = r.ReadUint16()

	// Offset 13: Sectors per cluster (1 byte)
	bs.SectorsPerCluster, _ = r.ReadUint8()

	// Offset 14: Reserved (26 bytes) - skip to offset 40
	r.Skip(26)

	// Offset 40: Total sectors (8 bytes)
	bs.TotalSectors, _ = r.ReadUint64()

	// Offset 48: MFT cluster (8 bytes)
	bs.MFTCluster, _ = r.ReadUint64()

	// Offset 56: MFT Mirror cluster (8 bytes)
	bs.MFTMirrorCluster, _ = r.ReadUint64()

	// Offset 64: Clusters per MFT record (1 signed byte)
	bs.ClustersPerMFTRecord, _ = r.ReadInt8()

	// Offset 65: Reserved (3 bytes)
	r.Skip(3)

	// Offset 68: Clusters per index record (1 signed byte)
	bs.ClustersPerIndexRecord, _ = r.ReadInt8()

	// Offset 69: Reserved (3 bytes)
	r.Skip(3)

	// Offset 72: Volume serial number (8 bytes)
	bs.VolumeSerialNumber, _ = r.ReadUint64()

	// Offset 80: Checksum (4 bytes)
	bs.Checksum, _ = r.ReadUint32()

	// Offset 84-509: Boot code (426 bytes) - skip to offset 510
	r.Seek(510)

	// Offset 510: End marker (2 bytes) - should be 0xAA55
	bs.EndMarker, _ = r.ReadUint16()

	// Validate boot sector
	if bs.EndMarker != BootSectorMagic {
		return fmt.Errorf("%w: invalid end marker 0x%X", ErrInvalidBootSector, bs.EndMarker)
	}

	if bs.BytesPerSector == 0 || bs.BytesPerSector > MaxSectorSize {
		return fmt.Errorf("%w: invalid sector size %d", ErrInvalidBootSector, bs.BytesPerSector)
	}

	sectorsPerCluster, ok := decodeSectorsPerCluster(bs.SectorsPerCluster)
	if !ok {
		return fmt.Errorf("%w: invalid sectors per cluster %d", ErrInvalidBootSector, bs.SectorsPerCluster)
	}

	// Calculate derived values
	v.bootSector = bs
	v.bytesPerSector = uint32(bs.BytesPerSector)
	v.sectorsPerCluster = sectorsPerCluster
	v.bytesPerCluster = v.bytesPerSector * v.sectorsPerCluster
	v.mftCluster = bs.MFTCluster
	v.volumeSize = bs.TotalSectors * uint64(bs.BytesPerSector)
	v.clusterCount = v.volumeSize / uint64(v.bytesPerCluster)

	// Calculate MFT record size
	// Positive value = number of clusters per record
	// Negative value = 2^abs(value) bytes per record
	if bs.ClustersPerMFTRecord >= 0 {
		v.mftRecordSize = uint32(bs.ClustersPerMFTRecord) * v.bytesPerCluster
	} else {
		// Negative value means size is 2^abs(value)
		v.mftRecordSize = 1 << uint(-bs.ClustersPerMFTRecord)
	}

	// Calculate index record size
	if bs.ClustersPerIndexRecord >= 0 {
		v.indexRecordSize = uint32(bs.ClustersPerIndexRecord) * v.bytesPerCluster
	} else {
		v.indexRecordSize = 1 << uint(-bs.ClustersPerIndexRecord)
	}

	// Sanity checks with better error messages
	if v.mftRecordSize == 0 {
		return fmt.Errorf("%w: invalid MFT record size %d (ClustersPerMFTRecord=%d, BytesPerCluster=%d)",
			ErrInvalidBootSector, v.mftRecordSize, bs.ClustersPerMFTRecord, v.bytesPerCluster)
	}

	if v.mftRecordSize < MinMFTEntrySize || v.mftRecordSize > MaxMFTEntrySize {
		return fmt.Errorf("%w: invalid MFT record size %d (ClustersPerMFTRecord=%d)",
			ErrInvalidBootSector, v.mftRecordSize, bs.ClustersPerMFTRecord)
	}

	// An INDX record has to hold its own 24-byte header plus a 16-byte node
	// header before any entry, and the fixup array assumes 512-byte sectors.
	// A damaged boot sector can encode a size below that -- ClustersPerIndexRecord
	// of -1 yields 2 -- which the index reader would otherwise take at face value.
	if v.indexRecordSize < MinIndexRecordSize || v.indexRecordSize > MaxIndexRecordSize ||
		v.indexRecordSize&(v.indexRecordSize-1) != 0 {
		return fmt.Errorf("%w: invalid index record size %d (ClustersPerIndexRecord=%d)",
			ErrInvalidBootSector, v.indexRecordSize, bs.ClustersPerIndexRecord)
	}

	if v.bytesPerCluster > MaxClusterSize {
		return fmt.Errorf("%w: invalid cluster size %d", ErrInvalidBootSector, v.bytesPerCluster)
	}

	return nil
}

// decodeSectorsPerCluster decodes the boot sector's sectors-per-cluster byte.
//
// Up to 0x80 the byte is the count itself. Above it, the byte is a negative
// exponent: the count is 2^(256-b), so 0xF8 is 256 sectors and 0xF4 is 4096.
// That encoding is how Windows formats clusters larger than 64 KiB. A shift too
// large to describe a cluster NTFS supports is rejected here; the resulting
// cluster size is checked against MaxClusterSize by the caller.
func decodeSectorsPerCluster(b uint8) (uint32, bool) {
	switch {
	case b == 0:
		return 0, false
	case b <= 0x80:
		return uint32(b), true
	}
	shift := 256 - int(b)
	if shift > 21 { // 2^21 sectors is 1 GiB at 512 bytes, far past MaxClusterSize
		return 0, false
	}
	return 1 << shift, true
}

// initializeMFT reads the $MFT entry and extracts its data runs.
func (v *Volume) initializeMFT() error {
	// Read the first MFT entry (entry 0 = $MFT itself)
	// The $MFT entry describes where the MFT is located on disk
	mftOffset := v.clusterToVolumeOffset(v.mftCluster)

	mftEntry, err := v.readMFTEntryAt(0, mftOffset)
	if err != nil {
		return fmt.Errorf("failed to read $MFT entry: %w", err)
	}

	// Resolve $ATTRIBUTE_LIST so that fragmented $MFT DATA runs stored in
	// extension records (common on large volumes) are merged in VCN order.
	_ = v.resolveAttributeList(mftEntry, 0)

	// Find the non-resident primary $DATA attribute which contains the MFT data runs.
	// Prefer unnamed stream to avoid selecting alternate data streams first.
	dataAttr := mftEntry.FindPrimaryNonResidentDataAttribute()
	if dataAttr == nil {
		return fmt.Errorf("$MFT entry missing $DATA attribute")
	}

	if dataAttr.NonResident == nil {
		return fmt.Errorf("$MFT $DATA attribute must be non-resident")
	}

	// Store the data runs for future MFT entry lookups
	v.mftDataRuns = dataAttr.NonResident.DataRuns
	v.rebuildMFTRunIndex()

	return nil
}

// ClusterToOffset converts a cluster number to an image-absolute byte offset.
//
// The result includes Options.BaseOffset, so it locates the cluster in the
// reader the volume was opened over rather than within the volume. It is the
// inverse of [Volume.OffsetToCluster].
func (v *Volume) ClusterToOffset(cluster uint64) int64 {
	return v.baseOffset + v.clusterToVolumeOffset(cluster)
}

// clusterToVolumeOffset converts a cluster number to a volume-relative byte
// offset, which is the space v.reader reads in. Every internal caller wants
// this one; [Volume.ClusterToOffset] is for callers outside the package.
func (v *Volume) clusterToVolumeOffset(cluster uint64) int64 {
	return int64(cluster * uint64(v.bytesPerCluster))
}

// OffsetToCluster converts an image-absolute byte offset to a cluster number.
//
// It expects the same space [Volume.ClusterToOffset] produces, so an offset
// below Options.BaseOffset is not on this volume at all. Having no error to
// return, it answers zero for that and for a volume with no geometry, rather
// than wrapping a negative offset into an enormous cluster number.
func (v *Volume) OffsetToCluster(offset int64) uint64 {
	if v.bytesPerCluster == 0 || offset < v.baseOffset {
		return 0
	}
	return uint64(offset-v.baseOffset) / uint64(v.bytesPerCluster)
}

// ReadAt reads len(p) bytes from the volume at the given image-absolute offset.
//
// The offset is in the same space the library reports offsets in, so a
// [FileFragment]'s StartOffset can be passed straight in. It includes
// Options.BaseOffset; an offset below that is not on this volume and returns
// [ErrInvalidOffset].
//
// This method is thread-safe.
func (v *Volume) ReadAt(p []byte, offset int64) (int, error) {
	// A closed volume is closed whatever it was handed, matching how
	// [Volume.WriteAt] reports its gate before looking at the offset. The cost
	// is one extra read lock on a path that is about to do I/O anyway.
	if v.IsClosed() {
		return 0, ErrVolumeClosed
	}

	if offset < v.baseOffset {
		return 0, ErrInvalidOffset
	}

	return v.readAtVolume(p, offset-v.baseOffset)
}

// readAtVolume is [Volume.ReadAt] in volume space, which is what v.reader
// speaks.
//
// The whole body lives below the image-to-volume translation deliberately. The
// sector alignment below rounds the offset down to a sector boundary and hands
// the result to v.reader, which then adds the base itself; doing that rounding
// on an image offset would land the read on a boundary in the wrong space.
func (v *Volume) readAtVolume(p []byte, offset int64) (int, error) {
	if v.IsClosed() {
		return 0, ErrVolumeClosed
	}

	if offset < 0 {
		return 0, ErrInvalidOffset
	}

	if len(p) == 0 {
		return 0, nil
	}

	sectorSize := int64(v.bytesPerSector)
	if sectorSize > 0 {
		unalignedOffset := offset%sectorSize != 0
		unalignedLength := int64(len(p))%sectorSize != 0
		if unalignedOffset || unalignedLength {
			return v.readAtSectorAligned(p, offset, sectorSize)
		}
	}

	return v.reader.ReadAt(p, offset)
}

func (v *Volume) readAtSectorAligned(p []byte, offset, sectorSize int64) (int, error) {
	alignedStart := offset - (offset % sectorSize)
	alignedEnd := alignUpInt64(offset+int64(len(p)), sectorSize)
	if alignedEnd <= alignedStart {
		return 0, ErrInvalidOffset
	}

	bufLen := alignedEnd - alignedStart
	buf := make([]byte, bufLen)

	n, err := v.reader.ReadAt(buf, alignedStart)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, v.ioErrorAt("read", alignedStart, len(buf), err)
	}

	start := offset - alignedStart
	readLen := copy(p, buf[start:])
	if readLen < len(p) {
		return readLen, io.EOF
	}
	if err != nil {
		return readLen, io.EOF
	}

	_ = n
	return readLen, nil
}

func alignUpInt64(v, align int64) int64 {
	if align <= 0 {
		return v
	}
	rem := v % align
	if rem == 0 {
		return v
	}
	return v + (align - rem)
}

// ReadClusters reads the specified number of clusters starting at the given cluster.
// This method is thread-safe.
func (v *Volume) ReadClusters(cluster uint64, count uint32) ([]byte, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if cluster >= v.clusterCount {
		return nil, ErrInvalidCluster
	}

	size := uint32(count) * v.bytesPerCluster
	buf := make([]byte, size)
	offset := v.clusterToVolumeOffset(cluster)

	if _, err := v.reader.ReadAt(buf, offset); err != nil {
		return nil, v.ioErrorAt("read", offset, int(size), err)
	}

	return buf, nil
}

// GetBuffer obtains a scratch buffer of DefaultBufferSize bytes.
//
// Buffers come from a pool shared across goroutines, so a buffer must be
// returned with PutBuffer exactly once and must not be used afterwards. The
// contents are whatever the previous holder left behind; callers that need
// zeroed memory must clear it.
//
// A Volume assembled without Open has no pool, in which case this allocates
// rather than panicking: the pool is an optimisation, and losing it should cost
// speed rather than correctness.
func (v *Volume) GetBuffer() *[]byte {
	if v.bufferPool == nil {
		buf := make([]byte, DefaultBufferSize)
		return &buf
	}

	buf, ok := v.bufferPool.Get().(*[]byte)
	if !ok || buf == nil {
		fresh := make([]byte, DefaultBufferSize)
		return &fresh
	}

	return buf
}

// PutBuffer returns a buffer obtained from GetBuffer to the pool.
//
// Returning a buffer twice, or using it after returning it, hands the same
// memory to two goroutines. A nil buffer, or one from a Volume with no pool, is
// ignored.
func (v *Volume) PutBuffer(buf *[]byte) {
	if buf == nil || v.bufferPool == nil {
		return
	}

	v.bufferPool.Put(buf)
}

// SetMFTParentFallback enables or disables the TSK-style parent-link fallback
// used by ReadDir when recovering entries missing from the directory index.
// When enabled, the first fallback query can be expensive on large volumes
// because it may scan the full MFT to build parent-link caches.
func (v *Volume) SetMFTParentFallback(enabled bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.useMFTParentFallback = enabled
}

func (v *Volume) mftParentFallbackEnabled() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.useMFTParentFallback
}

// GetBootSector returns a copy of the boot sector information.
func (v *Volume) GetBootSector() *BootSector {
	v.mu.RLock()
	defer v.mu.RUnlock()

	bs := *v.bootSector
	return &bs
}

// BytesPerCluster returns the number of bytes per cluster.
func (v *Volume) BytesPerCluster() uint32 {
	return v.bytesPerCluster
}

// BytesPerSector returns the number of bytes per sector.
func (v *Volume) BytesPerSector() uint32 {
	return v.bytesPerSector
}

// MFTRecordSize returns the size of each MFT record in bytes.
func (v *Volume) MFTRecordSize() uint32 {
	return v.mftRecordSize
}

// IndexRecordSize returns the size of each index record in bytes.
func (v *Volume) IndexRecordSize() uint32 {
	return v.indexRecordSize
}

// VolumeSize returns the total size of the volume in bytes.
func (v *Volume) VolumeSize() uint64 {
	return v.volumeSize
}

// ClusterCount returns the total number of clusters in the volume.
func (v *Volume) ClusterCount() uint64 {
	return v.clusterCount
}

// VolumeSerialNumber returns the volume serial number.
func (v *Volume) VolumeSerialNumber() uint64 {
	return v.bootSector.VolumeSerialNumber
}

// GetRootDirectory returns the root directory as a File.
// This is a convenience method equivalent to Open(MFTEntryRoot).
func (v *Volume) GetRootDirectory() (*File, error) {
	return v.Open(MFTEntryRoot)
}

// GetRootMFTEntry returns the root directory MFT entry.
func (v *Volume) GetRootMFTEntry() (*MFTEntry, error) {
	return v.GetMFTEntry(MFTEntryRoot)
}

// String returns a string representation of the volume.
func (v *Volume) String() string {
	return fmt.Sprintf("NTFS Volume: %d bytes (%d clusters of %d bytes), MFT record size: %d bytes",
		v.volumeSize, v.clusterCount, v.bytesPerCluster, v.mftRecordSize)
}
