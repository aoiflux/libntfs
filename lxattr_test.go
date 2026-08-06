package libntfs

import (
	"errors"
	"testing"
	"time"
)

// buildLXATTRBValue assembles a WSL2 LXATTRB extended attribute value.
func buildLXATTRBValue(mode, uid, gid, rdev uint32, atime, mtime, ctime time.Time) []byte {
	buf := make([]byte, linuxAttributesSize)

	WriteUint16LE(buf, 0x00, 0) // Flags
	WriteUint16LE(buf, 0x02, 1) // Version
	WriteUint32LE(buf, 0x04, mode)
	WriteUint32LE(buf, 0x08, uid)
	WriteUint32LE(buf, 0x0C, gid)
	WriteUint32LE(buf, 0x10, rdev)
	WriteUint32LE(buf, 0x14, uint32(atime.Nanosecond()))
	WriteUint32LE(buf, 0x18, uint32(mtime.Nanosecond()))
	WriteUint32LE(buf, 0x1C, uint32(ctime.Nanosecond()))
	WriteUint64LE(buf, 0x20, uint64(atime.Unix()))
	WriteUint64LE(buf, 0x28, uint64(mtime.Unix()))
	WriteUint64LE(buf, 0x30, uint64(ctime.Unix()))

	return buf
}

// buildLXValue assembles a WSL1 single-value attribute.
func buildLXValue(value uint32) []byte {
	buf := make([]byte, 4)
	WriteUint32LE(buf, 0, value)
	return buf
}

func TestParseLinuxAttributes(t *testing.T) {
	atime := time.Unix(1_700_000_000, 123).UTC()
	mtime := time.Unix(1_700_000_500, 0).UTC()
	ctime := time.Unix(1_700_000_900, 456).UTC()

	attrs, err := ParseLinuxAttributes(
		buildLXATTRBValue(0o100644, 1000, 1000, 0, atime, mtime, ctime))
	if err != nil {
		t.Fatalf("ParseLinuxAttributes failed: %v", err)
	}

	if attrs.Version != 1 {
		t.Errorf("expected version 1, got %d", attrs.Version)
	}
	if attrs.UID != 1000 || attrs.GID != 1000 {
		t.Errorf("expected owner 1000:1000, got %d:%d", attrs.UID, attrs.GID)
	}
	if attrs.Permissions() != 0o644 {
		t.Errorf("expected permissions 0644, got %#o", attrs.Permissions())
	}
	if attrs.FileType() != "regular" {
		t.Errorf("expected a regular file, got %q", attrs.FileType())
	}
	if !attrs.AccessTime.Equal(atime) || !attrs.ModifyTime.Equal(mtime) || !attrs.ChangeTime.Equal(ctime) {
		t.Errorf("unexpected timestamps: %v %v %v",
			attrs.AccessTime, attrs.ModifyTime, attrs.ChangeTime)
	}
	if attrs.FromLegacyAttributes {
		t.Error("expected LXATTRB rather than the WSL1 attributes")
	}
}

// TestLinuxAttributesZeroTimestamp covers a POSIX zero, which is the epoch and
// not a real timestamp. Reporting 1970 in a forensic timeline is worse than
// reporting nothing.
func TestLinuxAttributesZeroTimestamp(t *testing.T) {
	attrs, err := ParseLinuxAttributes(make([]byte, linuxAttributesSize))
	if err != nil {
		t.Fatalf("ParseLinuxAttributes failed: %v", err)
	}

	if !attrs.AccessTime.IsZero() || !attrs.ModifyTime.IsZero() || !attrs.ChangeTime.IsZero() {
		t.Errorf("expected zero timestamps, got %v %v %v",
			attrs.AccessTime, attrs.ModifyTime, attrs.ChangeTime)
	}
}

func TestParseLinuxAttributesRejectsShortValue(t *testing.T) {
	_, err := ParseLinuxAttributes(make([]byte, linuxAttributesSize-1))
	if !errors.Is(err, ErrInvalidExtendedAttribute) {
		t.Errorf("expected ErrInvalidExtendedAttribute, got %v", err)
	}
}

func TestLinuxAttributesModeString(t *testing.T) {
	tests := []struct {
		name string
		mode uint32
		want string
	}{
		{name: "regular 644", mode: 0o100644, want: "-rw-r--r--"},
		{name: "directory 755", mode: 0o040755, want: "drwxr-xr-x"},
		{name: "symlink 777", mode: 0o120777, want: "lrwxrwxrwx"},
		{name: "fifo", mode: 0o010644, want: "prw-r--r--"},
		{name: "character device", mode: 0o020666, want: "crw-rw-rw-"},
		{name: "block device", mode: 0o060660, want: "brw-rw----"},
		{name: "socket", mode: 0o140755, want: "srwxr-xr-x"},
		{name: "setuid with execute", mode: 0o104755, want: "-rwsr-xr-x"},
		{name: "setuid without execute", mode: 0o104644, want: "-rwSr--r--"},
		{name: "setgid with execute", mode: 0o102755, want: "-rwxr-sr-x"},
		{name: "sticky directory", mode: 0o041777, want: "drwxrwxrwt"},
		{name: "sticky without execute", mode: 0o041666, want: "drw-rw-rwT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := &LinuxAttributes{Mode: tt.mode}
			if got := attrs.ModeString(); got != tt.want {
				t.Errorf("ModeString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLinuxAttributesNilReceiver(t *testing.T) {
	var attrs *LinuxAttributes

	if attrs.ModeString() != "" || attrs.FileType() != "" || attrs.String() != "" {
		t.Error("expected nil attributes to report nothing")
	}
	if attrs.Permissions() != 0 || attrs.IsSymlink() {
		t.Error("expected nil attributes to report no permissions")
	}
}

func TestFileLinuxAttributes(t *testing.T) {
	mtime := time.Unix(1_700_000_500, 0).UTC()
	value := buildEAValue(buildEAEntry("$LXATTRB",
		buildLXATTRBValue(0o100755, 1000, 1000, 0, mtime, mtime, mtime), 0, 0))

	_, f := openFragmentTestFile(t, "wsl.sh",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxAttributes()
	if err != nil {
		t.Fatalf("LinuxAttributes failed: %v", err)
	}
	if attrs == nil {
		t.Fatal("expected POSIX metadata")
	}

	if attrs.ModeString() != "-rwxr-xr-x" {
		t.Errorf("unexpected mode %q", attrs.ModeString())
	}
	if attrs.String() != "-rwxr-xr-x 1000:1000" {
		t.Errorf("unexpected String() %q", attrs.String())
	}
}

// TestFileLinuxAttributesMatchesNameWithoutPrefix covers the disagreement
// between WSL generations and the tools that read them over the leading '$'.
func TestFileLinuxAttributesMatchesNameWithoutPrefix(t *testing.T) {
	mtime := time.Unix(1_700_000_500, 0).UTC()
	value := buildEAValue(buildEAEntry("LXATTRB",
		buildLXATTRBValue(0o040700, 0, 0, 0, mtime, mtime, mtime), 0, 0))

	_, f := openFragmentTestFile(t, "wsldir",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxAttributes()
	if err != nil {
		t.Fatalf("LinuxAttributes failed: %v", err)
	}
	if attrs == nil || attrs.FileType() != "directory" {
		t.Fatalf("expected a directory, got %+v", attrs)
	}
}

// TestFileLinuxAttributesFromWSL1 covers the older layout, where the same fields
// live in separate attributes rather than one structure.
func TestFileLinuxAttributesFromWSL1(t *testing.T) {
	value := buildEAValue(
		buildEAEntry("$LXUID", buildLXValue(1001), 0, 0),
		buildEAEntry("$LXGID", buildLXValue(1002), 0, 0),
		buildEAEntry("$LXMOD", buildLXValue(0o100600), 0, 0),
	)

	_, f := openFragmentTestFile(t, "wsl1.txt",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxAttributes()
	if err != nil {
		t.Fatalf("LinuxAttributes failed: %v", err)
	}
	if attrs == nil {
		t.Fatal("expected POSIX metadata assembled from the WSL1 attributes")
	}

	if !attrs.FromLegacyAttributes {
		t.Error("expected FromLegacyAttributes set")
	}
	if attrs.UID != 1001 || attrs.GID != 1002 {
		t.Errorf("expected owner 1001:1002, got %d:%d", attrs.UID, attrs.GID)
	}
	if attrs.ModeString() != "-rw-------" {
		t.Errorf("unexpected mode %q", attrs.ModeString())
	}
	// WSL1 records no POSIX timestamps, which must read as absent rather than
	// as the epoch.
	if !attrs.ModifyTime.IsZero() {
		t.Errorf("expected no POSIX timestamps, got %v", attrs.ModifyTime)
	}
}

// TestFileLinuxAttributesIgnoresUnrelatedEAs covers a file with extended
// attributes that have nothing to do with WSL, which must not be reported as
// carrying POSIX metadata.
func TestFileLinuxAttributesIgnoresUnrelatedEAs(t *testing.T) {
	value := buildEAValue(buildEAEntry("VENDOR.TAG", []byte("payload"), 0, 0))

	_, f := openFragmentTestFile(t, "other.txt",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxAttributes()
	if err != nil {
		t.Fatalf("LinuxAttributes failed: %v", err)
	}
	if attrs != nil {
		t.Errorf("expected no POSIX metadata, got %+v", attrs)
	}
}

// buildLXXATTRValue assembles a WSL LXXATTR value from name/value pairs.
func buildLXXATTRValue(pairs ...[2]string) []byte {
	buf := make([]byte, lxxattrHeaderSize)
	WriteUint16LE(buf, 0, 0) // Flags
	WriteUint16LE(buf, 2, 1) // Version

	entries := make([][]byte, 0, len(pairs))
	for _, pair := range pairs {
		name, value := pair[0], pair[1]

		size := AlignUp(lxxattrEntryHeaderSize+len(name)+1+len(value), 4)
		entry := make([]byte, size)

		entry[2] = 0 // Flags
		entry[3] = uint8(len(name))
		WriteUint16LE(entry, 4, uint16(len(value)))
		copy(entry[lxxattrEntryHeaderSize:], name)
		copy(entry[lxxattrEntryHeaderSize+len(name)+1:], value)

		entries = append(entries, entry)
	}

	for i, entry := range entries {
		if i == len(entries)-1 {
			WriteUint16LE(entry, 0, 0)
		} else {
			WriteUint16LE(entry, 0, uint16(len(entry)))
		}
		buf = append(buf, entry...)
	}

	return buf
}

func TestParseLinuxExtendedAttributes(t *testing.T) {
	value := buildLXXATTRValue(
		[2]string{"user.comment", "hello"},
		[2]string{"security.capability", "\x01\x00\x00\x02"},
	)

	attrs, err := ParseLinuxExtendedAttributes(value)
	if err != nil {
		t.Fatalf("ParseLinuxExtendedAttributes failed: %v", err)
	}

	if len(attrs) != 2 {
		t.Fatalf("expected 2 attributes, got %d: %+v", len(attrs), attrs)
	}
	if attrs[0].Name != "user.comment" || string(attrs[0].Value) != "hello" {
		t.Errorf("unexpected first attribute: %+v", attrs[0])
	}
	if attrs[1].Name != "security.capability" {
		t.Errorf("unexpected second attribute name %q", attrs[1].Name)
	}
	if attrs[0].String() != "user.comment (5 bytes)" {
		t.Errorf("unexpected String() %q", attrs[0].String())
	}
}

func TestParseLinuxExtendedAttributesRejectsBadInput(t *testing.T) {
	truncated := buildLXXATTRValue([2]string{"user.test", "0123456789"})
	truncated = truncated[:len(truncated)-8]

	// An offset that does not clear the entry would overlap the next one.
	overlapping := buildLXXATTRValue([2]string{"user.test", "value"})
	WriteUint16LE(overlapping, lxxattrHeaderSize, 4)

	tests := []struct {
		name  string
		value []byte
	}{
		{name: "short header", value: []byte{1, 2}},
		{name: "truncated value", value: truncated},
		{name: "non-advancing offset", value: overlapping},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseLinuxExtendedAttributes(tt.value); !errors.Is(err, ErrInvalidExtendedAttribute) {
				t.Errorf("expected ErrInvalidExtendedAttribute, got %v", err)
			}
		})
	}
}

func TestParseLinuxExtendedAttributesEmpty(t *testing.T) {
	attrs, err := ParseLinuxExtendedAttributes(nil)
	if err != nil {
		t.Fatalf("expected no error for an empty value, got %v", err)
	}
	if attrs != nil {
		t.Errorf("expected no attributes, got %+v", attrs)
	}
}

func TestFileLinuxExtendedAttributes(t *testing.T) {
	lxxattr := buildLXXATTRValue([2]string{"user.origin", "downloaded"})
	value := buildEAValue(buildEAEntry("$LXXATTR", lxxattr, 0, 0))

	_, f := openFragmentTestFile(t, "tagged.txt",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxExtendedAttributes()
	if err != nil {
		t.Fatalf("LinuxExtendedAttributes failed: %v", err)
	}

	if len(attrs) != 1 || attrs[0].Name != "user.origin" {
		t.Fatalf("unexpected attributes: %+v", attrs)
	}
	if string(attrs[0].Value) != "downloaded" {
		t.Errorf("unexpected value %q", attrs[0].Value)
	}
}

// TestFileLinuxExtendedAttributesIndependentOfLXATTRB covers a file carrying
// mode metadata but no POSIX xattrs, which is the common case.
func TestFileLinuxExtendedAttributesIndependentOfLXATTRB(t *testing.T) {
	mtime := time.Unix(1_700_000_500, 0).UTC()
	value := buildEAValue(buildEAEntry("$LXATTRB",
		buildLXATTRBValue(0o100644, 0, 0, 0, mtime, mtime, mtime), 0, 0))

	_, f := openFragmentTestFile(t, "modeonly.txt",
		buildResidentAttr(AttrTypeEA, 2, value))

	attrs, err := f.LinuxExtendedAttributes()
	if err != nil {
		t.Fatalf("LinuxExtendedAttributes failed: %v", err)
	}
	if attrs != nil {
		t.Errorf("expected no POSIX xattrs, got %+v", attrs)
	}

	if mode, err := f.LinuxAttributes(); err != nil || mode == nil {
		t.Errorf("expected LXATTRB still readable, got %+v (%v)", mode, err)
	}
}

func TestFileLinuxAttributesAbsent(t *testing.T) {
	_, f := openFragmentTestFile(t, "plain.txt",
		buildResidentAttr(AttrTypeData, 2, []byte("no eas")))

	attrs, err := f.LinuxAttributes()
	if err != nil {
		t.Fatalf("expected no error for a file without $EA, got %v", err)
	}
	if attrs != nil {
		t.Errorf("expected nil, got %+v", attrs)
	}
}
