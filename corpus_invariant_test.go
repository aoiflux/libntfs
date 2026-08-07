package libntfs

import (
	"errors"
	"fmt"
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

	checked, directories, stale := 0, 0, 0

	walkCorpus(t, vol, func(path string, entries []DirEntry) {
		for _, entry := range entries {
			record, err := vol.GetMFTEntry(entry.EntryNum)
			if err != nil {
				continue
			}

			// An index entry names the object that held this MFT slot when the
			// name was written. NTFS bumps the record's sequence number every
			// time the slot is reused, so a mismatch means the name outlived
			// the object -- which is routine for a deleted name recovered from
			// index slack. Comparing the two then compares a dead file against
			// whatever now occupies its slot, which says nothing about the
			// parser. A real volume carries plenty of these: on one test image
			// the slack-recovered names sat at sequence 1 against records
			// already reused up to sequence 4.
			if entry.SequenceNum != record.SequenceNum {
				stale++
				continue
			}

			file, err := vol.Open(entry.EntryNum)
			if err != nil {
				continue
			}

			checked++
			if file.IsDirectory() {
				directories++
			}

			if entry.IsDirectory != file.IsDirectory() {
				t.Errorf("%s/%s (entry %d, sequence %d): ReadDir says directory=%v, the MFT record says %v",
					path, entry.Name, entry.EntryNum, entry.SequenceNum,
					entry.IsDirectory, file.IsDirectory())
			}
		}
	})

	if checked == 0 {
		t.Fatal("expected the walk to check some entries")
	}
	if directories == 0 {
		t.Error("expected the volume to contain at least one directory")
	}

	t.Logf("checked %d entries, %d directories, skipped %d stale references", checked, directories, stale)
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

		// A partially readable directory still yields the names that survived,
		// and dropping them here would hide exactly the entries an invariant
		// test most wants to see.
		entries, err := current.file.ReadDir()
		if err != nil && !isPartialDirError(err) {
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

// TestCorpusResolvesTargetAttribute is what the open attribute table exists for:
// turning a log record from "UpdateResidentValue on attribute 0x18" into
// "UpdateResidentValue on $MFT's $DATA".
//
// The entry layout was derived from a real volume rather than a specification,
// so this is the test that keeps it honest. Every assertion below is a way the
// decoding could be wrong and would show: an attribute type NTFS does not
// define, a file reference outside the MFT, a name on an attribute type that
// cannot carry one, or an LSN outside the log that recorded it.
func TestCorpusResolvesTargetAttribute(t *testing.T) {
	vol := openCorpus(t)

	table, err := vol.LogAttributeTable()
	if err != nil {
		t.Fatalf("LogAttributeTable failed: %v", err)
	}
	if table.Len() == 0 {
		t.Skip("the log window holds no open attribute table dump")
	}

	entryCount, err := vol.MFTEntryCount()
	if err != nil {
		t.Fatalf("MFTEntryCount failed: %v", err)
	}

	named := 0
	for _, entry := range table.Entries() {
		if !entry.InUse {
			continue
		}

		// Only a handful of attribute types are ever held open.
		switch entry.AttributeType {
		case AttrTypeData, AttrTypeIndexAllocation, AttrTypeBitmap,
			AttrTypeIndexRoot, AttrTypeReparsePoint, AttrTypeEA:
		default:
			t.Errorf("entry at 0x%04X names attribute type 0x%X (%s), which is not one "+
				"NTFS holds open — the type field may be at the wrong offset",
				entry.Offset, entry.AttributeType, entry.AttributeTypeName())
		}

		if entry.RecordNumber >= entryCount {
			t.Errorf("entry at 0x%04X names MFT record %d, past the %d the volume has — "+
				"the file reference may be at the wrong offset",
				entry.Offset, entry.RecordNumber, entryCount)
		}

		if entry.Name != "" {
			named++

			// A name belongs to an index or a named stream, never to a bitmap.
			if entry.AttributeType == AttrTypeBitmap {
				t.Errorf("entry at 0x%04X carries the name %q on a $BITMAP, which does "+
					"not have one — names may be keyed to the wrong entries",
					entry.Offset, entry.Name)
			}
		}
	}

	if named == 0 {
		t.Error("expected some attributes to carry names ($I30 on directories at least)")
	}

	// Now resolve real records and check the rate. A log holding a recent dump
	// should resolve nearly everything; a low rate means the offsets the records
	// carry do not line up with the offsets the table reports.
	var withTarget, resolved int

	if err := vol.EachLogRecord(func(record *LogRecord) error {
		if !record.IsClientRecord() || record.TargetAttribute == 0 {
			return nil
		}

		withTarget++
		if _, ok := table.Resolve(record); ok {
			resolved++
		}

		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if withTarget == 0 {
		t.Skip("no records in the log name an attribute")
	}

	rate := float64(resolved) / float64(withTarget)
	t.Logf("%d entries (%d named), %d of %d records resolved (%.1f%%)",
		table.Len(), named, resolved, withTarget, rate*100)

	if rate < 0.5 {
		t.Errorf("only %.1f%% of records naming an attribute resolved; the table "+
			"offsets and the records' TargetAttribute may not be the same quantity", rate*100)
	}
}

// TestCorpusLogTableDumpShapes checks the two dump shapes stay distinct. An
// AttributeNamesDump is a table dump but not a restart table, and reading one as
// a restart table decodes an entry size out of the first name's bytes.
func TestCorpusLogTableDumpShapes(t *testing.T) {
	vol := openCorpus(t)

	restartTables, nameDumps := 0, 0

	if err := vol.EachLogRecord(func(record *LogRecord) error {
		if !record.IsTableDump() {
			return nil
		}

		if record.RedoOperation == LogOpAttributeNamesDump {
			if record.IsRestartTableDump() {
				t.Errorf("LSN %d: an AttributeNamesDump reported itself a restart table",
					record.LSN)
			}
			if table, _ := record.RestartTable(); table != nil {
				t.Errorf("LSN %d: a restart table was decoded from attribute names: %+v",
					record.LSN, table)
			}

			names, err := record.AttributeNames()
			if err != nil && len(names) == 0 {
				t.Logf("LSN %d: names did not decode, likely clipped by the log: %v",
					record.LSN, err)
				return nil
			}
			nameDumps++

			for offset, name := range names {
				if name == "" {
					t.Errorf("LSN %d: empty name at offset 0x%04X", record.LSN, offset)
				}
			}
			return nil
		}

		if !record.IsRestartTableDump() {
			t.Errorf("LSN %d: %s should be a restart table dump",
				record.LSN, record.RedoOperationName())
			return nil
		}

		table, err := record.RestartTable()
		if err != nil || table == nil {
			return nil
		}
		restartTables++

		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	t.Logf("%d restart table dumps, %d attribute name dumps", restartTables, nameDumps)
}

// TestCorpusDirEntryMergeIsConsistent is the invariant form of the v0.3.0
// ReadDir panic.
//
// A directory index stores a file under several names -- an 8.3 name and a
// long name for the same reference -- and ReadDir merges them into one entry.
// Which name lands in $INDEX_ROOT and which lands in an $INDEX_ALLOCATION
// block is decided by B-tree order, so on a large enough directory the merge
// has to reach across both. v0.3.0 collected the two into separate slices
// while sharing the position maps between them, and indexed one slice with a
// position belonging to the other.
//
// On a real volume that panicked. The properties below are what a correct
// merge guarantees, and none of them can be asserted on a fixture that never
// splits a directory across both attributes.
func TestCorpusDirEntryMergeIsConsistent(t *testing.T) {
	vol := openCorpus(t)

	listings, entries := 0, 0

	walkCorpus(t, vol, func(path string, dirEntries []DirEntry) {
		listings++
		entries += len(dirEntries)

		seen := make(map[string]bool, len(dirEntries))
		liveByName := make(map[string]uint64, len(dirEntries))
		liveNamesByRef := make(map[uint64][]string, len(dirEntries))

		for _, entry := range dirEntries {
			// NTFS has no nameless entry, and one cannot be opened, pathed, or
			// reported. A zero-length $FILE_NAME parses without error, so this
			// is reachable from index slack and from records damaged mid-write.
			if entry.Name == "" {
				t.Errorf("%s/: entry %d has an empty name", path, entry.EntryNum)
				continue
			}

			name := strings.ToLower(entry.Name)

			// The accumulator deduplicates on reference and name together, so
			// that pair must be unique. Reference alone must not be, since a
			// file hard-linked twice into one directory is two entries, and
			// name alone must not be either: a deleted generation recovered
			// from slack legitimately shares its name with the live file that
			// replaced it, under a different reference. Both were observed on
			// a real volume.
			key := fmt.Sprintf("%d|%s", entry.EntryNum, name)
			if seen[key] {
				t.Errorf("%s/: reference %d listed twice under the name %q",
					path, entry.EntryNum, entry.Name)
			}
			seen[key] = true

			if entry.Deleted {
				continue
			}

			// A directory index cannot hold two live entries with one name.
			if previous, dup := liveByName[name]; dup {
				t.Errorf("%s/: name %q is live under both reference %d and %d",
					path, entry.Name, previous, entry.EntryNum)
			}
			liveByName[name] = entry.EntryNum
			liveNamesByRef[entry.EntryNum] = append(liveNamesByRef[entry.EntryNum], entry.Name)
		}

		// The merge collapses the several names of one file into a single
		// entry, preferring the long name over the 8.3 alias. A reference left
		// holding both is the silent half of the v0.3.0 defect: wherever the
		// stale index did not fall out of range and panic, it failed to
		// replace the alias and the directory listed the file twice.
		for ref, names := range liveNamesByRef {
			if len(names) < 2 {
				continue
			}
			for _, name := range names {
				if isShortNameAlias(name) {
					t.Errorf("%s/: reference %d kept the 8.3 alias %q alongside %v",
						path, ref, name, names)
				}
			}
		}
	})

	if listings == 0 {
		t.Fatal("walked no directories")
	}
	t.Logf("checked %d entries across %d directory listings", entries, listings)
}

// isShortNameAlias reports whether name looks like an 8.3 alias NTFS generated
// for a long name: uppercase, at most 8.3 characters, with the "~N" tail that
// disambiguates colliding stems.
//
// This is a heuristic, and deliberately a narrow one. It is used only to
// strengthen an invariant that already holds without it, so a false negative
// costs nothing and a false positive would cost a spurious failure.
func isShortNameAlias(name string) bool {
	if len(name) > 12 || !strings.Contains(name, "~") {
		return false
	}
	if name != strings.ToUpper(name) {
		return false
	}

	stem, ext, _ := strings.Cut(name, ".")
	return len(stem) <= 8 && len(ext) <= 3
}

// TestCorpusReadDirPartialContract checks the other half of the contract: a
// damaged directory must hand back what it could read rather than nothing, and
// its own accounting must agree with what it returned.
func TestCorpusReadDirPartialContract(t *testing.T) {
	vol := openCorpus(t)

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	const maxDirectories = 512

	queue := []*File{root}
	seen := map[uint64]bool{root.EntryNumber(): true}
	visited, partials := 0, 0

	for len(queue) > 0 && visited < maxDirectories {
		current := queue[0]
		queue = queue[1:]
		visited++

		entries, err := current.ReadDir()

		var partial *PartialDirError
		if errors.As(err, &partial) {
			partials++

			if partial.Recovered != len(entries) {
				t.Errorf("entry %d: PartialDirError reports %d recovered but ReadDir returned %d",
					partial.EntryNum, partial.Recovered, len(entries))
			}
			if partial.Skipped == 0 && partial.Err == nil {
				t.Errorf("entry %d: PartialDirError reports no loss and no cause",
					partial.EntryNum)
			}
			// The typed error has to stay matchable both ways, since callers
			// reach for whichever suits them.
			if !errors.Is(err, ErrInvalidIndex) {
				t.Errorf("entry %d: partial error does not wrap ErrInvalidIndex: %v",
					partial.EntryNum, err)
			}
		} else if err != nil && len(entries) != 0 {
			t.Errorf("entry %d: ReadDir returned %d entries with a non-partial error %v",
				current.EntryNumber(), len(entries), err)
		}

		for _, entry := range entries {
			if entry.Deleted || seen[entry.EntryNum] {
				continue
			}
			child, err := vol.Open(entry.EntryNum)
			if err != nil || !child.IsDirectory() || child.IsReparsePoint() {
				continue
			}
			seen[entry.EntryNum] = true
			queue = append(queue, child)
		}
	}

	t.Logf("walked %d directories, %d partially readable", visited, partials)
}
