package libntfs

import (
	"io"
	"strings"
	"testing"
)

// Invariant tests assert properties that must hold on any NTFS volume rather
// than facts about a particular fixture, so they run against whatever image
// LIBNTFS_TEST_IMAGE names.
//
// This is where a real volume earns its place. A parser can agree with
// synthetic fixtures indefinitely when both share a wrong assumption, and only
// bytes from a real writer break the tie. One already did, which is what
// TestCorpusDirEntryTypeMatchesMFT below exists to keep fixed.

// TestCorpusDirEntryTypeMatchesMFT is the regression test for a bug the
// synthetic suite could not have caught.
//
// ReadDir classified entries from the index entry's FileAttributes, testing the
// Win32 FILE_ATTRIBUTE_DIRECTORY bit — which NTFS never sets on disk. Every
// directory on a real volume read as a file, and every fixture agreed, because
// the fixtures set the same bit the parser looked for.
//
// The MFT record header flag is authoritative, so the two must agree.
func TestCorpusDirEntryTypeMatchesMFT(t *testing.T) {
	vol := openCorpus(t)

	checked, directories := 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			file, err := vol.Open(entry.EntryNum)
			if err != nil {
				continue
			}

			checked++
			if file.IsDirectory() {
				directories++
			}

			if entry.IsDirectory != file.IsDirectory() {
				t.Errorf("%s/%s (entry %d): ReadDir says directory=%v, the MFT record says %v",
					path, entry.Name, entry.EntryNum, entry.IsDirectory, file.IsDirectory())
			}
		}
	})

	if checked == 0 {
		t.Fatal("expected the walk to check some entries")
	}
	if directories == 0 {
		t.Error("expected the volume to contain at least one directory")
	}

	t.Logf("checked %d entries, %d directories", checked, directories)
}

// TestCorpusFragmentsTileEveryStream checks the fragment map against every file
// on the volume rather than one built for the purpose. Fragments must tile the
// stream with no gap and no overlap, must land inside the volume, and must stop
// at the file's real size rather than at the end of its last cluster.
func TestCorpusFragmentsTileEveryStream(t *testing.T) {
	vol := openCorpus(t)

	volumeSize := int64(vol.VolumeSize())
	checked, fragmented, holes := 0, 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			file, err := vol.Open(entry.EntryNum)
			if err != nil || file.IsDirectory() || !file.HasData() {
				continue
			}

			// A compressed stream's runs map compression units rather than
			// stream bytes, which the API documents and this invariant does not
			// apply to.
			if file.IsCompressed() {
				continue
			}

			frags, err := file.Fragments()
			if err != nil {
				t.Errorf("%s/%s: Fragments failed: %v", path, entry.Name, err)
				continue
			}
			if len(frags) == 0 {
				continue
			}

			checked++
			if file.IsFragmented() {
				fragmented++
			}

			var covered int64
			for i, frag := range frags {
				if frag.FileOffset != covered {
					t.Errorf("%s/%s fragment %d: starts at stream offset %d, expected %d",
						path, entry.Name, i, frag.FileOffset, covered)
					break
				}
				if frag.Length <= 0 {
					t.Errorf("%s/%s fragment %d: length is %d", path, entry.Name, i, frag.Length)
					break
				}

				switch {
				case frag.Sparse:
					holes++
				case !frag.Resident:
					if frag.StartOffset < 0 || frag.EndOffset > volumeSize {
						t.Errorf("%s/%s fragment %d: [%d,%d) is outside the %d byte volume",
							path, entry.Name, i, frag.StartOffset, frag.EndOffset, volumeSize)
					}
					if frag.EndOffset-frag.StartOffset != frag.Length {
						t.Errorf("%s/%s fragment %d: volume span %d does not match length %d",
							path, entry.Name, i, frag.EndOffset-frag.StartOffset, frag.Length)
					}
				}

				covered += frag.Length
			}

			if covered != file.Size() {
				t.Errorf("%s/%s: fragments cover %d bytes, the file is %d bytes",
					path, entry.Name, covered, file.Size())
			}
		}
	})

	if checked == 0 {
		t.Skip("the volume has no mappable files")
	}

	t.Logf("mapped %d files, %d fragmented, %d holes", checked, fragmented, holes)
}

// TestCorpusReadsBackEveryFile reads the head and the final byte of every file,
// which is where a run-list decoded one cluster short or long shows up, and
// checks that reading past the end reports EOF rather than slack.
func TestCorpusReadsBackEveryFile(t *testing.T) {
	vol := openCorpus(t)

	const probe = 4096
	read := 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			file, err := vol.Open(entry.EntryNum)
			if err != nil || file.IsDirectory() {
				continue
			}

			support := file.ReadSupport()
			if !support.Readable || file.Size() == 0 {
				continue
			}

			size := file.Size()

			head := make([]byte, probe)
			if size < probe {
				head = make([]byte, size)
			}
			if _, err := file.ReadAt(head, 0); err != nil && err != io.EOF {
				t.Errorf("%s/%s: reading the first %d bytes failed: %v",
					path, entry.Name, len(head), err)
				continue
			}

			// The last byte is the one a mis-sized final run gets wrong.
			if _, err := file.ReadAt(make([]byte, 1), size-1); err != nil && err != io.EOF {
				t.Errorf("%s/%s: reading the last byte at %d failed: %v",
					path, entry.Name, size-1, err)
				continue
			}

			if n, err := file.ReadAt(make([]byte, 1), size); err != io.EOF || n != 0 {
				t.Errorf("%s/%s: reading past the end of a %d byte file returned (%d, %v), want (0, EOF)",
					path, entry.Name, size, n, err)
			}

			read++
		}
	})

	if read == 0 {
		t.Skip("the volume has no readable files")
	}

	t.Logf("read back %d files", read)
}

// TestCorpusStreamsEnumerate checks that every file's stream list is coherent
// and that each alternate stream opens to the size it advertised.
func TestCorpusStreamsEnumerate(t *testing.T) {
	vol := openCorpus(t)

	files, alternates := 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			file, err := vol.Open(entry.EntryNum)
			if err != nil {
				continue
			}

			files++

			for _, stream := range file.Streams() {
				if !stream.IsAlternate() {
					continue
				}
				alternates++

				opened, err := file.OpenStream(stream.Name)
				if err != nil {
					t.Errorf("%s/%s:%s: OpenStream failed: %v",
						path, entry.Name, stream.Name, err)
					continue
				}
				if uint64(opened.Size()) != stream.Size {
					t.Errorf("%s/%s:%s: Streams reported %d bytes, OpenStream reported %d",
						path, entry.Name, stream.Name, stream.Size, opened.Size())
				}
			}
		}
	})

	t.Logf("enumerated streams on %d files, %d alternate streams", files, alternates)
}

// TestCorpusReportMatchesWalk cross-checks the two ways the library enumerates a
// volume. Report walks the MFT directly; ReadDir walks the directory indexes.
// A live file one finds should be a file the other finds.
func TestCorpusReportMatchesWalk(t *testing.T) {
	vol := openCorpus(t)

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	reported := make(map[string]bool, len(report.Files))
	for _, file := range report.Files {
		reported[strings.ToLower(pathBase(file.Filename))] = true
	}

	checked, missing := 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			// Report deliberately skips the reserved system records.
			if entry.Deleted || entry.EntryNum <= MFTEntryExtend {
				continue
			}

			checked++
			if reported[strings.ToLower(entry.Name)] {
				continue
			}

			missing++
			if missing <= 10 {
				t.Errorf("%s/%s (entry %d) was found by ReadDir but is absent from Report",
					path, entry.Name, entry.EntryNum)
			}
		}
	})

	if checked == 0 {
		t.Skip("the directory walk found nothing to compare")
	}

	t.Logf("cross-checked %d entries, %d absent from the report", checked, missing)
}

// TestCorpusSecurityDescriptorsResolve checks $Secure lookup across the volume
// rather than for a single file.
func TestCorpusSecurityDescriptorsResolve(t *testing.T) {
	vol := openCorpus(t)

	resolved, withOwner := 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			file, err := vol.Open(entry.EntryNum)
			if err != nil {
				continue
			}

			sd, err := file.SecurityDescriptor()
			if err != nil || sd == nil {
				continue
			}

			resolved++
			if sd.Owner != nil && sd.Owner.String() != "" {
				withOwner++
			}
		}
	})

	if resolved == 0 {
		t.Skip("no security descriptors resolved on this volume")
	}
	if withOwner == 0 {
		t.Error("expected at least one descriptor to name an owner")
	}

	t.Logf("resolved %d descriptors, %d naming an owner", resolved, withOwner)
}

// TestCorpusMFTWalkIsClean walks every MFT record, which is where a parser that
// panics or loops on unusual metadata surfaces.
func TestCorpusMFTWalkIsClean(t *testing.T) {
	vol := openCorpus(t)

	count, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}
	if count == 0 {
		t.Fatal("expected a non-zero MFT entry count")
	}

	parsed, inUse, directories := 0, 0, 0

	if err := vol.EachMFTEntry(func(entryNum uint64, entry *MFTEntry) error {
		parsed++
		if entry.IsInUse() {
			inUse++
		}
		if entry.IsDirectory() {
			directories++
		}

		// Every attribute accessor must survive whatever the record holds.
		for _, attr := range entry.Attributes {
			_ = attr.ReadSupport()
			_ = attr.GetAttributeName()
		}

		return nil
	}); err != nil {
		t.Fatalf("EachMFTEntry failed: %v", err)
	}

	if parsed == 0 {
		t.Fatal("expected at least one parsable MFT record")
	}

	t.Logf("%d of %d records parsed, %d in use, %d directories", parsed, count, inUse, directories)
}

// walkCorpus walks the volume breadth-first from the root, calling fn for each
// directory with its entries.
//
// Reparse points are not followed, and the walk is bounded, so a cyclic or
// corrupt tree cannot run forever.
func walkCorpus(t *testing.T, vol *Volume, fn func(path string, entries []DirEntry)) {
	t.Helper()

	const maxDirectories = 512

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	type queued struct {
		path string
		file *File
	}

	queue := []queued{{file: root}}
	seen := map[uint64]bool{root.EntryNumber(): true}
	visited := 0

	for len(queue) > 0 && visited < maxDirectories {
		current := queue[0]
		queue = queue[1:]
		visited++

		entries, err := current.file.ReadDir()
		if err != nil {
			continue
		}

		fn(current.path, entries)

		for _, entry := range entries {
			if entry.Deleted || seen[entry.EntryNum] {
				continue
			}

			child, err := vol.Open(entry.EntryNum)
			if err != nil || !child.IsDirectory() {
				continue
			}

			// A directory carrying a reparse point is a junction. Following one
			// risks walking in a circle and says nothing new about the volume.
			if child.IsReparsePoint() {
				continue
			}

			seen[entry.EntryNum] = true
			queue = append(queue, queued{path: current.path + "/" + entry.Name, file: child})
		}
	}
}

// pathBase returns the last element of a slash-separated path.
func pathBase(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
