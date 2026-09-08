package libntfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// newReadOnlyFixture builds an image whose backing store implements both
// io.ReaderAt and io.WriterAt — the exact shape that silently produced a
// mutable handle on evidence before Options.ReadOnly existed.
func newReadOnlyFixture(t testing.TB) *mockReadWriterAt {
	t.Helper()

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse,
		name:       "resident.txt",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("hello resident world"))},
	})

	return &mockReadWriterAt{data: buildTestNTFSImage(t, [][]byte{record})}
}

// TestOpenPromotesWriterAt pins the behaviour Options.ReadOnly exists to opt out
// of, so the two modes are tested against each other rather than in isolation.
func TestOpenPromotesWriterAt(t *testing.T) {
	backend := newReadOnlyFixture(t)

	vol, err := Open(backend)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	if !vol.IsWritable() {
		t.Fatal("Open over an io.WriterAt should yield a writable volume")
	}
	if vol.IsReadOnly() {
		t.Fatal("Open should not produce a read-only volume")
	}
}

// TestOpenReadOnlyRefusesEveryWrite is the acceptance test for read-only mode:
// a reader that also implements io.WriterAt must not yield a handle that can
// modify the volume through any entry point.
func TestOpenReadOnlyRefusesEveryWrite(t *testing.T) {
	backend := newReadOnlyFixture(t)

	before := make([]byte, len(backend.data))
	copy(before, backend.data)

	vol, err := OpenReadOnly(backend)
	if err != nil {
		t.Fatalf("OpenReadOnly failed: %v", err)
	}
	defer vol.Close()

	if vol.IsWritable() {
		t.Fatal("a read-only volume must not report itself writable")
	}
	if !vol.IsReadOnly() {
		t.Fatal("IsReadOnly should report true")
	}

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open(1) failed: %v", err)
	}

	raw, recordOffset, err := vol.readMFTRecordRaw(1)
	if err != nil {
		t.Fatalf("readMFTRecordRaw failed: %v", err)
	}

	// Every path that can reach the underlying handle, exported or not.
	writes := []struct {
		name string
		call func() error
	}{
		{"Volume.WriteAt", func() error {
			_, err := vol.WriteAt([]byte("XX"), 0)
			return err
		}},
		{"Volume.WriteDataRuns", func() error {
			_, err := vol.WriteDataRuns([]DataRun{{StartCluster: 1, LengthClusters: 1}}, 0, []byte("XX"))
			return err
		}},
		{"File.Write", func() error {
			_, err := f.Write([]byte("XX"))
			return err
		}},
		{"File.WriteAt", func() error {
			_, err := f.WriteAt([]byte("XX"), 0)
			return err
		}},
		{"Volume.writeMFTRecord", func() error {
			return vol.writeMFTRecord(1, raw, recordOffset)
		}},
	}

	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) {
			err := write.call()

			if !errors.Is(err, ErrReadOnlyMode) {
				t.Fatalf("expected ErrReadOnlyMode, got %v", err)
			}

			// The new sentinel must not break callers already matching the old one.
			if !errors.Is(err, ErrReadOnlyVolume) {
				t.Fatalf("ErrReadOnlyMode should wrap ErrReadOnlyVolume, got %v", err)
			}
		})
	}

	if !bytes.Equal(backend.data, before) {
		t.Fatal("the backing store changed under a read-only volume")
	}
}

// TestOpenReadOnlyHidesTheWriter pins the defence that makes the mode a
// guarantee rather than a convention: the stored handle cannot be asserted back
// to an io.WriterAt, so no future code path can recover the ability to write.
func TestOpenReadOnlyHidesTheWriter(t *testing.T) {
	vol, err := OpenReadOnly(newReadOnlyFixture(t))
	if err != nil {
		t.Fatalf("OpenReadOnly failed: %v", err)
	}
	defer vol.Close()

	if _, ok := vol.reader.(io.WriterAt); ok {
		t.Fatal("a read-only volume must not hold a handle that can be asserted to io.WriterAt")
	}
	if vol.writer != nil {
		t.Fatal("a read-only volume must hold no writer")
	}
}

// TestOpenReadOnlyStillReads guards against the obvious overreach: refusing
// writes must not cost any read.
func TestOpenReadOnlyStillReads(t *testing.T) {
	const content = "hello resident world"

	vol, err := OpenReadOnly(newReadOnlyFixture(t))
	if err != nil {
		t.Fatalf("OpenReadOnly failed: %v", err)
	}
	defer vol.Close()

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open(1) failed: %v", err)
	}

	data, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != content {
		t.Fatalf("read %q, want %q", data, content)
	}
}

// TestOpenWithOptionsBaseOffset covers a volume that does not start at offset
// zero, the partition-in-a-disk-image case.
func TestOpenWithOptionsBaseOffset(t *testing.T) {
	const base = 1 << 20

	image := buildTestNTFSImage(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{
			flags:      MFTFlagInUse,
			name:       "resident.txt",
			namespace:  NamespaceWin32,
			extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("offset content"))},
		}),
	})

	disk := make([]byte, base+len(image))
	copy(disk[base:], image)

	vol, err := OpenWithOptions(&mockReaderAt{data: disk}, Options{BaseOffset: base, ReadOnly: true})
	if err != nil {
		t.Fatalf("OpenWithOptions failed: %v", err)
	}
	defer vol.Close()

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open(1) failed: %v", err)
	}

	data, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != "offset content" {
		t.Fatalf("read %q, want %q", data, "offset content")
	}

	// Offsets the library reports stay relative to the volume, not to the disk
	// image containing it, so a caller can compare them against a report taken
	// from the same partition acquired on its own.
	if got := vol.VolumeSize(); got != uint64(len(image)) {
		t.Fatalf("VolumeSize = %d, want %d", got, len(image))
	}
}

// TestOpenWithOptionsBaseOffsetWrites confirms a shifted volume writes to the
// shifted location rather than to the front of the disk.
func TestOpenWithOptionsBaseOffsetWrites(t *testing.T) {
	const (
		base     = 4096
		original = "hello resident world"
	)

	image := buildTestNTFSImage(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{
			flags:      MFTFlagInUse,
			name:       "resident.txt",
			namespace:  NamespaceWin32,
			extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte(original))},
		}),
	})

	backend := &mockReadWriterAt{data: make([]byte, base+len(image))}
	copy(backend.data[base:], image)

	vol, err := OpenWithOptions(backend, Options{BaseOffset: base})
	if err != nil {
		t.Fatalf("OpenWithOptions failed: %v", err)
	}
	defer vol.Close()

	if !vol.IsWritable() {
		t.Fatal("expected a writable volume")
	}

	f, err := vol.Open(1)
	if err != nil {
		t.Fatalf("Open(1) failed: %v", err)
	}

	if _, err := f.WriteAt([]byte("HELLO"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Nothing before the volume may be touched.
	for i := range base {
		if backend.data[i] != 0 {
			t.Fatalf("byte %d before the volume was written: %#x", i, backend.data[i])
		}
	}

	reopened, err := OpenWithOptions(backend, Options{BaseOffset: base, ReadOnly: true})
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer reopened.Close()

	rf, err := reopened.Open(1)
	if err != nil {
		t.Fatalf("reopen Open(1) failed: %v", err)
	}

	data, err := rf.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != "HELLO resident world" {
		t.Fatalf("read %q after write, want %q", data, "HELLO resident world")
	}
}

func TestOpenWithOptionsRejectsNegativeBaseOffset(t *testing.T) {
	_, err := OpenWithOptions(newReadOnlyFixture(t), Options{BaseOffset: -1})
	if !errors.Is(err, ErrInvalidOffset) {
		t.Fatalf("expected ErrInvalidOffset, got %v", err)
	}
}

// TestNonWritableHandleReportsPlainReadOnly keeps the two refusals
// distinguishable: a reader that simply cannot write is not the same thing as a
// caller who asked for evidence safety.
func TestNonWritableHandleReportsPlainReadOnly(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{
			flags:      MFTFlagInUse,
			name:       "resident.txt",
			namespace:  NamespaceWin32,
			extraAttrs: [][]byte{buildResidentAttr(AttrTypeData, 2, []byte("content"))},
		}),
	})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	if vol.IsWritable() {
		t.Fatal("a plain io.ReaderAt must not yield a writable volume")
	}
	if vol.IsReadOnly() {
		t.Fatal("a volume opened without Options.ReadOnly is not in read-only mode")
	}

	_, err = vol.WriteAt([]byte("XX"), 0)
	if !errors.Is(err, ErrReadOnlyVolume) {
		t.Fatalf("expected ErrReadOnlyVolume, got %v", err)
	}
	if errors.Is(err, ErrReadOnlyMode) {
		t.Fatal("a non-writable handle must not be reported as read-only mode")
	}
}
