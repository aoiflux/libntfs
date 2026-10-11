package libntfs

import (
	"errors"
	"testing"
)

// buildNameBuffer assembles the substitute/print name pair shared by the symlink
// and mount point layouts. headerSize is where the path buffer starts, which
// differs because a symlink carries a Flags word that a mount point does not.
func buildNameBuffer(headerSize int, substitute, print string, flags uint32) []byte {
	substituteBytes := utf16LEFromString(substitute)
	printBytes := utf16LEFromString(print)

	buf := make([]byte, headerSize+len(substituteBytes)+len(printBytes))

	WriteUint16LE(buf, 0, 0)                            // Substitute name offset
	WriteUint16LE(buf, 2, uint16(len(substituteBytes))) // Substitute name length
	WriteUint16LE(buf, 4, uint16(len(substituteBytes))) // Print name offset
	WriteUint16LE(buf, 6, uint16(len(printBytes)))      // Print name length

	if headerSize >= 12 {
		WriteUint32LE(buf, 8, flags)
	}

	copy(buf[headerSize:], substituteBytes)
	copy(buf[headerSize+len(substituteBytes):], printBytes)

	return buf
}

func buildSymlinkData(substitute, print string, relative bool) []byte {
	var flags uint32
	if relative {
		flags = SymlinkFlagRelative
	}
	return buildNameBuffer(12, substitute, print, flags)
}

func buildMountPointData(substitute, print string) []byte {
	return buildNameBuffer(8, substitute, print, 0)
}

// buildReparseValue assembles a Microsoft-owned reparse buffer, which carries no
// owner GUID.
func buildReparseValue(tag uint32, data []byte) []byte {
	buf := make([]byte, ReparseHeaderSize+len(data))
	WriteUint32LE(buf, 0, tag)
	WriteUint16LE(buf, 4, uint16(len(data)))
	copy(buf[ReparseHeaderSize:], data)
	return buf
}

// buildReparseGUIDValue assembles a third-party reparse buffer, whose data
// follows a 16-byte owner GUID.
func buildReparseGUIDValue(tag uint32, guid [16]byte, data []byte) []byte {
	buf := make([]byte, ReparseHeaderSize+reparseGUIDSize+len(data))
	WriteUint32LE(buf, 0, tag)
	WriteUint16LE(buf, 4, uint16(len(data)))
	copy(buf[ReparseHeaderSize:], guid[:])
	copy(buf[ReparseHeaderSize+reparseGUIDSize:], data)
	return buf
}

func TestParseReparsePointSymlink(t *testing.T) {
	value := buildReparseValue(ReparsePointSymlink,
		buildSymlinkData(`\??\C:\target.txt`, `C:\target.txt`, false))

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	if reparse.ReparseType != ReparsePointSymlink {
		t.Errorf("expected the symlink tag, got 0x%08X", reparse.ReparseType)
	}
	if !reparse.IsMicrosoft() {
		t.Error("expected the symlink tag to be Microsoft-owned")
	}
	if reparse.TagName() != "SYMLINK" {
		t.Errorf("expected TagName SYMLINK, got %q", reparse.TagName())
	}

	substitute, print, ok := reparse.Target()
	if !ok {
		t.Fatal("expected a symlink to name a target")
	}
	if substitute != `\??\C:\target.txt` {
		t.Errorf("unexpected substitute name %q", substitute)
	}
	if print != `C:\target.txt` {
		t.Errorf("unexpected print name %q", print)
	}

	if reparse.IsRelative() {
		t.Error("expected an absolute symlink")
	}
}

func TestParseReparsePointRelativeSymlink(t *testing.T) {
	value := buildReparseValue(ReparsePointSymlink,
		buildSymlinkData(`sibling.txt`, `sibling.txt`, true))

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	if !reparse.IsRelative() {
		t.Error("expected a relative symlink")
	}
}

// TestParseReparsePointMountPoint covers the junction layout, whose path buffer
// starts four bytes earlier than a symlink's because it has no Flags word.
func TestParseReparsePointMountPoint(t *testing.T) {
	value := buildReparseValue(ReparsePointMountPoint,
		buildMountPointData(`\??\D:\data`, `D:\data`))

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	substitute, print, ok := reparse.Target()
	if !ok {
		t.Fatal("expected a mount point to name a target")
	}
	if substitute != `\??\D:\data` {
		t.Errorf("unexpected substitute name %q", substitute)
	}
	if print != `D:\data` {
		t.Errorf("unexpected print name %q", print)
	}

	if reparse.IsRelative() {
		t.Error("expected IsRelative to be meaningless for a mount point")
	}
}

// TestParseReparsePointWSLSymlink covers WSL's layout, a UTF-8 path after a
// version word rather than the UTF-16 name buffer every other link tag uses.
func TestParseReparsePointWSLSymlink(t *testing.T) {
	data := append([]byte{2, 0, 0, 0}, []byte("/usr/bin/bash")...)
	value := buildReparseValue(ReparsePointLXSymlink, data)

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	substitute, print, ok := reparse.Target()
	if !ok {
		t.Fatal("expected a WSL symlink to name a target")
	}
	if substitute != "/usr/bin/bash" || print != "/usr/bin/bash" {
		t.Errorf("unexpected target %q / %q", substitute, print)
	}
	if reparse.TagName() != "LX_SYMLINK" {
		t.Errorf("expected TagName LX_SYMLINK, got %q", reparse.TagName())
	}
}

// TestParseReparsePointThirdParty covers the buffer shape that carries an owner
// GUID, which must be stripped from Data rather than read as content.
func TestParseReparsePointThirdParty(t *testing.T) {
	guid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	value := buildReparseGUIDValue(0x00000001, guid, []byte("vendor payload"))

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	if reparse.IsMicrosoft() {
		t.Error("expected a third-party tag")
	}
	if reparse.GUID != guid {
		t.Errorf("expected the owner GUID preserved, got %v", reparse.GUID)
	}
	if string(reparse.Data) != "vendor payload" {
		t.Errorf("expected the GUID stripped from Data, got %q", reparse.Data)
	}

	if _, _, ok := reparse.Target(); ok {
		t.Error("expected an unknown tag to name no target")
	}
}

func TestParseReparsePointThirdPartyRejectsMissingGUID(t *testing.T) {
	buf := make([]byte, ReparseHeaderSize+4)
	WriteUint32LE(buf, 0, 0x00000001)
	WriteUint16LE(buf, 4, 4)

	if _, err := ParseReparsePoint(buf); !errors.Is(err, ErrInvalidReparsePoint) {
		t.Errorf("expected ErrInvalidReparsePoint, got %v", err)
	}
}

func TestParseReparsePointRejectsShortHeader(t *testing.T) {
	if _, err := ParseReparsePoint([]byte{1, 2, 3}); !errors.Is(err, ErrInvalidReparsePoint) {
		t.Errorf("expected ErrInvalidReparsePoint, got %v", err)
	}
}

// TestParseReparsePointClampsOverlongDataLength covers a header claiming more
// data than the attribute holds, which must yield the bytes present rather than
// read past them.
func TestParseReparsePointClampsOverlongDataLength(t *testing.T) {
	value := buildReparseValue(ReparsePointWOF, []byte("short"))
	WriteUint16LE(value, 4, 4096)

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	if string(reparse.Data) != "short" {
		t.Errorf("expected the data clamped to what is present, got %q", reparse.Data)
	}
	if reparse.DataLength != 4096 {
		t.Errorf("expected the recorded length preserved, got %d", reparse.DataLength)
	}
}

func TestReparsePointTagNameUnknown(t *testing.T) {
	value := buildReparseValue(0xA000BEEF, []byte{})

	reparse, err := ParseReparsePoint(value)
	if err != nil {
		t.Fatalf("ParseReparsePoint failed: %v", err)
	}

	if reparse.TagName() != "0xA000BEEF" {
		t.Errorf("expected the raw tag rendered, got %q", reparse.TagName())
	}
}

func TestReparsePointNilReceiver(t *testing.T) {
	var reparse *ReparsePoint

	if reparse.IsMicrosoft() || reparse.IsRelative() || reparse.TagName() != "" {
		t.Error("expected a nil reparse point to report nothing")
	}
	if _, _, ok := reparse.Target(); ok {
		t.Error("expected a nil reparse point to name no target")
	}
}

func TestFileReparsePoint(t *testing.T) {
	value := buildReparseValue(ReparsePointMountPoint,
		buildMountPointData(`\??\Volume{deadbeef}\`, ""))

	_, f := openFragmentTestFile(t, "junction",
		buildResidentAttr(AttrTypeReparsePoint, 2, value))

	if !f.IsReparsePoint() {
		t.Error("expected IsReparsePoint to report the attribute")
	}

	reparse, err := f.ReparsePoint()
	if err != nil {
		t.Fatalf("ReparsePoint failed: %v", err)
	}
	if reparse == nil {
		t.Fatal("expected a reparse point")
	}

	substitute, print, ok := reparse.Target()
	if !ok {
		t.Fatal("expected a junction to name a target")
	}
	if substitute != `\??\Volume{deadbeef}\` {
		t.Errorf("unexpected substitute name %q", substitute)
	}
	// A junction routinely carries no print name, which must not fail the parse.
	if print != "" {
		t.Errorf("expected no print name, got %q", print)
	}
}

func TestFileReparsePointAbsent(t *testing.T) {
	_, f := openFragmentTestFile(t, "plain.txt",
		buildResidentAttr(AttrTypeData, 2, []byte("no reparse")))

	if f.IsReparsePoint() {
		t.Error("expected no $REPARSE_POINT attribute")
	}

	reparse, err := f.ReparsePoint()
	if err != nil {
		t.Fatalf("expected no error for a file without a reparse point, got %v", err)
	}
	if reparse != nil {
		t.Errorf("expected nil, got %+v", reparse)
	}
}
