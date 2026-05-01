package libntfs

import (
	"fmt"
	"io"
	"io/fs"
	"sync"
)

// Volume represents an NTFS volume with thread-safe access.
type Volume struct {
	reader io.ReaderAt // Underlying storage (disk, image file, etc.)

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
	mu                   sync.RWMutex                       // Protects the entire volume
	mftCache             map[uint64]*MFTEntry               // Cached MFT entries
	mftCacheMu           sync.RWMutex                       // Protects MFT cache
	bufferPool           *sync.Pool                         // Pool for reusable buffers
	mftParentMap         map[uint64]map[uint16][]IndexEntry // parent MFT -> parent sequence -> child entries
	mftParentMapMu       sync.RWMutex                       // Protects mftParentMap state
	mftParentMapInit     bool                               // True when mftParentMap has been populated
	useMFTParentFallback bool                               // Enable expensive full-MFT parent-link fallback in ReadDir

	// State
	closed  bool
	closeMu sync.RWMutex
}

// Open opens an NTFS volume from the given reader.
// The reader should provide access to the raw volume data (e.g., a disk or disk image).
func Open(reader io.ReaderAt) (*Volume, error) {
	if reader == nil {
		return nil, wrapVolumeError("open", fmt.Errorf("reader is nil"))
	}

	// If the reader can be stat'ed, reject directory inputs explicitly.
	type statReader interface {
		Stat() (fs.FileInfo, error)
	}
	if sr, ok := reader.(statReader); ok {
		if info, err := sr.Stat(); err == nil && info.IsDir() {
			return nil, wrapVolumeError("open", fmt.Errorf("%w: %s", ErrInputIsDirectory, info.Name()))
		}
	}

	v := &Volume{
		reader:   reader,
		mftCache: make(map[uint64]*MFTEntry, DefaultMFTCacheSize),
		bufferPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, DefaultBufferSize)
				return &buf
			},
		},
	}

	// Parse boot sector
	if err := v.parseBootSector(); err != nil {
		return nil, wrapVolumeError("open", err)
	}

	// Initialize MFT
	if err := v.initializeMFT(); err != nil {
		return nil, wrapVolumeError("open", err)
	}

	return v, nil
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

// parseBootSector reads and parses the NTFS boot sector.
func (v *Volume) parseBootSector() error {
	// Read boot sector (sector 0)
	buf := make([]byte, BootSectorSize)
	if _, err := v.reader.ReadAt(buf, 0); err != nil {
		return wrapIOError("read", 0, BootSectorSize, err)
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

	if bs.SectorsPerCluster == 0 || bs.SectorsPerCluster > 128 {
		return fmt.Errorf("%w: invalid sectors per cluster %d", ErrInvalidBootSector, bs.SectorsPerCluster)
	}

	// Calculate derived values
	v.bootSector = bs
	v.bytesPerSector = uint32(bs.BytesPerSector)
	v.sectorsPerCluster = uint32(bs.SectorsPerCluster)
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

	if v.mftRecordSize < 512 || v.mftRecordSize > MaxMFTEntrySize {
		return fmt.Errorf("%w: invalid MFT record size %d (ClustersPerMFTRecord=%d)",
			ErrInvalidBootSector, v.mftRecordSize, bs.ClustersPerMFTRecord)
	}

	if v.indexRecordSize == 0 {
		return fmt.Errorf("%w: invalid index record size %d (ClustersPerIndexRecord=%d)",
			ErrInvalidBootSector, v.indexRecordSize, bs.ClustersPerIndexRecord)
	}

	if v.bytesPerCluster > MaxClusterSize {
		return fmt.Errorf("%w: invalid cluster size %d", ErrInvalidBootSector, v.bytesPerCluster)
	}

	return nil
}

// initializeMFT reads the $MFT entry and extracts its data runs.
func (v *Volume) initializeMFT() error {
	// Read the first MFT entry (entry 0 = $MFT itself)
	// The $MFT entry describes where the MFT is located on disk
	mftOffset := v.ClusterToOffset(v.mftCluster)

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

// ClusterToOffset converts a cluster number to a byte offset.
func (v *Volume) ClusterToOffset(cluster uint64) int64 {
	return int64(cluster * uint64(v.bytesPerCluster))
}

// OffsetToCluster converts a byte offset to a cluster number.
func (v *Volume) OffsetToCluster(offset int64) uint64 {
	return uint64(offset) / uint64(v.bytesPerCluster)
}

// ReadAt reads len(p) bytes from the volume at the specified offset.
// This method is thread-safe.
func (v *Volume) ReadAt(p []byte, offset int64) (int, error) {
	if v.IsClosed() {
		return 0, ErrVolumeClosed
	}

	return v.reader.ReadAt(p, offset)
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
	offset := v.ClusterToOffset(cluster)

	if _, err := v.reader.ReadAt(buf, offset); err != nil {
		return nil, wrapIOError("read", offset, int(size), err)
	}

	return buf, nil
}

// GetBuffer obtains a buffer from the pool.
func (v *Volume) GetBuffer() *[]byte {
	return v.bufferPool.Get().(*[]byte)
}

// PutBuffer returns a buffer to the pool.
func (v *Volume) PutBuffer(buf *[]byte) {
	if buf != nil {
		v.bufferPool.Put(buf)
	}
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
