package libntfs

import (
	"testing"
	"time"
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
