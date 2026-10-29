package libntfs

import (
	"errors"
	"strings"
	"testing"
)

// A file compacted by the Windows Overlay Filter keeps nothing in its unnamed
// $DATA stream: that stream is sparse, and the content lives compressed in the
// WofCompressedData named stream. libntfs reported such a file as readable and
// returned zeros of its full size up to v0.3.5 (NTFS-U1).

// buildWOFExternalInfo builds the reparse data of a WOF reparse point:
// WOF_EXTERNAL_INFO, then the provider's own structure.
func buildWOFExternalInfo(version, provider, fileVersion, algorithm uint32) []byte {
	data := make([]byte, 16)
	WriteUint32LE(data, 0, version)
	WriteUint32LE(data, 4, provider)
	WriteUint32LE(data, 8, fileVersion)
	WriteUint32LE(data, 12, algorithm)
	return data
}

// buildSparseDataAttr builds a wholly sparse non-resident $DATA attribute,
// which is what a WOF file's unnamed stream looks like: an allocation with no
// clusters behind it and a real size equal to the uncompressed content.
func buildSparseDataAttr(t testing.TB, attrID uint16, clusters uint64,
	clusterSize uint32, realSize uint64,
) []byte {
	t.Helper()

	if clusters > 0x7F {
		t.Fatalf("test data run needs single-byte fields, got %d clusters", clusters)
	}

	const dataRunOffset = NonResidentAttributeHeaderSize
	runs := []byte{0x01, byte(clusters), 0x00} // Sparse run: length only, no offset
	length := AlignUp(dataRunOffset+len(runs), 8)
	attr := make([]byte, length)

	WriteUint32LE(attr, 0, AttrTypeData)
	WriteUint32LE(attr, 4, uint32(length))
	attr[8] = 1 // Non-resident
	attr[9] = 0 // The unnamed stream
	WriteUint16LE(attr, 10, NonResidentAttributeHeaderSize)
	WriteUint16LE(attr, 14, attrID)
	WriteUint64LE(attr, 16, 0)
	WriteUint64LE(attr, 24, clusters-1)
	WriteUint16LE(attr, 32, dataRunOffset)
	WriteUint64LE(attr, 40, clusters*uint64(clusterSize))
	WriteUint64LE(attr, 48, realSize)
	WriteUint64LE(attr, 56, realSize)
	WriteUint16LE(attr, 12, AttrFlagSparse) // The flags field; 0x8000 marks it sparse
	copy(attr[dataRunOffset:], runs)

	return attr
}

// wofFixture is a volume carrying one WOF-compacted file and one ordinary file,
// so a test can check that the refusal is confined to the WOF file.
type wofFixture struct {
	vol *Volume

	// wofEntry is the WOF-compacted file, plainEntry an ordinary one.
	wofEntry, plainEntry uint64

	// uncompressedSize is the WOF file's real content length, which its unnamed
	// $DATA reports while holding nothing.
	uncompressedSize uint64

	// compressedData is the bytes placed in the WofCompressedData stream.
	compressedData []byte
}

// newWOFFixture builds an image whose entry 1 is a WOF-compacted file using the
// given algorithm and entry 2 an ordinary non-resident file.
func newWOFFixture(t testing.TB, algorithm uint32) wofFixture {
	t.Helper()

	const (
		clusterSize      = uint32(4096)
		uncompressedSize = uint64(200000)
	)

	compressed := incompressible(int(clusterSize))

	wofRecord := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "compacted.bin",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildSparseDataAttr(t, 2, 49, clusterSize, uncompressedSize),
			buildNamedNonResidentDataAttr(t, 3, WOFCompressedDataStream, 8, 1,
				clusterSize, uint64(len(compressed))),
			buildResidentAttr(AttrTypeReparsePoint, 4, buildReparseValue(ReparsePointWOF,
				buildWOFExternalInfo(WOFCurrentVersion, WOFProviderFile,
					WOFFileProviderCurrentVersion, algorithm))),
		},
	})

	plainRecord := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "ordinary.bin",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildNonResidentDataAttr(t, 2, 9, 1, clusterSize),
		},
	})

	image := buildTestNTFSImage(t, [][]byte{wofRecord, plainRecord})
	copy(image[8*int(clusterSize):], compressed)

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("opening the WOF fixture failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	return wofFixture{
		vol:              vol,
		wofEntry:         1,
		plainEntry:       2,
		uncompressedSize: uncompressedSize,
		compressedData:   compressed,
	}
}

// TestParseWOFExternalInfo decodes the reparse data of a WOF reparse point.
func TestParseWOFExternalInfo(t *testing.T) {
	t.Run("each algorithm", func(t *testing.T) {
		for _, tc := range []struct {
			algorithm WOFAlgorithm
			name      string
			chunkSize int
		}{
			{WOFXpress4K, "XPRESS4K", 4096},
			{WOFLZX, "LZX", 32768},
			{WOFXpress8K, "XPRESS8K", 8192},
			{WOFXpress16K, "XPRESS16K", 16384},
		} {
			info, err := ParseWOFExternalInfo(buildWOFExternalInfo(
				WOFCurrentVersion, WOFProviderFile, WOFFileProviderCurrentVersion,
				uint32(tc.algorithm)))
			if err != nil {
				t.Fatalf("%s: ParseWOFExternalInfo failed: %v", tc.name, err)
			}
			if info.Algorithm != tc.algorithm {
				t.Errorf("%s: algorithm = %d, want %d", tc.name, info.Algorithm, tc.algorithm)
			}
			if got := info.Algorithm.String(); got != tc.name {
				t.Errorf("algorithm %d named %q, want %q", tc.algorithm, got, tc.name)
			}
			if got := info.Algorithm.ChunkSize(); got != tc.chunkSize {
				t.Errorf("%s: chunk size = %d, want %d", tc.name, got, tc.chunkSize)
			}
			if info.Provider != WOFProviderFile {
				t.Errorf("%s: provider = %d, want %d", tc.name, info.Provider, WOFProviderFile)
			}
		}
	})

	t.Run("the WIM provider parses and names itself", func(t *testing.T) {
		// A WIM-backed file's content is in a WIM elsewhere, not on this
		// volume, so it parses but can never be decoded from here.
		info, err := ParseWOFExternalInfo(buildWOFExternalInfo(
			WOFCurrentVersion, WOFProviderWIM, 1, 0))
		if err != nil {
			t.Fatalf("ParseWOFExternalInfo failed on the WIM provider: %v", err)
		}
		if info.Provider != WOFProviderWIM {
			t.Fatalf("provider = %d, want %d", info.Provider, WOFProviderWIM)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		cases := map[string][]byte{
			"empty":                  {},
			"header only":            buildWOFExternalInfo(WOFCurrentVersion, WOFProviderFile, 0, 0)[:8],
			"short of the algorithm": buildWOFExternalInfo(WOFCurrentVersion, WOFProviderFile, 1, 0)[:12],
			"unknown version": buildWOFExternalInfo(99, WOFProviderFile,
				WOFFileProviderCurrentVersion, 0),
			"unknown provider": buildWOFExternalInfo(WOFCurrentVersion, 99,
				WOFFileProviderCurrentVersion, 0),
			"unknown file provider version": buildWOFExternalInfo(WOFCurrentVersion,
				WOFProviderFile, 99, 0),
			"unknown algorithm": buildWOFExternalInfo(WOFCurrentVersion, WOFProviderFile,
				WOFFileProviderCurrentVersion, 99),
		}

		for name, data := range cases {
			if _, err := ParseWOFExternalInfo(data); err == nil {
				t.Errorf("%s: ParseWOFExternalInfo accepted %x", name, data)
			} else if !errors.Is(err, ErrInvalidReparsePoint) {
				t.Errorf("%s: error %v does not match ErrInvalidReparsePoint", name, err)
			}
		}
	})
}

// TestWOFFileReportsUnreadable is the headline: a WOF file must not claim its
// unnamed stream is readable, because reading it yields zeros.
func TestWOFFileReportsUnreadable(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFXpress4K))

	file, err := fixture.vol.Open(fixture.wofEntry)
	if err != nil {
		t.Fatalf("opening the WOF file failed: %v", err)
	}

	support := file.ReadSupport()
	if support.Readable {
		t.Error("ReadSupport reports the unnamed stream readable; reading it returns zeros")
	}
	if !support.WOFCompressed {
		t.Error("ReadSupport does not report the file as WOF-compressed")
	}
	if !errors.Is(support.BlockingError, ErrWOFCompressed) {
		t.Fatalf("BlockingError = %v, want one matching ErrWOFCompressed", support.BlockingError)
	}
	if !strings.Contains(support.BlockingError.Error(), "XPRESS4K") {
		t.Errorf("BlockingError %q does not name the algorithm", support.BlockingError)
	}

	// The stream is still described accurately: sparse, non-resident, present.
	if !support.HasData || !support.NonResident || !support.Sparse {
		t.Errorf("ReadSupport = %+v, want a sparse non-resident stream", support)
	}
}

// TestWOFFileReadReturnsError checks the read path rather than the advisory
// verdict. ReadAt re-derives its own decision, so both have to refuse.
func TestWOFFileReadReturnsError(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFLZX))

	file, err := fixture.vol.Open(fixture.wofEntry)
	if err != nil {
		t.Fatalf("opening the WOF file failed: %v", err)
	}

	buf := make([]byte, 64)

	n, err := file.ReadAt(buf, 0)
	if !errors.Is(err, ErrWOFCompressed) {
		t.Fatalf("ReadAt = (%d, %v), want an error matching ErrWOFCompressed", n, err)
	}
	if n != 0 {
		t.Errorf("ReadAt reported %d bytes alongside the refusal", n)
	}

	if _, err := file.Read(buf); !errors.Is(err, ErrWOFCompressed) {
		t.Errorf("Read = %v, want an error matching ErrWOFCompressed", err)
	}
	if _, err := file.ReadAll(); !errors.Is(err, ErrWOFCompressed) {
		t.Errorf("ReadAll = %v, want an error matching ErrWOFCompressed", err)
	}

	// Reading in the middle of the stream must refuse too, not fall through to
	// a sparse read of zeros.
	if _, err := file.ReadAt(buf, int64(fixture.uncompressedSize/2)); !errors.Is(err, ErrWOFCompressed) {
		t.Errorf("ReadAt mid-stream = %v, want an error matching ErrWOFCompressed", err)
	}
}

// TestWOFFileStillListed checks that refusing the content does not hide the
// file: its name, size and times stay available, which is what a forensic
// caller needs in order to report it as unavailable.
func TestWOFFileStillListed(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFXpress16K))

	file, err := fixture.vol.Open(fixture.wofEntry)
	if err != nil {
		t.Fatalf("opening the WOF file failed: %v", err)
	}

	if got := file.Name(); got != "compacted.bin" {
		t.Errorf("Name = %q, want %q", got, "compacted.bin")
	}
	if got := file.Size(); uint64(got) != fixture.uncompressedSize {
		t.Errorf("Size = %d, want the uncompressed size %d", got, fixture.uncompressedSize)
	}
	if file.IsDirectory() {
		t.Error("the WOF file reports as a directory")
	}
	if !file.IsReparsePoint() {
		t.Error("the WOF file does not report a reparse point")
	}

	reparse, err := file.ReparsePoint()
	if err != nil {
		t.Fatalf("ReparsePoint failed: %v", err)
	}
	if reparse.TagName() != "WOF" {
		t.Errorf("tag named %q, want WOF", reparse.TagName())
	}

	// Both streams are still described, with their sizes, which is what lets a
	// caller record the content as unavailable rather than lose the file.
	streams := file.Streams()
	if len(streams) != 2 {
		t.Fatalf("the WOF file reports %d streams, want 2", len(streams))
	}
	if streams[0].Name != "" || streams[0].Size != fixture.uncompressedSize {
		t.Errorf("the unnamed stream is %q of %d bytes, want the unnamed one of %d",
			streams[0].Name, streams[0].Size, fixture.uncompressedSize)
	}
	if streams[0].Support.Readable || !streams[0].Support.WOFCompressed {
		t.Errorf("the unnamed stream is listed as %+v, want unreadable and WOF-compressed",
			streams[0].Support)
	}
	if streams[1].Name != WOFCompressedDataStream || !streams[1].Support.Readable {
		t.Errorf("the second stream is %q, readable %v; want %q, readable",
			streams[1].Name, streams[1].Support.Readable, WOFCompressedDataStream)
	}
}

// TestWOFCompressedDataStreamStaysReadable is the escape hatch, and the reason
// the refusal is confined to the unnamed stream: a caller that wants to decode
// the content itself still needs the compressed bytes.
func TestWOFCompressedDataStreamStaysReadable(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFXpress4K))

	file, err := fixture.vol.Open(fixture.wofEntry)
	if err != nil {
		t.Fatalf("opening the WOF file failed: %v", err)
	}

	stream, err := file.OpenStream(WOFCompressedDataStream)
	if err != nil {
		t.Fatalf("OpenStream(%q) failed: %v", WOFCompressedDataStream, err)
	}

	if support := stream.ReadSupport(); !support.Readable {
		t.Fatalf("the compressed stream reports unreadable: %v", support.BlockingError)
	}
	if support := stream.ReadSupport(); support.WOFCompressed {
		t.Error("the compressed stream reports itself WOF-compressed; it holds the compressed bytes")
	}

	got, err := stream.ReadAll()
	if err != nil {
		t.Fatalf("reading the compressed stream failed: %v", err)
	}
	if len(got) != len(fixture.compressedData) {
		t.Fatalf("read %d bytes from the compressed stream, want %d",
			len(got), len(fixture.compressedData))
	}
	if diff := firstDifference(got, fixture.compressedData); diff != len(got) {
		t.Errorf("the compressed stream differs from the bytes on disk at offset %d", diff)
	}

	// The unnamed stream reached through OpenStream("") is the content stream,
	// so it must refuse exactly as the file itself does.
	unnamed, err := file.OpenStream("")
	if err != nil {
		t.Fatalf("OpenStream(\"\") failed: %v", err)
	}
	if _, err := unnamed.ReadAt(make([]byte, 8), 0); !errors.Is(err, ErrWOFCompressed) {
		t.Errorf("the unnamed stream read = %v, want an error matching ErrWOFCompressed", err)
	}
}

// TestWOFStreamsAgreeWithReadSupport guards against Streams() and ReadSupport()
// contradicting each other, which they would if only one consulted the reparse
// point.
func TestWOFStreamsAgreeWithReadSupport(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFXpress8K))

	entry, err := fixture.vol.GetMFTEntry(fixture.wofEntry)
	if err != nil {
		t.Fatalf("GetMFTEntry failed: %v", err)
	}

	streams := fixture.vol.streamsFromEntry(fixture.wofEntry, entry, fixture.vol.resolveWOF(entry))
	if len(streams) != 2 {
		t.Fatalf("entry has %d streams, want 2", len(streams))
	}

	if streams[0].Name != "" {
		t.Fatalf("the first stream is %q, want the unnamed one first", streams[0].Name)
	}
	if streams[0].Support.Readable {
		t.Error("the unnamed stream is listed readable; reading it returns zeros")
	}
	if !streams[0].Support.WOFCompressed {
		t.Error("the unnamed stream is not listed as WOF-compressed")
	}

	if streams[1].Name != WOFCompressedDataStream {
		t.Fatalf("the second stream is %q, want %q", streams[1].Name, WOFCompressedDataStream)
	}
	if !streams[1].Support.Readable {
		t.Error("the compressed stream is listed unreadable")
	}
}

// TestNonWOFFilesUnaffected checks that an ordinary file, and a file carrying a
// different reparse tag, still read.
func TestNonWOFFilesUnaffected(t *testing.T) {
	fixture := newWOFFixture(t, uint32(WOFXpress4K))

	file, err := fixture.vol.Open(fixture.plainEntry)
	if err != nil {
		t.Fatalf("opening the ordinary file failed: %v", err)
	}

	support := file.ReadSupport()
	if !support.Readable {
		t.Fatalf("the ordinary file reports unreadable: %v", support.BlockingError)
	}
	if support.WOFCompressed {
		t.Error("the ordinary file reports itself WOF-compressed")
	}
	if _, err := file.ReadAll(); err != nil {
		t.Errorf("reading the ordinary file failed: %v", err)
	}
}

// TestSymlinkReparseStillReads checks a reparse tag that is not WOF: its
// unnamed $DATA is still its content, so nothing should change for it.
func TestSymlinkReparseStillReads(t *testing.T) {
	const clusterSize = uint32(4096)

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "link.txt",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildResidentAttr(AttrTypeData, 2, []byte("link content")),
			buildResidentAttr(AttrTypeReparsePoint, 3,
				buildReparseValue(ReparsePointSymlink, make([]byte, 20))),
		},
	})

	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{record})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	file, err := vol.Open(1)
	if err != nil {
		t.Fatalf("opening the symlink failed: %v", err)
	}

	if support := file.ReadSupport(); !support.Readable || support.WOFCompressed {
		t.Fatalf("ReadSupport = %+v, want a readable non-WOF stream", support)
	}

	got, err := file.ReadAll()
	if err != nil {
		t.Fatalf("reading the symlink's $DATA failed: %v", err)
	}
	if string(got) != "link content" {
		t.Errorf("content = %q, want %q", got, "link content")
	}
	_ = clusterSize
}

// TestWOFDamagedReparseDataStaysUnreadable covers a WOF reparse point libntfs
// cannot make sense of. The file must not fall back to reading zeros out of the
// sparse unnamed stream and calling them content.
func TestWOFDamagedReparseDataStaysUnreadable(t *testing.T) {
	const clusterSize = uint32(4096)

	record := buildTestMFTRecord(t, testRecordOpts{
		flags:     MFTFlagInUse,
		name:      "damaged.bin",
		namespace: NamespaceWin32,
		extraAttrs: [][]byte{
			buildSparseDataAttr(t, 2, 4, clusterSize, 16384),
			// A WOF tag whose reparse data is truncated to the header.
			buildResidentAttr(AttrTypeReparsePoint, 3,
				buildReparseValue(ReparsePointWOF, []byte{1, 0, 0, 0})),
		},
	})

	vol, err := Open(&mockReaderAt{data: buildTestNTFSImage(t, [][]byte{record})})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	file, err := vol.Open(1)
	if err != nil {
		t.Fatalf("opening the damaged WOF file failed: %v", err)
	}

	support := file.ReadSupport()
	if support.Readable {
		t.Error("a WOF file with unparsable reparse data reports readable")
	}
	if !errors.Is(support.BlockingError, ErrWOFCompressed) {
		t.Fatalf("BlockingError = %v, want one matching ErrWOFCompressed", support.BlockingError)
	}

	if _, err := file.ReadAt(make([]byte, 16), 0); !errors.Is(err, ErrWOFCompressed) {
		t.Errorf("ReadAt = %v, want an error matching ErrWOFCompressed", err)
	}
}
