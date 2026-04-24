package libntfs

import (
	"time"
)

// BootSector represents the NTFS boot sector (located at sector 0 in $Boot).
// It contains critical file system parameters and volume information.
type BootSector struct {
	Jump                   [3]byte   // Jump instruction
	OEMName                [8]byte   // OEM Name (usually "NTFS    ")
	BytesPerSector         uint16    // Bytes per sector (usually 512)
	SectorsPerCluster      uint8     // Sectors per cluster
	Reserved1              [26]byte  // Reserved
	MediaDescriptor        uint8     // Media descriptor
	Reserved2              [2]byte   // Reserved (always 0)
	SectorsPerTrack        uint16    // Sectors per track
	NumberOfHeads          uint16    // Number of heads
	HiddenSectors          uint32    // Hidden sectors
	Reserved3              [8]byte   // Reserved
	TotalSectors           uint64    // Total sectors in volume
	MFTCluster             uint64    // Starting cluster of $MFT
	MFTMirrorCluster       uint64    // Starting cluster of $MFTMirr
	ClustersPerMFTRecord   int8      // Clusters per MFT record (can be negative)
	Reserved4              [3]byte   // Reserved
	ClustersPerIndexRecord int8      // Clusters per index record
	Reserved5              [3]byte   // Reserved
	VolumeSerialNumber     uint64    // Volume serial number
	Checksum               uint32    // Checksum
	BootCode               [426]byte // Boot code
	EndMarker              uint16    // End of sector marker (0xAA55)
}

// MFTEntry represents a Master File Table entry.
// Each file and directory has at least one MFT entry.
type MFTEntry struct {
	Magic           uint32 // Magic number ("FILE" = 0x454C4946)
	UpdateSeqOffset uint16 // Offset to update sequence
	UpdateSeqSize   uint16 // Size of update sequence (entries + 1)
	LogFileSeqNum   uint64 // $LogFile sequence number
	SequenceNum     uint16 // Sequence number (for reuse detection)
	HardLinkCount   uint16 // Hard link count
	FirstAttrOffset uint16 // Offset to first attribute
	Flags           uint16 // Flags (in use, directory)
	UsedSize        uint32 // Used size of MFT entry
	AllocatedSize   uint32 // Allocated size of MFT entry
	BaseRecordRef   uint64 // Base file record reference
	NextAttrID      uint16 // Next attribute ID
	Reserved        uint16 // Reserved (XP+)
	RecordNumber    uint32 // MFT record number (XP+)

	// Parsed data (not from disk)
	UpdateSeqArray []uint16     // Update sequence array
	Attributes     []*Attribute // List of attributes
}

// MFT entry flags
const (
	MFTFlagInUse     = 0x0001 // Entry is in use
	MFTFlagDirectory = 0x0002 // Entry is a directory
)

// MFT magic values
const (
	MFTMagicFILE = 0x454C4946 // "FILE"
	MFTMagicBAAD = 0x44414142 // "BAAD" - corrupted entry
	MFTMagicZERO = 0x00000000 // Unallocated entry
)

// Well-known MFT entry numbers
const (
	MFTEntryMFT     = 0  // $MFT - Master File Table
	MFTEntryMFTMirr = 1  // $MFTMirr - MFT Mirror
	MFTEntryLogFile = 2  // $LogFile - Transaction log
	MFTEntryVolume  = 3  // $Volume - Volume metadata
	MFTEntryAttrDef = 4  // $AttrDef - Attribute definitions
	MFTEntryRoot    = 5  // Root directory "."
	MFTEntryBitmap  = 6  // $Bitmap - Cluster allocation bitmap
	MFTEntryBoot    = 7  // $Boot - Boot sector
	MFTEntryBadClus = 8  // $BadClus - Bad cluster list
	MFTEntrySecure  = 9  // $Secure - Security descriptors
	MFTEntryUpCase  = 10 // $UpCase - Uppercase conversion table
	MFTEntryExtend  = 11 // $Extend - Extended metadata directory
)

// AttributeHeader is the common header for all NTFS attributes.
type AttributeHeader struct {
	Type        uint32 // Attribute type
	Length      uint32 // Length of attribute (including header)
	NonResident uint8  // 0 = resident, 1 = non-resident
	NameLength  uint8  // Length of attribute name
	NameOffset  uint16 // Offset to attribute name
	Flags       uint16 // Attribute flags
	AttributeID uint16 // Unique attribute ID within MFT entry
}

// Attribute flags
const (
	AttrFlagCompressed = 0x0001 // Attribute is compressed
	AttrFlagEncrypted  = 0x4000 // Attribute is encrypted
	AttrFlagSparse     = 0x8000 // Attribute is sparse
)

// ResidentAttribute represents a resident attribute (data stored in MFT).
type ResidentAttribute struct {
	Header      AttributeHeader
	ValueLength uint32 // Length of attribute value
	ValueOffset uint16 // Offset to attribute value
	IndexedFlag uint8  // Indexed flag
	Reserved    uint8  // Reserved
	Name        string // Attribute name (UTF-16 converted to UTF-8)
	Value       []byte // Attribute value
}

// NonResidentAttribute represents a non-resident attribute (data outside MFT).
type NonResidentAttribute struct {
	Header          AttributeHeader
	StartingVCN     uint64    // Starting Virtual Cluster Number
	LastVCN         uint64    // Last Virtual Cluster Number
	DataRunOffset   uint16    // Offset to data runs
	CompressionUnit uint16    // Compression unit size (2^x)
	Reserved        uint32    // Reserved
	AllocatedSize   uint64    // Allocated size of stream
	RealSize        uint64    // Actual size of stream
	InitializedSize uint64    // Initialized stream size
	Name            string    // Attribute name
	DataRuns        []DataRun // Parsed data runs
}

// DataRun represents a contiguous run of clusters.
type DataRun struct {
	LengthClusters uint64 // Length in clusters
	StartCluster   int64  // Starting cluster (relative or absolute)
	IsSparse       bool   // True if sparse (no allocated clusters)
}

// Attribute wraps either resident or non-resident attributes.
type Attribute struct {
	Header      AttributeHeader
	Resident    *ResidentAttribute
	NonResident *NonResidentAttribute
}

// Attribute types
const (
	AttrTypeStandardInfo    = 0x10  // Standard information
	AttrTypeAttributeList   = 0x20  // Attribute list
	AttrTypeFileName        = 0x30  // File name
	AttrTypeObjectID        = 0x40  // Object ID
	AttrTypeSecurityDesc    = 0x50  // Security descriptor
	AttrTypeVolumeName      = 0x60  // Volume name
	AttrTypeVolumeInfo      = 0x70  // Volume information
	AttrTypeData            = 0x80  // Data
	AttrTypeIndexRoot       = 0x90  // Index root
	AttrTypeIndexAllocation = 0xA0  // Index allocation
	AttrTypeBitmap          = 0xB0  // Bitmap
	AttrTypeReparsePoint    = 0xC0  // Reparse point
	AttrTypeEAInfo          = 0xD0  // EA information
	AttrTypeEA              = 0xE0  // EA
	AttrTypePropertySet     = 0xF0  // Property set
	AttrTypeLoggedUtility   = 0x100 // Logged utility stream
)

// StandardInformation represents the $STANDARD_INFORMATION attribute.
type StandardInformation struct {
	CreateTime        time.Time // File creation time
	ModifyTime        time.Time // File modification time
	MFTChangeTime     time.Time // MFT entry change time
	AccessTime        time.Time // File access time
	FileAttributes    uint32    // DOS file attributes
	MaxVersions       uint32    // Maximum versions
	VersionNumber     uint32    // Version number
	ClassID           uint32    // Class ID
	OwnerID           uint32    // Owner ID (2K+)
	SecurityID        uint32    // Security ID (2K+)
	QuotaCharged      uint64    // Quota charged (2K+)
	UpdateSequenceNum uint64    // Update sequence number (2K+)
}

// File attribute flags
const (
	FileAttrReadOnly          = 0x0001
	FileAttrHidden            = 0x0002
	FileAttrSystem            = 0x0004
	FileAttrDirectory         = 0x0010
	FileAttrArchive           = 0x0020
	FileAttrDevice            = 0x0040
	FileAttrNormal            = 0x0080
	FileAttrTemporary         = 0x0100
	FileAttrSparseFile        = 0x0200
	FileAttrReparsePoint      = 0x0400
	FileAttrCompressed        = 0x0800
	FileAttrOffline           = 0x1000
	FileAttrNotContentIndexed = 0x2000
	FileAttrEncrypted         = 0x4000
)

// FileName represents the $FILE_NAME attribute.
type FileName struct {
	ParentDirectory uint64    // MFT entry of parent directory
	ParentSeqNum    uint16    // Sequence number of parent
	CreateTime      time.Time // File creation time
	ModifyTime      time.Time // File modification time
	MFTChangeTime   time.Time // MFT change time
	AccessTime      time.Time // File access time
	AllocatedSize   uint64    // Allocated size
	RealSize        uint64    // Real size
	FileAttributes  uint64    // File attributes
	ReparseValue    uint32    // Reparse point value
	NameLength      uint8     // Length of file name
	Namespace       uint8     // Namespace
	Name            string    // File name (UTF-16 converted to UTF-8)
}

// File name namespace types
const (
	NamespacePOSIX  = 0 // Case sensitive, any characters except NULL and \
	NamespaceWin32  = 1 // Case insensitive, restricted characters
	NamespaceDOS    = 2 // 8.3 format, uppercase
	NamespaceWinDOS = 3 // Both Win32 and DOS compatible
)

// IndexRoot represents the $INDEX_ROOT attribute.
type IndexRoot struct {
	AttributeType    uint32 // Attribute type being indexed
	CollationRule    uint32 // Collation sorting rule
	IndexBlockSize   uint32 // Size of index allocation entries
	ClustersPerIndex uint8  // Clusters per index record
	Reserved         [3]byte
	NodeHeader       IndexNodeHeader
	Entries          []IndexEntry
}

// IndexNodeHeader describes the index entry list.
type IndexNodeHeader struct {
	EntryListOffset uint32 // Offset to first entry
	EntryListEnd    uint32 // End of entry list
	EntryListAlloc  uint32 // Allocated size
	Flags           uint32 // Flags
}

// Index node flags
const (
	IndexNodeLeaf = 0x00 // Leaf node (no children)
	IndexNodeNode = 0x01 // Non-leaf node (has children)
)

// IndexEntry represents an entry in an index (e.g., directory entry).
type IndexEntry struct {
	FileReference uint64 // MFT entry reference
	SequenceNum   uint16 // Sequence number
	EntryLength   uint16 // Length of this entry
	StreamLength  uint16 // Length of stream data
	Flags         uint8  // Flags
	Deleted       bool   // True if entry was recovered from deleted/unallocated index space
	Reserved      [3]byte
	Stream        []byte // Stream data (e.g., FileName attribute)
	SubNodeVCN    uint64 // VCN of sub-node (if IndexFlagNode)

	// Parsed data
	FileName *FileName // Parsed file name (if available)
}

// Index entry flags
const (
	IndexFlagNode = 0x01 // Entry points to sub-node
	IndexFlagLast = 0x02 // Last entry in list
)

// IndexAllocation represents the $INDEX_ALLOCATION attribute.
// This contains the B-tree nodes for large directories.
type IndexAllocation struct {
	Magic           uint32 // "INDX" = 0x58444E49
	UpdateSeqOffset uint16
	UpdateSeqSize   uint16
	LogFileSeqNum   uint64
	VCN             uint64 // VCN of this index buffer
	NodeHeader      IndexNodeHeader
	UpdateSeqArray  []uint16
	Entries         []IndexEntry
}

// AttributeList represents the $ATTRIBUTE_LIST attribute.
// Used when a file has too many attributes to fit in one MFT entry.
type AttributeListEntry struct {
	AttributeType uint32 // Attribute type
	RecordLength  uint16 // Length of this entry
	NameLength    uint8  // Length of attribute name
	NameOffset    uint8  // Offset to name
	StartingVCN   uint64 // Starting VCN (or 0 for resident)
	BaseFileRef   uint64 // MFT entry containing attribute
	AttributeID   uint16 // Attribute ID
	Name          string // Attribute name
}

// VolumeInformation represents the $VOLUME_INFORMATION attribute.
type VolumeInformation struct {
	Reserved     uint64
	MajorVersion uint8
	MinorVersion uint8
	Flags        uint16
	Reserved2    uint32
}

// Volume info flags
const (
	VolumeFlagDirty          = 0x0001 // Volume is dirty
	VolumeFlagResizeLog      = 0x0002 // Resize log file
	VolumeFlagUpgradeMount   = 0x0004 // Upgrade on mount
	VolumeFlagMountedNT4     = 0x0008 // Mounted on NT4
	VolumeFlagDeleteUSN      = 0x0010 // Delete USN underway
	VolumeFlagRepairObjID    = 0x0020 // Repair object IDs
	VolumeFlagModifiedChkdsk = 0x8000 // Modified by chkdsk
)

// ObjectID represents the $OBJECT_ID attribute.
type ObjectID struct {
	ObjectID      [16]byte // GUID of the object
	BirthVolumeID [16]byte // Volume where created
	BirthObjectID [16]byte // Original object ID
	BirthDomainID [16]byte // Domain where created
}

// ReparsePoint represents the $REPARSE_POINT attribute.
type ReparsePoint struct {
	ReparseType uint32 // Type of reparse point
	DataLength  uint16 // Length of reparse data
	Reserved    uint16
	Data        []byte // Reparse point data
}

// Common reparse point types
const (
	ReparsePointSymlink    = 0xA000000C // Symbolic link
	ReparsePointMountPoint = 0xA0000003 // Mount point
	ReparsePointHSM        = 0xC0000004 // Hierarchical Storage Management
	ReparsePointSIS        = 0x80000007 // Single Instance Storage
)

// UpdateSequenceArray is used to detect torn writes and ensure data integrity.
type UpdateSequenceArray struct {
	SequenceNumber uint16   // Current sequence number
	Array          []uint16 // Array of original values
}
