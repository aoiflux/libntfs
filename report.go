package libntfs

import (
	"context"
	"time"
)

// NTFSReport holds a summary of an NTFS volume and its files, suitable for
// consumption by callers of the library.
//
// A report is evidence, and evidence has to stay readable by a tool built
// against a different version of this library. SchemaVersion is what lets such
// a tool refuse a document it does not understand instead of misreading it, and
// LibraryVersion and Generated say what produced it and when.
type NTFSReport struct {
	// SchemaVersion is the shape of this document, not the version of the
	// volume or of the library. Compare it against [ReportSchemaVersion].
	SchemaVersion int `json:"schema_version"`

	// LibraryVersion is the libntfs release that produced this document, and
	// Generated the moment it did, in UTC.
	LibraryVersion string    `json:"library_version"`
	Generated      time.Time `json:"generated"`

	Name string `json:"name"`

	// StartOffset and EndOffset span the volume in the image it was read from:
	// StartOffset is Options.BaseOffset and EndOffset is one byte past the last,
	// so the span is half-open and EndOffset-StartOffset is the volume size.
	//
	// They are in the same image-absolute space as [FileFragment.StartOffset],
	// so every offset in a report can be compared with every other without
	// knowing where any of them came from.
	StartOffset int64 `json:"start_offset"`
	EndOffset   int64 `json:"end_offset"`

	Filesystem NTFSMeta   `json:"ntfs_meta"`
	Files      []NTFSFile `json:"files"`
}

// NTFSMeta describes the file-system geometry.
type NTFSMeta struct {
	Type      string `json:"type"`
	BlockSize int    `json:"block_size"`

	// Offset locates the $MFT, not the volume. It is image-absolute like every
	// other offset here, so on a volume opened with a non-zero
	// Options.BaseOffset it is the base plus the MFT's position within the
	// volume. Use [NTFSReport.StartOffset] for where the volume itself begins.
	Offset int64 `json:"offset"`
}

// NTFSFile describes a single file or directory found on the volume.
//
// The identity fields are what make two reports of the same volume comparable.
// A path is not an identity: it changes when a file is renamed or its parent is,
// and it is reused when a new file takes an old name. EntryNumber and
// SequenceNumber together are the identity NTFS itself uses — the record, and
// which occupant of that record this was. Diffing on EntryNumber alone reports a
// deleted file whose record was reused as a modified one; diffing on Filename
// alone reports a rename as a delete plus a create.
type NTFSFile struct {
	Filename string `json:"filename"`
	Type     string `json:"type"`

	// EntryNumber is the file's MFT record number.
	EntryNumber uint64 `json:"entry_number"`

	// SequenceNumber is that record's sequence number, incremented by NTFS each
	// time the record is reused. It is what tells a modified file apart from a
	// different file occupying the same record.
	SequenceNumber uint16 `json:"sequence_number"`

	// ParentReference is the MFT record number of the directory named by this
	// file's $FILE_NAME, and ParentSequence that record's sequence number. The
	// pair matches [USNRecord.ParentReference] and [USNRecord.ParentSequence], so
	// a report and the change journal can be joined on it. A ParentSequence that
	// no longer matches the parent record means Filename is a reconstruction of
	// where the file was, not a statement of where it is.
	ParentReference uint64 `json:"parent_reference"`
	ParentSequence  uint16 `json:"parent_sequence"`

	IsFragmented bool  `json:"is_fragmented"`
	IsDeleted    bool  `json:"is_deleted"`
	Size         int64 `json:"size"`

	// FileAttributes is the $STANDARD_INFORMATION attribute flags word, and
	// HardLinkCount the MFT record's link count. A link count above one means
	// Filename is one of several paths the same data is reachable by.
	FileAttributes uint32 `json:"file_attributes"`
	HardLinkCount  uint16 `json:"hard_link_count"`

	// Timestamps are the $STANDARD_INFORMATION times: the set a timeline is
	// built from, and the set that changes when a file is touched.
	Timestamps NTFSTimestamps `json:"timestamps"`

	// Streams lists every $DATA stream on the record, alternate data streams
	// included. Size and Fragments describe the default stream only.
	Streams []StreamInfo `json:"streams,omitempty"`

	Fragments []FileFragment `json:"fragments"`
}

// NTFSTimestamps is the MACB set from $STANDARD_INFORMATION.
//
// These are the times a mounted volume reports, and the ones an anti-forensic
// tool rewrites. $FILE_NAME carries a second copy that most tools do not touch;
// read it with [File.GetFileName] when the two need comparing.
type NTFSTimestamps struct {
	Created    time.Time `json:"created"`
	Modified   time.Time `json:"modified"`
	MFTChanged time.Time `json:"mft_changed"`
	Accessed   time.Time `json:"accessed"`
}

// Report scans every MFT entry on the volume and returns an NTFSReport with
// a flat list of all files and directories — including deleted entries and
// files that are orphaned (no longer linked to any directory index).
//
// Paths are reconstructed by following $FILE_NAME parent references; files whose
// parent chain cannot be resolved are placed under [OrphanDir]. Each entry
// carries its own identity — MFT record number, sequence number, and parent
// reference — so two reports of the same volume can be diffed on something
// sturdier than the path string. Errors from individual MFT entries are silently
// skipped so a partial result is always returned.
//
// It reads nothing but the volume and writes nothing to it. Use [Volume.ReportContext]
// to cancel a report over a large volume.
func (v *Volume) Report() (*NTFSReport, error) {
	return v.ReportContext(context.Background())
}

// ReportContext is [Volume.Report] with cancellation.
//
// A report walks the MFT twice — once to map every record's name and parent, and
// once to describe the files — which on a volume with tens of millions of
// records is the longest thing this library does. Cancelling ctx stops it and
// returns the context's error; no partial report is returned, since a truncated
// listing that looks complete is worse than none.
func (v *Volume) ReportContext(ctx context.Context) (*NTFSReport, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	if ctx == nil {
		ctx = context.Background()
	}

	report := &NTFSReport{
		SchemaVersion:  ReportSchemaVersion,
		LibraryVersion: Version,
		Generated:      time.Now().UTC(),
		Name:           v.getVolumeName(),
		StartOffset:    v.baseOffset,
		EndOffset:      v.baseOffset + int64(v.VolumeSize()),
		Filesystem: NTFSMeta{
			Type:      "NTFS",
			BlockSize: int(v.BytesPerCluster()),
			Offset:    v.ClusterToOffset(v.mftCluster),
		},
	}

	maxEntries := v.maxMFTEntries()
	if maxEntries == 0 {
		return report, nil
	}

	// Pass 1: name and parent for every valid record, so paths can be
	// reconstructed without walking ancestors once per file.
	idx, err := v.buildPathIndex(ctx, WalkConfig{})
	if err != nil {
		return nil, err
	}

	// Pass 2: emit one NTFSFile per user-space MFT entry.
	// Skip the 12 reserved system metadata entries (0–11).
	for i := uint64(MFTEntryExtend + 1); i < maxEntries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		meta, ok := idx.meta[i]
		if !ok {
			continue
		}

		mftEntry, err := v.GetMFTEntry(i)
		if err != nil {
			continue
		}

		report.Files = append(report.Files, v.describeEntry(i, meta, idx, mftEntry))
	}

	return report, nil
}

// describeEntry turns one MFT record into its report entry.
func (v *Volume) describeEntry(entryNum uint64, meta pathMeta, idx *pathIndex, entry *MFTEntry) NTFSFile {
	fileType := "file"
	if meta.isDir {
		fileType = "directory"
	}

	ntfsFile := NTFSFile{
		Filename:        idx.pathFor(entryNum),
		Type:            fileType,
		EntryNumber:     entryNum,
		SequenceNumber:  entry.SequenceNum,
		ParentReference: meta.parent,
		ParentSequence:  meta.parentSeq,
		IsDeleted:       !entry.IsInUse(),
		HardLinkCount:   entry.HardLinkCount,
		Streams:         v.streamsFromEntry(entryNum, entry, v.resolveWOF(entry)),
	}

	// A record with no readable $STANDARD_INFORMATION still belongs in the
	// report; it simply has no timestamps to contribute.
	if si, err := entry.GetStandardInformation(); err == nil && si != nil {
		ntfsFile.FileAttributes = si.FileAttributes
		ntfsFile.Timestamps = NTFSTimestamps{
			Created:    si.CreateTime,
			Modified:   si.ModifyTime,
			MFTChanged: si.MFTChangeTime,
			Accessed:   si.AccessTime,
		}
	}

	dataAttr := entry.FindPrimaryDataAttribute()
	if dataAttr == nil {
		return ntfsFile
	}

	if dataAttr.Resident != nil {
		ntfsFile.Size = int64(len(dataAttr.Resident.Value))
		return ntfsFile
	}

	if dataAttr.NonResident != nil {
		ntfsFile.Size = int64(dataAttr.NonResident.RealSize)
		if !meta.isDir {
			frags := v.allocatedFragments(dataAttr)
			ntfsFile.Fragments = frags
			ntfsFile.IsFragmented = len(frags) > 1
		}
	}

	return ntfsFile
}

// getVolumeName reads the $VOLUME_NAME attribute from the $Volume MFT entry.
// Returns an empty string if the name cannot be determined.
func (v *Volume) getVolumeName() string {
	entry, err := v.GetMFTEntry(MFTEntryVolume)
	if err != nil {
		return ""
	}

	attr := entry.FindAttribute(AttrTypeVolumeName, "")
	if attr == nil || attr.Resident == nil || len(attr.Resident.Value) == 0 {
		return ""
	}

	r := NewBinaryReader(attr.Resident.Value)
	name, err := r.ReadUTF16String(len(attr.Resident.Value) / 2)
	if err != nil {
		return ""
	}

	return name
}

// allocatedFragments returns only the fragments that occupy disk, dropping
// sparse holes. The report describes physical regions of the volume, so a hole
// has nothing to contribute to it; callers needing the full stream map, holes
// included, should use (*File).Fragments.
func (v *Volume) allocatedFragments(attr *Attribute) []FileFragment {
	all, err := v.AttributeFragments(attr)
	if err != nil {
		return nil
	}

	frags := make([]FileFragment, 0, len(all))
	for _, frag := range all {
		if frag.Sparse {
			continue
		}
		frags = append(frags, frag)
	}

	return frags
}
