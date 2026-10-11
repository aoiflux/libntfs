package libntfs

import (
	"errors"
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
//
// BlockingError carries why an unreadable stream is unreadable. It is excluded
// from the JSON form, where an error marshals to nothing useful; the boolean
// flags alongside it say which condition applies.
type FileReadSupport struct {
	HasData       bool  `json:"has_data"`
	Resident      bool  `json:"resident"`
	NonResident   bool  `json:"non_resident"`
	Sparse        bool  `json:"sparse"`
	Compressed    bool  `json:"compressed"`
	Encrypted     bool  `json:"encrypted"`
	Readable      bool  `json:"readable"`
	BlockingError error `json:"-"`
}

// DirEntry represents a directory entry with basic information.
//
// The times, sizes and Attributes come from the copy of $FILE_NAME the
// directory index holds, not from the file's own record. NTFS updates that copy
// only when the name itself changes, so its values can lag the file's
// $STANDARD_INFORMATION and $DATA; use [File.GetMetadata] for current times.
// The one exception is a Size of 0 on a file, which ReadDir replaces with the
// size of its $DATA, since Windows leaves it 0 for several system files.
// IsDirectory comes from the index entry's attributes as well.
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
	MFTChangeTime time.Time
	AccessTime    time.Time
	Attributes    uint32

	// Namespace is the name's namespace: NamespacePOSIX, NamespaceWin32,
	// NamespaceDOS or NamespaceWinDOS.
	Namespace uint8

	// ParentReference is the parent directory's packed file reference, as
	// [MakeFileReference] builds it, recorded in the entry's $FILE_NAME.
	ParentReference uint64

	// FileName is the index entry's whole $FILE_NAME.
	FileName *FileName
}

type dirTypeResolver func(entryNum uint64) (bool, error)

func resolveDirEntryIsDirectory(idxEntry IndexEntry, resolveType dirTypeResolver) bool {
	fromFileName := isDirectoryFileAttributes(idxEntry.FileName.FileAttributes)
	if resolveType == nil {
		return fromFileName
	}

	isDir, err := resolveType(idxEntry.FileReference)
	if err != nil {
		return fromFileName
	}

	return isDir
}

// isDirectoryFileAttributes reports whether a FileAttributes field marks a
// directory.
//
// NTFS sets FileAttrDupFileNameIndexPresent for a directory and leaves the Win32
// FileAttrDirectory bit clear, so the former is what an index entry actually
// carries. The Win32 bit is accepted as well, since tools that synthesise NTFS
// structures from a Win32 view do set it, and accepting both costs nothing.
func isDirectoryFileAttributes(attributes uint32) bool {
	const directoryBits = uint32(FileAttrDupFileNameIndexPresent) | uint32(FileAttrDirectory)
	return attributes&directoryBits != 0
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

// dirEntryAccumulator collects the entries of a single directory from every
// source that contributes to it: $INDEX_ROOT, $INDEX_ALLOCATION, and the
// optional MFT parent-link fallback.
//
// The dedup maps hold positions into entries, so all three sources have to
// append to one slice. Collecting a source into a slice of its own and
// concatenating afterwards rebases those positions: a recorded index then
// addresses an unrelated entry, or none at all. Keeping the slice and the maps
// behind one type is what stops that from being expressible.
type dirEntryAccumulator struct {
	entries []DirEntry

	// dosIndexByRef maps a file reference to the position of its 8.3 name,
	// so a long name found later can replace it in place.
	dosIndexByRef map[uint64]int
	// longSeenByRef records file references whose long name is already
	// present, so a later 8.3 name for the same file is dropped.
	longSeenByRef map[uint64]bool
	// seenByName maps reference|name to a position, collapsing the duplicate
	// names that slack-space recovery produces.
	seenByName map[string]int

	resolveType dirTypeResolver
}

func newDirEntryAccumulator(resolveType dirTypeResolver) *dirEntryAccumulator {
	return &dirEntryAccumulator{
		dosIndexByRef: make(map[uint64]int),
		longSeenByRef: make(map[uint64]bool),
		seenByName:    make(map[string]int),
		resolveType:   resolveType,
	}
}

// at reports whether pos addresses a live entry. A recorded position can only
// be stale if a caller collected entries somewhere other than this
// accumulator, which the type is arranged to prevent; the check is here so
// that a future regression degrades into a duplicate entry rather than a
// panic in the middle of a volume walk.
func (a *dirEntryAccumulator) at(pos int) bool {
	return pos >= 0 && pos < len(a.entries)
}

// add merges one index entry into the accumulated listing.
func (a *dirEntryAccumulator) add(idxEntry IndexEntry) {
	if idxEntry.FileName == nil {
		return
	}

	// NTFS has no nameless directory entry. A $FILE_NAME whose name length is
	// zero parses without error and yields one anyway, which reaches a caller
	// as an entry it can neither open, path, nor report. Real volumes do carry
	// them, in index slack and in records damaged mid-write.
	if idxEntry.FileName.Name == "" {
		return
	}

	entry := DirEntry{
		Name:          idxEntry.FileName.Name,
		EntryNum:      idxEntry.FileReference,
		SequenceNum:   idxEntry.SequenceNum,
		IsDirectory:   resolveDirEntryIsDirectory(idxEntry, a.resolveType),
		Deleted:       idxEntry.Deleted,
		Size:          idxEntry.FileName.RealSize,
		AllocatedSize: idxEntry.FileName.AllocatedSize,
		CreateTime:    idxEntry.FileName.CreateTime,
		ModifyTime:    idxEntry.FileName.ModifyTime,
		MFTChangeTime: idxEntry.FileName.MFTChangeTime,
		AccessTime:    idxEntry.FileName.AccessTime,
		Attributes:    idxEntry.FileName.FileAttributes,

		Namespace:       idxEntry.FileName.Namespace,
		ParentReference: MakeFileReference(idxEntry.FileName.ParentDirectory, idxEntry.FileName.ParentSeqNum),
		FileName:        idxEntry.FileName,
	}

	if entry.Name == "." || entry.Name == ".." {
		return
	}

	key := strings.ToLower(fmt.Sprintf("%d|%s", entry.EntryNum, entry.Name))
	if existingIdx, exists := a.seenByName[key]; exists {
		if a.at(existingIdx) {
			if a.entries[existingIdx].Deleted && !entry.Deleted {
				a.entries[existingIdx] = entry
			}
			return
		}
		delete(a.seenByName, key)
	}

	namespace := idxEntry.FileName.Namespace
	if namespace == NamespaceDOS {
		if a.longSeenByRef[entry.EntryNum] {
			return
		}
		if _, exists := a.dosIndexByRef[entry.EntryNum]; exists {
			return
		}

		a.entries = append(a.entries, entry)
		a.dosIndexByRef[entry.EntryNum] = len(a.entries) - 1
		a.seenByName[key] = len(a.entries) - 1
		return
	}

	a.longSeenByRef[entry.EntryNum] = true
	if dosIdx, exists := a.dosIndexByRef[entry.EntryNum]; exists {
		delete(a.dosIndexByRef, entry.EntryNum)
		if a.at(dosIdx) {
			// Replace the 8.3 name already recorded for this file.
			a.entries[dosIdx] = entry
			a.seenByName[key] = dosIdx
			return
		}
	}

	a.entries = append(a.entries, entry)
	a.seenByName[key] = len(a.entries) - 1
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

			fn, err := ParseFileName(attr.Resident.Value)
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

	if v.strict {
		if err := v.verifyDataCoverage(entryNum, entry); err != nil {
			return nil, err
		}
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
//
// Each component is matched against the directory's live names with
// strings.EqualFold, which folds case by Unicode rules rather than through the
// volume's $UpCase table. The two agree for every name a mainstream formatter
// produces; use [Volume.UpCase] and [UpCase.Compare] over [File.ReadDir] for
// NTFS's own rule.
//
// A directory that is only partly readable is searched as far as it goes; the
// damage is reported only if the component is not found. Under Options.Strict
// that damage is fatal instead.
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

		// Read directory entries. A partially readable directory may still
		// hold the component being looked up, so the error is deferred until
		// the search over what was recovered actually fails.
		entries, err := current.ReadDir()
		if err != nil && !isPartialDirError(err) {
			return nil, wrapPathError("readdir", filePath, pathSoFar, err)
		}
		readErr := err

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
			// The component is genuinely absent from what could be read, so
			// a damaged index is now the better explanation to report.
			if readErr != nil {
				return nil, wrapPathError("readdir", filePath, pathSoFar, readErr)
			}
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
//
// It identifies the record, not the file: NTFS reuses records, so an entry
// number recorded at one point in time may name a different file later. Pair it
// with [File.SequenceNumber], or use [File.FileReference], whenever the identity
// has to survive being written down.
func (f *File) EntryNumber() uint64 {
	return f.entryNum
}

// SequenceNumber returns the MFT record's sequence number, which NTFS increments
// each time the record is reused.
//
// It is what distinguishes this file from the next one to occupy the same
// record. A report that carries an entry number without it cannot tell a
// modified file from a deleted file whose record was handed to something else.
func (f *File) SequenceNumber() uint16 {
	if f.entry == nil {
		return 0
	}
	return f.entry.SequenceNum
}

// FileReference returns the file's 64-bit NTFS file reference: the MFT record
// number in the low 48 bits and the sequence number in the high 16, exactly as
// NTFS itself stores it in $FILE_NAME parent references, index entries, and USN
// records.
//
// It is the value to compare against [USNRecord.FileReference] and
// [USNRecord.FileSequence], and the one to record when a file's identity has to
// be meaningful across two acquisitions of the same volume. Unpack it with
// [FileReferenceEntry] and [FileReferenceSequence].
func (f *File) FileReference() uint64 {
	return MakeFileReference(f.entryNum, f.SequenceNumber())
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
//
// On a damaged volume ReadDir returns the entries it could parse together with
// a *PartialDirError, so both return values can be non-nil. A caller walking a
// whole volume can then record the loss and descend into what survived instead
// of losing the subtree; a caller that wants nothing but complete listings can
// keep testing err != nil. A nil slice with a non-nil error means nothing at
// all could be read, and the error wraps ErrCorruptIndex or ErrNotDirectory.
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
		return nil, fmt.Errorf("%w: directory missing $INDEX_ROOT attribute", ErrCorruptIndex)
	}

	// Parse $INDEX_ROOT
	indexRoot, err := parseIndexRoot(indexRootAttr.Resident.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse $INDEX_ROOT: %w", ErrCorruptIndex, err)
	}

	acc := newDirEntryAccumulator(nil)

	entryListOffset := int(indexRoot.NodeHeader.EntryListOffset)
	entryListUsedEnd := int(indexRoot.NodeHeader.EntryListEnd)
	entryListAllocEnd := int(indexRoot.NodeHeader.EntryListAlloc)
	entriesStart := 16 + entryListOffset
	entriesUsedEnd := 16 + entryListUsedEnd
	entriesAllocEnd := 16 + entryListAllocEnd

	if entriesStart < 16 || entriesUsedEnd < entriesStart || entriesAllocEnd < entriesUsedEnd || entriesAllocEnd > len(indexRootAttr.Resident.Value) {
		return nil, fmt.Errorf("%w: invalid $INDEX_ROOT entry bounds (start=%d usedEnd=%d allocEnd=%d size=%d)",
			ErrCorruptIndex, entriesStart, entriesUsedEnd, entriesAllocEnd, len(indexRootAttr.Resident.Value))
	}

	rootEntriesData := indexRootAttr.Resident.Value[entriesStart:entriesAllocEnd]
	rootUsedLen := entriesUsedEnd - entriesStart
	rootEntries, err := parseIndexEntriesRecoverDeleted(rootEntriesData, rootUsedLen)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse $INDEX_ROOT entries: %w", ErrCorruptIndex, err)
	}

	// Process index root entries (including recoverable deleted entries in slack)
	for _, idxEntry := range rootEntries {
		if idxEntry.Flags&IndexFlagLast != 0 && idxEntry.FileName == nil {
			// Last entry marker, skip
			continue
		}

		acc.add(idxEntry)
	}

	// Check if there's an $INDEX_ALLOCATION attribute (for large directories)
	var skipped int
	var indexErr error
	indexAllocAttr := f.entry.FindAttribute(AttrTypeIndexAllocation, "$I30")
	if indexAllocAttr != nil && indexAllocAttr.NonResident != nil {
		// A damaged $INDEX_ALLOCATION must not cost the caller the entries
		// $INDEX_ROOT already yielded, so the records that fail are counted
		// and reported rather than aborting the listing.
		skipped, indexErr = f.readIndexAllocation(indexAllocAttr.NonResident, f.volume.indexBlockSize(indexRoot), acc)
		f.resolveZeroSizedEntries(acc.entries)
	}

	// Optional TSK-style fallback: include children inferred from MFT
	// $FILE_NAME parent links. This can be expensive on large volumes.
	if f.volume.mftParentFallbackEnabled() {
		for _, candidate := range f.volume.getMFTParentCandidates(f.entryNum, f.entry) {
			acc.add(candidate)
		}
	}

	if indexErr != nil || skipped > 0 {
		if indexErr == nil {
			indexErr = ErrCorruptIndex
		}
		partial := &PartialDirError{
			EntryNum:  f.entryNum,
			Recovered: len(acc.entries),
			Skipped:   skipped,
			Err:       indexErr,
		}
		if f.volume.strict {
			return nil, &StrictError{Record: f.entryNum, Index: "$I30", VCN: -1, Check: "partial-listing", Err: partial}
		}
		return acc.entries, partial
	}

	if f.volume.strict {
		if err := f.verifyDirectory(); err != nil {
			return nil, err
		}
	}

	return acc.entries, nil
}

// readIndexAllocation merges the entries of the $INDEX_ALLOCATION attribute
// into acc.
//
// Block i of the allocation sits at i*indexRecordSize and is in use when bit i
// of $BITMAP:$I30 is set. The entries of a block whose bit is clear are merged
// as Deleted: the directory released the block, so its names are history. A
// directory with no $BITMAP:$I30 at all is damaged; every block is then read as
// in use, which is what ReadDir did before it consulted the bitmap.
//
// It returns the number of index records it had to skip. A skipped record is
// not fatal on its own -- a directory whose INDX blocks are partly damaged
// still yields the names in the blocks that survived -- but it is never
// silent, because a caller cannot otherwise tell a small directory from a
// truncated one. A free block that fails to parse is not counted: nothing live
// was lost with it.
func (f *File) readIndexAllocation(attr *NonResidentAttribute, indexRecordSize int, acc *dirEntryAccumulator) (int, error) {
	// Read all index allocation data
	size := attr.RealSize
	if size > 1<<30 {
		// Limit to 1 GB for safety
		return 0, fmt.Errorf("%w: index allocation too large: %d bytes", ErrCorruptIndex, size)
	}

	data := make([]byte, size)
	_, err := f.volume.ReadDataRuns(attr.DataRuns, 0, data)
	if err != nil {
		return 0, fmt.Errorf("failed to read index allocation: %w", err)
	}

	// Parse index records. The record size can come from the boot sector, so a
	// damaged one can make it too small to hold even the INDX header the
	// reads below assume.
	if indexRecordSize < IndexRecordHeaderSize {
		return 0, fmt.Errorf("%w: invalid index record size: %d", ErrCorruptIndex, indexRecordSize)
	}

	bitmap, err := f.loadIndexBitmap("$I30")
	if err != nil {
		return 0, err
	}

	var skipped int
	for offset, block := 0, 0; offset+indexRecordSize <= len(data); offset, block = offset+indexRecordSize, block+1 {
		inUse := bitmap == nil || bitmapBit(bitmap, block)

		idxEntries, err := parseIndexRecord(data[offset : offset+indexRecordSize])
		if err != nil {
			if inUse && !errors.Is(err, errNotIndexRecord) {
				// Unallocated blocks in the run are expected and carry no
				// magic; anything that claims to be an INDX record and then
				// fails to parse is a real loss and is counted.
				skipped++
			}
			continue
		}

		for _, idxEntry := range idxEntries {
			if idxEntry.Flags&IndexFlagLast != 0 && idxEntry.FileName == nil {
				continue
			}
			if !inUse {
				idxEntry.Deleted = true
			}

			acc.add(idxEntry)
		}
	}

	if skipped > 0 {
		return skipped, fmt.Errorf("%w: skipped %d unreadable index records", ErrCorruptIndex, skipped)
	}

	return 0, nil
}

// errNotIndexRecord marks a block that never claimed to be an INDX record.
// An $INDEX_ALLOCATION run routinely covers blocks the directory has not
// filled yet, so this is the one skip that is not a sign of damage.
var errNotIndexRecord = errors.New("not an index record")

// parseIndexRecord validates one INDX record and returns the index entries it
// holds, including entries recovered from slack space.
//
// The record is modified in place: applying the update sequence is what makes
// the last two bytes of every sector readable.
func parseIndexRecord(record []byte) ([]IndexEntry, error) {
	// Every read below is at a fixed offset in the INDX header or the node
	// header that follows it, so the record has to be long enough for both
	// before any of them happen. The record size comes from the boot sector,
	// which on a damaged volume can describe something far smaller.
	if len(record) < IndexRecordHeaderSize+IndexNodeHeaderSize {
		return nil, fmt.Errorf("%w: index record is %d bytes, need %d",
			ErrTruncatedRecord, len(record), IndexRecordHeaderSize+IndexNodeHeaderSize)
	}

	if ReadUint32LE(record, 0) != IndexAllocationMagic {
		return nil, errNotIndexRecord
	}

	updateSeqOffset := ReadUint16LE(record, 4)
	updateSeqSize := ReadUint16LE(record, 6)
	if err := applyUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptIndex, err)
	}

	// Node header offsets are relative to the node header itself.
	const nodeHeaderOffset = IndexRecordHeaderSize
	entryListOffset := ReadUint32LE(record, nodeHeaderOffset)
	entryListEnd := ReadUint32LE(record, nodeHeaderOffset+4)
	entryListAlloc := ReadUint32LE(record, nodeHeaderOffset+8)
	entriesStart := nodeHeaderOffset + int(entryListOffset)
	entriesUsedEnd := nodeHeaderOffset + int(entryListEnd)
	entriesAllocEnd := nodeHeaderOffset + int(entryListAlloc)

	if entriesStart < nodeHeaderOffset || entriesUsedEnd < entriesStart ||
		entriesAllocEnd < entriesUsedEnd || entriesAllocEnd > len(record) {
		return nil, fmt.Errorf("%w: invalid index node bounds (start=%d usedEnd=%d allocEnd=%d size=%d)",
			ErrCorruptIndex, entriesStart, entriesUsedEnd, entriesAllocEnd, len(record))
	}

	// Parse entries, including slack-space records that can indicate deleted names.
	entries, err := parseIndexEntriesRecoverDeleted(record[entriesStart:entriesAllocEnd], entriesUsedEnd-entriesStart)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptIndex, err)
	}

	return entries, nil
}

// resolveZeroSizedEntries fills in sizes the directory index does not carry.
//
// $FILE_NAME.RealSize is not always updated by Windows for system metadata
// files ($LogFile, $AttrDef, $Boot, $UpCase, etc.). Resolve the actual DATA
// attribute size for any non-directory entry that reports 0 bytes so callers
// see the real on-disk size.
func (f *File) resolveZeroSizedEntries(entries []DirEntry) {
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
}

// GetMetadata returns the standard information for this file.
func (f *File) GetMetadata() (*StandardInformation, error) {
	return f.entry.GetStandardInformation()
}

// GetFileName returns the file name attribute.
func (f *File) GetFileName() (*FileName, error) {
	return f.entry.GetFileName()
}

// isPartialDirError reports whether err carries entries worth keeping.
func isPartialDirError(err error) bool {
	var partial *PartialDirError
	return errors.As(err, &partial)
}

// ListFiles is a convenience method to list all files in a directory.
//
// A partially readable directory yields the files that could be recovered
// together with a *PartialDirError; see ReadDir.
func (f *File) ListFiles() ([]DirEntry, error) {
	entries, err := f.ReadDir()
	if err != nil && !isPartialDirError(err) {
		return nil, err
	}

	var files []DirEntry
	for _, entry := range entries {
		if !entry.IsDirectory {
			files = append(files, entry)
		}
	}

	return files, err
}

// ListDirectories is a convenience method to list all subdirectories.
//
// A partially readable directory yields the subdirectories that could be
// recovered together with a *PartialDirError; see ReadDir.
func (f *File) ListDirectories() ([]DirEntry, error) {
	entries, err := f.ReadDir()
	if err != nil && !isPartialDirError(err) {
		return nil, err
	}

	var dirs []DirEntry
	for _, entry := range entries {
		if entry.IsDirectory {
			dirs = append(dirs, entry)
		}
	}

	return dirs, err
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
