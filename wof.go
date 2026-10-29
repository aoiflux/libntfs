package libntfs

import "fmt"

// Windows Overlay Filter (WOF) system compression, which Windows calls
// CompactOS and `compact /c /exe:...` applies.
//
// A compacted file keeps nothing in its unnamed $DATA stream: that stream is
// sparse and reads as zeros. The content lives compressed in a named stream
// called WofCompressedData, and an IO_REPARSE_TAG_WOF reparse point says which
// algorithm compressed it. A reader that takes the unnamed stream for the
// content therefore returns zeros of the right length and no error, which is
// worse than failing.
//
// The layout here follows Eric Biggers' ntfs-3g-system-compression, the
// reference implementation: WOF_EXTERNAL_INFO in the reparse data, then one
// structure per provider.
const (
	// WOFCompressedDataStream is the named $DATA stream holding the compressed
	// content. [File.OpenStream] reaches it, and reading it yields the
	// compressed bytes rather than the file's content.
	WOFCompressedDataStream = "WofCompressedData"

	// WOFCurrentVersion is the only WOF_EXTERNAL_INFO version Windows writes.
	WOFCurrentVersion = 1

	// WOFProviderWIM marks content held in a WIM archive elsewhere, as a
	// Windows installation image's files are. Nothing on this volume holds it,
	// so such a file can never be read from the volume alone.
	WOFProviderWIM = 1

	// WOFProviderFile marks content held in this file's own
	// WofCompressedData stream, which is what CompactOS produces.
	WOFProviderFile = 2

	// WOFFileProviderCurrentVersion is the only file-provider structure version
	// Windows writes.
	WOFFileProviderCurrentVersion = 1

	// wofExternalInfoSize is WOF_EXTERNAL_INFO: version, then provider.
	wofExternalInfoSize = 8

	// wofFileProviderInfoSize is the file provider's structure: version, then
	// compression format.
	wofFileProviderInfoSize = 8
)

// WOFAlgorithm is the compression format a WOF file-provider reparse point
// names. The three XPRESS formats differ only in chunk size; LZX compresses
// harder over larger chunks and is what `compact /exe:lzx` selects.
type WOFAlgorithm uint32

// The FILE_PROVIDER_COMPRESSION_FORMAT values.
const (
	WOFXpress4K  WOFAlgorithm = 0
	WOFLZX       WOFAlgorithm = 1
	WOFXpress8K  WOFAlgorithm = 2
	WOFXpress16K WOFAlgorithm = 3
)

// String returns the name Windows uses for the format, as `compact /exe:`
// spells it.
func (a WOFAlgorithm) String() string {
	switch a {
	case WOFXpress4K:
		return "XPRESS4K"
	case WOFLZX:
		return "LZX"
	case WOFXpress8K:
		return "XPRESS8K"
	case WOFXpress16K:
		return "XPRESS16K"
	default:
		return fmt.Sprintf("WOFAlgorithm(%d)", uint32(a))
	}
}

// ChunkSize returns the uncompressed size of each chunk this format divides the
// content into, which is also the window a chunk's matches may reach back over.
// It returns 0 for a format libntfs does not know.
func (a WOFAlgorithm) ChunkSize() int {
	switch a {
	case WOFXpress4K:
		return 1 << 12
	case WOFXpress8K:
		return 1 << 13
	case WOFXpress16K:
		return 1 << 14
	case WOFLZX:
		return 1 << 15
	default:
		return 0
	}
}

// WOFInfo describes a WOF reparse point: the WOF_EXTERNAL_INFO header and, for
// [WOFProviderFile], the file provider's version and compression format.
//
// Algorithm is meaningful only when Provider is [WOFProviderFile]; a
// [WOFProviderWIM] file's content is not on this volume at all.
type WOFInfo struct {
	Version     uint32       `json:"version"`
	Provider    uint32       `json:"provider"`
	FileVersion uint32       `json:"file_version"`
	Algorithm   WOFAlgorithm `json:"algorithm"`
}

// ParseWOFExternalInfo decodes the data of an [ReparsePointWOF] reparse point,
// that is [ReparsePoint.Data] with the header and any GUID already stripped.
//
// Only the versions and providers Windows writes are accepted. A value libntfs
// does not recognise is an error rather than a guess, because the alternative
// is decoding a stream with the wrong algorithm and reporting the result as
// content.
func ParseWOFExternalInfo(data []byte) (WOFInfo, error) {
	var info WOFInfo

	if len(data) < wofExternalInfoSize {
		return info, fmt.Errorf("%w: WOF external info is %d bytes, need %d",
			ErrInvalidReparsePoint, len(data), wofExternalInfoSize)
	}

	info.Version = ReadUint32LE(data, 0)
	info.Provider = ReadUint32LE(data, 4)

	if info.Version != WOFCurrentVersion {
		return info, fmt.Errorf("%w: WOF external info version %d, want %d",
			ErrInvalidReparsePoint, info.Version, WOFCurrentVersion)
	}

	switch info.Provider {
	case WOFProviderWIM:
		// The provider's own structure describes a resource in a WIM file that
		// is not on this volume, so there is nothing here worth decoding.
		return info, nil

	case WOFProviderFile:
		if len(data) < wofExternalInfoSize+wofFileProviderInfoSize {
			return info, fmt.Errorf("%w: WOF file provider info is %d bytes, need %d",
				ErrInvalidReparsePoint, len(data)-wofExternalInfoSize, wofFileProviderInfoSize)
		}

		info.FileVersion = ReadUint32LE(data, wofExternalInfoSize)
		info.Algorithm = WOFAlgorithm(ReadUint32LE(data, wofExternalInfoSize+4))

		if info.FileVersion != WOFFileProviderCurrentVersion {
			return info, fmt.Errorf("%w: WOF file provider version %d, want %d",
				ErrInvalidReparsePoint, info.FileVersion, WOFFileProviderCurrentVersion)
		}
		if info.Algorithm.ChunkSize() == 0 {
			return info, fmt.Errorf("%w: WOF compression format %d",
				ErrInvalidReparsePoint, uint32(info.Algorithm))
		}

		return info, nil

	default:
		return info, fmt.Errorf("%w: WOF provider %d",
			ErrInvalidReparsePoint, info.Provider)
	}
}

// wofState is what a File remembers about a WOF reparse point on its entry.
//
// It is resolved once, where the File is built, rather than on every read: the
// reparse point is a separate attribute, and consulting it per ReadAt would put
// an attribute lookup on the hot path of every read on the volume.
//
// A nil *wofState means the file is not WOF-compacted. A non-nil one with a nil
// Info means the reparse point is a WOF tag libntfs could not decode, which is
// still a reason to refuse the unnamed stream: whatever the content is, it is
// not the zeros that stream holds.
type wofState struct {
	info *WOFInfo
	err  error
}

// blockingError returns the error a read of the unnamed stream should fail
// with, naming the algorithm when it is known.
func (w *wofState) blockingError() error {
	if w == nil {
		return nil
	}
	if w.info == nil {
		return fmt.Errorf("%w: unreadable WOF reparse point: %v", ErrWOFCompressed, w.err)
	}
	if w.info.Provider == WOFProviderWIM {
		return fmt.Errorf("%w: content is held in a WIM archive, not on this volume",
			ErrWOFCompressed)
	}

	return fmt.Errorf("%w: content is %s in the %s stream",
		ErrWOFCompressed, w.info.Algorithm, WOFCompressedDataStream)
}

// resolveWOF reports the WOF reparse point on an entry, or nil when it has
// none.
//
// The reparse point is almost always resident, so this costs no I/O in the
// common case; a non-resident one costs the read that File.ReparsePoint would.
func (v *Volume) resolveWOF(e *MFTEntry) *wofState {
	if e == nil {
		return nil
	}

	attr := e.FindAttribute(AttrTypeReparsePoint, "")
	if attr == nil {
		return nil
	}

	value, err := v.readAttributeValue(attr, maxReparseValueBytes)
	if err != nil {
		// An unreadable $REPARSE_POINT says nothing about which tag it carries,
		// so it cannot be assumed to be WOF.
		return nil
	}

	return wofStateFromReparseValue(value)
}

// wofStateFromReparseValue decides whether a $REPARSE_POINT value is a WOF
// reparse point, and what it says.
func wofStateFromReparseValue(value []byte) *wofState {
	reparse, err := ParseReparsePoint(value)
	if err != nil || reparse.ReparseType != ReparsePointWOF {
		return nil
	}

	info, err := ParseWOFExternalInfo(reparse.Data)
	if err != nil {
		return &wofState{err: err}
	}

	return &wofState{info: &info}
}

// applyWOFSupport marks a stream's read support unreadable when WOF holds its
// content elsewhere. It is a no-op for every stream that is not the unnamed
// $DATA of a compacted file.
func applyWOFSupport(support *FileReadSupport, state *wofState) {
	if state == nil {
		return
	}

	support.WOFCompressed = true
	support.Readable = false
	support.BlockingError = state.blockingError()
}

// wofStateForStream returns the WOF state that applies to one $DATA stream of
// the entry.
//
// It applies to the unnamed stream alone. The WofCompressedData stream holds
// the compressed bytes, and a caller decoding them itself has to be able to
// read it; so does this library. Any other named stream is an ordinary
// alternate data stream that WOF does not touch.
func wofStateForStream(state *wofState, attr *Attribute) *wofState {
	if state == nil || attributeName(attr) != "" {
		return nil
	}

	return state
}
