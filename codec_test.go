package libntfs

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func testFixupRecord(size int, seed uint64) []byte {
	buf := make([]byte, size)
	r := rand.New(rand.NewPCG(seed, seed))
	for i := range buf {
		buf[i] = byte(r.Uint32())
	}
	copy(buf, "FILE")
	WriteUint16LE(buf, 4, MFTEntryHeaderSize)
	WriteUint16LE(buf, 6, uint16(size/UpdateSequenceStride+1))
	return buf
}

func TestFixupRoundTrip(t *testing.T) {
	for _, size := range []int{512, 1024, 4096} {
		orig := testFixupRecord(size, uint64(size))
		buf := slices.Clone(orig)
		if err := ProtectFixup(buf, 0x0102); err != nil {
			t.Fatalf("ProtectFixup(%d): %v", size, err)
		}
		for end := UpdateSequenceStride - 2; end < size; end += UpdateSequenceStride {
			if got := ReadUint16LE(buf, end); got != 0x0102 {
				t.Fatalf("size %d: sector end %d = %#x, want the USN", size, end, got)
			}
		}
		if err := ApplyFixup(buf); err != nil {
			t.Fatalf("ApplyFixup(%d): %v", size, err)
		}
		// Outside the array the record is restored; the array holds the USN
		// and then each sector's original last two bytes.
		usaEnd := MFTEntryHeaderSize + 2*(size/UpdateSequenceStride+1)
		if !bytes.Equal(buf[:MFTEntryHeaderSize], orig[:MFTEntryHeaderSize]) || !bytes.Equal(buf[usaEnd:], orig[usaEnd:]) {
			t.Fatalf("size %d: ApplyFixup did not restore the record", size)
		}
		for i := 0; i < size/UpdateSequenceStride; i++ {
			if got, want := ReadUint16LE(buf, MFTEntryHeaderSize+2+2*i), ReadUint16LE(orig, (i+1)*UpdateSequenceStride-2); got != want {
				t.Fatalf("size %d: array slot %d = %#x, want %#x", size, i, got, want)
			}
		}
	}
}

func TestApplyFixupDetectsTornSector(t *testing.T) {
	buf := testFixupRecord(1024, 1)
	if err := ProtectFixup(buf, 9); err != nil {
		t.Fatal(err)
	}
	WriteUint16LE(buf, 2*UpdateSequenceStride-2, 8)
	if err := ApplyFixup(buf); !errors.Is(err, ErrUpdateSequence) {
		t.Fatalf("ApplyFixup on a torn sector = %v, want ErrUpdateSequence", err)
	}
}

func TestFixupRejectsMalformedArray(t *testing.T) {
	cases := map[string]func([]byte){
		"zero count":         func(b []byte) { WriteUint16LE(b, 6, 0) },
		"too many sectors":   func(b []byte) { WriteUint16LE(b, 6, 4) },
		"array past the end": func(b []byte) { WriteUint16LE(b, 4, 1020) },
		"array in header":    func(b []byte) { WriteUint16LE(b, 4, 4) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			buf := testFixupRecord(1024, 2)
			mutate(buf)
			if err := ApplyFixup(slices.Clone(buf)); !errors.Is(err, ErrUpdateSequence) {
				t.Fatalf("ApplyFixup = %v, want ErrUpdateSequence", err)
			}
			if err := ProtectFixup(slices.Clone(buf), 1); !errors.Is(err, ErrUpdateSequence) {
				t.Fatalf("ProtectFixup = %v, want ErrUpdateSequence", err)
			}
		})
	}
	if err := ApplyFixup([]byte{1, 2, 3}); !errors.Is(err, ErrUpdateSequence) {
		t.Fatalf("ApplyFixup(short) = %v, want ErrUpdateSequence", err)
	}
}

// The runlist vectors are what Windows, mkntfs and fsagen write: each field
// in the fewest bytes that hold it as a signed value.
func TestEncodeRunlistVectors(t *testing.T) {
	cases := []struct {
		name string
		runs []DataRun
		want []byte
	}{
		{"empty", nil, []byte{0x00}},
		{"one run", []DataRun{{LengthClusters: 8, StartCluster: 4}},
			[]byte{0x11, 0x08, 0x04, 0x00}},
		{"0x80 needs a sign byte", []DataRun{{LengthClusters: 0x80, StartCluster: 0x100}},
			[]byte{0x22, 0x80, 0x00, 0x00, 0x01, 0x00}},
		{"sparse and backwards", []DataRun{
			{LengthClusters: 16, StartCluster: 100},
			{LengthClusters: 4, IsSparse: true},
			{LengthClusters: 2, StartCluster: 50},
		}, []byte{0x11, 0x10, 0x64, 0x01, 0x04, 0x11, 0x02, 0xCE, 0x00}},
		{"wide", []DataRun{{LengthClusters: 1 << 40, StartCluster: 0x7FFFFFFFFF}},
			[]byte{0x56, 0, 0, 0, 0, 0, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F, 0x00}},
		{"length above MaxInt64", []DataRun{{LengthClusters: math.MaxUint64, IsSparse: true}},
			[]byte{0x08, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeRunlist(tc.runs)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("EncodeRunlist = % x, want % x", got, tc.want)
			}
			back, err := DecodeRunlist(got)
			if err != nil {
				t.Fatalf("DecodeRunlist: %v", err)
			}
			if !slices.Equal(back, normalizeRuns(tc.runs)) {
				t.Fatalf("round trip = %+v, want %+v", back, tc.runs)
			}
		})
	}
}

// normalizeRuns clears StartCluster on sparse runs, which the encoding does
// not carry.
func normalizeRuns(runs []DataRun) []DataRun {
	out := slices.Clone(runs)
	for i := range out {
		if out[i].IsSparse {
			out[i].StartCluster = 0
		}
	}
	return out
}

func TestRunlistRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for range 2000 {
		runs := make([]DataRun, r.IntN(8))
		for i := range runs {
			runs[i].LengthClusters = r.Uint64N(1 << uint(1+r.IntN(62)))
			if r.IntN(4) == 0 {
				runs[i].IsSparse = true
				continue
			}
			runs[i].StartCluster = r.Int64N(1 << uint(1+r.IntN(62)))
		}
		back, err := DecodeRunlist(EncodeRunlist(runs))
		if err != nil {
			t.Fatalf("DecodeRunlist(%+v): %v", runs, err)
		}
		if !slices.Equal(back, normalizeRuns(runs)) {
			t.Fatalf("round trip of %+v = %+v", runs, back)
		}
	}
}
