package libntfs

import (
	"fmt"
	"math"
)

// FileFragment describes a contiguous region of a stream.
//
// A fragment carries both axes a forensic caller needs: where the region sits
// inside the stream (FileOffset, Length) and where its bytes sit on disk
// (StartOffset, EndOffset). Consecutive fragments tile the stream without gaps,
// so FileOffset+Length of one fragment is the FileOffset of the next.
//
// # Which offsets these are
//
// StartOffset and EndOffset are image-absolute: they include Options.BaseOffset
// and so locate the bytes in the reader the volume was opened over, not within
// the volume. For a volume opened at the start of its reader, which is the
// ordinary case, the two are the same number.
//
// That is what makes a fragment directly comparable with a range expressed in
// whole-disk coordinates, and it is why [Volume.ReadAt] takes the same space —
// vol.ReadAt(buf, frag.StartOffset) reads this fragment's bytes at any base.
//
// EndOffset is exclusive: it is StartOffset+Length, the first byte past the
// region.
//
// Fragments are returned in stream order, and the run-list they come from is
// reported as NTFS recorded it: adjacent runs are not coalesced, so two
// physically contiguous fragments may be reported separately.
//
// # The zero sentinels
//
// Sparse fragments occupy stream space but no disk space. They carry a
// FileOffset and Length but no disk location, and StartOffset and EndOffset are
// left at zero rather than at the base. Callers carving disk regions should skip
// them; callers mapping stream offsets must not. A zero StartOffset therefore
// means "nowhere", never "the first byte of the image" — nothing real is ever
// reported at zero, because the boot sector occupies it.
//
// Resident fragments locate a value stored inside its MFT record rather than in
// allocated clusters. Their StartOffset points at those bytes, but the record
// they live in carries update sequence fixups: the last two bytes of every
// sector hold the record's sequence number rather than real content. A resident
// value that spans a sector boundary therefore cannot be read straight off the
// disk at StartOffset — read it through (*File).ReadAt, which applies the
// fixups. StartOffset and EndOffset are zero, the same sentinel, when the
// attribute was merged from an extension record whose origin was not recorded.
type FileFragment struct {
	StartOffset int64 `json:"start_offset"` // Image byte offset, including Options.BaseOffset; 0 when Sparse
	EndOffset   int64 `json:"end_offset"`   // Image byte offset one past the end; 0 when Sparse
	FileOffset  int64 `json:"file_offset"`  // Byte offset within the stream
	Length      int64 `json:"length"`       // Length in bytes
	Sparse      bool  `json:"sparse"`       // Region is a hole with no allocated clusters
	Resident    bool  `json:"resident"`     // Content lives inside the MFT record
}

// Fragments returns the on-disk layout of the file's $DATA stream, in stream
// order.
//
// The stream is the one this File was opened on, so a handle from OpenStream
// reports that alternate data stream's layout rather than the file's default
// stream.
//
// A file with no $DATA attribute returns no fragments and no error, matching
// ReadAt's treatment of it as empty.
//
// Compressed streams are a documented exception to the two-axis contract: their
// data runs describe compression units, not a byte-for-byte map of decompressed
// content. For a compressed stream the fragments describe the physical
// allocation only, and FileOffset is a running total of allocated bytes rather
// than a position in the decompressed stream. Gate on IsCompressed before
// treating fragment offsets as stream offsets.
func (f *File) Fragments() ([]FileFragment, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if f.dataAttr == nil {
		return nil, nil
	}

	return f.volume.AttributeFragments(f.dataAttr)
}

// IsFragmented reports whether the file's $DATA stream is split across more than
// one contiguous region of the volume.
//
// Sparse holes are not fragmentation: a file stored in one run with a hole
// punched in it is not scattered across the disk. Resident files are never
// fragmented, since their content is a single span inside the MFT record.
func (f *File) IsFragmented() bool {
	if f.dataAttr == nil || f.dataAttr.NonResident == nil {
		return false
	}

	allocated := 0
	for _, run := range f.dataAttr.NonResident.DataRuns {
		if run.IsSparse || run.LengthClusters == 0 {
			continue
		}
		allocated++
		if allocated > 1 {
			return true
		}
	}

	return false
}

// AttributeFragments returns the on-disk layout of any attribute's value.
//
// It is the escape hatch for streams that are not reachable as a File — $Bitmap,
// $Secure:$SDS, $UsnJrnl:$J, a non-resident $EA — and the shared implementation
// behind (*File).Fragments.
func (v *Volume) AttributeFragments(attr *Attribute) ([]FileFragment, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if attr == nil {
		return nil, fmt.Errorf("%w: nil attribute", ErrAttributeNotFound)
	}

	if attr.Resident != nil {
		return v.residentFragments(attr)
	}

	if attr.NonResident != nil {
		return v.nonResidentFragments(attr), nil
	}

	return nil, fmt.Errorf("%w: attribute is neither resident nor non-resident",
		ErrInvalidAttribute)
}

// residentFragments locates a resident value's bytes inside its MFT record.
func (v *Volume) residentFragments(attr *Attribute) ([]FileFragment, error) {
	length := int64(len(attr.Resident.Value))
	if length == 0 {
		return nil, nil
	}

	frag := FileFragment{
		FileOffset: 0,
		Length:     length,
		Resident:   true,
	}

	// The recorded offset is only meaningful for the record the attribute was
	// parsed from. An attribute synthesised by $ATTRIBUTE_LIST merging has bytes
	// in no single record, so it gets no disk location rather than a wrong one.
	//
	// That zero is a sentinel and must stay zero. The base offset is added below
	// rather than to frag, so a fragment with no known location does not come
	// back pointing at the start of the volume.
	if !attr.sourceKnown {
		return []FileFragment{frag}, nil
	}

	recordOffset, err := v.mftEntryVolumeOffset(attr.sourceRecord)
	if err != nil {
		return nil, wrapMFTError(attr.sourceRecord, "calculate offset", err)
	}

	frag.StartOffset = v.baseOffset + recordOffset +
		int64(attr.Header.RecordOffset) +
		int64(attr.Resident.ValueOffset)
	frag.EndOffset = frag.StartOffset + length

	return []FileFragment{frag}, nil
}

// nonResidentFragments walks a run-list into fragments, clamping the last run to
// the stream's real size so allocation slack is not reported as content.
func (v *Volume) nonResidentFragments(attr *Attribute) []FileFragment {
	nonResident := attr.NonResident
	bytesPerCluster := int64(v.bytesPerCluster)

	// A compressed run-list maps compression units rather than stream bytes, so
	// RealSize is not an upper bound on it and clamping to RealSize would drop
	// real allocation. Report the whole physical layout instead.
	limit := int64(nonResident.RealSize)
	if attr.IsCompressed() {
		limit = math.MaxInt64
	}

	fragments := make([]FileFragment, 0, len(nonResident.DataRuns))
	fileOffset := int64(0)

	for _, run := range nonResident.DataRuns {
		if fileOffset >= limit {
			// Every remaining run is allocation past the end of the stream.
			break
		}

		length := int64(run.LengthClusters) * bytesPerCluster
		if length <= 0 {
			continue
		}

		if fileOffset+length > limit {
			length = limit - fileOffset
		}

		fragment := FileFragment{
			FileOffset: fileOffset,
			Length:     length,
			Sparse:     run.IsSparse,
		}

		// A sparse run occupies no disk, so its offsets stay at the zero
		// sentinel rather than gaining the base like a real location would.
		if !run.IsSparse {
			fragment.StartOffset = v.baseOffset + run.StartCluster*bytesPerCluster
			fragment.EndOffset = fragment.StartOffset + length
		}

		fragments = append(fragments, fragment)
		fileOffset += length
	}

	return fragments
}
