package libntfs

import (
	"fmt"
	"io"
)

// Everything in this library that can modify a volume lives in this file, and
// nothing here is reachable without a handle that was deliberately opened for
// writing.
//
// # The boundary
//
// libntfs is a forensic reader first. Reads are the whole point of the library;
// writes exist only to support the narrow case of patching bytes back into an
// image that a caller owns. The two are kept apart on purpose:
//
//   - A volume opened with [OpenReadOnly], or Options.ReadOnly, refuses every
//     entry point below with [ErrReadOnlyMode] before touching the handle, and
//     holds a reader that cannot be asserted back to an [io.WriterAt].
//   - A volume opened with [Open] over a reader that is not an [io.WriterAt]
//     refuses them with [ErrReadOnlyVolume].
//   - Only [Open] over a reader that does implement [io.WriterAt] produces a
//     writable volume, and [Volume.IsWritable] says so.
//
// Read APIs never call anything here. A caller that never opens a writable
// volume cannot reach a write through any code path in this package.
//
// # What writing supports
//
// In-place overwrites of the same length, and nothing else. No allocation, no
// growth, no truncation, no metadata repair, no journal updates. Sparse,
// compressed, and encrypted streams are refused rather than guessed at. A write
// through this library leaves the file system exactly as consistent as it found
// it, which on a live or half-understood volume is not a guarantee worth
// relying on — take an image and work on the copy.

// IsWritable reports whether this volume supports in-place writes.
//
// It is false for a volume opened read-only and for one whose reader does not
// implement [io.WriterAt]. Use [Volume.IsReadOnly] to tell those two apart.
func (v *Volume) IsWritable() bool {
	return !v.readOnly && v.writer != nil
}

// checkWritable is the single gate every write entry point passes through.
//
// Ordering matters for the error a caller sees: a closed volume is closed
// whatever its mode, a volume opened read-only reports that it was asked to be,
// and only then does the handle's own capability come into it.
func (v *Volume) checkWritable() error {
	if v.IsClosed() {
		return ErrVolumeClosed
	}

	if v.readOnly {
		return ErrReadOnlyMode
	}

	if v.writer == nil {
		return ErrReadOnlyVolume
	}

	return nil
}

// WriteAt writes len(p) bytes to the volume at the specified absolute offset.
//
// This method is thread-safe and requires a writable volume: one opened with
// [Open] over a handle implementing [io.WriterAt]. A read-only volume returns
// [ErrReadOnlyMode] and a non-writable handle returns [ErrReadOnlyVolume],
// in both cases without writing anything.
func (v *Volume) WriteAt(p []byte, offset int64) (int, error) {
	if err := v.checkWritable(); err != nil {
		return 0, err
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
			return v.writeAtSectorAligned(p, offset, sectorSize)
		}
	}

	n, err := v.writer.WriteAt(p, offset)
	if err != nil {
		return n, wrapIOError("write", offset, len(p), err)
	}

	return n, nil
}

func (v *Volume) writeAtSectorAligned(p []byte, offset, sectorSize int64) (int, error) {
	alignedStart := offset - (offset % sectorSize)
	alignedEnd := alignUpInt64(offset+int64(len(p)), sectorSize)
	if alignedEnd <= alignedStart {
		return 0, ErrInvalidOffset
	}

	bufLen := alignedEnd - alignedStart
	buf := make([]byte, bufLen)

	if _, err := v.reader.ReadAt(buf, alignedStart); err != nil {
		return 0, wrapIOError("read", alignedStart, len(buf), err)
	}

	copy(buf[offset-alignedStart:], p)

	n, err := v.writer.WriteAt(buf, alignedStart)
	if err != nil {
		return 0, wrapIOError("write", alignedStart, len(buf), err)
	}
	if n != len(buf) {
		return 0, wrapIOError("write", alignedStart, len(buf), io.ErrShortWrite)
	}

	return len(p), nil
}

// WriteDataRuns writes data into non-resident data runs at the logical stream offset.
//
// This method performs in-place overwrites only. It does not allocate clusters,
// update metadata, or support sparse/compressed/encrypted streams. It requires a
// writable volume; see [Volume.WriteAt] for what that means.
func (v *Volume) WriteDataRuns(runs []DataRun, offset int64, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if offset < 0 {
		return 0, ErrInvalidOffset
	}

	if err := v.checkWritable(); err != nil {
		return 0, err
	}

	bytesWritten := 0
	offsetU := uint64(offset)
	currentOffset := uint64(0)

	for _, run := range runs {
		runBytes := run.LengthClusters * uint64(v.bytesPerCluster)

		// Skip runs before our offset.
		if currentOffset+runBytes <= offsetU {
			currentOffset += runBytes
			continue
		}

		// Calculate write position within this run.
		offsetInRun := uint64(0)
		if offsetU > currentOffset {
			offsetInRun = offsetU - currentOffset
		}

		bytesToWrite := uint64(len(buf) - bytesWritten)
		bytesAvailable := runBytes - offsetInRun
		if bytesToWrite > bytesAvailable {
			bytesToWrite = bytesAvailable
		}

		writeLen := int(bytesToWrite)

		if run.IsSparse {
			return bytesWritten, fmt.Errorf("%w: %v", ErrWriteUnsupported, ErrSparseData)
		}

		if run.StartCluster < 0 {
			return bytesWritten, fmt.Errorf("%w: negative start cluster %d", ErrInvalidDataRun, run.StartCluster)
		}

		cluster := uint64(run.StartCluster)
		clusterOffset := cluster * uint64(v.bytesPerCluster)
		writeOffset := int64(clusterOffset + offsetInRun)

		n, err := v.WriteAt(buf[bytesWritten:bytesWritten+writeLen], writeOffset)
		bytesWritten += n
		if err != nil {
			return bytesWritten, err
		}

		if n != writeLen {
			return bytesWritten, io.ErrShortWrite
		}

		currentOffset += runBytes

		if bytesWritten >= len(buf) {
			break
		}
	}

	if bytesWritten < len(buf) {
		return bytesWritten, io.EOF
	}

	return bytesWritten, nil
}

// Write writes len(p) bytes to the file at the current file offset.
// The current offset is advanced by the number of bytes written.
func (f *File) Write(p []byte) (int, error) {
	f.readMu.Lock()
	defer f.readMu.Unlock()

	n, err := f.WriteAt(p, f.readPos)
	f.readPos += int64(n)
	return n, err
}

// WriteAt writes len(p) bytes to the file starting at the logical file offset.
//
// Current write support is intentionally narrow to avoid unsafe NTFS metadata edits:
// - Requires a writable volume; a read-only one returns [ErrReadOnlyMode]
// - Refuses sparse, compressed, and encrypted streams
// - Does not extend file size
//
// Non-resident streams are written directly to their clusters. Resident streams,
// whose content lives inside the MFT record, are patched by rewriting that record
// with its fixups reapplied; the value cannot change length, and an attribute
// merged in from an extension record is refused rather than risking a write to
// the wrong record.
func (f *File) WriteAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if err := f.volume.checkWritable(); err != nil {
		return 0, err
	}

	if f.isDir {
		return 0, ErrNotFile
	}

	if offset < 0 {
		return 0, ErrInvalidOffset
	}

	if f.dataAttr == nil {
		return 0, ErrWriteUnsupported
	}

	if offset >= int64(f.size) {
		return 0, io.EOF
	}

	if f.dataAttr.Resident != nil {
		return f.writeResidentAt(f.dataAttr, p, offset)
	}

	if f.dataAttr.NonResident == nil {
		return 0, ErrWriteUnsupported
	}

	if f.dataAttr.IsEncrypted() {
		return 0, fmt.Errorf("%w: %v", ErrWriteUnsupported, ErrEncryptedData)
	}

	if f.dataAttr.IsCompressed() {
		return 0, fmt.Errorf("%w: %v", ErrWriteUnsupported, ErrCompressedData)
	}

	if f.dataAttr.IsSparse() {
		return 0, fmt.Errorf("%w: %v", ErrWriteUnsupported, ErrSparseData)
	}

	bytesToWrite := len(p)
	if offset+int64(bytesToWrite) > int64(f.size) {
		bytesToWrite = int(int64(f.size) - offset)
	}

	if bytesToWrite <= 0 {
		return 0, io.EOF
	}

	n, err := f.volume.WriteDataRuns(f.dataAttr.NonResident.DataRuns, offset, p[:bytesToWrite])
	if err != nil {
		return n, err
	}

	if bytesToWrite < len(p) {
		return n, io.EOF
	}

	return n, nil
}
