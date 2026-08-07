package libntfs

import "testing"

// These cover two bugs that a real volume found and the synthetic suite could
// not, because the fixtures were built from the parser's own assumptions and so
// agreed with it. The fixtures here deliberately encode what NTFS actually
// writes rather than what the parser used to expect.

// TestIsDirectoryFileAttributes pins the flag NTFS actually uses.
//
// A directory carries FileAttrDupFileNameIndexPresent (0x10000000) in the
// FileAttributes field of $FILE_NAME and leaves the Win32 FileAttrDirectory bit
// (0x0010) clear. Testing the Win32 bit against on-disk bytes reported every
// directory on a real volume as a file.
func TestIsDirectoryFileAttributes(t *testing.T) {
	tests := []struct {
		name       string
		attributes uint64
		want       bool
	}{
		{
			// What Windows writes for a plain directory.
			name:       "NTFS directory bit alone",
			attributes: FileAttrDupFileNameIndexPresent,
			want:       true,
		},
		{
			// Observed on a real volume: the directory bit plus hidden+system.
			name:       "NTFS directory bit with other flags",
			attributes: FileAttrDupFileNameIndexPresent | FileAttrHidden | FileAttrSystem,
			want:       true,
		},
		{
			// Accepted for tools that synthesise NTFS from a Win32 view.
			name:       "Win32 directory bit alone",
			attributes: FileAttrDirectory,
			want:       true,
		},
		{
			// What Windows writes for an ordinary file.
			name:       "archive file",
			attributes: FileAttrArchive,
			want:       false,
		},
		{
			name:       "no attributes",
			attributes: 0,
			want:       false,
		},
		{
			// The bits either side of the Win32 directory bit must not match.
			name:       "system and hidden only",
			attributes: FileAttrHidden | FileAttrSystem | FileAttrReadOnly,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDirectoryFileAttributes(tt.attributes); got != tt.want {
				t.Errorf("isDirectoryFileAttributes(%#x) = %v, want %v",
					tt.attributes, got, tt.want)
			}
		})
	}
}

// TestResolveDirEntryIsDirectoryFromNTFSBit covers the path ReadDir takes when
// no resolver is supplied, which is where the wrong bit did its damage.
func TestResolveDirEntryIsDirectoryFromNTFSBit(t *testing.T) {
	directory := IndexEntry{
		FileName: &FileName{FileAttributes: FileAttrDupFileNameIndexPresent},
	}
	if !resolveDirEntryIsDirectory(directory, nil) {
		t.Error("expected an entry carrying the NTFS directory bit to be a directory")
	}

	file := IndexEntry{
		FileName: &FileName{FileAttributes: FileAttrArchive},
	}
	if resolveDirEntryIsDirectory(file, nil) {
		t.Error("expected an archive file not to be a directory")
	}
}

// TestResolveDirEntryIsDirectoryPrefersResolver covers the MFT record header
// winning over the index entry's flags, since the record is authoritative.
func TestResolveDirEntryIsDirectoryPrefersResolver(t *testing.T) {
	entry := IndexEntry{
		FileReference: 42,
		FileName:      &FileName{FileAttributes: FileAttrArchive},
	}

	resolver := func(entryNum uint64) (bool, error) {
		if entryNum != 42 {
			t.Errorf("expected the resolver called for entry 42, got %d", entryNum)
		}
		return true, nil
	}

	if !resolveDirEntryIsDirectory(entry, resolver) {
		t.Error("expected the MFT record to override the index entry's flags")
	}
}

// TestIsPlausibleRecoveredFileName pins the validation that keeps index-slack
// recovery from inventing files.
//
// The recovery scans unallocated space at every alignment boundary, so it is
// offered far more garbage than genuine entries. A real volume produced entries
// with mojibake names, which in a forensic context is worse than missing a
// marginal entry: it sends an examiner chasing something that never existed.
func TestIsPlausibleRecoveredFileName(t *testing.T) {
	valid := func() *FileName {
		return &FileName{
			Name:            "report.docx",
			Namespace:       NamespaceWin32,
			ParentDirectory: MFTEntryRoot,
		}
	}

	if !isPlausibleRecoveredFileName(valid()) {
		t.Fatal("expected a well-formed name to be accepted")
	}

	tests := []struct {
		name   string
		mutate func(*FileName)
	}{
		{
			name:   "namespace out of range",
			mutate: func(fn *FileName) { fn.Namespace = 7 },
		},
		{
			// Entry 0 is $MFT, which is never a parent.
			name:   "parent reference of zero",
			mutate: func(fn *FileName) { fn.ParentDirectory = 0 },
		},
		{
			name:   "empty name",
			mutate: func(fn *FileName) { fn.Name = "" },
		},
		{
			name:   "control characters",
			mutate: func(fn *FileName) { fn.Name = "re\x01port" },
		},
		{
			name:   "path separator",
			mutate: func(fn *FileName) { fn.Name = "dir/file" },
		},
		{
			name:   "reserved character",
			mutate: func(fn *FileName) { fn.Name = "wild*card" },
		},
		{
			name:   "undecodable UTF-16",
			mutate: func(fn *FileName) { fn.Name = "bad�name" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := valid()
			tt.mutate(fn)

			if isPlausibleRecoveredFileName(fn) {
				t.Errorf("expected %q (namespace %d, parent %d) to be rejected",
					fn.Name, fn.Namespace, fn.ParentDirectory)
			}
		})
	}
}

// TestIsPlausibleFileNameTextAcceptsRealNames guards against the validation
// becoming so strict it drops names NTFS genuinely permits.
func TestIsPlausibleFileNameTextAcceptsRealNames(t *testing.T) {
	names := []string{
		"$MFT",
		"NTUSER.DAT",
		"file with spaces.txt",
		"Résumé.pdf",
		"日本語.txt",
		"a.very.long.name.with.dots.tar.gz",
		"~$tempfile.docx",
		"file[1].html",
		"100% done.txt",
	}

	for _, name := range names {
		if !isPlausibleFileNameText(name) {
			t.Errorf("expected %q to be accepted", name)
		}
	}
}
