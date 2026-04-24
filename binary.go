package libntfs

import (
	"encoding/binary"
	"time"
	"unicode/utf16"
)

// BinaryReader provides helper methods for reading little-endian binary data.
// All NTFS structures use little-endian byte order.
type BinaryReader struct {
	Data   []byte // Source data buffer
	Offset int    // Current read position
}

// NewBinaryReader creates a new BinaryReader for the given data.
func NewBinaryReader(data []byte) *BinaryReader {
	return &BinaryReader{
		Data:   data,
		Offset: 0,
	}
}

// Remaining returns the number of bytes remaining from current offset.
func (r *BinaryReader) Remaining() int {
	return len(r.Data) - r.Offset
}

// Seek sets the read position to the specified offset.
func (r *BinaryReader) Seek(offset int) error {
	if offset < 0 || offset > len(r.Data) {
		return ErrInvalidOffset
	}
	r.Offset = offset
	return nil
}

// Skip advances the read position by n bytes.
func (r *BinaryReader) Skip(n int) error {
	if r.Offset+n > len(r.Data) || r.Offset+n < 0 {
		return ErrInvalidOffset
	}
	r.Offset += n
	return nil
}

// ReadBytes reads n bytes from the current position.
func (r *BinaryReader) ReadBytes(n int) ([]byte, error) {
	if r.Offset+n > len(r.Data) {
		return nil, ErrBufferTooSmall
	}
	result := make([]byte, n)
	copy(result, r.Data[r.Offset:r.Offset+n])
	r.Offset += n
	return result, nil
}

// ReadBytesAt reads n bytes from the specified offset without changing position.
func (r *BinaryReader) ReadBytesAt(offset, n int) ([]byte, error) {
	if offset+n > len(r.Data) || offset < 0 {
		return nil, ErrBufferTooSmall
	}
	result := make([]byte, n)
	copy(result, r.Data[offset:offset+n])
	return result, nil
}

// ReadUint8 reads a single byte.
func (r *BinaryReader) ReadUint8() (uint8, error) {
	if r.Offset+1 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	val := r.Data[r.Offset]
	r.Offset++
	return val, nil
}

// ReadInt8 reads a signed byte.
func (r *BinaryReader) ReadInt8() (int8, error) {
	val, err := r.ReadUint8()
	return int8(val), err
}

// ReadUint16 reads a 16-bit little-endian unsigned integer.
func (r *BinaryReader) ReadUint16() (uint16, error) {
	if r.Offset+2 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	val := binary.LittleEndian.Uint16(r.Data[r.Offset:])
	r.Offset += 2
	return val, nil
}

// ReadInt16 reads a 16-bit little-endian signed integer.
func (r *BinaryReader) ReadInt16() (int16, error) {
	val, err := r.ReadUint16()
	return int16(val), err
}

// ReadUint32 reads a 32-bit little-endian unsigned integer.
func (r *BinaryReader) ReadUint32() (uint32, error) {
	if r.Offset+4 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	val := binary.LittleEndian.Uint32(r.Data[r.Offset:])
	r.Offset += 4
	return val, nil
}

// ReadInt32 reads a 32-bit little-endian signed integer.
func (r *BinaryReader) ReadInt32() (int32, error) {
	val, err := r.ReadUint32()
	return int32(val), err
}

// ReadUint64 reads a 64-bit little-endian unsigned integer.
func (r *BinaryReader) ReadUint64() (uint64, error) {
	if r.Offset+8 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	val := binary.LittleEndian.Uint64(r.Data[r.Offset:])
	r.Offset += 8
	return val, nil
}

// ReadInt64 reads a 64-bit little-endian signed integer.
func (r *BinaryReader) ReadInt64() (int64, error) {
	val, err := r.ReadUint64()
	return int64(val), err
}

// ReadFileReference reads a 6-byte file reference (MFT entry number).
func (r *BinaryReader) ReadFileReference() (uint64, error) {
	if r.Offset+6 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	// File reference is 48 bits (6 bytes)
	val := uint64(r.Data[r.Offset]) |
		uint64(r.Data[r.Offset+1])<<8 |
		uint64(r.Data[r.Offset+2])<<16 |
		uint64(r.Data[r.Offset+3])<<24 |
		uint64(r.Data[r.Offset+4])<<32 |
		uint64(r.Data[r.Offset+5])<<40
	r.Offset += 6
	return val, nil
}

// ReadNTFSTime reads an 8-byte NTFS timestamp and converts to time.Time.
// NTFS timestamps are 64-bit values representing 100-nanosecond intervals
// since January 1, 1601 (UTC).
func (r *BinaryReader) ReadNTFSTime() (time.Time, error) {
	ntfsTime, err := r.ReadUint64()
	if err != nil {
		return time.Time{}, err
	}
	return NTFSTimeToTime(ntfsTime), nil
}

// ReadUTF16String reads a UTF-16 encoded string of the specified length (in characters).
// Returns the string converted to UTF-8.
func (r *BinaryReader) ReadUTF16String(numChars int) (string, error) {
	numBytes := numChars * 2
	if r.Offset+numBytes > len(r.Data) {
		return "", ErrBufferTooSmall
	}

	// Convert UTF-16LE to uint16 slice
	utf16Data := make([]uint16, numChars)
	for i := 0; i < numChars; i++ {
		utf16Data[i] = binary.LittleEndian.Uint16(r.Data[r.Offset+i*2:])
	}
	r.Offset += numBytes

	// Convert UTF-16 to UTF-8
	return string(utf16.Decode(utf16Data)), nil
}

// PeekUint8 reads a byte without advancing the position.
func (r *BinaryReader) PeekUint8() (uint8, error) {
	if r.Offset+1 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	return r.Data[r.Offset], nil
}

// PeekUint16 reads a 16-bit value without advancing the position.
func (r *BinaryReader) PeekUint16() (uint16, error) {
	if r.Offset+2 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	return binary.LittleEndian.Uint16(r.Data[r.Offset:]), nil
}

// PeekUint32 reads a 32-bit value without advancing the position.
func (r *BinaryReader) PeekUint32() (uint32, error) {
	if r.Offset+4 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	return binary.LittleEndian.Uint32(r.Data[r.Offset:]), nil
}

// PeekUint64 reads a 64-bit value without advancing the position.
func (r *BinaryReader) PeekUint64() (uint64, error) {
	if r.Offset+8 > len(r.Data) {
		return 0, ErrBufferTooSmall
	}
	return binary.LittleEndian.Uint64(r.Data[r.Offset:]), nil
}

// Utility functions for time conversion

// NTFSTimeToTime converts NTFS time (100-nanosecond intervals since 1601-01-01)
// to Go time.Time.
func NTFSTimeToTime(ntfsTime uint64) time.Time {
	if ntfsTime == 0 {
		return time.Time{}
	}

	// Convert to Unix time
	if ntfsTime < NTFSTimeOffset {
		return time.Time{} // Before Unix epoch
	}

	intervals := ntfsTime - NTFSTimeOffset
	seconds := intervals / 10000000
	nanos := (intervals % 10000000) * 100

	return time.Unix(int64(seconds), int64(nanos)).UTC()
}

// TimeToNTFSTime converts Go time.Time to NTFS time format.
func TimeToNTFSTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}

	unixNano := t.UnixNano()
	if unixNano < 0 {
		return 0 // Before Unix epoch
	}

	intervals := uint64(unixNano) / 100
	return intervals + NTFSTimeOffset
}

// Utility functions for reading specific structures

// ReadUint16LE reads a 16-bit little-endian value from a byte slice.
func ReadUint16LE(data []byte, offset int) uint16 {
	return binary.LittleEndian.Uint16(data[offset:])
}

// ReadUint32LE reads a 32-bit little-endian value from a byte slice.
func ReadUint32LE(data []byte, offset int) uint32 {
	return binary.LittleEndian.Uint32(data[offset:])
}

// ReadUint64LE reads a 64-bit little-endian value from a byte slice.
func ReadUint64LE(data []byte, offset int) uint64 {
	return binary.LittleEndian.Uint64(data[offset:])
}

// ReadFileRefAt reads a 6-byte file reference at the specified offset.
func ReadFileRefAt(data []byte, offset int) uint64 {
	return uint64(data[offset]) |
		uint64(data[offset+1])<<8 |
		uint64(data[offset+2])<<16 |
		uint64(data[offset+3])<<24 |
		uint64(data[offset+4])<<32 |
		uint64(data[offset+5])<<40
}

// WriteUint16LE writes a 16-bit little-endian value to a byte slice.
func WriteUint16LE(data []byte, offset int, val uint16) {
	binary.LittleEndian.PutUint16(data[offset:], val)
}

// WriteUint32LE writes a 32-bit little-endian value to a byte slice.
func WriteUint32LE(data []byte, offset int, val uint32) {
	binary.LittleEndian.PutUint32(data[offset:], val)
}

// WriteUint64LE writes a 64-bit little-endian value to a byte slice.
func WriteUint64LE(data []byte, offset int, val uint64) {
	binary.LittleEndian.PutUint64(data[offset:], val)
}

// AlignUp rounds up to the nearest multiple of alignment.
func AlignUp(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}

// AlignUp64 rounds up to the nearest multiple of alignment (64-bit version).
func AlignUp64(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}

// Min returns the minimum of two integers.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Min64 returns the minimum of two 64-bit integers.
func Min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Max returns the maximum of two integers.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Max64 returns the maximum of two 64-bit integers.
func Max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
