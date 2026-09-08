package libntfs

import "fmt"

// readMFTRecordRaw reads one MFT record's bytes and applies its fixups, leaving
// the buffer in the same form parseMFTRecordCore works on. It also returns the
// record's byte offset on the volume, ready for a write-back.
func (v *Volume) readMFTRecordRaw(entryNum uint64) ([]byte, int64, error) {
	offset, err := v.getMFTEntryOffset(entryNum)
	if err != nil {
		return nil, 0, wrapMFTError(entryNum, "calculate offset", err)
	}

	buf := make([]byte, v.mftRecordSize)
	if _, err := v.reader.ReadAt(buf, offset); err != nil {
		return nil, 0, wrapIOError("read", offset, int(v.mftRecordSize), err)
	}

	if magic := ReadUint32LE(buf, 0); magic != MFTMagicFILE {
		return nil, 0, fmt.Errorf("%w: entry %d has magic 0x%X",
			ErrInvalidMFTEntry, entryNum, magic)
	}

	updateSeqOffset := ReadUint16LE(buf, 4)
	updateSeqSize := ReadUint16LE(buf, 6)

	if err := applyUpdateSequence(buf, updateSeqOffset, updateSeqSize); err != nil {
		return nil, 0, fmt.Errorf("update sequence failed: %w", err)
	}

	return buf, offset, nil
}

// writeMFTRecord writes a record back to the volume at its entry offset.
//
// The buffer must be in fixed-up form, as readMFTRecordRaw returns it: the real
// bytes present at each sector tail and the update sequence array holding the
// sequence number. This reverses that, stashing each sector tail into the array
// and stamping the sequence number in its place, exactly as NTFS stores it.
//
// The entry is evicted from the MFT cache so later reads see the new bytes.
func (v *Volume) writeMFTRecord(entryNum uint64, record []byte, offset int64) error {
	if err := v.checkWritable(); err != nil {
		return err
	}

	updateSeqOffset := ReadUint16LE(record, 4)
	updateSeqSize := ReadUint16LE(record, 6)

	if err := reverseUpdateSequence(record, updateSeqOffset, updateSeqSize); err != nil {
		return err
	}

	n, err := v.WriteAt(record, offset)
	if err != nil {
		return err
	}
	if n != len(record) {
		return wrapIOError("write", offset, len(record), ErrBufferTooSmall)
	}

	// Evict so later reads see the new bytes. Clearing the saturation flag with
	// it matters: the flag is what lets a full cache skip the write lock, and
	// leaving it set after making room would keep this entry out of the cache
	// for the life of the volume.
	v.mftCacheMu.Lock()
	delete(v.mftCache, entryNum)
	v.mftCacheFull.Store(len(v.mftCache) >= DefaultMFTCacheSize)
	v.mftCacheMu.Unlock()

	return nil
}

// reverseUpdateSequence converts a fixed-up record back to its on-disk form by
// moving each sector's trailing two bytes into the update sequence array and
// writing the sequence number in their place.
//
// The sequence number is left unchanged. NTFS increments it on every write, but
// holding it steady produces an equally consistent record and avoids a second
// failure mode when a write is interrupted.
func reverseUpdateSequence(buf []byte, offset, size uint16) error {
	if size == 0 {
		return nil // No update sequence
	}

	if int(offset)+int(size)*2 > len(buf) {
		return ErrUpdateSequence
	}

	usn := ReadUint16LE(buf, int(offset))

	numFixups := int(size) - 1
	for i := 0; i < numFixups; i++ {
		sectorOffset := (i + 1) * UpdateSequenceStride
		if sectorOffset > len(buf) {
			break
		}

		sectorEnd := sectorOffset - 2

		// Stash the real bytes, then stamp the sequence number over them.
		WriteUint16LE(buf, int(offset)+2+i*2, ReadUint16LE(buf, sectorEnd))
		WriteUint16LE(buf, sectorEnd, usn)
	}

	return nil
}

// writeResidentAt overwrites part of a resident attribute's value in place.
//
// Only same-length overwrites are supported: the value cannot grow or shrink, so
// the record's layout is untouched and no other attribute moves.
func (f *File) writeResidentAt(attr *Attribute, p []byte, offset int64) (int, error) {
	resident := attr.Resident

	if attr.IsEncrypted() {
		return 0, fmt.Errorf("%w: %v", ErrWriteUnsupported, ErrEncryptedData)
	}

	// The attribute's recorded offset is only meaningful for the record it was
	// parsed from. $ATTRIBUTE_LIST merging can place an attribute from an
	// extension record onto this entry, where patching that offset would corrupt
	// an unrelated record.
	if !attr.sourceKnown || attr.sourceRecord != f.entryNum {
		return 0, fmt.Errorf("%w: resident attribute lives in another MFT record",
			ErrWriteUnsupported)
	}

	valueLength := int64(resident.ValueLength)
	if offset >= valueLength {
		return 0, ErrWriteOutOfRange
	}

	// Resident values cannot grow: a longer write would need the attribute
	// resized and every later attribute in the record shifted.
	if offset+int64(len(p)) > valueLength {
		return 0, fmt.Errorf("%w: write of %d bytes at %d exceeds the %d byte resident value",
			ErrWriteOutOfRange, len(p), offset, valueLength)
	}

	record, recordOffset, err := f.volume.readMFTRecordRaw(f.entryNum)
	if err != nil {
		return 0, err
	}

	valueStart := attr.Header.RecordOffset + int(resident.ValueOffset)
	patchStart := valueStart + int(offset)
	patchEnd := patchStart + len(p)

	if valueStart < 0 || patchEnd > len(record) {
		return 0, fmt.Errorf("%w: resident value at %d+%d is outside the %d byte record",
			ErrInvalidMFTEntry, valueStart, offset, len(record))
	}

	copy(record[patchStart:patchEnd], p)

	if err := f.volume.writeMFTRecord(f.entryNum, record, recordOffset); err != nil {
		return 0, err
	}

	// Keep the in-memory copy consistent with what is now on disk.
	if int(offset)+len(p) <= len(resident.Value) {
		copy(resident.Value[offset:], p)
	}

	return len(p), nil
}
