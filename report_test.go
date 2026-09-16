package libntfs

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files instead of comparing against them")

// reportGoldenPath is the recorded output of Report() over the fixture below.
const reportGoldenPath = "testdata/report.golden.json"

// buildReportFixtureImage assembles an NTFS image exercising every shape
// Report() distinguishes: a resident file, a fragmented one, a sparse one, a
// directory, and a deleted entry.
//
// The entry numbers start at 12 because Report() skips the reserved system
// records below that.
func buildReportFixtureImage(t testing.TB) []byte {
	t.Helper()

	const clusterSize = 4096

	volumeRecord := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "$Volume",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildResidentAttr(AttrTypeVolumeName, 2, utf16LEFromString("TESTVOL")),
		},
	})

	resident := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "notes.txt",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildResidentAttr(AttrTypeData, 2, []byte("inline content")),
		},
	})

	// Two allocated runs with a real size short of the allocation, so the
	// golden output pins both the fragment count and the tail clamping.
	fragmented := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "big.bin",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildTwoRunDataAttr(t, 2, 20, 2, 40, 2, clusterSize, 4*clusterSize-1000),
		},
	})

	sparse := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "sparse.bin",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildSparseThenDataAttr(t, 2, "", 2, 60, 1, clusterSize, 3*clusterSize),
		},
	})

	directory := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse | MFTFlagDirectory,
		name:      "docs",
		namespace: NamespaceWin32,
	})

	deleted := buildTestMFTRecord(t, testRecordOpts{
		flags:     0, // Not in use
		name:      "deleted.txt",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNamedNonResidentDataAttr(t, 2, "", 70, 1, clusterSize, 2000),
		},
	})

	return buildTestNTFSImageAt(t, map[uint64][]byte{
		MFTEntryVolume: volumeRecord,
		12:             resident,
		13:             fragmented,
		14:             sparse,
		15:             directory,
		16:             deleted,
	})
}

// TestReportGolden pins Report()'s output, which is the library's widest surface
// and the one most easily changed by accident. It has shifted twice without a
// test watching it.
//
// Run `go test -run TestReportGolden -update` to rewrite the golden file after
// an intended change, then read the diff before committing it.
func TestReportGolden(t *testing.T) {
	image := buildReportFixtureImage(t)

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	// Two fields on the report root are deliberately volatile: Generated is the
	// moment the report was taken and LibraryVersion changes at every release.
	// Pinning either would make this test fail for reasons that have nothing to
	// do with the report's shape, so they are checked for presence and then
	// blanked. TestReportProvenance is what actually asserts their values.
	if report.Generated.IsZero() {
		t.Error("expected the report to carry a generation timestamp")
	}
	if report.LibraryVersion == "" {
		t.Error("expected the report to carry a library version")
	}
	report.Generated = time.Time{}
	report.LibraryVersion = ""

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshalling the report failed: %v", err)
	}
	got = append(got, '\n')

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(reportGoldenPath), 0o755); err != nil {
			t.Fatalf("creating the golden directory failed: %v", err)
		}
		if err := os.WriteFile(reportGoldenPath, got, 0o644); err != nil {
			t.Fatalf("writing the golden file failed: %v", err)
		}
		t.Logf("rewrote %s", reportGoldenPath)
		return
	}

	want, err := os.ReadFile(reportGoldenPath)
	if err != nil {
		t.Fatalf("reading %s failed (run with -update to create it): %v", reportGoldenPath, err)
	}

	// Compare on content rather than on line endings. .gitattributes keeps git
	// from rewriting the fixture, but a test that fails because of how a file
	// was checked out is testing the wrong thing, and this one did: it passed on
	// Linux and macOS and failed on Windows.
	if normalizeNewlines(string(got)) != normalizeNewlines(string(want)) {
		t.Errorf("Report() output changed.\n--- want ---\n%s\n--- got ---\n%s\n"+
			"If the change is intended, rerun with -update and review the diff.", want, got)
	}
}

// TestReportFragmentsExcludeHoles pins the one place Report() deliberately
// differs from (*File).Fragments: it describes physical regions, so a hole has
// nothing to contribute.
func TestReportFragmentsExcludeHoles(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildReportFixtureImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	var sparse *NTFSFile
	for i := range report.Files {
		if filepath.Base(report.Files[i].Filename) == "sparse.bin" {
			sparse = &report.Files[i]
			break
		}
	}

	if sparse == nil {
		t.Fatal("expected sparse.bin in the report")
	}

	if len(sparse.Fragments) != 1 {
		t.Fatalf("expected the hole dropped, leaving 1 fragment, got %d: %+v",
			len(sparse.Fragments), sparse.Fragments)
	}
	if sparse.Fragments[0].Sparse {
		t.Errorf("expected an allocated fragment, got %+v", sparse.Fragments[0])
	}
	if sparse.IsFragmented {
		t.Error("expected a single allocated run not to count as fragmented")
	}
}

// normalizeNewlines makes a comparison indifferent to CRLF, so a golden test
// checks the output rather than the checkout.
func normalizeNewlines(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

// TestReportProvenance covers what TestReportGolden deliberately blanks.
//
// The golden test zeroes Generated and LibraryVersion so a release or the
// passage of a second does not fail it, which means it would keep passing if
// neither field were ever assigned. This is the test that says they are.
func TestReportProvenance(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildReportFixtureImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	before := time.Now().UTC()
	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	after := time.Now().UTC()

	if report.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", report.SchemaVersion, ReportSchemaVersion)
	}
	if report.SchemaVersion == 0 {
		t.Error("SchemaVersion must be non-zero, or a consumer cannot tell it apart from an absent field")
	}
	if report.LibraryVersion != Version {
		t.Errorf("LibraryVersion = %q, want %q", report.LibraryVersion, Version)
	}
	if report.Generated.Before(before) || report.Generated.After(after) {
		t.Errorf("Generated = %v, want a time within [%v, %v]", report.Generated, before, after)
	}
	if loc := report.Generated.Location(); loc != time.UTC {
		t.Errorf("Generated is in %v, want UTC so two reports are comparable", loc)
	}
}

// TestNTFSReportRoundTrip pins the one property a machine consumer depends on:
// that a document written today can be read back and still say which schema it
// was written against.
func TestNTFSReportRoundTrip(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildReportFixtureImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshalling the report failed: %v", err)
	}

	var decoded NTFSReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshalling the report failed: %v", err)
	}

	if decoded.SchemaVersion == 0 {
		t.Fatal("SchemaVersion did not survive the round trip")
	}
	if decoded.SchemaVersion != report.SchemaVersion {
		t.Errorf("SchemaVersion = %d after the round trip, want %d",
			decoded.SchemaVersion, report.SchemaVersion)
	}
	if decoded.LibraryVersion != report.LibraryVersion {
		t.Errorf("LibraryVersion = %q after the round trip, want %q",
			decoded.LibraryVersion, report.LibraryVersion)
	}
	if !decoded.Generated.Equal(report.Generated) {
		t.Errorf("Generated = %v after the round trip, want %v",
			decoded.Generated, report.Generated)
	}
	if len(decoded.Files) != len(report.Files) {
		t.Errorf("Files = %d after the round trip, want %d",
			len(decoded.Files), len(report.Files))
	}
}
