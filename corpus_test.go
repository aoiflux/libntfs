package libntfs

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// corpusPath is a small NTFS volume that Windows formatted and populated,
// produced by testdata/make_corpus.ps1.
//
// Every other test in this package builds NTFS structures byte by byte, which
// proves the parsers against bytes libntfs itself wrote. These tests prove them
// against bytes Windows wrote, which is the only way to catch an assumption the
// fixtures and the parser happen to share.
const corpusPath = "testdata/ntfs-corpus.img.gz"

// loadCorpus decompresses the corpus image, skipping the test when it has not
// been generated. Generating it needs an elevated Windows prompt, so it is not
// something every checkout can do on demand.
func loadCorpus(t *testing.T) []byte {
	t.Helper()

	f, err := os.Open(corpusPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s not present; generate it from an elevated PowerShell prompt with "+
			"testdata\\make_corpus.ps1", corpusPath)
	}
	if err != nil {
		t.Fatalf("opening the corpus failed: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("reading the corpus gzip header failed: %v", err)
	}
	defer gz.Close()

	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompressing the corpus failed: %v", err)
	}

	return data
}

// openCorpus opens the corpus image as a volume.
func openCorpus(t *testing.T) *Volume {
	t.Helper()

	vol, err := Open(&mockReaderAt{data: loadCorpus(t)})
	if err != nil {
		t.Fatalf("opening the corpus volume failed: %v", err)
	}

	t.Cleanup(func() { vol.Close() })

	return vol
}

// corpusFiles indexes the corpus by path so each test can find its fixture
// without walking the volume again.
func corpusFiles(t *testing.T, vol *Volume) map[string]*NTFSFile {
	t.Helper()

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report over the corpus failed: %v", err)
	}

	files := make(map[string]*NTFSFile, len(report.Files))
	for i := range report.Files {
		files[strings.ToLower(report.Files[i].Filename)] = &report.Files[i]
	}

	return files
}

func TestCorpusOpensAndReportsGeometry(t *testing.T) {
	vol := openCorpus(t)

	if vol.BytesPerSector() == 0 || vol.BytesPerCluster() == 0 {
		t.Fatalf("expected a sane geometry, got sector %d cluster %d",
			vol.BytesPerSector(), vol.BytesPerCluster())
	}
	if vol.MFTRecordSize() == 0 {
		t.Error("expected a non-zero MFT record size")
	}

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}
	if !root.IsDirectory() {
		t.Error("expected the root to be a directory")
	}

	entries, err := root.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir on the root failed: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected the root directory to have entries")
	}
}

// TestCorpusReadsFileContent checks the whole read path end to end against a
// file Windows wrote, rather than against runs the test built itself.
func TestCorpusReadsFileContent(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/host.txt")
	if err != nil {
		t.Fatalf("opening /host.txt failed: %v", err)
	}

	content, err := file.ReadAll()
	if err != nil {
		t.Fatalf("reading /host.txt failed: %v", err)
	}

	if string(content) != "visible content" {
		t.Errorf("expected %q, got %q", "visible content", content)
	}
}

// TestCorpusAlternateDataStream covers a stream Windows wrote, including the
// Zone.Identifier every downloaded file carries.
func TestCorpusAlternateDataStream(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/host.txt")
	if err != nil {
		t.Fatalf("opening /host.txt failed: %v", err)
	}

	streams := file.Streams()
	if len(streams) < 2 {
		t.Fatalf("expected an alternate stream alongside the default, got %+v", streams)
	}

	ads, err := file.OpenStream("Zone.Identifier")
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	content, err := ads.ReadAll()
	if err != nil {
		t.Fatalf("reading the alternate stream failed: %v", err)
	}
	if !strings.Contains(string(content), "ZoneId") {
		t.Errorf("expected Zone.Identifier content, got %q", content)
	}
}

// TestCorpusFragmentsTileRealFile is the fragment API's real test: a file
// Windows fragmented, whose extents must tile the stream and stop at its true
// size rather than at the end of the last cluster.
func TestCorpusFragmentsTileRealFile(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/fragmented.bin")
	if err != nil {
		t.Fatalf("opening /fragmented.bin failed: %v", err)
	}

	frags, err := file.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}
	if len(frags) == 0 {
		t.Fatal("expected at least one fragment")
	}

	var covered int64
	for i, frag := range frags {
		if frag.FileOffset != covered {
			t.Fatalf("fragment %d starts at stream offset %d, expected %d",
				i, frag.FileOffset, covered)
		}
		if frag.Length <= 0 {
			t.Fatalf("fragment %d has length %d", i, frag.Length)
		}
		covered += frag.Length
	}

	if covered != file.Size() {
		t.Errorf("fragments cover %d bytes, file is %d bytes", covered, file.Size())
	}
}

// TestCorpusSparseFileKeepsHoles checks that a hole Windows punched is reported
// rather than dropped, so stream offsets after it stay correct.
func TestCorpusSparseFileKeepsHoles(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/sparse.bin")
	if err != nil {
		t.Fatalf("opening /sparse.bin failed: %v", err)
	}

	if !file.IsSparse() {
		t.Skip("the corpus sparse file was not stored sparse on this Windows build")
	}

	frags, err := file.Fragments()
	if err != nil {
		t.Fatalf("Fragments failed: %v", err)
	}

	holes := 0
	var covered int64
	for _, frag := range frags {
		if frag.Sparse {
			holes++
		}
		if frag.FileOffset != covered {
			t.Fatalf("fragment at stream offset %d leaves a gap after %d",
				frag.FileOffset, covered)
		}
		covered += frag.Length
	}

	if holes == 0 {
		t.Error("expected at least one hole reported")
	}
	if covered != file.Size() {
		t.Errorf("fragments cover %d bytes, file is %d bytes", covered, file.Size())
	}
}

// TestCorpusCompressedFileReads covers the decompressor against data NTFS
// compressed, which is the only way to be sure of the compression unit handling.
func TestCorpusCompressedFileReads(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/compressed.txt")
	if err != nil {
		t.Fatalf("opening /compressed.txt failed: %v", err)
	}

	if !file.IsCompressed() {
		t.Skip("the corpus file was not stored compressed on this Windows build")
	}

	content, err := file.ReadAll()
	if err != nil {
		t.Fatalf("reading the compressed file failed: %v", err)
	}

	if int64(len(content)) != file.Size() {
		t.Fatalf("expected %d decompressed bytes, got %d", file.Size(), len(content))
	}
	for i, b := range content {
		if b != 'A' {
			t.Fatalf("byte %d decompressed to %q, expected 'A'", i, b)
		}
	}
}

// TestCorpusReparsePoints covers a symlink and a junction Windows created.
func TestCorpusReparsePoints(t *testing.T) {
	vol := openCorpus(t)

	for _, tc := range []struct{ path, want string }{
		{path: "/link.txt", want: "SYMLINK"},
		{path: "/junction", want: "MOUNT_POINT"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			file, err := vol.OpenPath(tc.path)
			if err != nil {
				t.Skipf("opening %s failed, the fixture may not exist: %v", tc.path, err)
			}

			reparse, err := file.ReparsePoint()
			if err != nil {
				t.Fatalf("ReparsePoint failed: %v", err)
			}
			if reparse == nil {
				t.Fatalf("expected %s to carry a reparse point", tc.path)
			}

			if reparse.TagName() != tc.want {
				t.Errorf("expected tag %s, got %s", tc.want, reparse.TagName())
			}

			substitute, _, ok := reparse.Target()
			if !ok || substitute == "" {
				t.Errorf("expected %s to name a target, got %q", tc.path, substitute)
			}
		})
	}
}

// TestCorpusDeletedEntryRecovered covers recovery from index slack against an
// entry Windows deleted, rather than one the test marked deleted itself.
func TestCorpusDeletedEntryRecovered(t *testing.T) {
	vol := openCorpus(t)

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	entries, err := root.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	for _, entry := range entries {
		if strings.EqualFold(entry.Name, "deleted.txt") {
			return
		}
	}

	// Windows may reuse or fully overwrite the slack, so this is reported rather
	// than failed: its absence is not proof the recovery path is broken.
	t.Skip("deleted.txt was not recoverable from index slack in this image")
}

// TestCorpusSecurityDescriptors covers $Secure lookup against descriptors
// Windows wrote.
func TestCorpusSecurityDescriptors(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/host.txt")
	if err != nil {
		t.Fatalf("opening /host.txt failed: %v", err)
	}

	sd, err := file.SecurityDescriptor()
	if err != nil {
		t.Fatalf("SecurityDescriptor failed: %v", err)
	}
	if sd == nil {
		t.Fatal("expected a security descriptor")
	}
	if sd.Owner == nil || sd.Owner.String() == "" {
		t.Error("expected an owner SID")
	}
}

// TestCorpusLogFile covers $LogFile against a log Windows wrote, which is the
// only place the page-spanning reassembly meets real data.
func TestCorpusLogFile(t *testing.T) {
	vol := openCorpus(t)

	restart, err := vol.LogRestartArea()
	if err != nil {
		t.Fatalf("LogRestartArea failed: %v", err)
	}

	if restart.LogPageSize == 0 {
		t.Error("expected a non-zero log page size")
	}
	if len(restart.Clients) == 0 {
		t.Error("expected at least one log client")
	}

	records := 0
	operations := make(map[string]int)

	if err := vol.EachLogRecord(func(r *LogRecord) error {
		records++
		if r.IsClientRecord() {
			operations[r.RedoOperationName()]++
		}
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if records == 0 {
		t.Fatal("expected the log to hold records after the volume was populated")
	}

	// A freshly written volume must have logged index and record updates.
	t.Logf("%d log records, %d distinct redo operations", records, len(operations))
	if len(operations) < 2 {
		t.Errorf("expected several distinct operations, got %v", operations)
	}
}

// TestCorpusUSNJournal covers the change journal, if the volume has one. A
// freshly formatted volume may not until something enables it.
func TestCorpusUSNJournal(t *testing.T) {
	vol := openCorpus(t)

	if _, err := vol.OpenUSNJournal(); err != nil {
		t.Skipf("the corpus volume has no USN journal: %v", err)
	}

	records := 0
	if err := vol.EachUSNRecord(func(*USNRecord) error {
		records++
		return nil
	}); err != nil {
		t.Fatalf("EachUSNRecord failed: %v", err)
	}

	t.Logf("%d USN records", records)
}

// TestCorpusExtendedAttributes covers WSL metadata, present only when the
// generator had a WSL distribution to write through.
func TestCorpusExtendedAttributes(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/wsl-metadata.txt")
	if err != nil {
		t.Skipf("the corpus has no WSL-written file: %v", err)
	}

	attrs, err := file.LinuxAttributes()
	if err != nil {
		t.Fatalf("LinuxAttributes failed: %v", err)
	}
	if attrs == nil {
		t.Fatal("expected POSIX metadata on a file WSL wrote")
	}

	if attrs.Permissions() != 0o754 {
		t.Errorf("expected mode 0754, got %#o (%s)", attrs.Permissions(), attrs.ModeString())
	}
}

// TestCorpusLinuxExtendedAttributes is the fixture LXXATTR most needs. The
// structure is not documented by Microsoft, so libntfs decodes a layout inferred
// from what WSL writes; every other test of it feeds bytes libntfs itself built,
// which would agree with a wrong inference. This one does not.
func TestCorpusLinuxExtendedAttributes(t *testing.T) {
	vol := openCorpus(t)

	file, err := vol.OpenPath("/wsl-xattr.txt")
	if err != nil {
		t.Skipf("the corpus has no file with POSIX extended attributes: %v", err)
	}

	attrs, err := file.LinuxExtendedAttributes()
	if err != nil {
		t.Fatalf("LinuxExtendedAttributes failed: %v", err)
	}
	if len(attrs) == 0 {
		t.Fatal("expected the POSIX extended attribute WSL set")
	}

	for _, attr := range attrs {
		if attr.Name == "user.origin" {
			if string(attr.Value) != "corpus" {
				t.Errorf("expected user.origin=corpus, got %q", attr.Value)
			}
			return
		}
	}

	t.Errorf("expected user.origin among %v", attrs)
}

// TestCorpusLogTransactions groups a real log into transactions, which is where
// a grouping that looks right against synthetic records meets one that Windows
// actually wrote.
func TestCorpusLogTransactions(t *testing.T) {
	vol := openCorpus(t)

	var records []*LogRecord
	if err := vol.EachLogRecord(func(r *LogRecord) error {
		records = append(records, r)
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if len(records) == 0 {
		t.Fatal("expected the log to hold records")
	}

	transactions := GroupLogTransactions(records)
	if len(transactions) == 0 {
		t.Fatal("expected the records to group into transactions")
	}

	committed := 0
	for _, transaction := range transactions {
		if transaction.FirstLSN > transaction.LastLSN {
			t.Errorf("transaction %d spans %d..%d, which is backwards",
				transaction.ID, transaction.FirstLSN, transaction.LastLSN)
		}
		if transaction.Committed {
			committed++
		}
	}

	t.Logf("%d records in %d transactions, %d committed",
		len(records), len(transactions), committed)

	// A volume that was formatted and populated must have committed something.
	if committed == 0 {
		t.Error("expected at least one committed transaction")
	}
}

// TestCorpusLogTableDumps decodes the periodic table dumps. Only the table
// header is decoded, since entry layout varies by NTFS version, so this checks
// the header is coherent rather than what the entries mean.
func TestCorpusLogTableDumps(t *testing.T) {
	vol := openCorpus(t)

	dumps := 0

	if err := vol.EachLogRecord(func(r *LogRecord) error {
		if !r.IsTableDump() {
			return nil
		}

		table, err := r.RestartTable()
		if err != nil {
			// A dump whose payload was clipped by the circular log is expected,
			// so this is reported rather than failed.
			t.Logf("LSN %d: %s did not decode: %v", r.LSN, r.RedoOperationName(), err)
			return nil
		}

		dumps++

		if table.EntrySize == 0 {
			t.Errorf("LSN %d: table declares a zero entry size", r.LSN)
		}
		for i, entry := range table.Entries {
			if len(entry) != int(table.EntrySize) {
				t.Fatalf("LSN %d: entry %d is %d bytes, expected %d",
					r.LSN, i, len(entry), table.EntrySize)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	if dumps == 0 {
		t.Skip("the log window holds no complete table dumps")
	}

	t.Logf("%d table dumps decoded", dumps)
}

// TestCorpusReportIsComplete walks the whole volume, which exercises every
// parser at once over real structures and is where a crash on unusual metadata
// would surface.
func TestCorpusReportIsComplete(t *testing.T) {
	vol := openCorpus(t)
	files := corpusFiles(t, vol)

	for _, want := range []string{"/host.txt", "/fragmented.bin", "/docs"} {
		if _, ok := files[want]; !ok {
			t.Errorf("expected %s in the report", want)
		}
	}

	if len(files) < 25 {
		t.Errorf("expected the report to cover the fixture files, got %d entries", len(files))
	}
}
