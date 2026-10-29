package libntfs

import (
	"fmt"
	"strings"
)

// StreamInfo describes a single $DATA stream on a file.
//
// A file always has an unnamed stream, reported with an empty Name, and may
// carry any number of alternate data streams alongside it. Directories have no
// unnamed stream but can still carry alternate streams.
type StreamInfo struct {
	Name          string          `json:"name"`           // Stream name; empty for the default unnamed stream
	Size          uint64          `json:"size"`           // Stream length in bytes
	AllocatedSize uint64          `json:"allocated_size"` // Allocated size on disk; equals Size for resident streams
	Resident      bool            `json:"resident"`       // True when the content lives inside the MFT record
	Support       FileReadSupport `json:"support"`
}

// IsAlternate reports whether this is an alternate data stream rather than the
// file's default unnamed stream.
func (s StreamInfo) IsAlternate() bool {
	return s.Name != ""
}

// DataStreams returns every $DATA attribute on the entry, including alternate
// data streams. It needs no Volume, so it composes with ParseMFTRecord to
// enumerate stream names from a standalone $MFT, though reading their content
// still requires the volume the records came from.
func (e *MFTEntry) DataStreams() []*Attribute {
	return e.FindAllAttributes(AttrTypeData)
}

// Streams lists the file's $DATA streams, unnamed first when present.
func (f *File) Streams() []StreamInfo {
	return f.volume.streamsFromEntry(f.entryNum, f.entry, f.volume.resolveWOF(f.entry))
}

// streamsFromEntry describes an entry's $DATA streams without needing a File,
// so a report over the whole MFT can list streams without opening every record.
//
// wof is the entry's Windows Overlay Filter reparse point, or nil when it has
// none. It is passed in rather than resolved here so that this and
// [File.ReadSupport] cannot disagree about whether a stream holds content: the
// unnamed stream of a compacted file is listed unreadable in both. A
// compressed stream is judged by the same compressionUnitError as a read.
func (v *Volume) streamsFromEntry(entryNum uint64, e *MFTEntry, wof *wofState) []StreamInfo {
	if e == nil {
		return nil
	}

	attrs := e.DataStreams()
	streams := make([]StreamInfo, 0, len(attrs))

	for _, attr := range attrs {
		info := StreamInfo{
			Name:     attributeName(attr),
			Resident: attr.Resident != nil,
			Support:  attr.ReadSupport(),
		}
		v.applyCompressionSupport(&info.Support, entryNum, attr)
		applyWOFSupport(&info.Support, wofStateForStream(wof, attr))

		if attr.Resident != nil {
			info.Size = uint64(len(attr.Resident.Value))
			info.AllocatedSize = info.Size
		} else if attr.NonResident != nil {
			info.Size = attr.NonResident.RealSize
			info.AllocatedSize = attr.NonResident.AllocatedSize
		}

		if info.Name == "" {
			// Keep the default stream first regardless of on-disk attribute order.
			streams = append([]StreamInfo{info}, streams...)
			continue
		}

		streams = append(streams, info)
	}

	return streams
}

// OpenStream opens one of the file's $DATA streams by name, returning a File
// that reads that stream. An empty name selects the default unnamed stream.
//
// Stream names are matched case-insensitively, matching NTFS itself. A name
// with no matching stream returns ErrAttributeNotFound.
//
// The returned File supports the full read API, including compressed and sparse
// streams, and carries its own read offset independent of the file it came from.
func (f *File) OpenStream(name string) (*File, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if name == "" {
		// Return an independent handle rather than the receiver, so the caller's
		// read offset is its own.
		return &File{
			volume:   f.volume,
			entry:    f.entry,
			entryNum: f.entryNum,
			name:     f.name,
			isDir:    f.isDir,
			size:     f.size,
			dataAttr: f.dataAttr,
			// The unnamed stream is the content stream, so a WOF reparse point
			// applies to this handle exactly as it does to the file.
			wof: f.wof,
		}, nil
	}

	attr := f.findStreamAttribute(name)
	if attr == nil {
		return nil, fmt.Errorf("%w: $DATA stream %q on entry %d",
			ErrAttributeNotFound, name, f.entryNum)
	}

	stream := &File{
		volume:   f.volume,
		entry:    f.entry,
		entryNum: f.entryNum,
		name:     f.name + ":" + attributeName(attr),
		// A named stream holds file content even when its host entry is a
		// directory, so it must not inherit the directory's read block.
		isDir:    false,
		dataAttr: attr,
		// wof is deliberately left nil. A named stream is not what WOF
		// redirects: WofCompressedData holds the compressed bytes, and a caller
		// decoding them itself has to be able to read it.
	}

	if attr.Resident != nil {
		stream.size = uint64(len(attr.Resident.Value))
	} else if attr.NonResident != nil {
		stream.size = attr.NonResident.RealSize
	}

	return stream, nil
}

// findStreamAttribute returns the named $DATA attribute, preferring an exact
// match before falling back to a case-insensitive one.
func (f *File) findStreamAttribute(name string) *Attribute {
	var fallback *Attribute

	for _, attr := range f.entry.DataStreams() {
		attrName := attributeName(attr)
		if attrName == name {
			return attr
		}
		if fallback == nil && strings.EqualFold(attrName, name) {
			fallback = attr
		}
	}

	return fallback
}
