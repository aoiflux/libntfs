package libntfs

import (
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

// File represents an open NTFS file or directory.
// Provides thread-safe access to file data and metadata.
type File struct {
	volume   *Volume
	entry    *MFTEntry
	entryNum uint64
	name     string
	isDir    bool
	size     uint64
	dataAttr *Attribute // Primary $DATA attribute
	readMu   sync.Mutex
	readPos  int64
}

// FileReadSupport summarizes whether and how this file's primary $DATA stream can be read.
type FileReadSupport struct {
	HasData       bool
	Resident      bool
	NonResident   bool
	Sparse        bool
	Compressed    bool
	Encrypted     bool
	Readable      bool
	BlockingError error
}

// DirEntry represents a directory entry with basic information.
type DirEntry struct {
	Name          string
	EntryNum      uint64
	SequenceNum   uint16
	IsDirectory   bool
	Deleted       bool
	Size          uint64
	AllocatedSize uint64
	CreateTime    time.Time
	ModifyTime    time.Time
	AccessTime    time.Time
	Attributes    uint32
}

type dirTypeResolver func(entryNum uint64) (bool, error)

func resolveDirEntryIsDirectory(idxEntry IndexEntry, resolveType dirTypeResolver) bool {
	fromFileName := (idxEntry.FileName.FileAttributes & uint64(FileAttrDirectory)) != 0
	if resolveType == nil {
		return fromFileName
	}

	isDir, err := resolveType(idxEntry.FileReference)
	if err != nil {
		return fromFileName
	}

	return isDir
}

// normalizeNTFSPath normalizes user input to an NTFS-internal absolute path.
func normalizeNTFSPath(filePath string) string {
	filePath = strings.TrimSpace(filePath)
	filePath = strings.ReplaceAll(filePath, "\\", "/")

	// Allow optional drive prefix like C:/Windows or C:Windows.
	if len(filePath) >= 2 && filePath[1] == ':' {
		filePath = filePath[2:]
	}

	filePath = path.Clean(filePath)
	if filePath == "" || filePath == "." {
		return "/"
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}

	return filePath
}

func addIndexDirEntry(entries []DirEntry, idxEntry IndexEntry,
	dosIndexByRef map[uint64]int,
	longSeenByRef map[uint64]bool,
	seenByName map[string]int,
	resolveType dirTypeResolver,
) []DirEntry {
	if idxEntry.FileName == nil {
		return entries
	}

	entry := DirEntry{
		Name:          idxEntry.FileName.Name,
		EntryNum:      idxEntry.FileReference,
		SequenceNum:   idxEntry.SequenceNum,
		IsDirectory:   resolveDirEntryIsDirectory(idxEntry, resolveType),
		Deleted:       idxEntry.Deleted,
		Size:          idxEntry.FileName.RealSize,
		AllocatedSize: idxEntry.FileName.AllocatedSize,
		CreateTime:    idxEntry.FileName.CreateTime,
		ModifyTime:    idxEntry.FileName.ModifyTime,
		AccessTime:    idxEntry.FileName.AccessTime,
		Attributes:    uint32(idxEntry.FileName.FileAttributes),
	}

	if entry.Name == "." || entry.Name == ".." {
		return entries
	}

	key := strings.ToLower(fmt.Sprintf("%d|%s", entry.EntryNum, entry.Name))
	if existingIdx, exists := seenByName[key]; exists {
		if entries[existingIdx].Deleted && !entry.Deleted {
			entries[existingIdx] = entry
		}
		return entries
	}

	namespace := idxEntry.FileName.Namespace
	if namespace == NamespaceDOS {
		if longSeenByRef[entry.EntryNum] {
			return entries
		}
		if _, exists := dosIndexByRef[entry.EntryNum]; exists {
			return entries
		}

		entries = append(entries, entry)
		dosIndexByRef[entry.EntryNum] = len(entries) - 1
		seenByName[key] = len(entries) - 1
		return entries
	}

	longSeenByRef[entry.EntryNum] = true
	if dosIdx, exists := dosIndexByRef[entry.EntryNum]; exists {
		entries[dosIdx] = entry
		delete(dosIndexByRef, entry.EntryNum)
		seenByName[key] = dosIdx
		return entries
	}

	entries = append(entries, entry)
	seenByName[key] = len(entries) - 1
	return entries
}

func mftParentLookupSequences(dirEntry *MFTEntry) []uint16 {
	seqs := []uint16{dirEntry.SequenceNum}
	if !dirEntry.IsInUse() && dirEntry.SequenceNum > 0 {
		seqs = append(seqs, dirEntry.SequenceNum-1)
	}
	return seqs
}

func (v *Volume) maxMFTEntries() uint64 {
	if v.mftRecordSize == 0 {
		return 0
	}

	var totalMFTBytes uint64
	for _, run := range v.mftDataRuns {
		totalMFTBytes += run.LengthClusters * uint64(v.bytesPerCluster)
	}

	return totalMFTBytes / uint64(v.mftRecordSize)
}

func (v *Volume) ensureMFTParentMap() {
	v.mftParentMapMu.RLock()
	if v.mftParentMapInit {
		v.mftParentMapMu.RUnlock()
		return
	}
	v.mftParentMapMu.RUnlock()

	v.mftParentMapMu.Lock()
	defer v.mftParentMapMu.Unlock()
	if v.mftParentMapInit {
		return
	}

	parentMap := make(map[uint64]map[uint16][]IndexEntry)
	maxEntries := v.maxMFTEntries()

	for entryNum := uint64(0); entryNum < maxEntries; entryNum++ {
		entry, err := v.GetMFTEntry(entryNum)
		if err != nil {
			continue
		}

		fnAttrs := entry.FindAllAttributes(AttrTypeFileName)
		if len(fnAttrs) == 0 {
			continue
		}

		for _, attr := range fnAttrs {
			if attr.Resident == nil {
				continue
			}

			fn, err := parseFileNameAttribute(attr.Resident.Value)
			if err != nil || fn.Name == "" {
				continue
			}

			byParentSeq, ok := parentMap[fn.ParentDirectory]
			if !ok {
				byParentSeq = make(map[uint16][]IndexEntry)
				parentMap[fn.ParentDirectory] = byParentSeq
			}

			idxEntry := IndexEntry{
				FileReference: entryNum,
				SequenceNum:   entry.SequenceNum,
				Deleted:       !entry.IsInUse(),
				FileName:      fn,
			}

			byParentSeq[fn.ParentSeqNum] = append(byParentSeq[fn.ParentSeqNum], idxEntry)
		}
	}

	v.mftParentMap = parentMap
	v.mftParentMapInit = true
}

func (v *Volume) getMFTParentCandidates(parentEntryNum uint64, parentEntry *MFTEntry) []IndexEntry {
	v.ensureMFTParentMap()

	v.mftParentMapMu.RLock()
	defer v.mftParentMapMu.RUnlock()

	byParentSeq, ok := v.mftParentMap[parentEntryNum]
	if !ok {
		return nil
	}

	seqs := mftParentLookupSequences(parentEntry)
	var out []IndexEntry
	for _, seq := range seqs {
		if candidates, ok := byParentSeq[seq]; ok {
			out = append(out, candidates...)
		}
	}

	return out
}

// Open opens a file or directory by its MFT entry number.
func (v *Volume) Open(entryNum uint64) (*File, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	entry, err := v.GetMFTEntry(entryNum)
	if err != nil {
		return nil, fmt.Errorf("failed to get MFT entry %d: %w", entryNum, err)
	}

	if !entry.IsInUse() {
		return nil, fmt.Errorf("MFT entry %d is not in use", entryNum)
	}

	// Get file name
	fn, err := entry.GetFileName()
	if err != nil {
		// Fall back to well-known name if available
		if name, ok := GetWellKnownMFTEntryName(entryNum); ok {
			fn = &FileName{Name: name}
		} else {
			return nil, fmt.Errorf("failed to get file name: %w", err)
		}
	}

	// Find primary $DATA attribute.
	// Prefer unnamed stream so ADS ordering does not affect default reads.
	dataAttr := entry.FindPrimaryDataAttribute()

	file := &File{
		volume:   v,
		entry:    entry,
		entryNum: entryNum,
		name:     fn.Name,
		isDir:    entry.IsDirectory(),
		dataAttr: dataAttr,
	}

	// Get size from $DATA attribute or $FILE_NAME
	if dataAttr != nil {
		if dataAttr.Resident != nil {
			file.size = uint64(len(dataAttr.Resident.Value))
		} else if dataAttr.NonResident != nil {
			file.size = dataAttr.NonResident.RealSize
		}
	} else if fn != nil {
		file.size = fn.RealSize
	}

	return file, nil
}

// OpenPath opens a file or directory by its path (e.g., "/Windows/System32/ntdll.dll").
func (v *Volume) OpenPath(filePath string) (*File, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	// Normalize path
	filePath = normalizeNTFSPath(filePath)

	// Start from root
	current, err := v.Open(MFTEntryRoot)
	if err != nil {
		return nil, wrapPathError("open", filePath, "/", err)
	}

	// Handle root
	if filePath == "/" {
		return current, nil
	}

	// Split path and traverse
	parts := strings.Split(strings.Trim(filePath, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}

		pathSoFar := "/" + strings.Join(parts[:i+1], "/")

		if !current.IsDirectory() {
			return nil, wrapPathError("traverse", filePath, pathSoFar,
				fmt.Errorf("%w: %s is not a directory", ErrNotDirectory, current.name))
		}

		// Read directory entries
		entries, err := current.ReadDir()
		if err != nil {
			return nil, wrapPathError("readdir", filePath, pathSoFar, err)
		}

		// Find matching entry
		found := false
		for _, entry := range entries {
			if entry.Deleted {
				continue
			}
			if strings.EqualFold(entry.Name, part) {
				current, err = v.Open(entry.EntryNum)
				if err != nil {
					return nil, wrapPathError("open", filePath, pathSoFar, err)
				}
				found = true
				break
			}
		}

		if !found {
			return nil, wrapPathError("lookup", filePath, pathSoFar,
				fmt.Errorf("%w: %s", ErrFileNotFound, pathSoFar))
		}
	}

	return current, nil
}

// Name returns the file or directory name.
func (f *File) Name() string {
	return f.name
}

// EntryNumber returns the MFT entry number.
func (f *File) EntryNumber() uint64 {
	return f.entryNum
}

// IsDirectory returns true if this is a directory.
func (f *File) IsDirectory() bool {
	return f.isDir
}

// Size returns the file size in bytes.
func (f *File) Size() int64 {
	return int64(f.size)
}

// HasData returns true when the file has a primary $DATA attribute.
func (f *File) HasData() bool {
	return f.dataAttr != nil
}

// IsCompressed returns true when the primary $DATA attribute is compressed.
func (f *File) IsCompressed() bool {
	return f.dataAttr != nil && f.dataAttr.IsCompressed()
}

// IsEncrypted returns true when the primary $DATA attribute is encrypted.
func (f *File) IsEncrypted() bool {
	return f.dataAttr != nil && f.dataAttr.IsEncrypted()
}

// IsSparse returns true when the primary $DATA attribute is sparse.
func (f *File) IsSparse() bool {
	return f.dataAttr != nil && f.dataAttr.IsSparse()
}

// ReadSupport reports whether the primary $DATA stream is readable by libntfs.
func (f *File) ReadSupport() FileReadSupport {
	support := FileReadSupport{
		HasData:  f.dataAttr != nil,
		Readable: !f.isDir,
	}

	if f.isDir {
		support.BlockingError = ErrNotFile
		return support
	}

	if f.dataAttr == nil {
		return support
	}

	return f.dataAttr.ReadSupport()
}

// Read reads up to len(p) bytes from the file.
func (f *File) Read(p []byte) (int, error) {
	f.readMu.Lock()
	defer f.readMu.Unlock()

	n, err := f.ReadAt(p, f.readPos)
	f.readPos += int64(n)
	return n, err
}

// ReadAt reads len(p) bytes from the file starting at offset.
func (f *File) ReadAt(p []byte, offset int64) (int, error) {
	if f.volume.IsClosed() {
		return 0, ErrVolumeClosed
	}

	if f.isDir {
		return 0, ErrNotFile
	}

	if offset < 0 {
		return 0, ErrInvalidOffset
	}

	if offset >= int64(f.size) {
		return 0, io.EOF
	}

	if f.dataAttr == nil {
		// No data attribute means empty file
		return 0, io.EOF
	}

	// Handle resident data
	if f.dataAttr.Resident != nil {
		data := f.dataAttr.Resident.Value
		if offset >= int64(len(data)) {
			return 0, io.EOF
		}

		n := copy(p, data[offset:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	// Handle non-resident data
	if f.dataAttr.NonResident != nil {
		// Check for unsupported features
		if f.dataAttr.IsEncrypted() {
			return 0, ErrEncryptedData
		}

		bytesToRead := len(p)
		if offset+int64(bytesToRead) > int64(f.size) {
			bytesToRead = int(int64(f.size) - offset)
		}

		if bytesToRead <= 0 {
			return 0, io.EOF
		}

		if f.dataAttr.IsCompressed() {
			n, err := f.volume.readCompressedData(f.dataAttr.NonResident, offset, p[:bytesToRead])
			if err != nil {
				return n, err
			}
			if n < len(p) {
				return n, io.EOF
			}
			return n, nil
		}

		n, err := f.volume.ReadDataRuns(f.dataAttr.NonResident.DataRuns, offset, p[:bytesToRead])
		if err != nil {
			return n, err
		}

		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	return 0, fmt.Errorf("no data in $DATA attribute")
}

// ReadAll reads the entire file contents.
func (f *File) ReadAll() ([]byte, error) {
	if f.isDir {
		return nil, ErrNotFile
	}

	if f.size == 0 {
		return []byte{}, nil
	}

	if f.size > 1<<30 {
		// Limit to 1 GB for safety
		return nil, fmt.Errorf("file too large: %d bytes", f.size)
	}

	buf := make([]byte, f.size)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}

	return buf[:n], nil
}

// ReadDir reads the directory entries.
// Only valid for directories.
func (f *File) ReadDir() ([]DirEntry, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if !f.isDir {
		return nil, ErrNotDirectory
	}

	// Find $INDEX_ROOT attribute
	indexRootAttr := f.entry.FindAttribute(AttrTypeIndexRoot, "$I30")
	if indexRootAttr == nil || indexRootAttr.Resident == nil {
		return nil, fmt.Errorf("directory missing $INDEX_ROOT attribute")
	}

	// Parse $INDEX_ROOT
	indexRoot, err := parseIndexRoot(indexRootAttr.Resident.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to parse $INDEX_ROOT: %w", err)
	}

	var entries []DirEntry
	dosIndexByRef := make(map[uint64]int)
	longSeenByRef := make(map[uint64]bool)
	seenByName := make(map[string]int)
	resolvedTypeByRef := make(map[uint64]bool)

	resolveType := func(entryNum uint64) (bool, error) {
		if isDir, ok := resolvedTypeByRef[entryNum]; ok {
			return isDir, nil
		}

		entry, err := f.volume.GetMFTEntry(entryNum)
		if err != nil {
			return false, err
		}

		isDir := entry.IsDirectory()
		resolvedTypeByRef[entryNum] = isDir
		return isDir, nil
	}

	entryListOffset := int(indexRoot.NodeHeader.EntryListOffset)
	entryListUsedEnd := int(indexRoot.NodeHeader.EntryListEnd)
	entryListAllocEnd := int(indexRoot.NodeHeader.EntryListAlloc)
	entriesStart := 16 + entryListOffset
	entriesUsedEnd := 16 + entryListUsedEnd
	entriesAllocEnd := 16 + entryListAllocEnd

	if entriesStart < 16 || entriesUsedEnd < entriesStart || entriesAllocEnd < entriesUsedEnd || entriesAllocEnd > len(indexRootAttr.Resident.Value) {
		return nil, fmt.Errorf("invalid $INDEX_ROOT entry bounds (start=%d usedEnd=%d allocEnd=%d size=%d)",
			entriesStart, entriesUsedEnd, entriesAllocEnd, len(indexRootAttr.Resident.Value))
	}

	rootEntriesData := indexRootAttr.Resident.Value[entriesStart:entriesAllocEnd]
	rootUsedLen := entriesUsedEnd - entriesStart
	rootEntries, err := parseIndexEntriesRecoverDeleted(rootEntriesData, rootUsedLen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse $INDEX_ROOT entries: %w", err)
	}

	// Process index root entries (including recoverable deleted entries in slack)
	for _, idxEntry := range rootEntries {
		if idxEntry.Flags&IndexFlagLast != 0 && idxEntry.FileName == nil {
			// Last entry marker, skip
			continue
		}

		entries = addIndexDirEntry(entries, idxEntry, dosIndexByRef, longSeenByRef, seenByName, resolveType)
	}

	// Check if there's an $INDEX_ALLOCATION attribute (for large directories)
	indexAllocAttr := f.entry.FindAttribute(AttrTypeIndexAllocation, "$I30")
	if indexAllocAttr != nil && indexAllocAttr.NonResident != nil {
		// Read index allocation entries
		allocEntries, err := f.readIndexAllocation(indexAllocAttr.NonResident, dosIndexByRef, longSeenByRef, seenByName, resolveType)
		if err != nil {
			// Log error but continue with what we have
			// Some corrupted entries shouldn't prevent reading the rest
		} else {
			entries = append(entries, allocEntries...)
		}
	}

	// TSK-style fallback: include children inferred from MFT $FILE_NAME parent links.
	for _, candidate := range f.volume.getMFTParentCandidates(f.entryNum, f.entry) {
		entries = addIndexDirEntry(entries, candidate, dosIndexByRef, longSeenByRef, seenByName, nil)
	}

	return entries, nil
}

// readIndexAllocation reads entries from the $INDEX_ALLOCATION attribute.
func (f *File) readIndexAllocation(
	attr *NonResidentAttribute,
	dosIndexByRef map[uint64]int,
	longSeenByRef map[uint64]bool,
	seenByName map[string]int,
	resolveType dirTypeResolver,
) ([]DirEntry, error) {
	var entries []DirEntry

	// Read all index allocation data
	size := attr.RealSize
	if size > 1<<30 {
		// Limit to 1 GB for safety
		return nil, fmt.Errorf("index allocation too large: %d bytes", size)
	}

	data := make([]byte, size)
	_, err := f.volume.ReadDataRuns(attr.DataRuns, 0, data)
	if err != nil {
		return nil, fmt.Errorf("failed to read index allocation: %w", err)
	}

	// Parse index records
	indexRecordSize := int(f.volume.IndexRecordSize())
	if indexRecordSize <= 0 {
		return nil, fmt.Errorf("invalid index record size: %d", indexRecordSize)
	}
	for offset := 0; offset < len(data); offset += indexRecordSize {
		if offset+indexRecordSize > len(data) {
			break
		}

		record := data[offset : offset+indexRecordSize]

		// Check magic
		magic := ReadUint32LE(record, 0)
		if magic != IndexAllocationMagic {
			continue // Skip invalid records
		}

		// Apply update sequence
		updateSeqOffset := ReadUint16LE(record, 4)
		updateSeqSize := ReadUint16LE(record, 6)
		if err := f.volume.applyUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
			continue // Skip corrupted records
		}

		// Parse node header
		nodeHeaderOffset := 24 // After INDX header
		if nodeHeaderOffset+16 > len(record) {
			continue
		}

		entryListOffset := ReadUint32LE(record, nodeHeaderOffset)
		entryListEnd := ReadUint32LE(record, nodeHeaderOffset+4)
		entryListAlloc := ReadUint32LE(record, nodeHeaderOffset+8)
		entriesStart := nodeHeaderOffset + int(entryListOffset)
		entriesUsedEnd := nodeHeaderOffset + int(entryListEnd)
		entriesAllocEnd := nodeHeaderOffset + int(entryListAlloc)

		if entriesStart < nodeHeaderOffset || entriesUsedEnd < entriesStart || entriesAllocEnd < entriesUsedEnd || entriesAllocEnd > len(record) {
			continue
		}

		// Parse entries, including slack-space records that can indicate deleted names.
		idxEntries, err := parseIndexEntriesRecoverDeleted(record[entriesStart:entriesAllocEnd], entriesUsedEnd-entriesStart)
		if err != nil {
			continue
		}

		for _, idxEntry := range idxEntries {
			if idxEntry.Flags&IndexFlagLast != 0 && idxEntry.FileName == nil {
				continue
			}

			entries = addIndexDirEntry(entries, idxEntry, dosIndexByRef, longSeenByRef, seenByName, resolveType)
		}
	}

	// $FILE_NAME.RealSize in the directory index is not always updated by Windows
	// for system metadata files ($LogFile, $AttrDef, $Boot, $UpCase, etc.).
	// Resolve the actual DATA attribute size for any non-directory entry that
	// reports 0 bytes so callers see the real on-disk size.
	for i := range entries {
		if entries[i].IsDirectory || entries[i].Size != 0 {
			continue
		}
		mftEntry, err := f.volume.GetMFTEntry(entries[i].EntryNum)
		if err != nil {
			continue
		}
		if dataAttr := mftEntry.FindPrimaryDataAttribute(); dataAttr != nil {
			if dataAttr.Resident != nil {
				entries[i].Size = uint64(len(dataAttr.Resident.Value))
			} else if dataAttr.NonResident != nil {
				entries[i].Size = dataAttr.NonResident.RealSize
			}
		}
	}

	return entries, nil
}

// GetMetadata returns the standard information for this file.
func (f *File) GetMetadata() (*StandardInformation, error) {
	return f.entry.GetStandardInformation()
}

// GetFileName returns the file name attribute.
func (f *File) GetFileName() (*FileName, error) {
	return f.entry.GetFileName()
}

// ListFiles is a convenience method to list all files in a directory.
func (f *File) ListFiles() ([]DirEntry, error) {
	entries, err := f.ReadDir()
	if err != nil {
		return nil, err
	}

	var files []DirEntry
	for _, entry := range entries {
		if !entry.IsDirectory {
			files = append(files, entry)
		}
	}

	return files, nil
}

// ListDirectories is a convenience method to list all subdirectories.
func (f *File) ListDirectories() ([]DirEntry, error) {
	entries, err := f.ReadDir()
	if err != nil {
		return nil, err
	}

	var dirs []DirEntry
	for _, entry := range entries {
		if entry.IsDirectory {
			dirs = append(dirs, entry)
		}
	}

	return dirs, nil
}

// String returns a string representation of the file.
func (f *File) String() string {
	if f.isDir {
		return fmt.Sprintf("Directory: %s (MFT entry %d)", f.name, f.entryNum)
	}
	return fmt.Sprintf("File: %s (MFT entry %d, %d bytes)", f.name, f.entryNum, f.size)
}

// String returns a string representation of a directory entry.
func (d *DirEntry) String() string {
	typeStr := "File"
	if d.IsDirectory {
		typeStr = "Dir "
	}
	return fmt.Sprintf("[%s] %-40s %10d bytes  Modified: %s",
		typeStr, d.Name, d.Size, d.ModifyTime.Format("2006-01-02 15:04:05"))
}
