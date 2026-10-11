package libntfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// defaultUpCaseSHA256 pins DefaultUpCase to the table Windows 8+ and mkntfs
// write. It was taken from fsagen's upcaseBytes, which fsagen byte-matches
// against mkntfs output.
const defaultUpCaseSHA256 = "41c26bc7a12bdaeb26025c93118697c7e3ef81ee048b00fe5cce2a472e0e0742"

func TestDefaultUpCasePinned(t *testing.T) {
	b := DefaultUpCase().Bytes()
	if len(b) != UpCaseSize {
		t.Fatalf("DefaultUpCase is %d bytes, want %d", len(b), UpCaseSize)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != defaultUpCaseSHA256 {
		t.Fatalf("DefaultUpCase sha256 = %s, want %s", got, defaultUpCaseSHA256)
	}
}

func TestDefaultUpCaseIsACopy(t *testing.T) {
	u := DefaultUpCase()
	u['a'] = 'a'
	if DefaultUpCase().ToUpper('a') != 'A' {
		t.Fatal("modifying a returned table changed the default")
	}
}

func TestUpCaseCompare(t *testing.T) {
	u := DefaultUpCase()
	cases := []struct {
		a, b string
		want int
	}{
		{"abc", "ABC", 0},
		{"a", "B", -1},
		{"ab", "abc", -1},
		{"abc", "ab", 1},
		{"", "", 0},
		// '_' (0x5F) sorts after every upper-cased letter, not between the
		// cases as it would in a plain byte order.
		{"_x", "Zx", 1},
		{"_x", "zx", 1},
		{"é", "É", 0},          // Latin-1
		{"ÿ", "Ÿ", 0},          // 0xFF maps to 0x178
		{"ß", "SS", 1},         // ß has no single upper case and stays 0xDF
		{"σ", "Σ", 0},          // Greek
		{"ς", "Σ", 1},          // final sigma is not mapped
		{"я", "Я", 0},          // Cyrillic
		{"ё", "Ё", 0},          // 0x451 maps to 0x401
		{"ｚ", "Ｚ", 0},          // Fullwidth
		{"\U0001F600", "a", 1}, // surrogates compare as units
	}
	for _, tc := range cases {
		if got := u.Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseUpCase(t *testing.T) {
	u := DefaultUpCase()
	back, err := ParseUpCase(u.Bytes())
	if err != nil {
		t.Fatalf("ParseUpCase: %v", err)
	}
	if back.Compare("x", "X") != 0 || len(back) != len(u) {
		t.Fatal("ParseUpCase(Bytes()) did not round trip")
	}
	if _, err := ParseUpCase(make([]byte, 100)); !errors.Is(err, ErrInvalidUpCase) {
		t.Fatalf("ParseUpCase(short) = %v, want ErrInvalidUpCase", err)
	}
}

func TestCollateULongs(t *testing.T) {
	le := func(words ...uint32) []byte {
		var b []byte
		for _, w := range words {
			b = appendUint32LE(b, w)
		}
		return b
	}
	cases := []struct {
		a, b []byte
		want int
	}{
		{le(1, 2), le(1, 3), -1},
		{le(2), le(1, 9), 1},
		{le(1), le(1, 0), -1},
		{le(0xA000000C, 30, 0), le(0xA000000C, 30, 0), 0},
		{le(0x80000000), le(0x7FFFFFFF), 1}, // unsigned
	}
	for _, tc := range cases {
		if got := CollateULongs(tc.a, tc.b); got != tc.want {
			t.Errorf("CollateULongs(% x, % x) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if CollateULong(le(5), le(0x100)) != -1 || CollateULong(le(7), le(7)) != 0 || CollateULong([]byte{1}, le(0)) != -1 {
		t.Error("CollateULong ordering is wrong")
	}
}

// upcaseImage builds an image whose $UpCase (entry 10) holds table in
// clusters 32 to 63.
func upcaseImage(t *testing.T, table []byte, clusters uint64) []byte {
	t.Helper()
	g := defaultTestGeometry
	g.totalClusters = 64
	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "$UpCase",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildNonResidentDataAttr(t, 2, 32, clusters, 4096)},
	})
	image := buildTestNTFSImageGeometry(t, g, map[uint64][]byte{MFTEntryUpCase: record})
	copy(image[32*4096:], table)
	return image
}

func TestVolumeUpCaseReadsTheVolumesTable(t *testing.T) {
	table := DefaultUpCase()
	table[0xE4] = 0xE4 // 'ä' upper-cases to itself on this volume
	v, err := Open(&mockReaderAt{data: upcaseImage(t, table.Bytes(), 32)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer v.Close()

	u, err := v.UpCase()
	if err != nil {
		t.Fatalf("UpCase: %v", err)
	}
	if u.ToUpper(0xE4) != 0xE4 || u.ToUpper('q') != 'Q' {
		t.Fatalf("UpCase did not return the volume's table")
	}
	again, _ := v.UpCase()
	if &again[0] != &u[0] {
		t.Fatal("UpCase was not cached")
	}
}

func TestVolumeUpCaseRejectsShortTable(t *testing.T) {
	v, err := Open(&mockReaderAt{data: upcaseImage(t, nil, 1)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer v.Close()
	if _, err := v.UpCase(); !errors.Is(err, ErrInvalidUpCase) {
		t.Fatalf("UpCase = %v, want ErrInvalidUpCase", err)
	}
}
