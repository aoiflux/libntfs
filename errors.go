package libntfs

import (
	"errors"
	"fmt"
)

// Common errors
var (
	// ErrInvalidBootSector indicates the boot sector is invalid or corrupted.
	ErrInvalidBootSector = errors.New("invalid or corrupted NTFS boot sector")

	// ErrInvalidMagic indicates an unexpected magic number was encountered.
	ErrInvalidMagic = errors.New("invalid magic number")

	// ErrInvalidMFTEntry indicates an MFT entry is invalid or corrupted.
	ErrInvalidMFTEntry = errors.New("invalid or corrupted MFT entry")

	// ErrInvalidAttribute indicates an attribute is malformed.
	ErrInvalidAttribute = errors.New("invalid or malformed attribute")

	// ErrAttributeNotFound indicates a requested attribute was not found.
	ErrAttributeNotFound = errors.New("attribute not found")

	// ErrUpdateSequence indicates update sequence validation failed.
	ErrUpdateSequence = errors.New("update sequence validation failed")

	// ErrNotDirectory indicates the entry is not a directory.
	ErrNotDirectory = errors.New("not a directory")

	// ErrNotFile indicates the entry is not a regular file.
	ErrNotFile = errors.New("not a regular file")

	// ErrInputIsDirectory indicates the provided input reader points to a directory,
	// but a volume device or image file was expected.
	ErrInputIsDirectory = errors.New("input is a directory")

	// ErrFileNotFound indicates the requested file was not found.
	ErrFileNotFound = errors.New("file not found")

	// ErrInvalidPath indicates the provided path is invalid.
	ErrInvalidPath = errors.New("invalid path")

	// ErrInvalidCluster indicates an invalid cluster number was encountered.
	ErrInvalidCluster = errors.New("invalid cluster number")

	// ErrInvalidDataRun indicates a data run is malformed.
	ErrInvalidDataRun = errors.New("invalid or malformed data run")

	// ErrCompressedData indicates data is compressed (not yet supported).
	ErrCompressedData = errors.New("compressed data not supported")

	// ErrEncryptedData indicates data is encrypted (not yet supported).
	ErrEncryptedData = errors.New("encrypted data not supported")

	// ErrSparseData indicates data is sparse.
	ErrSparseData = errors.New("sparse data encountered")

	// ErrInvalidOffset indicates an invalid offset was provided.
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrBufferTooSmall indicates the provided buffer is too small.
	ErrBufferTooSmall = errors.New("buffer too small")

	// ErrVolumeClosed indicates the volume has been closed.
	ErrVolumeClosed = errors.New("volume is closed")

	// ErrReadOnlyVolume indicates the volume cannot be written to, either because
	// it was opened read-only or because the underlying handle does not support
	// writes.
	ErrReadOnlyVolume = errors.New("volume is read-only")

	// ErrReadOnlyMode indicates the volume was deliberately opened read-only,
	// with [OpenReadOnly] or Options.ReadOnly, and so refuses every write
	// regardless of what the underlying handle supports.
	//
	// It wraps ErrReadOnlyVolume, so callers already matching that keep matching;
	// match this one to tell "the caller asked for evidence-safe" apart from "the
	// handle happens not to be writable".
	ErrReadOnlyMode = fmt.Errorf("%w: opened with Options.ReadOnly", ErrReadOnlyVolume)

	// ErrWriteUnsupported indicates the requested write operation is unsupported.
	ErrWriteUnsupported = errors.New("write operation not supported")

	// ErrWriteOutOfRange indicates a write extends beyond the current file bounds.
	ErrWriteOutOfRange = errors.New("write out of range")

	// ErrInvalidIndex indicates an index structure is invalid.
	ErrInvalidIndex = errors.New("invalid index structure")

	// ErrCorruptIndex indicates a directory index entry or index record could
	// not be parsed. It wraps ErrInvalidIndex so callers already matching the
	// broader error keep matching.
	ErrCorruptIndex = fmt.Errorf("%w: corrupt directory index", ErrInvalidIndex)

	// ErrTruncatedRecord indicates a record ended before the structures it
	// declared did. It wraps ErrInvalidIndex for the same reason.
	ErrTruncatedRecord = fmt.Errorf("%w: truncated index record", ErrInvalidIndex)

	// ErrCacheFull indicates the cache is full.
	ErrCacheFull = errors.New("cache is full")

	// ErrInvalidSecurityDescriptor indicates a malformed security descriptor.
	ErrInvalidSecurityDescriptor = errors.New("invalid or malformed security descriptor")

	// ErrSecurityDescriptorNotFound indicates no descriptor exists for a security ID.
	ErrSecurityDescriptorNotFound = errors.New("security descriptor not found")

	// ErrInvalidUSNRecord indicates a malformed USN change journal record.
	ErrInvalidUSNRecord = errors.New("invalid or malformed USN record")

	// ErrInvalidExtendedAttribute indicates a malformed $EA or $EA_INFORMATION value.
	ErrInvalidExtendedAttribute = errors.New("invalid or malformed extended attribute")

	// ErrInvalidReparsePoint indicates a malformed $REPARSE_POINT value.
	ErrInvalidReparsePoint = errors.New("invalid or malformed reparse point")

	// ErrInvalidLogRecord indicates a malformed $LogFile page or record.
	ErrInvalidLogRecord = errors.New("invalid or malformed log record")
)

// PartialDirError reports that a directory listing is incomplete.
//
// ReadDir returns it alongside the entries it did recover, so a walk over a
// damaged volume can record what was lost and carry on rather than treating
// the whole subtree as unreadable. Callers that only care whether the listing
// is trustworthy can keep testing err != nil; callers that want the surviving
// names should check for this type:
//
//	entries, err := dir.ReadDir()
//	var partial *libntfs.PartialDirError
//	if errors.As(err, &partial) {
//		// entries holds partial.Recovered names; partial.Skipped were lost.
//	}
type PartialDirError struct {
	EntryNum  uint64 // MFT entry of the directory
	Recovered int    // Entries successfully parsed and returned
	Skipped   int    // Index records that could not be parsed
	Err       error  // Underlying error (ErrCorruptIndex, ErrTruncatedRecord, ...)
}

func (e *PartialDirError) Error() string {
	return fmt.Sprintf("directory %d: recovered %d entries, skipped %d index records: %v",
		e.EntryNum, e.Recovered, e.Skipped, e.Err)
}

func (e *PartialDirError) Unwrap() error {
	return e.Err
}

// VolumeError wraps errors specific to volume operations.
type VolumeError struct {
	Op  string // Operation that failed
	Err error  // Underlying error
}

func (e *VolumeError) Error() string {
	return fmt.Sprintf("volume %s: %v", e.Op, e.Err)
}

func (e *VolumeError) Unwrap() error {
	return e.Err
}

// MFTError wraps errors specific to MFT operations.
type MFTError struct {
	Entry uint64 // MFT entry number
	Op    string // Operation that failed
	Err   error  // Underlying error
}

func (e *MFTError) Error() string {
	return fmt.Sprintf("MFT entry %d: %s: %v", e.Entry, e.Op, e.Err)
}

func (e *MFTError) Unwrap() error {
	return e.Err
}

// AttributeError wraps errors specific to attribute operations.
type AttributeError struct {
	Type uint32 // Attribute type
	Name string // Attribute name
	Op   string // Operation that failed
	Err  error  // Underlying error
}

func (e *AttributeError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("attribute type 0x%X (%s): %s: %v", e.Type, e.Name, e.Op, e.Err)
	}
	return fmt.Sprintf("attribute type 0x%X: %s: %v", e.Type, e.Op, e.Err)
}

func (e *AttributeError) Unwrap() error {
	return e.Err
}

// ParseError wraps errors that occur during parsing.
type ParseError struct {
	Offset int64  // Offset where error occurred
	Field  string // Field being parsed
	Err    error  // Underlying error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error at offset 0x%X (%s): %v", e.Offset, e.Field, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// IOError wraps I/O errors with context.
type IOError struct {
	Offset int64  // Offset where I/O failed
	Size   int    // Number of bytes attempted
	Op     string // Operation (read/write/seek)
	Err    error  // Underlying error
}

func (e *IOError) Error() string {
	return fmt.Sprintf("I/O %s at offset 0x%X (size %d): %v", e.Op, e.Offset, e.Size, e.Err)
}

func (e *IOError) Unwrap() error {
	return e.Err
}

// PathError wraps path traversal failures with path context.
type PathError struct {
	Op        string // Operation (open, readdir, lookup, traverse)
	Path      string // Full path requested
	Component string // Component or traversed subpath that failed
	Err       error  // Underlying error
}

func (e *PathError) Error() string {
	if e.Component != "" {
		return fmt.Sprintf("path %s (%s, component %s): %v", e.Op, e.Path, e.Component, e.Err)
	}
	return fmt.Sprintf("path %s (%s): %v", e.Op, e.Path, e.Err)
}

func (e *PathError) Unwrap() error {
	return e.Err
}

// Helper functions for creating errors

// wrapVolumeError creates a VolumeError.
func wrapVolumeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &VolumeError{Op: op, Err: err}
}

// wrapMFTError creates an MFTError.
func wrapMFTError(entry uint64, op string, err error) error {
	if err == nil {
		return nil
	}
	return &MFTError{Entry: entry, Op: op, Err: err}
}

// wrapAttributeError creates an AttributeError.
func wrapAttributeError(attrType uint32, name, op string, err error) error {
	if err == nil {
		return nil
	}
	return &AttributeError{Type: attrType, Name: name, Op: op, Err: err}
}

// wrapParseError creates a ParseError.
func wrapParseError(offset int64, field string, err error) error {
	if err == nil {
		return nil
	}
	return &ParseError{Offset: offset, Field: field, Err: err}
}

// wrapIOError creates an IOError.
func wrapIOError(op string, offset int64, size int, err error) error {
	if err == nil {
		return nil
	}
	return &IOError{Op: op, Offset: offset, Size: size, Err: err}
}

// ioErrorAt is wrapIOError for a failure at a volume-relative offset.
//
// It lifts the offset into image space before reporting it, so IOError.Offset
// speaks the same coordinates as every other offset the library hands out. The
// read and write paths work in volume space throughout; this is the one place
// that matters to a caller, because the offset in the error is the only part of
// it they see.
func (v *Volume) ioErrorAt(op string, volumeOffset int64, size int, err error) error {
	return wrapIOError(op, v.baseOffset+volumeOffset, size, err)
}

// wrapPathError creates a PathError.
func wrapPathError(op, fullPath, component string, err error) error {
	if err == nil {
		return nil
	}
	return &PathError{Op: op, Path: fullPath, Component: component, Err: err}
}
