package libntfs

import (
	"fmt"
	"io"
	"sort"
)

// GetMFTEntry retrieves an MFT entry by its entry number.
// This method is thread-safe and uses caching for frequently accessed entries.
func (v *Volume) GetMFTEntry(entryNum uint64) (*MFTEntry, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	// Check cache first
	v.mftCacheMu.RLock()
	if entry, ok := v.mftCache[entryNum]; ok {
		v.mftCacheMu.RUnlock()
		return entry, nil
	}
	v.mftCacheMu.RUnlock()

	// Not in cache, read from disk
	entry, err := v.readMFTEntry(entryNum)
	if err != nil {
		return nil, err
	}

	// Best-effort merge of attributes referenced via $ATTRIBUTE_LIST.
	// Some system files (for example $LogFile) keep primary $DATA extents in extension records.
	_ = v.resolveAttributeList(entry, entryNum)

	// Add to cache
	v.mftCacheMu.Lock()
	if len(v.mftCache) < DefaultMFTCacheSize {
		v.mftCache[entryNum] = entry
	}
	v.mftCacheMu.Unlock()

	return entry, nil
}

func fileReferenceRecordNumber(ref uint64) uint64 {
	return ref & 0x0000FFFFFFFFFFFF
}

func parseAttributeListEntries(data []byte) ([]AttributeListEntry, error) {
	const minEntryLen = 26

	var entries []AttributeListEntry
	offset := 0

	for offset < len(data) {
		if len(data)-offset < minEntryLen {
			break
		}

		entry := AttributeListEntry{}
		entry.AttributeType = ReadUint32LE(data, offset)
		entry.RecordLength = ReadUint16LE(data, offset+4)
		entry.NameLength = data[offset+6]
		entry.NameOffset = data[offset+7]
		entry.StartingVCN = ReadUint64LE(data, offset+8)
		entry.BaseFileRef = fileReferenceRecordNumber(ReadUint64LE(data, offset+16))
		entry.AttributeID = ReadUint16LE(data, offset+24)

		recordLen := int(entry.RecordLength)
		if recordLen < minEntryLen {
			return nil, fmt.Errorf("%w: invalid attribute list record length %d", ErrInvalidAttribute, recordLen)
		}
		if offset+recordLen > len(data) {
			return nil, fmt.Errorf("%w: attribute list record exceeds buffer", ErrInvalidAttribute)
		}

		if entry.NameLength > 0 {
			nameBytes := int(entry.NameLength) * 2
			nameStart := offset + int(entry.NameOffset)
			nameEnd := nameStart + nameBytes
			if int(entry.NameOffset) < minEntryLen || nameEnd > offset+recordLen {
				return nil, fmt.Errorf("%w: attribute list name out of bounds", ErrInvalidAttribute)
			}

			nameReader := NewBinaryReader(data[nameStart:nameEnd])
			name, err := nameReader.ReadUTF16String(int(entry.NameLength))
			if err != nil {
				return nil, fmt.Errorf("failed to parse attribute list name: %w", err)
			}
			entry.Name = name
		}

		entries = append(entries, entry)
		offset += recordLen
	}

	return entries, nil
}

// attrKey identifies a logical NTFS attribute stream by type and name.
type attrKey struct {
	attrType uint32
	name     string
}

// attrChunk pairs an attribute fragment with its starting VCN for ordering.
type attrChunk struct {
	startVCN uint64
	attr     *Attribute
}

// resolveAttributeList merges attributes from MFT extension records into the
// base entry.  For non-resident attributes split across multiple records (e.g.
// large $INDEX_ALLOCATION, $DATA, $MFT) it stitches the data runs in
// VCN order so callers see a single, contiguous run-list.
func (v *Volume) resolveAttributeList(entry *MFTEntry, entryNum uint64) error {
	attrList := entry.FindAttribute(AttrTypeAttributeList, "")
	if attrList == nil {
		return nil
	}

	var listData []byte
	if attrList.Resident != nil {
		listData = attrList.Resident.Value
	} else if attrList.NonResident != nil {
		size := attrList.NonResident.RealSize
		if size == 0 {
			return nil
		}

		const maxAttributeListSize = 64 << 20
		if size > maxAttributeListSize {
			return fmt.Errorf("attribute list too large: %d", size)
		}

		buf := make([]byte, size)
		n, err := v.ReadDataRuns(attrList.NonResident.DataRuns, 0, buf)
		if err != nil && err != io.EOF {
			return err
		}
		listData = buf[:n]
	}

	if len(listData) == 0 {
		return nil
	}

	listEntries, err := parseAttributeListEntries(listData)
	if err != nil {
		return err
	}

	// Collect extension-record chunks per logical attribute (type + name).
	// We keep startVCN from the attribute list entry, not from the attribute
	// header, because NTFS guarantees the list is sorted by VCN.
	chunksByKey := make(map[attrKey][]attrChunk)

	// Cache loaded extension entries so each record is read at most once.
	extCache := make(map[uint64]*MFTEntry)

	for _, listEntry := range listEntries {
		extNum := listEntry.BaseFileRef
		if extNum == 0 || extNum == entryNum {
			// Belongs to base record; attributes already parsed.
			continue
		}

		extEntry, ok := extCache[extNum]
		if !ok {
			var fetchErr error
			extEntry, fetchErr = v.readMFTEntry(extNum) // avoid recursive resolveAttributeList
			if fetchErr != nil {
				continue
			}
			extCache[extNum] = extEntry
		}

		key := attrKey{listEntry.AttributeType, listEntry.Name}
		for _, attr := range extEntry.Attributes {
			if attr.Header.Type != listEntry.AttributeType {
				continue
			}
			if attributeName(attr) != listEntry.Name {
				continue
			}
			chunksByKey[key] = append(chunksByKey[key], attrChunk{
				startVCN: listEntry.StartingVCN,
				attr:     attr,
			})
			break // one match per attribute-list entry
		}
	}

	// Merge each logical attribute's chunks into the base entry.
	for key, chunks := range chunksByKey {
		// Sort fragments by starting VCN so run-lists are contiguous.
		sort.Slice(chunks, func(i, j int) bool {
			return chunks[i].startVCN < chunks[j].startVCN
		})

		// Find the base-record attribute for this (type, name) if present.
		var baseAttr *Attribute
		for _, attr := range entry.Attributes {
			if attr.Header.Type == key.attrType && attributeName(attr) == key.name {
				baseAttr = attr
				break
			}
		}

		if baseAttr != nil && baseAttr.NonResident != nil {
			// Stitch extension runs onto the base attribute's run-list.
			for _, chunk := range chunks {
				if chunk.attr.NonResident == nil {
					continue
				}
				baseAttr.NonResident.DataRuns = append(
					baseAttr.NonResident.DataRuns,
					chunk.attr.NonResident.DataRuns...,
				)
			}
			// Take size metadata from the last (highest-VCN) chunk.
			last := chunks[len(chunks)-1].attr
			if last.NonResident != nil {
				baseAttr.NonResident.LastVCN = last.NonResident.LastVCN
				baseAttr.NonResident.RealSize = last.NonResident.RealSize
				baseAttr.NonResident.AllocatedSize = last.NonResident.AllocatedSize
				baseAttr.NonResident.InitializedSize = last.NonResident.InitializedSize
			}
		} else {
			// No base-record attribute exists; expose the first extension chunk
			// (or all of them for non-resident, stitched together).
			if len(chunks) == 1 {
				entry.Attributes = append(entry.Attributes, chunks[0].attr)
				continue
			}

			// Multiple chunks with no base: stitch into a synthetic attribute.
			first := chunks[0].attr
			if first.NonResident == nil {
				entry.Attributes = append(entry.Attributes, first)
				continue
			}

			merged := &Attribute{
				Header:      first.Header,
				NonResident: &NonResidentAttribute{},
			}
			*merged.NonResident = *first.NonResident
			merged.NonResident.DataRuns = make([]DataRun, len(first.NonResident.DataRuns))
			copy(merged.NonResident.DataRuns, first.NonResident.DataRuns)

			for _, chunk := range chunks[1:] {
				if chunk.attr.NonResident == nil {
					continue
				}
				merged.NonResident.DataRuns = append(
					merged.NonResident.DataRuns,
					chunk.attr.NonResident.DataRuns...,
				)
			}
			last := chunks[len(chunks)-1].attr
			if last.NonResident != nil {
				merged.NonResident.LastVCN = last.NonResident.LastVCN
				merged.NonResident.RealSize = last.NonResident.RealSize
				merged.NonResident.AllocatedSize = last.NonResident.AllocatedSize
				merged.NonResident.InitializedSize = last.NonResident.InitializedSize
			}
			entry.Attributes = append(entry.Attributes, merged)
		}
	}

	return nil
}

// readMFTEntry reads an MFT entry from disk.
func (v *Volume) readMFTEntry(entryNum uint64) (*MFTEntry, error) {
	// Calculate the offset of the MFT entry
	offset, err := v.getMFTEntryOffset(entryNum)
	if err != nil {
		return nil, wrapMFTError(entryNum, "calculate offset", err)
	}

	return v.readMFTEntryAt(entryNum, offset)
}

// readMFTEntryAt reads an MFT entry from a specific offset.
func (v *Volume) readMFTEntryAt(entryNum uint64, offset int64) (*MFTEntry, error) {
	// Read MFT entry data
	buf := make([]byte, v.mftRecordSize)
	if _, err := v.reader.ReadAt(buf, offset); err != nil {
		return nil, wrapIOError("read", offset, int(v.mftRecordSize), err)
	}

	// Parse MFT entry
	entry, err := v.parseMFTEntry(buf, entryNum)
	if err != nil {
		return nil, wrapMFTError(entryNum, "parse", err)
	}

	return entry, nil
}

// getMFTEntryOffset calculates the byte offset for a given MFT entry number.
func (v *Volume) getMFTEntryOffset(entryNum uint64) (int64, error) {
	// Calculate which cluster the MFT entry is in
	entryOffset := entryNum * uint64(v.mftRecordSize)

	// Find the data run that contains this offset
	currentOffset := uint64(0)

	for _, run := range v.mftDataRuns {
		runSize := run.LengthClusters * uint64(v.bytesPerCluster)

		if entryOffset < currentOffset+runSize {
			// This run contains the entry
			offsetInRun := entryOffset - currentOffset

			if run.IsSparse {
				return 0, fmt.Errorf("MFT entry %d is in sparse region", entryNum)
			}

			cluster := uint64(run.StartCluster)
			clusterOffset := cluster * uint64(v.bytesPerCluster)
			return int64(clusterOffset + offsetInRun), nil
		}

		currentOffset += runSize
	}

	return 0, fmt.Errorf("MFT entry %d beyond MFT size", entryNum)
}

// parseMFTEntry parses an MFT entry from raw bytes.
func (v *Volume) parseMFTEntry(buf []byte, entryNum uint64) (*MFTEntry, error) {
	if len(buf) < int(v.mftRecordSize) {
		return nil, ErrInvalidMFTEntry
	}

	r := NewBinaryReader(buf)
	entry := &MFTEntry{}

	// Parse MFT entry header
	entry.Magic, _ = r.ReadUint32()
	entry.UpdateSeqOffset, _ = r.ReadUint16()
	entry.UpdateSeqSize, _ = r.ReadUint16()
	entry.LogFileSeqNum, _ = r.ReadUint64()
	entry.SequenceNum, _ = r.ReadUint16()
	entry.HardLinkCount, _ = r.ReadUint16()
	entry.FirstAttrOffset, _ = r.ReadUint16()
	entry.Flags, _ = r.ReadUint16()
	entry.UsedSize, _ = r.ReadUint32()
	entry.AllocatedSize, _ = r.ReadUint32()
	entry.BaseRecordRef, _ = r.ReadUint64()
	entry.NextAttrID, _ = r.ReadUint16()

	// Validate magic number
	if entry.Magic != MFTMagicFILE {
		if entry.Magic == MFTMagicBAAD {
			return nil, fmt.Errorf("%w: entry marked as bad (BAAD)", ErrInvalidMFTEntry)
		}
		if entry.Magic == MFTMagicZERO {
			return nil, fmt.Errorf("%w: entry not allocated", ErrInvalidMFTEntry)
		}
		return nil, fmt.Errorf("%w: invalid magic 0x%X", ErrInvalidMFTEntry, entry.Magic)
	}

	// Validate sizes
	if entry.UsedSize > entry.AllocatedSize || entry.AllocatedSize > v.mftRecordSize {
		return nil, fmt.Errorf("%w: invalid sizes (used=%d, allocated=%d, max=%d)",
			ErrInvalidMFTEntry, entry.UsedSize, entry.AllocatedSize, v.mftRecordSize)
	}

	// Apply update sequence array (fixup)
	if err := v.applyUpdateSequence(buf, entry.UpdateSeqOffset, entry.UpdateSeqSize); err != nil {
		return nil, fmt.Errorf("update sequence failed: %w", err)
	}

	// Parse attributes
	if entry.FirstAttrOffset < MFTEntryHeaderSize || uint32(entry.FirstAttrOffset) >= entry.UsedSize {
		return nil, fmt.Errorf("%w: invalid first attribute offset %d",
			ErrInvalidMFTEntry, entry.FirstAttrOffset)
	}

	attrs, err := v.parseAttributes(buf, int(entry.FirstAttrOffset), int(entry.UsedSize))
	if err != nil {
		return nil, fmt.Errorf("parse attributes failed: %w", err)
	}
	entry.Attributes = attrs

	return entry, nil
}

// applyUpdateSequence applies the update sequence array to fix torn writes.
// The update sequence array is NTFS's mechanism for detecting incomplete writes.
func (v *Volume) applyUpdateSequence(buf []byte, offset, size uint16) error {
	if size == 0 {
		return nil // No update sequence
	}

	if int(offset)+int(size)*2 > len(buf) {
		return ErrUpdateSequence
	}

	// Read the update sequence number
	usn := ReadUint16LE(buf, int(offset))

	// Apply fixups
	// Each sector should have the USN at its end, which gets replaced with the real value
	numFixups := int(size) - 1
	for i := 0; i < numFixups; i++ {
		sectorOffset := (i + 1) * UpdateSequenceStride
		if sectorOffset > len(buf) {
			break
		}

		// Check if the last 2 bytes of the sector match the USN
		sectorEnd := sectorOffset - 2
		storedUSN := ReadUint16LE(buf, sectorEnd)

		if storedUSN != usn {
			return fmt.Errorf("%w: mismatch at sector %d (expected 0x%X, got 0x%X)",
				ErrUpdateSequence, i, usn, storedUSN)
		}

		// Replace with the actual value from the update sequence array
		actualValue := ReadUint16LE(buf, int(offset)+2+i*2)
		WriteUint16LE(buf, sectorEnd, actualValue)
	}

	return nil
}

// parseAttributes parses all attributes in an MFT entry.
func (v *Volume) parseAttributes(buf []byte, startOffset, endOffset int) ([]*Attribute, error) {
	var attrs []*Attribute
	offset := startOffset

	for offset < endOffset {
		// Check for end marker
		if offset+4 > len(buf) {
			break
		}

		attrType := ReadUint32LE(buf, offset)
		if attrType == 0xFFFFFFFF {
			// End of attributes
			break
		}

		// Parse attribute
		attr, err := v.parseAttribute(buf, offset)
		if err != nil {
			// Some attributes may be partially damaged; continue if possible
			return attrs, fmt.Errorf("at offset %d: %w", offset, err)
		}

		attrs = append(attrs, attr)

		// Move to next attribute
		if attr.Header.Length == 0 {
			break // Prevent infinite loop
		}
		offset += int(attr.Header.Length)

		// Align to 8-byte boundary
		offset = AlignUp(offset, 8)
	}

	return attrs, nil
}

// parseAttribute parses a single attribute (calls attributes.go functions).
func (v *Volume) parseAttribute(buf []byte, offset int) (*Attribute, error) {
	// This will be implemented in attributes.go
	// For now, provide a stub that reads the header
	return parseAttributeHeader(buf, offset)
}

// IsDirectory returns true if the MFT entry represents a directory.
func (e *MFTEntry) IsDirectory() bool {
	return e.Flags&MFTFlagDirectory != 0
}

// IsInUse returns true if the MFT entry is currently in use.
func (e *MFTEntry) IsInUse() bool {
	return e.Flags&MFTFlagInUse != 0
}

// FindAttribute finds the first attribute of the given type and name.
// If name is empty, it matches any attribute of that type.
func (e *MFTEntry) FindAttribute(attrType uint32, name string) *Attribute {
	for _, attr := range e.Attributes {
		if attr.Header.Type != attrType {
			continue
		}

		// Get attribute name
		attrName := ""
		if attr.Resident != nil {
			attrName = attr.Resident.Name
		} else if attr.NonResident != nil {
			attrName = attr.NonResident.Name
		}

		// Match name if specified
		if name == "" || attrName == name {
			return attr
		}
	}

	return nil
}

// FindAllAttributes finds all attributes of the given type.
func (e *MFTEntry) FindAllAttributes(attrType uint32) []*Attribute {
	var results []*Attribute
	for _, attr := range e.Attributes {
		if attr.Header.Type == attrType {
			results = append(results, attr)
		}
	}
	return results
}

func attributeName(attr *Attribute) string {
	if attr == nil {
		return ""
	}

	if attr.Resident != nil {
		return attr.Resident.Name
	}

	if attr.NonResident != nil {
		return attr.NonResident.Name
	}

	return ""
}

// FindPrimaryDataAttribute returns the best $DATA stream for regular file reads.
// It prefers unnamed streams to avoid selecting alternate named streams first.
func (e *MFTEntry) FindPrimaryDataAttribute() *Attribute {
	dataAttrs := e.FindAllAttributes(AttrTypeData)
	if len(dataAttrs) == 0 {
		return nil
	}

	for _, attr := range dataAttrs {
		if attributeName(attr) == "" {
			return attr
		}
	}

	return dataAttrs[0]
}

// FindPrimaryNonResidentDataAttribute returns the best non-resident $DATA stream.
// It is primarily used for $MFT, which must be located via non-resident data runs.
func (e *MFTEntry) FindPrimaryNonResidentDataAttribute() *Attribute {
	dataAttrs := e.FindAllAttributes(AttrTypeData)
	if len(dataAttrs) == 0 {
		return nil
	}

	for _, attr := range dataAttrs {
		if attr.NonResident != nil && attributeName(attr) == "" {
			return attr
		}
	}

	for _, attr := range dataAttrs {
		if attr.NonResident != nil {
			return attr
		}
	}

	for _, attr := range dataAttrs {
		if attributeName(attr) == "" {
			return attr
		}
	}

	return dataAttrs[0]
}

// GetFileName returns the primary file name for this entry.
// It prefers Win32 namespace over DOS namespace.
func (e *MFTEntry) GetFileName() (*FileName, error) {
	attrs := e.FindAllAttributes(AttrTypeFileName)
	if len(attrs) == 0 {
		return nil, ErrAttributeNotFound
	}

	var bestName *FileName
	var bestNamespace uint8 = 0xFF

	for _, attr := range attrs {
		if attr.Resident == nil {
			continue
		}

		fn, err := parseFileNameAttribute(attr.Resident.Value)
		if err != nil {
			continue
		}

		// Prefer Win32 > WinDOS > POSIX > DOS
		if bestName == nil {
			bestName = fn
			bestNamespace = fn.Namespace
		} else if fn.Namespace == NamespaceWin32 && bestNamespace != NamespaceWin32 {
			bestName = fn
			bestNamespace = fn.Namespace
		} else if fn.Namespace == NamespaceWinDOS && bestNamespace != NamespaceWin32 {
			bestName = fn
			bestNamespace = fn.Namespace
		}
	}

	if bestName == nil {
		return nil, ErrAttributeNotFound
	}

	return bestName, nil
}

// GetStandardInformation returns the standard information for this entry.
func (e *MFTEntry) GetStandardInformation() (*StandardInformation, error) {
	attr := e.FindAttribute(AttrTypeStandardInfo, "")
	if attr == nil || attr.Resident == nil {
		return nil, ErrAttributeNotFound
	}

	return parseStandardInformationAttribute(attr.Resident.Value)
}

// ReadDataRuns reads data from non-resident data runs.
func (v *Volume) ReadDataRuns(runs []DataRun, offset int64, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	bytesRead := 0
	currentOffset := int64(0)

	for _, run := range runs {
		runBytes := int64(run.LengthClusters) * int64(v.bytesPerCluster)

		// Skip runs before our offset
		if currentOffset+runBytes <= offset {
			currentOffset += runBytes
			continue
		}

		// Calculate read position within this run
		offsetInRun := int64(0)
		if offset > currentOffset {
			offsetInRun = offset - currentOffset
		}

		bytesToRead := int64(len(buf) - bytesRead)
		bytesAvailable := runBytes - offsetInRun
		if bytesToRead > bytesAvailable {
			bytesToRead = bytesAvailable
		}

		if run.IsSparse {
			// Sparse run - fill with zeros
			for i := int64(0); i < bytesToRead; i++ {
				buf[bytesRead+int(i)] = 0
			}
			bytesRead += int(bytesToRead)
		} else {
			// Read from disk
			cluster := uint64(run.StartCluster)
			clusterOffset := int64(cluster) * int64(v.bytesPerCluster)
			readOffset := clusterOffset + offsetInRun

			n, err := v.reader.ReadAt(buf[bytesRead:bytesRead+int(bytesToRead)], readOffset)
			bytesRead += n
			if err != nil {
				return bytesRead, wrapIOError("read", readOffset, int(bytesToRead), err)
			}
		}

		currentOffset += runBytes

		if bytesRead >= len(buf) {
			break
		}
	}

	return bytesRead, nil
}
