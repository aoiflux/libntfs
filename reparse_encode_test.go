package libntfs

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The vectors are fsagen's internal/reparse output for the same targets,
// which fsagen pins against the buffers Windows and libntfs-3g write.
func TestEncodeSymlinkReparseVectors(t *testing.T) {
	cases := []struct {
		name              string
		substitute, print string
		relative          bool
		wantHex           string
	}{
		{"absolute", `\??\C:\Windows\System32`, `C:\Windows\System32`, false,
			"0c0000a06000000026002e00000026000000000043003a005c00570069006e0064006f00770073005c00530079007300740065006d00330032005c003f003f005c0043003a005c00570069006e0064006f00770073005c00530079007300740065006d0033003200"},
		{"UNC", `\??\UNC\server\share\dir`, `\\server\share\dir`, false,
			"0c0000a0600000002400300000002400000000005c005c007300650072007600650072005c00730068006100720065005c006400690072005c003f003f005c0055004e0043005c007300650072007600650072005c00730068006100720065005c00640069007200"},
		{"relative", `..\sibling\file.txt`, `..\sibling\file.txt`, true,
			"0c0000a0580000002600260000002600010000002e002e005c007300690062006c0069006e0067005c00660069006c0065002e007400780074002e002e005c007300690062006c0069006e0067005c00660069006c0065002e00740078007400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeSymlinkReparse(tc.substitute, tc.print, tc.relative)
			if err != nil {
				t.Fatalf("EncodeSymlinkReparse: %v", err)
			}
			want, _ := hex.DecodeString(tc.wantHex)
			if !bytes.Equal(got, want) {
				t.Fatalf("EncodeSymlinkReparse =\n%x\nwant\n%x", got, want)
			}

			rp, err := ParseReparsePoint(got)
			if err != nil {
				t.Fatalf("ParseReparsePoint: %v", err)
			}
			sub, prn, ok := rp.Target()
			if !ok || sub != tc.substitute || prn != tc.print {
				t.Fatalf("Target = %q, %q, %v", sub, prn, ok)
			}
			if rp.IsRelative() != tc.relative {
				t.Fatalf("IsRelative = %v, want %v", rp.IsRelative(), tc.relative)
			}
		})
	}
}

func TestEncodeMountPointReparseVector(t *testing.T) {
	got, err := EncodeMountPointReparse(`\??\C:\target`, `C:\target`)
	if err != nil {
		t.Fatalf("EncodeMountPointReparse: %v", err)
	}
	want, _ := hex.DecodeString("030000a03800000000001a001c0012005c003f003f005c0043003a005c00740061007200670065007400000043003a005c007400610072006700650074000000")
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeMountPointReparse =\n%x\nwant\n%x", got, want)
	}

	rp, err := ParseReparsePoint(got)
	if err != nil {
		t.Fatalf("ParseReparsePoint: %v", err)
	}
	sub, prn, ok := rp.Target()
	if !ok || sub != `\??\C:\target` || prn != `C:\target` {
		t.Fatalf("Target = %q, %q, %v", sub, prn, ok)
	}
}

func TestEncodeReparseRejectsOversizedBuffer(t *testing.T) {
	long := strings.Repeat("x", 8<<10)
	if _, err := EncodeSymlinkReparse(long, long, true); !errors.Is(err, ErrInvalidReparsePoint) {
		t.Fatalf("EncodeSymlinkReparse(16 KiB names) = %v, want ErrInvalidReparsePoint", err)
	}
	if _, err := EncodeMountPointReparse(long, ""); !errors.Is(err, ErrInvalidReparsePoint) {
		t.Fatalf("EncodeMountPointReparse(16 KiB name) = %v, want ErrInvalidReparsePoint", err)
	}
}
