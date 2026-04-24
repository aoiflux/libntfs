package libntfs

import (
	"fmt"
)

// parseAttributeHeader parses an attribute header from the buffer at the given offset.
func parseAttributeHeader(buf []byte, offset int) (*Attribute, error) {
	if offset+AttributeHeaderSize > len(buf) {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(buf[offset:])
	attr := &Attribute{}

	// Parse common header
	attr.Header.Type, _ = r.ReadUint32()
	attr.Header.Length, _ = r.ReadUint32()
	attr.Header.NonResident, _ = r.ReadUint8()
	attr.Header.NameLength, _ = r.ReadUint8()
	attr.Header.NameOffset, _ = r.ReadUint16()
	attr.Header.Flags, _ = r.ReadUint16()
	attr.Header.AttributeID, _ = r.ReadUint16()

	// Validate
	if attr.Header.Length < AttributeHeaderSize {
		return nil, fmt.Errorf("%w: length too small %d", ErrInvalidAttribute, attr.Header.Length)
	}

	if offset+int(attr.Header.Length) > len(buf) {
		return nil, fmt.Errorf("%w: length extends beyond buffer", ErrInvalidAttribute)
	}

	// Parse resident or non-resident specific parts
	if attr.Header.NonResident == 0 {
		// Resident attribute
		res, err := parseResidentAttribute(buf[offset:], &attr.Header)
		if err != nil {
			return nil, err
		}
		attr.Resident = res
	} else {
		// Non-resident attribute
		nonRes, err := parseNonResidentAttribute(buf[offset:], &attr.Header)
		if err != nil {
			return nil, err
		}
		attr.NonResident = nonRes
	}

	return attr, nil
}

// parseResidentAttribute parses a resident attribute.
func parseResidentAttribute(buf []byte, header *AttributeHeader) (*ResidentAttribute, error) {
	if len(buf) < ResidentAttributeHeaderSize {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(buf[AttributeHeaderSize:])
	res := &ResidentAttribute{
		Header: *header,
	}

	res.ValueLength, _ = r.ReadUint32()
	res.ValueOffset, _ = r.ReadUint16()
	res.IndexedFlag, _ = r.ReadUint8()
	res.Reserved, _ = r.ReadUint8()

	// Validate offsets
	if int(res.ValueOffset) < ResidentAttributeHeaderSize || int(res.ValueOffset) > len(buf) {
		return nil, fmt.Errorf("%w: value offset out of bounds", ErrInvalidAttribute)
	}

	valueStart := int(res.ValueOffset)
	valueEnd := valueStart + int(res.ValueLength)
	if valueEnd < valueStart || valueEnd > len(buf) {
		return nil, fmt.Errorf("%w: value extends beyond attribute", ErrInvalidAttribute)
	}

	// Read attribute name if present
	if header.NameLength > 0 {
		if int(header.NameOffset)+int(header.NameLength)*2 > len(buf) {
			return nil, fmt.Errorf("%w: name extends beyond attribute", ErrInvalidAttribute)
		}
		nameReader := NewBinaryReader(buf[header.NameOffset:])
		name, err := nameReader.ReadUTF16String(int(header.NameLength))
		if err != nil {
			return nil, fmt.Errorf("failed to read attribute name: %w", err)
		}
		res.Name = name
	}

	// Read attribute value
	res.Value = make([]byte, res.ValueLength)
	copy(res.Value, buf[valueStart:valueEnd])

	return res, nil
}

// parseNonResidentAttribute parses a non-resident attribute.
func parseNonResidentAttribute(buf []byte, header *AttributeHeader) (*NonResidentAttribute, error) {
	if len(buf) < NonResidentAttributeHeaderSize {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(buf[AttributeHeaderSize:])
	nonRes := &NonResidentAttribute{
		Header: *header,
	}

	nonRes.StartingVCN, _ = r.ReadUint64()
	nonRes.LastVCN, _ = r.ReadUint64()
	nonRes.DataRunOffset, _ = r.ReadUint16()
	nonRes.CompressionUnit, _ = r.ReadUint16()
	nonRes.Reserved, _ = r.ReadUint32()
	nonRes.AllocatedSize, _ = r.ReadUint64()
	nonRes.RealSize, _ = r.ReadUint64()
	nonRes.InitializedSize, _ = r.ReadUint64()

	// Validate
	if nonRes.AllocatedSize < nonRes.RealSize {
		// This can happen legitimately, so just warn
	}

	// Read attribute name if present
	if header.NameLength > 0 {
		if int(header.NameOffset)+int(header.NameLength)*2 > len(buf) {
			return nil, fmt.Errorf("%w: name extends beyond attribute", ErrInvalidAttribute)
		}
		nameReader := NewBinaryReader(buf[header.NameOffset:])
		name, err := nameReader.ReadUTF16String(int(header.NameLength))
		if err != nil {
			return nil, fmt.Errorf("failed to read attribute name: %w", err)
		}
		nonRes.Name = name
	}

	// Parse data runs
	if int(nonRes.DataRunOffset) < len(buf) {
		runs, err := parseDataRuns(buf[nonRes.DataRunOffset:])
		if err != nil {
			return nil, fmt.Errorf("failed to parse data runs: %w", err)
		}
		nonRes.DataRuns = runs
	}

	return nonRes, nil
}

// parseDataRuns parses a sequence of data runs.
func parseDataRuns(buf []byte) ([]DataRun, error) {
	var runs []DataRun
	offset := 0
	prevCluster := int64(0)

	for offset < len(buf) {
		if offset >= len(buf) {
			break
		}

		// Read run header
		header := buf[offset]
		if header == 0 {
			// End of data runs
			break
		}
		offset++

		// Extract length and offset sizes
		lengthSize := int(header & 0x0F)
		offsetSize := int((header & 0xF0) >> 4)

		if lengthSize == 0 || lengthSize > 8 {
			return nil, fmt.Errorf("%w: invalid length size %d", ErrInvalidDataRun, lengthSize)
		}

		if offsetSize > 8 {
			return nil, fmt.Errorf("%w: invalid offset size %d", ErrInvalidDataRun, offsetSize)
		}

		// Check bounds
		if offset+lengthSize+offsetSize > len(buf) {
			return nil, fmt.Errorf("%w: run extends beyond buffer", ErrInvalidDataRun)
		}

		// Read length (always positive)
		length := readVariableInt(buf[offset:], lengthSize, false)
		offset += lengthSize

		run := DataRun{
			LengthClusters: uint64(length),
			IsSparse:       offsetSize == 0,
		}

		// Read offset (can be negative for relative addressing)
		if offsetSize > 0 {
			clusterOffset := readVariableInt(buf[offset:], offsetSize, true)
			offset += offsetSize

			// Cluster offsets are relative to previous run
			prevCluster += clusterOffset
			run.StartCluster = prevCluster
		}

		runs = append(runs, run)
	}

	return runs, nil
}

// readVariableInt reads a variable-length integer (1-8 bytes).
// If signed is true, sign-extends the value.
func readVariableInt(buf []byte, size int, signed bool) int64 {
	if size == 0 || size > 8 {
		return 0
	}

	var val int64
	for i := 0; i < size; i++ {
		val |= int64(buf[i]) << (i * 8)
	}

	// Sign extend if needed
	if signed && size < 8 {
		// Check if the high bit is set
		if buf[size-1]&0x80 != 0 {
			// Sign extend
			for i := size; i < 8; i++ {
				val |= int64(0xFF) << (i * 8)
			}
		}
	}

	return val
}

// parseStandardInformationAttribute parses a $STANDARD_INFORMATION attribute.
func parseStandardInformationAttribute(data []byte) (*StandardInformation, error) {
	if len(data) < 48 {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(data)
	si := &StandardInformation{}

	si.CreateTime, _ = r.ReadNTFSTime()
	si.ModifyTime, _ = r.ReadNTFSTime()
	si.MFTChangeTime, _ = r.ReadNTFSTime()
	si.AccessTime, _ = r.ReadNTFSTime()
	si.FileAttributes, _ = r.ReadUint32()
	si.MaxVersions, _ = r.ReadUint32()
	si.VersionNumber, _ = r.ReadUint32()
	si.ClassID, _ = r.ReadUint32()

	// Extended fields (Windows 2000+)
	if len(data) >= 72 {
		si.OwnerID, _ = r.ReadUint32()
		si.SecurityID, _ = r.ReadUint32()
		si.QuotaCharged, _ = r.ReadUint64()
		si.UpdateSequenceNum, _ = r.ReadUint64()
	}

	return si, nil
}

// parseFileNameAttribute parses a $FILE_NAME attribute.
func parseFileNameAttribute(data []byte) (*FileName, error) {
	if len(data) < 66 {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(data)
	fn := &FileName{}

	fn.ParentDirectory, _ = r.ReadFileReference()
	fn.ParentSeqNum, _ = r.ReadUint16()
	fn.CreateTime, _ = r.ReadNTFSTime()
	fn.ModifyTime, _ = r.ReadNTFSTime()
	fn.MFTChangeTime, _ = r.ReadNTFSTime()
	fn.AccessTime, _ = r.ReadNTFSTime()
	fn.AllocatedSize, _ = r.ReadUint64()
	fn.RealSize, _ = r.ReadUint64()
	fileAttrs, _ := r.ReadUint64()
	fn.FileAttributes = fileAttrs
	fn.NameLength, _ = r.ReadUint8()
	fn.Namespace, _ = r.ReadUint8()

	// Read file name
	if fn.NameLength > 0 {
		if r.Remaining() < int(fn.NameLength)*2 {
			return nil, fmt.Errorf("%w: file name extends beyond attribute", ErrInvalidAttribute)
		}
		name, err := r.ReadUTF16String(int(fn.NameLength))
		if err != nil {
			return nil, fmt.Errorf("failed to read file name: %w", err)
		}
		fn.Name = name
	}

	return fn, nil
}

// parseIndexRoot parses an $INDEX_ROOT attribute.
func parseIndexRoot(data []byte) (*IndexRoot, error) {
	if len(data) < 16+16 {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(data)
	idx := &IndexRoot{}

	idx.AttributeType, _ = r.ReadUint32()
	idx.CollationRule, _ = r.ReadUint32()
	idx.IndexBlockSize, _ = r.ReadUint32()
	idx.ClustersPerIndex, _ = r.ReadUint8()
	r.Skip(3) // Reserved

	// Parse node header
	idx.NodeHeader.EntryListOffset, _ = r.ReadUint32()
	idx.NodeHeader.EntryListEnd, _ = r.ReadUint32()
	idx.NodeHeader.EntryListAlloc, _ = r.ReadUint32()
	idx.NodeHeader.Flags, _ = r.ReadUint32()

	entryListOffset := int(idx.NodeHeader.EntryListOffset)
	entryListEnd := int(idx.NodeHeader.EntryListEnd)
	entriesStart := 16 + entryListOffset
	entriesEnd := 16 + entryListEnd

	if entryListOffset < 0 || entryListEnd < entryListOffset || entriesEnd > len(data) {
		return nil, fmt.Errorf("%w: invalid index root entry list bounds (offset=%d, end=%d, size=%d)",
			ErrInvalidIndex, idx.NodeHeader.EntryListOffset, idx.NodeHeader.EntryListEnd, len(data))
	}

	// Parse entries
	entriesData := data[entriesStart:entriesEnd]
	entries, err := parseIndexEntries(entriesData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse index entries: %w", err)
	}
	idx.Entries = entries

	return idx, nil
}

// parseIndexEntries parses a sequence of index entries.
func parseIndexEntries(data []byte) ([]IndexEntry, error) {
	var entries []IndexEntry
	offset := 0

	for offset < len(data) {
		if offset+16 > len(data) {
			break
		}

		r := NewBinaryReader(data[offset:])
		entry := IndexEntry{}

		entry.FileReference, _ = r.ReadFileReference()
		entry.SequenceNum, _ = r.ReadUint16()
		entry.EntryLength, _ = r.ReadUint16()
		entry.StreamLength, _ = r.ReadUint16()
		entry.Flags, _ = r.ReadUint8()
		r.Skip(3) // Reserved

		// Validate
		if entry.EntryLength < 16 {
			break
		}

		if offset+int(entry.EntryLength) > len(data) {
			break
		}

		// Read stream data (usually a FileName attribute)
		if entry.StreamLength > 0 && entry.StreamLength < entry.EntryLength {
			streamStart := 16
			streamEnd := streamStart + int(entry.StreamLength)
			if streamEnd <= int(entry.EntryLength) {
				entry.Stream = make([]byte, entry.StreamLength)
				copy(entry.Stream, data[offset+streamStart:offset+streamEnd])

				// Try to parse as FileName
				if len(entry.Stream) >= 66 {
					fn, err := parseFileNameAttribute(entry.Stream)
					if err == nil {
						entry.FileName = fn
					}
				}
			}
		}

		// Read sub-node VCN if present
		if entry.Flags&IndexFlagNode != 0 && entry.EntryLength >= 24 {
			vcnOffset := offset + int(entry.EntryLength) - 8
			if vcnOffset+8 <= len(data) {
				entry.SubNodeVCN = ReadUint64LE(data, vcnOffset)
			}
		}

		entries = append(entries, entry)

		// Check for last entry
		if entry.Flags&IndexFlagLast != 0 {
			break
		}

		offset += int(entry.EntryLength)
	}

	return entries, nil
}

func parseDeletedFileNameFromIndexSlack(data []byte, offset int) (*FileName, int) {
	streamStart := offset + 16
	if streamStart+66 > len(data) {
		return nil, 0
	}

	nameLen := int(data[streamStart+64])
	if nameLen <= 0 {
		return nil, 0
	}

	nameEnd := streamStart + 66 + nameLen*2
	if nameEnd > len(data) {
		return nil, 0
	}

	fn, err := parseFileNameAttribute(data[streamStart:nameEnd])
	if err != nil || fn.Name == "" {
		return nil, 0
	}

	if fn.AllocatedSize < fn.RealSize {
		return nil, 0
	}

	consumed := AlignUp(16+66+nameLen*2, 4)
	return fn, consumed
}

// parseIndexEntriesRecoverDeleted parses index entries from an entire index entry
// allocation area and marks entries in slack space as deleted when possible.
func parseIndexEntriesRecoverDeleted(data []byte, usedLen int) ([]IndexEntry, error) {
	var entries []IndexEntry
	if len(data) < 16 {
		return entries, nil
	}

	if usedLen < 0 {
		usedLen = 0
	}
	if usedLen > len(data) {
		usedLen = len(data)
	}

	offset := 0
	for offset+16 <= len(data) {
		r := NewBinaryReader(data[offset:])
		entry := IndexEntry{}

		entry.FileReference, _ = r.ReadFileReference()
		entry.SequenceNum, _ = r.ReadUint16()
		entry.EntryLength, _ = r.ReadUint16()
		entry.StreamLength, _ = r.ReadUint16()
		entry.Flags, _ = r.ReadUint8()
		r.Skip(3)

		entryLen := int(entry.EntryLength)
		if entryLen < 16 || entryLen%4 != 0 || offset+entryLen > len(data) {
			offset += 4
			continue
		}

		usedBoundaryExceeded := offset+entryLen > usedLen
		entry.Deleted = usedBoundaryExceeded || entry.StreamLength == 0

		if entry.StreamLength > 0 && int(entry.StreamLength) <= entryLen-16 {
			streamStart := offset + 16
			streamEnd := streamStart + int(entry.StreamLength)
			entry.Stream = make([]byte, entry.StreamLength)
			copy(entry.Stream, data[streamStart:streamEnd])

			if len(entry.Stream) >= 66 {
				fn, err := parseFileNameAttribute(entry.Stream)
				if err == nil {
					entry.FileName = fn
				}
			}
		} else if fn, consumed := parseDeletedFileNameFromIndexSlack(data, offset); fn != nil {
			entry.FileName = fn
			entry.Deleted = true
			if consumed > entryLen {
				entry.EntryLength = uint16(consumed)
				entryLen = consumed
			}
		}

		if entry.Flags&IndexFlagNode != 0 && entryLen >= 24 {
			vcnOffset := offset + entryLen - 8
			if vcnOffset+8 <= len(data) {
				entry.SubNodeVCN = ReadUint64LE(data, vcnOffset)
			}
		}

		entries = append(entries, entry)
		offset += entryLen
	}

	return entries, nil
}

// parseVolumeInformation parses a $VOLUME_INFORMATION attribute.
func parseVolumeInformation(data []byte) (*VolumeInformation, error) {
	if len(data) < 16 {
		return nil, ErrInvalidAttribute
	}

	r := NewBinaryReader(data)
	vi := &VolumeInformation{}

	vi.Reserved, _ = r.ReadUint64()
	vi.MajorVersion, _ = r.ReadUint8()
	vi.MinorVersion, _ = r.ReadUint8()
	vi.Flags, _ = r.ReadUint16()
	vi.Reserved2, _ = r.ReadUint32()

	return vi, nil
}

// parseObjectID parses an $OBJECT_ID attribute.
func parseObjectID(data []byte) (*ObjectID, error) {
	if len(data) < 16 {
		return nil, ErrInvalidAttribute
	}

	oid := &ObjectID{}
	copy(oid.ObjectID[:], data[0:16])

	// Extended fields (optional)
	if len(data) >= 64 {
		copy(oid.BirthVolumeID[:], data[16:32])
		copy(oid.BirthObjectID[:], data[32:48])
		copy(oid.BirthDomainID[:], data[48:64])
	}

	return oid, nil
}

// GetAttributeName returns a human-readable name for an attribute.
func (a *Attribute) GetAttributeName() string {
	name := GetAttributeTypeName(a.Header.Type)

	var customName string
	if a.Resident != nil && a.Resident.Name != "" {
		customName = a.Resident.Name
	} else if a.NonResident != nil && a.NonResident.Name != "" {
		customName = a.NonResident.Name
	}

	if customName != "" {
		return fmt.Sprintf("%s:%s", name, customName)
	}
	return name
}

// IsResident returns true if the attribute is resident.
func (a *Attribute) IsResident() bool {
	return a.Header.NonResident == 0
}

// IsCompressed returns true if the attribute is compressed.
func (a *Attribute) IsCompressed() bool {
	return a.Header.Flags&AttrFlagCompressed != 0
}

// IsEncrypted returns true if the attribute is encrypted.
func (a *Attribute) IsEncrypted() bool {
	return a.Header.Flags&AttrFlagEncrypted != 0
}

// IsSparse returns true if the attribute is sparse.
func (a *Attribute) IsSparse() bool {
	return a.Header.Flags&AttrFlagSparse != 0
}
