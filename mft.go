package libntfs

import (
	"fmt"
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

	// Add to cache
	v.mftCacheMu.Lock()
	if len(v.mftCache) < DefaultMFTCacheSize {
		v.mftCache[entryNum] = entry
	}
	v.mftCacheMu.Unlock()

	return entry, nil
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
