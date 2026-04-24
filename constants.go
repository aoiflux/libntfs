package libntfs

const (
	// Boot sector constants
	BootSectorSize        = 512
	BootSectorMagic       = 0xAA55
	BootSectorOEMNameNTFS = "NTFS    "

	// Update sequence constants
	UpdateSequenceStride = 512 // Bytes per update sequence entry

	// MFT constants
	MFTEntryHeaderSize = 48   // Size of MFT entry header (without attributes)
	MaxMFTEntrySize    = 4096 // Maximum MFT entry size

	// Attribute constants
	AttributeHeaderSize            = 16 // Common header size
	ResidentAttributeHeaderSize    = 24 // Total header size for resident
	NonResidentAttributeHeaderSize = 64 // Total header size for non-resident

	// Index constants
	IndexRootHeaderSize  = 16
	IndexNodeHeaderSize  = 16
	IndexEntryHeaderSize = 16
	IndexAllocationMagic = 0x58444E49 // "INDX"

	// Time conversion constants
	// Number of 100-nanosecond intervals between 1601-01-01 and 1970-01-01
	NTFSTimeOffset    = 116444736000000000
	NTFSTimePrecision = 100 // nanoseconds per NTFS time unit

	// Cluster and sector limits
	MaxClusterSize = 65536 // Maximum cluster size (64 KB)
	MaxSectorSize  = 4096  // Maximum sector size (4 KB)
	MinSectorSize  = 512   // Minimum sector size

	// Data run constants
	MaxDataRunHeaderSize = 9 // Maximum size of data run header (1 + 8)

	// Cache sizes (for internal use)
	DefaultMFTCacheSize    = 1000  // Number of MFT entries to cache
	DefaultBufferCacheSize = 50    // Number of buffers to cache
	DefaultBufferSize      = 65536 // Default buffer size (64 KB)

	// File name lengths
	MaxFileNameLength     = 255     // Maximum file name length
	MaxFileNameLengthUTF8 = 255 * 4 // Maximum UTF-8 encoded length
	MaxPathLength         = 32767   // Maximum path length

	// Compression constants
	CompressionUnitSize      = 16    // Clusters per compression unit
	MaxCompressionBufferSize = 65536 // Maximum compression buffer size
)

// AttributeTypeNames maps attribute type codes to human-readable names.
var AttributeTypeNames = map[uint32]string{
	AttrTypeStandardInfo:    "$STANDARD_INFORMATION",
	AttrTypeAttributeList:   "$ATTRIBUTE_LIST",
	AttrTypeFileName:        "$FILE_NAME",
	AttrTypeObjectID:        "$OBJECT_ID",
	AttrTypeSecurityDesc:    "$SECURITY_DESCRIPTOR",
	AttrTypeVolumeName:      "$VOLUME_NAME",
	AttrTypeVolumeInfo:      "$VOLUME_INFORMATION",
	AttrTypeData:            "$DATA",
	AttrTypeIndexRoot:       "$INDEX_ROOT",
	AttrTypeIndexAllocation: "$INDEX_ALLOCATION",
	AttrTypeBitmap:          "$BITMAP",
	AttrTypeReparsePoint:    "$REPARSE_POINT",
	AttrTypeEAInfo:          "$EA_INFORMATION",
	AttrTypeEA:              "$EA",
	AttrTypePropertySet:     "$PROPERTY_SET",
	AttrTypeLoggedUtility:   "$LOGGED_UTILITY_STREAM",
}

// GetAttributeTypeName returns the human-readable name for an attribute type.
func GetAttributeTypeName(attrType uint32) string {
	if name, ok := AttributeTypeNames[attrType]; ok {
		return name
	}
	return "UNKNOWN"
}

// FileAttributeNames maps file attribute flags to names.
var FileAttributeNames = map[uint32]string{
	FileAttrReadOnly:          "ReadOnly",
	FileAttrHidden:            "Hidden",
	FileAttrSystem:            "System",
	FileAttrArchive:           "Archive",
	FileAttrDevice:            "Device",
	FileAttrNormal:            "Normal",
	FileAttrTemporary:         "Temporary",
	FileAttrSparseFile:        "Sparse",
	FileAttrReparsePoint:      "ReparsePoint",
	FileAttrCompressed:        "Compressed",
	FileAttrOffline:           "Offline",
	FileAttrNotContentIndexed: "NotIndexed",
	FileAttrEncrypted:         "Encrypted",
}

// NamespaceNames maps namespace values to names.
var NamespaceNames = map[uint8]string{
	NamespacePOSIX:  "POSIX",
	NamespaceWin32:  "Win32",
	NamespaceDOS:    "DOS",
	NamespaceWinDOS: "Win32+DOS",
}

// GetNamespaceName returns the human-readable name for a namespace.
func GetNamespaceName(ns uint8) string {
	if name, ok := NamespaceNames[ns]; ok {
		return name
	}
	return "UNKNOWN"
}

// WellKnownMFTEntries maps MFT entry numbers to their names.
var WellKnownMFTEntries = map[uint64]string{
	MFTEntryMFT:     "$MFT",
	MFTEntryMFTMirr: "$MFTMirr",
	MFTEntryLogFile: "$LogFile",
	MFTEntryVolume:  "$Volume",
	MFTEntryAttrDef: "$AttrDef",
	MFTEntryRoot:    ".",
	MFTEntryBitmap:  "$Bitmap",
	MFTEntryBoot:    "$Boot",
	MFTEntryBadClus: "$BadClus",
	MFTEntrySecure:  "$Secure",
	MFTEntryUpCase:  "$UpCase",
	MFTEntryExtend:  "$Extend",
}

// GetWellKnownMFTEntryName returns the name for a well-known MFT entry.
func GetWellKnownMFTEntryName(entry uint64) (string, bool) {
	name, ok := WellKnownMFTEntries[entry]
	return name, ok
}

// Collation rules for index sorting
const (
	CollationBinary       = 0x00 // Binary comparison
	CollationFileName     = 0x01 // File name comparison (case-insensitive)
	CollationUnicode      = 0x02 // Unicode string comparison
	CollationULong        = 0x10 // Unsigned long comparison
	CollationSID          = 0x11 // SID comparison
	CollationSecurityHash = 0x12 // Security hash comparison
	CollationULongs       = 0x13 // Multiple unsigned longs
)

// CollationRuleNames maps collation rules to names.
var CollationRuleNames = map[uint32]string{
	CollationBinary:       "Binary",
	CollationFileName:     "FileName",
	CollationUnicode:      "Unicode",
	CollationULong:        "ULong",
	CollationSID:          "SID",
	CollationSecurityHash: "SecurityHash",
	CollationULongs:       "ULongs",
}

// GetCollationRuleName returns the name of a collation rule.
func GetCollationRuleName(rule uint32) string {
	if name, ok := CollationRuleNames[rule]; ok {
		return name
	}
	return "UNKNOWN"
}
