package libntfs

import "testing"

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
