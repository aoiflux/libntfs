package libntfs

import (
	"math"
	"testing"
	"time"
)

// FILETIME counts 100 ns ticks from 1601-01-01 UTC and covers far more than
// the Unix range. Every value an NTFS volume can hold must survive the trip
// through time.Time, including those before 1970 and after 2262 that
// UnixNano cannot represent.
func TestFiletimeFullRange(t *testing.T) {
	cases := []struct {
		name string
		ft   uint64
		want time.Time
	}{
		{"one tick after 1601", 1, time.Date(1601, 1, 1, 0, 0, 0, 100, time.UTC)},
		{"1900-01-01", 0x014F373BFDE04000, time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"last tick before Unix epoch", NTFSTimeOffset - 1, time.Date(1969, 12, 31, 23, 59, 59, 999999900, time.UTC)},
		{"Unix epoch", NTFSTimeOffset, time.Unix(0, 0).UTC()},
		{"2262-04-12", NTFSTimeOffset + uint64(time.Date(2262, 4, 12, 0, 0, 0, 0, time.UTC).Unix())*10000000, time.Date(2262, 4, 12, 0, 0, 0, 0, time.UTC)},
		{"after UnixNano range", NTFSTimeOffset + uint64(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC).Unix())*10000000, time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"max int64 FILETIME", 0x7FFFFFFFFFFFFFFF, time.Date(30828, 9, 14, 2, 48, 5, 477580700, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FiletimeToTime(tc.ft); !got.Equal(tc.want) {
				t.Fatalf("FiletimeToTime(%#x) = %v, want %v", tc.ft, got, tc.want)
			}
			if got := NTFSTimeToTime(tc.ft); !got.Equal(tc.want) {
				t.Fatalf("NTFSTimeToTime(%#x) = %v, want %v", tc.ft, got, tc.want)
			}
			if got := TimeToFiletime(tc.want); got != tc.ft {
				t.Fatalf("TimeToFiletime(%v) = %#x, want %#x", tc.want, got, tc.ft)
			}
			if got := TimeToNTFSTime(tc.want); got != tc.ft {
				t.Fatalf("TimeToNTFSTime(%v) = %#x, want %#x", tc.want, got, tc.ft)
			}
		})
	}
}

func TestFiletimeZeroAndClamp(t *testing.T) {
	if got := FiletimeToTime(0); !got.Equal(time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("FiletimeToTime(0) = %v, want 1601-01-01", got)
	}
	if got := NTFSTimeToTime(0); !got.IsZero() {
		t.Fatalf("NTFSTimeToTime(0) = %v, want zero time (not set)", got)
	}
	if got := TimeToNTFSTime(time.Time{}); got != 0 {
		t.Fatalf("TimeToNTFSTime(zero) = %#x, want 0", got)
	}
	if got := TimeToFiletime(time.Date(1600, 12, 31, 23, 59, 59, 0, time.UTC)); got != 0 {
		t.Fatalf("TimeToFiletime(before 1601) = %#x, want 0", got)
	}
	if got := TimeToFiletime(time.Date(70000, 1, 1, 0, 0, 0, 0, time.UTC)); got != math.MaxUint64 {
		t.Fatalf("TimeToFiletime(past range) = %#x, want MaxUint64", got)
	}
	// The top of the uint64 range is a valid FILETIME and must round-trip.
	top := FiletimeToTime(math.MaxUint64)
	if got := TimeToFiletime(top); got != math.MaxUint64 {
		t.Fatalf("TimeToFiletime(FiletimeToTime(MaxUint64)) = %#x", got)
	}
}
