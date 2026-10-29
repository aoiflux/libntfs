package libntfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// The compression-unit field in a non-resident attribute header is log2 of the
// clusters per unit, which these tests pin down. libntfs read it as the cluster
// count up to v0.3.5, so every unit was decoded at a fraction of its size and
// split at the wrong cluster boundary (PAR-27).
//
// The LZNT1 payloads here come from lznt1Compress below rather than out of a
// real volume, so they prove the unit geometry rather than the token encoding.
// The token encoding is unchanged by the fix and is checked against
// ntfs-3g-written bytes outside the test suite.

// lznt1Shift returns the position-dependent split between a phrase token's
// offset and length fields, exactly as ntfsDecompressCompressionUnit computes
// it: the offset takes the high 4+shift bits and the length the low 12-shift.
func lznt1Shift(pos int) uint {
	var shift uint
	for i := pos - 1; i >= 0x10; i >>= 1 {
		shift++
	}
	return shift
}

// lznt1CompressChunk encodes one sub-block body of at most 4096 bytes: a flag
// byte every eight tokens, a literal byte for a clear bit, a 16-bit phrase
// token for a set one.
func lznt1CompressChunk(chunk []byte) []byte {
	body := make([]byte, 0, len(chunk))
	flagsPos := -1
	var flags byte
	tokens := 0

	emit := func(phrase bool, payload ...byte) {
		if tokens == 0 {
			flagsPos = len(body)
			body = append(body, 0)
			flags = 0
		}
		if phrase {
			flags |= 1 << tokens
		}
		body = append(body, payload...)
		tokens++
		if tokens == 8 {
			body[flagsPos] = flags
			tokens = 0
		}
	}

	for pos := 0; pos < len(chunk); {
		bestLen, bestOff := 0, 0

		if pos > 0 {
			shift := lznt1Shift(pos)
			maxOff := 1 << (4 + shift)
			if maxOff > pos {
				maxOff = pos
			}
			maxLen := (0x0FFF >> shift) + 3
			if maxLen > len(chunk)-pos {
				maxLen = len(chunk) - pos
			}

			for off := 1; off <= maxOff; off++ {
				n := 0
				for n < maxLen && chunk[pos+n] == chunk[pos-off+n] {
					n++
				}
				if n > bestLen {
					bestLen, bestOff = n, off
				}
			}
		}

		// A phrase token costs two bytes and encodes at least three, so a
		// shorter match is not worth one.
		if bestLen < 3 {
			emit(false, chunk[pos])
			pos++
			continue
		}

		shift := lznt1Shift(pos)
		token := uint16(bestOff-1)<<(12-shift) | uint16(bestLen-3)
		emit(true, byte(token), byte(token>>8))
		pos += bestLen
	}

	if tokens != 0 {
		body[flagsPos] = flags
	}

	return body
}

// lznt1Compress encodes data as the sequence of sub-blocks
// ntfsDecompressCompressionUnit decodes.
func lznt1Compress(data []byte) []byte {
	var out []byte

	for start := 0; start < len(data); start += ntfsSubBlockUncompressedDataSize {
		end := start + ntfsSubBlockUncompressedDataSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]

		body := lznt1CompressChunk(chunk)

		// The decoder reads a compressed sub-block whose body is exactly 4096
		// bytes as a stored one, so a body that long has to be stored instead.
		//
		// A stored sub-block of exactly one byte is the other way round: its
		// header would be 0x0000, which LZNT1 already spends on end of data, so
		// the compressed form goes out even though it is a byte longer. NTFS
		// has the same ambiguity and stores the whole unit instead; the
		// compressed form is what keeps this encoder's round trip honest.
		storable := len(chunk) > 1
		if storable && (len(body) == 0 || len(body) >= len(chunk) ||
			len(body) == ntfsSubBlockUncompressedDataSize) {
			out = binary.LittleEndian.AppendUint16(out, uint16(len(chunk)-1))
			out = append(out, chunk...)
			continue
		}

		out = binary.LittleEndian.AppendUint16(out, uint16(len(body)-1)|0x8000)
		out = append(out, body...)
	}

	return out
}

// compressedUnit describes one compression unit of a synthetic stream.
type compressedUnit struct {
	plain []byte // The unit's decompressed content; nil means an all-sparse hole.
	store bool   // Store plain in full rather than compressing it.
}

// buildCompressedStream lays out units as clusters on a synthetic volume and
// returns the volume, the attribute describing the stream, and the content the
// stream should read back as.
//
// Each unit spans 1<<compressionUnit clusters of the run list: a compressed one
// takes as many clusters as its LZNT1 encoding needs and a sparse run for the
// rest, which is how NTFS records that a unit is compressed; a stored one takes
// all of them; a hole takes none.
func buildCompressedStream(t testing.TB, clusterSize uint32, compressionUnit uint16,
	units []compressedUnit,
) (*Volume, *Attribute, []byte) {
	t.Helper()

	clustersPerUnit := 1 << compressionUnit
	unitBytes := clustersPerUnit * int(clusterSize)

	// Leave the first cluster unused so a zero StartCluster cannot pass by
	// accident.
	const firstDataCluster = 1

	var (
		disk    = make([]byte, firstDataCluster*int(clusterSize))
		runs    []DataRun
		want    []byte
		nextLCN = int64(firstDataCluster)
	)

	appendClusters := func(payload []byte) int {
		clusters := (len(payload) + int(clusterSize) - 1) / int(clusterSize)
		disk = append(disk, make([]byte, clusters*int(clusterSize))...)
		copy(disk[nextLCN*int64(clusterSize):], payload)
		runs = append(runs, DataRun{StartCluster: nextLCN, LengthClusters: uint64(clusters)})
		nextLCN += int64(clusters)
		return clusters
	}

	for i, unit := range units {
		switch {
		case unit.plain == nil:
			runs = append(runs, DataRun{LengthClusters: uint64(clustersPerUnit), IsSparse: true})
			want = append(want, make([]byte, unitBytes)...)

		case unit.store:
			if len(unit.plain) != unitBytes {
				t.Fatalf("unit %d: a stored unit must be exactly %d bytes, got %d",
					i, unitBytes, len(unit.plain))
			}
			if got := appendClusters(unit.plain); got != clustersPerUnit {
				t.Fatalf("unit %d: stored unit took %d clusters, want %d", i, got, clustersPerUnit)
			}
			want = append(want, unit.plain...)

		default:
			used := appendClusters(lznt1Compress(unit.plain))
			if used >= clustersPerUnit {
				t.Fatalf("unit %d: the encoding took %d of %d clusters, so the unit would "+
					"not read as compressed; use a more repetitive pattern",
					i, used, clustersPerUnit)
			}
			// The trailing hole is what marks the unit compressed.
			runs = append(runs, DataRun{
				LengthClusters: uint64(clustersPerUnit - used),
				IsSparse:       true,
			})
			want = append(want, unit.plain...)
		}
	}

	vol := &Volume{
		reader:          &mockReaderAt{data: disk},
		bytesPerCluster: clusterSize,
		clusterCount:    uint64(len(disk)) / uint64(clusterSize),
	}

	attr := &Attribute{
		Header: AttributeHeader{Type: AttrTypeData, Flags: AttrFlagCompressed},
		NonResident: &NonResidentAttribute{
			CompressionUnit: compressionUnit,
			DataRuns:        runs,
			AllocatedSize:   uint64(clustersPerUnit * len(units) * int(clusterSize)),
			RealSize:        uint64(len(want)),
		},
	}

	return vol, attr, want
}

// repeating returns n bytes of a pattern that compresses well, so a unit's
// encoding fits in fewer clusters than the unit holds.
func repeating(n int) []byte {
	const seed = "libntfs compression unit regression pattern "
	return []byte(strings.Repeat(seed, n/len(seed)+1))[:n]
}

// incompressible returns n bytes that LZNT1 cannot shorten, so a unit built
// from them is stored rather than compressed.
func incompressible(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 17)
	}
	return b
}

// TestReadCompressedDataUsesCompressionUnitAsShift reads a stream whose
// CompressionUnit is 4 over 512-byte clusters, so each unit is 1<<4 = 16
// clusters, or 8 KiB. Reading the field as 4 clusters decodes each unit at a
// quarter of its size and splits the run list at the wrong cluster.
func TestReadCompressedDataUsesCompressionUnitAsShift(t *testing.T) {
	const (
		clusterSize = uint32(512)
		unitShift   = uint16(4)
		unitBytes   = 8192
	)

	vol, attr, want := buildCompressedStream(t, clusterSize, unitShift, []compressedUnit{
		{plain: repeating(unitBytes)},
		{plain: incompressible(unitBytes), store: true},
		{plain: nil},
		{plain: repeating(unitBytes)},
	})

	if len(want) != 4*unitBytes {
		t.Fatalf("stream is %d bytes, want %d", len(want), 4*unitBytes)
	}

	file := &File{volume: vol, size: uint64(len(want)), dataAttr: attr}

	got := make([]byte, len(want))
	n, err := file.ReadAt(got, 0)
	if err != nil {
		t.Fatalf("ReadAt over the whole stream failed: %v", err)
	}
	if n != len(want) {
		t.Fatalf("read %d bytes, want %d", n, len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream content differs from the expected bytes at offset %d",
			firstDifference(got, want))
	}

	// Unit boundaries are where a wrong unit size shows up first, and odd
	// offsets catch an error in the within-unit copy arithmetic.
	offsets := []int64{0, 1, 511, 512, 8191, 8192, 8193, 12345, 16384, 24575}
	lengths := []int{1, 7, 512, 8192, 9000}

	for _, off := range offsets {
		for _, length := range lengths {
			if off+int64(length) > int64(len(want)) {
				continue
			}
			part := make([]byte, length)
			n, err := file.ReadAt(part, off)
			if err != nil {
				t.Fatalf("ReadAt(%d bytes at %d) failed: %v", length, off, err)
			}
			if n != length {
				t.Fatalf("ReadAt(%d bytes at %d) read %d", length, off, n)
			}
			if !bytes.Equal(part, want[off:off+int64(length)]) {
				t.Fatalf("ReadAt(%d bytes at %d) differs at byte %d of the result",
					length, off, firstDifference(part, want[off:off+int64(length)]))
			}
		}
	}
}

// TestReadCompressedDataShortFinalUnit covers a stream whose real size stops
// part way through its last unit, which is every compressed file whose size is
// not a multiple of the unit.
func TestReadCompressedDataShortFinalUnit(t *testing.T) {
	const (
		clusterSize = uint32(512)
		unitShift   = uint16(4)
		unitBytes   = 8192
		realSize    = unitBytes + 3392
	)

	vol, attr, full := buildCompressedStream(t, clusterSize, unitShift, []compressedUnit{
		{plain: repeating(unitBytes)},
		{plain: repeating(unitBytes)},
	})

	attr.NonResident.RealSize = realSize
	want := full[:realSize]

	file := &File{volume: vol, size: realSize, dataAttr: attr}

	got := make([]byte, realSize)
	n, err := file.ReadAt(got, 0)
	if err != nil {
		t.Fatalf("ReadAt over a short final unit failed: %v", err)
	}
	if n != realSize {
		t.Fatalf("read %d bytes, want %d", n, realSize)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs at offset %d", firstDifference(got, want))
	}

	// A read running past the end is short, not a decode failure.
	tail := make([]byte, 100)
	n, err = file.ReadAt(tail, realSize-50)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reading past the end returned %v, want io.EOF", err)
	}
	if n != 50 {
		t.Fatalf("reading past the end read %d bytes, want 50", n)
	}
	if !bytes.Equal(tail[:50], want[realSize-50:]) {
		t.Error("the final 50 bytes differ from the expected tail")
	}
}

// TestReadCompressedDataRejectsOutOfRangeCompressionUnit bounds the field
// before it is shifted, so a damaged value can neither overflow the shift nor
// size a huge unit buffer.
func TestReadCompressedDataRejectsOutOfRangeCompressionUnit(t *testing.T) {
	cases := []struct {
		name            string
		clusterSize     uint32
		compressionUnit uint16
	}{
		{"zero", 512, 0},
		{"above the widest shift", 512, 17},
		{"far above the widest shift", 512, 63},
		{"unit larger than the cap", 4096, 16},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := &Volume{
				reader:          &mockReaderAt{data: make([]byte, 64*1024)},
				bytesPerCluster: tc.clusterSize,
				clusterCount:    uint64(64 * 1024 / tc.clusterSize),
			}
			nr := &NonResidentAttribute{
				CompressionUnit: tc.compressionUnit,
				DataRuns:        []DataRun{{StartCluster: 1, LengthClusters: 1}},
				RealSize:        4096,
			}

			_, err := vol.readCompressedData(nr, 0, make([]byte, 16))
			if !errors.Is(err, ErrInvalidCompressionUnit) {
				t.Fatalf("error = %v, want one matching ErrInvalidCompressionUnit", err)
			}
			if !errors.Is(err, ErrCompressedData) {
				t.Errorf("error %v does not match ErrCompressedData, which it wraps", err)
			}
			if !strings.Contains(err.Error(), "compression unit") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// TestReadCompressedDataStrictRequiresStandardUnit checks the strict-mode
// check, which matches the Linux driver: it refuses any compression unit but 4.
func TestReadCompressedDataStrictRequiresStandardUnit(t *testing.T) {
	const clusterSize = uint32(512)

	build := func(strict bool, unitShift uint16) (*File, []byte) {
		vol, attr, want := buildCompressedStream(t, clusterSize, unitShift, []compressedUnit{
			{plain: repeating(int(clusterSize) << unitShift)},
		})
		vol.strict = strict
		return &File{volume: vol, size: uint64(len(want)), dataAttr: attr}, want
	}

	t.Run("non-standard unit refused", func(t *testing.T) {
		file, want := build(true, 3)
		if _, err := file.ReadAt(make([]byte, len(want)), 0); !errors.Is(err, ErrStrictViolation) {
			t.Fatalf("error = %v, want one matching ErrStrictViolation", err)
		}
	})

	t.Run("standard unit accepted", func(t *testing.T) {
		file, want := build(true, StandardCompressionUnit)
		got := make([]byte, len(want))
		if _, err := file.ReadAt(got, 0); err != nil {
			t.Fatalf("strict mode refused a standard compression unit: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("content differs at offset %d", firstDifference(got, want))
		}
	})

	t.Run("tolerant mode accepts a non-standard unit", func(t *testing.T) {
		file, want := build(false, 3)
		got := make([]byte, len(want))
		if _, err := file.ReadAt(got, 0); err != nil {
			t.Fatalf("tolerant mode refused compression unit 3: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("content differs at offset %d", firstDifference(got, want))
		}
	})
}

// TestLZNT1CompressRoundTrip checks the test encoder against the decoder under
// test, so a failure elsewhere in this file points at the unit geometry rather
// than at the encoder.
func TestLZNT1CompressRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"one byte":              {0x41},
		"short run":             bytes.Repeat([]byte{0x41}, 7),
		"long run":              bytes.Repeat([]byte{0x41}, 9000),
		"repeating":             repeating(10000),
		"incompressible":        incompressible(5000),
		"exactly one sub-block": repeating(ntfsSubBlockUncompressedDataSize),
	}

	for name, plain := range cases {
		t.Run(name, func(t *testing.T) {
			encoded := lznt1Compress(plain)
			got, err := ntfsDecompressCompressionUnit(encoded, len(plain))
			if err != nil {
				t.Fatalf("decoding %d encoded bytes failed: %v", len(encoded), err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("round trip differs at offset %d", firstDifference(got, plain))
			}
		})
	}
}

// firstDifference returns the index of the first differing byte, or the length
// of the shorter slice when one is a prefix of the other.
func firstDifference(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestCompressionUnitSupportAgreesWithRead checks that ReadSupport and Streams
// call a compressed stream unreadable exactly when ReadAt refuses it, and that
// the strict refusal is a *StrictError like every other strict check.
func TestCompressionUnitSupportAgreesWithRead(t *testing.T) {
	const clusterSize = uint32(512)

	build := func(strict bool, unitShift uint16) *File {
		vol, attr, want := buildCompressedStream(t, clusterSize, 3, []compressedUnit{
			{plain: repeating(int(clusterSize) << 3)},
		})
		vol.strict = strict
		attr.NonResident.CompressionUnit = unitShift
		return &File{
			volume: vol, entryNum: 30, size: uint64(len(want)), dataAttr: attr,
			entry: &MFTEntry{Attributes: []*Attribute{attr}},
		}
	}

	cases := []struct {
		name     string
		strict   bool
		shift    uint16
		readable bool
		wantErr  error
	}{
		{"tolerant, non-standard unit", false, 3, true, nil},
		{"tolerant, unit out of range", false, 17, false, ErrInvalidCompressionUnit},
		{"strict, non-standard unit", true, 3, false, ErrStrictViolation},
		{"strict, standard unit", true, StandardCompressionUnit, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := build(tc.strict, tc.shift)

			support := file.ReadSupport()
			if support.Readable != tc.readable {
				t.Fatalf("ReadSupport().Readable = %v, want %v (%v)", support.Readable, tc.readable, support.BlockingError)
			}
			streams := file.Streams()
			if len(streams) != 1 || streams[0].Support.Readable != tc.readable {
				t.Fatalf("Streams() = %+v, want one stream with Readable %v", streams, tc.readable)
			}
			if tc.readable {
				return
			}
			if !errors.Is(support.BlockingError, tc.wantErr) || !errors.Is(streams[0].Support.BlockingError, tc.wantErr) {
				t.Fatalf("BlockingError = %v / %v, want one matching %v",
					support.BlockingError, streams[0].Support.BlockingError, tc.wantErr)
			}
			if _, err := file.ReadAt(make([]byte, 16), 0); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ReadAt = %v, want one matching %v", err, tc.wantErr)
			}
			if tc.strict {
				_, err := file.ReadAt(make([]byte, 16), 0)
				var se *StrictError
				if !errors.As(err, &se) || se.Check != "compression-unit" || se.Record != 30 ||
					!errors.Is(err, ErrInvalidCompressionUnit) {
					t.Fatalf("strict ReadAt = %v, want a compression-unit *StrictError on record 30", err)
				}
			}
		})
	}
}
