package libntfs

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The parsers below are fed bytes lifted straight off a disk image, which in a
// forensic context is untrusted input by definition: the image may be damaged,
// deliberately malformed, or crafted. These targets assert the same thing of
// each one — that it terminates, does not panic, and never reports more data
// than it was given.

func FuzzParseExtendedAttributes(f *testing.F) {
	f.Add(buildEAValue(buildEAEntry("LXATTRB", []byte{1, 2, 3, 4}, 0, 0)))
	f.Add(buildEAValue(
		buildEAEntry("FIRST", []byte("a"), 0, 0),
		buildEAEntry("SECOND", []byte("b"), EAFlagNeedEA, 0),
	))
	f.Add(buildEAEntry("LOOP", []byte("value"), 0, 4))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		attrs, _ := ParseExtendedAttributes(data)

		for _, attr := range attrs {
			if len(attr.Name)+len(attr.Value) > len(data) {
				t.Fatalf("entry %q claims %d bytes from a %d byte value",
					attr.Name, len(attr.Name)+len(attr.Value), len(data))
			}
		}
	})
}

func FuzzParseEAInformation(f *testing.F) {
	f.Add(buildEAInfoValue(120, 1, 160))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := ParseEAInformation(data)
		if err == nil && info == nil {
			t.Fatal("ParseEAInformation returned no info and no error")
		}
	})
}

func FuzzParseReparsePoint(f *testing.F) {
	f.Add(buildReparseValue(ReparsePointSymlink,
		buildSymlinkData(`\??\C:\target.txt`, `C:\target.txt`, false)))
	f.Add(buildReparseValue(ReparsePointMountPoint,
		buildMountPointData(`\??\D:\data`, `D:\data`)))
	f.Add(buildReparseValue(ReparsePointLXSymlink,
		append([]byte{2, 0, 0, 0}, []byte("/usr/bin/bash")...)))
	f.Add(buildReparseGUIDValue(0x00000001, [16]byte{1}, []byte("payload")))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		reparse, err := ParseReparsePoint(data)
		if err != nil {
			return
		}

		if len(reparse.Data) > len(data) {
			t.Fatalf("reparse data is %d bytes from a %d byte value",
				len(reparse.Data), len(data))
		}

		// Every accessor must survive whatever the parser produced.
		reparse.Target()
		reparse.IsRelative()
		reparse.TagName()
		reparse.IsMicrosoft()
	})
}

func FuzzParseDataRuns(f *testing.F) {
	f.Add([]byte{0x11, 0x02, 0x06, 0x00})
	f.Add([]byte{0x01, 0x04, 0x11, 0x02, 0x06, 0x00}) // Sparse run then data
	f.Add([]byte{0x21, 0x00, 0x10, 0x00})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		runs, err := parseDataRuns(data)
		if err != nil {
			return
		}

		// Each run costs at least a header byte, so a valid run-list can never
		// hold more runs than the buffer has bytes.
		if len(runs) > len(data) {
			t.Fatalf("parsed %d runs from %d bytes", len(runs), len(data))
		}
	})
}

func FuzzParseLogRestartPage(f *testing.F) {
	f.Add(buildLogRestartPage(logRestartOpts{currentLSN: 42, clientName: "NTFS"}))
	f.Add(buildLogRestartPage(logRestartOpts{magic: LogChkdskPageMagic}))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		restart, err := ParseLogRestartPage(data)
		if err != nil {
			return
		}

		// The client array is bounded by the page it was read out of, so a
		// corrupt client count must not conjure entries from nowhere.
		if len(restart.Clients) > len(data) {
			t.Fatalf("parsed %d clients from a %d byte page", len(restart.Clients), len(data))
		}
	})
}

func FuzzParseLogRecord(f *testing.F) {
	f.Add(buildLogRecord(1, LogRecordTypeClient,
		buildNTFSLogData(LogOpUpdateResidentValue, LogOpNoop, 3,
			[]uint64{7, 8}, []byte("redo"), []byte("undo"))))
	f.Add(buildLogRecord(2, LogRecordTypeRestart, make([]byte, 32)))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		record, consumed, err := ParseLogRecord(data)
		if err != nil {
			return
		}

		if consumed <= 0 || consumed > len(data)+logAlignment {
			t.Fatalf("record consumed %d bytes of a %d byte buffer", consumed, len(data))
		}

		// Every payload is sliced out of the record's own client data, so none
		// can be longer than the buffer it came from.
		if len(record.RedoData) > len(data) || len(record.UndoData) > len(data) {
			t.Fatalf("payload longer than the %d byte record", len(data))
		}
		if len(record.LCNs)*8 > len(data) {
			t.Fatalf("parsed %d LCNs from a %d byte record", len(record.LCNs), len(data))
		}

		// Every accessor must survive whatever the parser produced.
		_ = record.RedoOperationName()
		_ = record.UndoOperationName()
		_ = record.String()
	})
}

// FuzzEachLogRecord drives the page walker rather than a single record, so the
// carry buffer that reassembles page-spanning records is exercised too.
func FuzzEachLogRecord(f *testing.F) {
	f.Add(buildLogFileImage(f, buildLogRecord(1, LogRecordTypeClient,
		buildNTFSLogData(LogOpCreateAttribute, LogOpNoop, 1, nil, []byte("seed"), nil))))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		_ = EachLogRecord(data, func(*LogRecord) error { return nil })
	})
}

func FuzzParseLogRestartTable(f *testing.F) {
	f.Add(buildLogRestartTable(0x28, 4))
	f.Add(buildLogRestartTable(0x10, 1))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		table, err := ParseLogRestartTable(data)
		if err != nil {
			return
		}

		// Entries are sliced out of the buffer, so a corrupt allocated count
		// must not conjure them from nowhere.
		if len(table.Entries)*int(table.EntrySize) > len(data) {
			t.Fatalf("parsed %d entries of %d bytes from a %d byte table",
				len(table.Entries), table.EntrySize, len(data))
		}
	})
}

// FuzzParseLinuxExtendedAttributes matters more than most: LXXATTR is not a
// documented structure, so its layout is inferred from what WSL writes.
func FuzzParseLinuxExtendedAttributes(f *testing.F) {
	f.Add(buildLXXATTRValue([2]string{"user.comment", "hello"}))
	f.Add(buildLXXATTRValue(
		[2]string{"user.a", "1"},
		[2]string{"security.capability", "\x01\x00"},
	))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		attrs, _ := ParseLinuxExtendedAttributes(data)

		for _, attr := range attrs {
			if len(attr.Name)+len(attr.Value) > len(data) {
				t.Fatalf("attribute %q claims %d bytes from a %d byte value",
					attr.Name, len(attr.Name)+len(attr.Value), len(data))
			}
		}
	})
}

func FuzzParseLinuxAttributes(f *testing.F) {
	now := time.Unix(1_700_000_000, 0).UTC()
	f.Add(buildLXATTRBValue(0o100644, 1000, 1000, 0, now, now, now))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		attrs, err := ParseLinuxAttributes(data)
		if err != nil {
			return
		}

		// Every accessor must survive whatever the parser produced.
		_ = attrs.ModeString()
		_ = attrs.FileType()
		_ = attrs.Permissions()
		_ = attrs.String()
	})
}

func FuzzParseLogOpenAttributeTable(f *testing.F) {
	f.Add(buildOpenAttributeTableDump(
		buildOpenAttributeEntry(true, AttrTypeData, 0, 1, 100),
		buildOpenAttributeEntry(false, AttrTypeIndexAllocation, 5, 5, 200),
	))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := ParseLogOpenAttributeTable(data)
		if err != nil {
			return
		}

		for _, entry := range entries {
			if len(entry.Raw) > len(data) {
				t.Fatalf("entry raw is %d bytes from a %d byte dump", len(entry.Raw), len(data))
			}
			if int(entry.Offset) > len(data) {
				t.Fatalf("entry offset %d is past the %d byte dump", entry.Offset, len(data))
			}

			_ = entry.AttributeTypeName()
			_ = entry.String()
		}
	})
}

// FuzzParseLogAttributeNames matters because the layout it decodes was derived
// from observed data rather than a specification.
func FuzzParseLogAttributeNames(f *testing.F) {
	f.Add(buildAttributeNamesDump(
		map[uint32]string{0x68: "$I30", 0x130: "$Q"}, 0x68, 0x130))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		names, _ := ParseLogAttributeNames(data)

		for offset, name := range names {
			// Each rune was decoded from at least one UTF-16 code unit, so the
			// name cannot hold more runes than the dump has pairs of bytes. The
			// decoded string itself can be longer in bytes than its source,
			// since UTF-16 to UTF-8 expands.
			if utf8.RuneCountInString(name)*2 > len(data) {
				t.Fatalf("name at 0x%X decodes %d runes from a %d byte dump",
					offset, utf8.RuneCountInString(name), len(data))
			}
		}
	})
}

func FuzzParseSecurityDescriptor(f *testing.F) {
	f.Add(make([]byte, 20))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		sd, err := ParseSecurityDescriptor(data)
		if err != nil {
			return
		}
		if sd == nil {
			t.Fatal("ParseSecurityDescriptor returned no descriptor and no error")
		}
	})
}

func FuzzParseUSNRecord(f *testing.F) {
	f.Add(make([]byte, 64))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, size, err := ParseUSNRecord(data)
		if err != nil {
			return
		}

		if size <= 0 || size > len(data) {
			t.Fatalf("record consumed %d bytes of a %d byte buffer", size, len(data))
		}
	})
}

// The index targets below cover the directory-listing path. They are split in
// two on purpose: the entry and record parsers take bytes off the disk, while
// the accumulator takes already-parsed entries and merges them. The panic that
// v0.3.0 shipped lived in the second, where no amount of malformed input to
// the first would have reached it.

func FuzzParseIndexEntries(f *testing.F) {
	f.Add(buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "seed.txt", NamespaceWin32, 4096, 1024, 0x20)))
	f.Add(make([]byte, 16)) // Header-only entry, EntryLength 0
	f.Add(make([]byte, 15)) // Shorter than a header
	f.Add([]byte{})

	overlong := buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "over.txt", NamespaceWin32, 4096, 1024, 0x20))
	WriteUint16LE(overlong, 8, 0xFFFF) // EntryLength past the buffer
	f.Add(overlong)

	unaligned := buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "odd.txt", NamespaceWin32, 4096, 1024, 0x20))
	WriteUint16LE(unaligned, 8, uint16(len(unaligned))-1) // Not 4-aligned
	f.Add(unaligned)

	overrun := buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "run.txt", NamespaceWin32, 4096, 1024, 0x20))
	WriteUint16LE(overrun, 10, 0xFFF0) // StreamLength past EntryLength
	f.Add(overrun)

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseIndexEntries(data)
		if err != nil {
			return
		}
		assertIndexEntriesWithin(t, entries, data)
	})
}

func FuzzParseIndexEntriesRecoverDeleted(f *testing.F) {
	seed := buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "seed.txt", NamespaceWin32, 4096, 1024, 0x20))
	f.Add(seed, len(seed))
	f.Add(seed, 0)              // Everything is slack
	f.Add(seed, len(seed)*4)    // usedLen past the buffer
	f.Add(seed, -1)             // Negative usedLen
	f.Add(make([]byte, 16), 16) // Header-only
	f.Add([]byte{}, 0)          // Empty
	f.Add(make([]byte, 3), 3)   // Shorter than a header

	f.Fuzz(func(t *testing.T, data []byte, usedLen int) {
		entries, err := parseIndexEntriesRecoverDeleted(data, usedLen)
		if err != nil {
			return
		}
		assertIndexEntriesWithin(t, entries, data)
	})
}

func FuzzParseIndexRecord(f *testing.F) {
	f.Add(buildIndexAllocationRecord(f, 4096, 0,
		buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "seed.txt", NamespaceWin32, 4096, 1024, 0x20))))
	f.Add(buildIndexAllocationRecord(f, 512, 0))

	// Records too short for the reads the header offsets imply. An
	// $INDEX_ALLOCATION whose boot-sector record size is bogus produces
	// exactly these.
	for _, size := range []int{0, 2, 4, 8, 24, 39} {
		short := make([]byte, size)
		if size >= 4 {
			WriteUint32LE(short, 0, IndexAllocationMagic)
		}
		f.Add(short)
	}

	// A well-formed record whose node bounds point outside it.
	bogusBounds := buildIndexAllocationRecord(f, 4096, 0)
	WriteUint32LE(bogusBounds, indexRecordHeaderSize+8, 0xFFFFFFFF)
	f.Add(bogusBounds)

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseIndexRecord(data)
		if err != nil {
			return
		}
		assertIndexEntriesWithin(t, entries, data)
	})
}

// FuzzDirEntryAccumulator drives the merge step that panicked in v0.3.0.
//
// The invariant it asserts is the one the old code broke: every position held
// in the dedup maps has to address a live entry. A stale position is not a
// cosmetic problem -- it is read back as a slice index on the next matching
// name, which is what turned one directory into a dead volume walk.
func FuzzDirEntryAccumulator(f *testing.F) {
	f.Add([]byte{5, 2, 0, 0, 5, 1, 1, 0})             // 8.3 then long name, same reference
	f.Add([]byte{5, 2, 0, 0, 6, 2, 1, 0, 6, 1, 2, 0}) // Two 8.3 names, long name for the second
	f.Add([]byte{9, 1, 0, 1, 9, 1, 0, 0})             // Deleted then allocated duplicate
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		acc := newDirEntryAccumulator(nil)

		for _, idxEntry := range decodeSyntheticIndexEntries(data) {
			acc.add(idxEntry)

			for ref, pos := range acc.dosIndexByRef {
				if pos < 0 || pos >= len(acc.entries) {
					t.Fatalf("dosIndexByRef[%d] = %d addresses nothing in %d entries",
						ref, pos, len(acc.entries))
				}
			}
			for key, pos := range acc.seenByName {
				if pos < 0 || pos >= len(acc.entries) {
					t.Fatalf("seenByName[%q] = %d addresses nothing in %d entries",
						key, pos, len(acc.entries))
				}
			}
		}

		// The maps exist to collapse duplicates, so the result must not hold
		// the same reference and name twice.
		seen := make(map[string]bool, len(acc.entries))
		for _, entry := range acc.entries {
			key := strings.ToLower(fmt.Sprintf("%d|%s", entry.EntryNum, entry.Name))
			if seen[key] {
				t.Fatalf("duplicate entry %s survived deduplication", key)
			}
			seen[key] = true
		}
	})
}

// decodeSyntheticIndexEntries turns fuzz bytes into index entries that are
// already well-formed, so the target exercises the merge rules rather than
// re-testing the parsers.
func decodeSyntheticIndexEntries(data []byte) []IndexEntry {
	names := []string{"a.txt", "A~1.TXT", "b.txt", "B~1.TXT", "", ".", "..", "Program Files"}

	var entries []IndexEntry
	for i := 0; i+4 <= len(data); i += 4 {
		fileName := &FileName{
			Name:      names[int(data[i+2])%len(names)],
			Namespace: data[i+1] % 4,
		}

		entry := IndexEntry{
			FileReference: uint64(data[i]),
			SequenceNum:   1,
			Deleted:       data[i+3]&1 != 0,
			FileName:      fileName,
		}
		if data[i+3]&2 != 0 {
			entry.FileName = nil // add must tolerate an entry with no name
		}

		entries = append(entries, entry)
	}

	return entries
}

// assertIndexEntriesWithin checks that no parsed entry claims more bytes than
// the buffer it came from.
func assertIndexEntriesWithin(t *testing.T, entries []IndexEntry, data []byte) {
	t.Helper()

	for _, entry := range entries {
		if int(entry.EntryLength) > len(data) {
			t.Fatalf("entry claims %d bytes of a %d byte buffer", entry.EntryLength, len(data))
		}
		if len(entry.Stream) > len(data) {
			t.Fatalf("entry stream is %d bytes of a %d byte buffer", len(entry.Stream), len(data))
		}
		// The name is measured in runes, not bytes: it is stored as UTF-16 and
		// returned as UTF-8, which expands by up to half again. Each rune still
		// costs at least one 2-byte code unit inside the buffer.
		if entry.FileName != nil && utf8.RuneCountInString(entry.FileName.Name) > len(data)/2 {
			t.Fatalf("entry name is %d runes from a %d byte buffer",
				utf8.RuneCountInString(entry.FileName.Name), len(data))
		}
	}
}

// The directory path does not end at the index parsers. ReadDir reaches an
// $INDEX_ROOT attribute, the $FILE_NAME inside every entry, and -- through
// size resolution and the MFT parent fallback -- whole MFT records. A panic in
// any of them ends the same walk, so each gets a target too.

func FuzzParseIndexRoot(f *testing.F) {
	f.Add(buildIndexRootValue(4096,
		buildIndexEntry(30, 1, buildFileNameAttrValue(5, 1, "seed.txt", NamespaceWin32, 4096, 1024, 0x20))))
	f.Add(buildIndexRootValue(4096))
	f.Add(make([]byte, 32)) // Header and node header, no entries
	f.Add(make([]byte, 31)) // One byte short of parsable
	f.Add([]byte{})

	// Node bounds pointing outside the attribute.
	bogus := buildIndexRootValue(4096)
	WriteUint32LE(bogus, 16, 0xFFFFFFFF) // Entry list offset
	f.Add(bogus)

	f.Fuzz(func(t *testing.T, data []byte) {
		root, err := parseIndexRoot(data)
		if err != nil {
			return
		}
		if root == nil {
			t.Fatal("parseIndexRoot returned no root and no error")
		}
		assertIndexEntriesWithin(t, root.Entries, data)
	})
}

func FuzzParseFileNameAttribute(f *testing.F) {
	f.Add(buildFileNameAttrValue(5, 1, "seed.txt", NamespaceWin32, 4096, 1024, 0x20))
	f.Add(buildFileNameAttrValue(5, 1, "", NamespaceDOS, 0, 0, 0))
	f.Add(make([]byte, 66)) // Header with a zero-length name
	f.Add(make([]byte, 65)) // One byte short of parsable
	f.Add([]byte{})

	// A name length that runs past the buffer.
	overrun := buildFileNameAttrValue(5, 1, "short", NamespaceWin32, 4096, 1024, 0x20)
	overrun[64] = 0xFF
	f.Add(overrun)

	f.Fuzz(func(t *testing.T, data []byte) {
		fn, err := parseFileNameAttribute(data)
		if err != nil {
			return
		}
		if fn == nil {
			t.Fatal("parseFileNameAttribute returned no name and no error")
		}
		// Each rune costs at least one 2-byte UTF-16 code unit in the buffer.
		if utf8.RuneCountInString(fn.Name) > len(data)/2 {
			t.Fatalf("name is %d runes from a %d byte buffer",
				utf8.RuneCountInString(fn.Name), len(data))
		}
	})
}

func FuzzParseMFTRecord(f *testing.F) {
	f.Add(buildTestMFTRecord(f, testRecordOpts{
		flags: MFTFlagInUse, name: "seed.txt", namespace: NamespaceWin32,
	}), uint32(DefaultMFTRecordSize))

	dir := buildTestMFTRecord(f, testRecordOpts{
		flags: MFTFlagInUse | MFTFlagDirectory, name: "dir", namespace: NamespaceWin32,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30",
			buildIndexRootValue(4096))},
	})
	f.Add(dir, uint32(DefaultMFTRecordSize))

	f.Add(make([]byte, DefaultMFTRecordSize), uint32(DefaultMFTRecordSize))
	f.Add([]byte{}, uint32(DefaultMFTRecordSize))
	f.Add(make([]byte, 48), uint32(48))
	f.Add(dir, uint32(0))
	f.Add(dir, uint32(0xFFFFFFFF))

	f.Fuzz(func(t *testing.T, data []byte, recordSize uint32) {
		entry, err := ParseMFTRecord(data, 0, recordSize)
		if err != nil {
			return
		}
		if entry == nil {
			t.Fatal("ParseMFTRecord returned no entry and no error")
		}

		// Whatever the record claimed, the parse must not have invented
		// attributes that reach outside the buffer it was given.
		for _, attr := range entry.Attributes {
			if attr == nil {
				t.Fatal("parsed a nil attribute")
			}
			if attr.Resident != nil && len(attr.Resident.Value) > len(data) {
				t.Fatalf("resident attribute holds %d bytes of a %d byte record",
					len(attr.Resident.Value), len(data))
			}
		}

		// The directory-listing path calls these on every entry it meets, so
		// they must be total on anything that parsed.
		_ = entry.IsDirectory()
		_ = entry.IsInUse()
		_ = entry.FindAttribute(AttrTypeIndexRoot, "$I30")
		_ = entry.FindPrimaryDataAttribute()
		_, _ = entry.GetFileName()
		_, _ = entry.GetStandardInformation()
		_ = entry.DataStreams()
	})
}
