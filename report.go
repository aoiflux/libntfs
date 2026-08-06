package libntfs

import (
	"strconv"
	"strings"
)

// NTFSReport holds a summary of an NTFS volume and its files, suitable for
// consumption by callers of the library.
type NTFSReport struct {
	Name        string     `json:"name"`
	StartOffset int64      `json:"start_offset"`
	EndOffset   int64      `json:"end_offset"`
	Filesystem  NTFSMeta   `json:"ntfs_meta"`
	Files       []NTFSFile `json:"files"`
}

// NTFSMeta describes the file-system geometry.
type NTFSMeta struct {
	Type      string `json:"type"`
	BlockSize int    `json:"block_size"`
	Offset    int64  `json:"offset"`
}

// NTFSFile describes a single file or directory found on the volume.
type NTFSFile struct {
	Filename     string         `json:"filename"`
	Type         string         `json:"type"`
	IsFragmented bool           `json:"is_fragmented"`
	IsDeleted    bool           `json:"is_deleted"`
	Size         int64          `json:"size"`
	Fragments    []FileFragment `json:"fragments"`
}

// Report scans every MFT entry on the volume and returns an NTFSReport with
// a flat list of all files and directories — including deleted entries and
// files that are orphaned (no longer linked to any directory index).
// Paths are reconstructed by following $FILE_NAME parent references.
// Files whose parent chain cannot be resolved are placed under /$ORPHAN/.
// Errors from individual MFT entries are silently skipped so a partial result
// is always returned.
func (v *Volume) Report() (*NTFSReport, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	report := &NTFSReport{
		Name:        v.getVolumeName(),
		StartOffset: 0,
		EndOffset:   int64(v.VolumeSize()),
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

	// Pass 1: collect name + parent for every valid MFT entry so we can
	// reconstruct full paths without re-reading the MFT a second time.
	type entryMeta struct {
		name      string
		parentNum uint64
		isDir     bool
	}
	metaMap := make(map[uint64]entryMeta, maxEntries)

	for i := uint64(0); i < maxEntries; i++ {
		mftEntry, err := v.GetMFTEntry(i)
		if err != nil {
			continue
		}
		fn, err := mftEntry.GetFileName()
		if err != nil || fn == nil || fn.Name == "" {
			continue
		}
		metaMap[i] = entryMeta{
			name:      fn.Name,
			parentNum: fileReferenceRecordNumber(fn.ParentDirectory),
			isDir:     mftEntry.IsDirectory(),
		}
	}

	// buildPath walks parent references up to the root, returning the
	// absolute path for the given entry.  Cycle detection prevents infinite
	// loops on corrupt or deleted directory trees.
	buildPath := func(entryNum uint64) string {
		const maxDepth = 64
		parts := make([]string, 0, 8)
		cur := entryNum
		seen := make(map[uint64]bool, maxDepth)

		for cur != MFTEntryRoot {
			if seen[cur] {
				// Cycle — treat as orphan.
				return "/$ORPHAN/" + strconv.FormatUint(entryNum, 10)
			}
			seen[cur] = true

			meta, ok := metaMap[cur]
			if !ok {
				return "/$ORPHAN/" + strconv.FormatUint(entryNum, 10)
			}
			parts = append(parts, meta.name)
			cur = meta.parentNum
		}

		// Reverse so root comes first.
		for lo, hi := 0, len(parts)-1; lo < hi; lo, hi = lo+1, hi-1 {
			parts[lo], parts[hi] = parts[hi], parts[lo]
		}
		return "/" + strings.Join(parts, "/")
	}

	// Pass 2: emit one NTFSFile per user-space MFT entry.
	// Skip the 12 reserved system metadata entries (0–11).
	for i := uint64(MFTEntryExtend + 1); i < maxEntries; i++ {
		meta, ok := metaMap[i]
		if !ok {
			continue
		}

		mftEntry, err := v.GetMFTEntry(i)
		if err != nil {
			continue
		}

		fileType := "file"
		if meta.isDir {
			fileType = "directory"
		}

		ntfsFile := NTFSFile{
			Filename:  buildPath(i),
			Type:      fileType,
			IsDeleted: !mftEntry.IsInUse(),
		}

		dataAttr := mftEntry.FindPrimaryDataAttribute()
		if dataAttr != nil {
			if dataAttr.Resident != nil {
				ntfsFile.Size = int64(len(dataAttr.Resident.Value))
			} else if dataAttr.NonResident != nil {
				ntfsFile.Size = int64(dataAttr.NonResident.RealSize)
				if !meta.isDir {
					frags := v.allocatedFragments(dataAttr)
					ntfsFile.Fragments = frags
					ntfsFile.IsFragmented = len(frags) > 1
				}
			}
		}

		report.Files = append(report.Files, ntfsFile)
	}

	return report, nil
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
