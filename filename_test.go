package libntfs

import "testing"

// $FILE_NAME holds a 32-bit FileAttributes at 0x38 and, at 0x3C, the reparse
// tag when FileAttrReparsePoint is set (the packed EA size otherwise). Reading
// 0x38 as a uint64 folds the tag into the attributes and loses it.
func TestParseFileNameSplitsAttributesAndReparseTag(t *testing.T) {
	value := buildFileNameAttrValue(5, 1, "link", NamespaceWin32, 0, 0, 0)
	WriteUint32LE(value, 0x38, FileAttrReparsePoint|FileAttrArchive)
	WriteUint32LE(value, 0x3C, ReparsePointSymlink)

	fn, err := ParseFileName(value)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fn.FileAttributes != FileAttrReparsePoint|FileAttrArchive {
		t.Fatalf("FileAttributes = %#x, want %#x", fn.FileAttributes, FileAttrReparsePoint|FileAttrArchive)
	}
	if fn.ReparseValue != ReparsePointSymlink {
		t.Fatalf("ReparseValue = %#x, want %#x", fn.ReparseValue, uint32(ReparsePointSymlink))
	}
	if fn.Name != "link" || fn.Namespace != NamespaceWin32 {
		t.Fatalf("name = %q ns %d", fn.Name, fn.Namespace)
	}
}
