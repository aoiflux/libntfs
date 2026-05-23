package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aoiflux/libntfs"
)

// FileInfoReport contains structured metadata about one NTFS path.
type FileInfoReport struct {
	VolumePath      string                  `json:"volume_path"`
	RequestedPath   string                  `json:"requested_path"`
	NormalizedName  string                  `json:"name"`
	EntryNumber     uint64                  `json:"entry_number"`
	IsDirectory     bool                    `json:"is_directory"`
	IsDeleted       bool                    `json:"is_deleted"`
	Size            int64                   `json:"size"`
	StartOffset     int64                   `json:"start_offset"`
	EndOffset       int64                   `json:"end_offset"`
	HasData         bool                    `json:"has_data"`
	ResidentData    bool                    `json:"resident_data"`
	NonResidentData bool                    `json:"non_resident_data"`
	IsFragmented    bool                    `json:"is_fragmented"`
	ReadSupport     libntfs.FileReadSupport `json:"read_support"`
	CreatedAt       string                  `json:"created_at,omitempty"`
	ModifiedAt      string                  `json:"modified_at,omitempty"`
	MFTChangedAt    string                  `json:"mft_changed_at,omitempty"`
	AccessedAt      string                  `json:"accessed_at,omitempty"`
	Fragments       []FileFragmentReport    `json:"fragments"`
}

// FileFragmentReport describes one contiguous physical run in bytes.
type FileFragmentReport struct {
	StartOffset int64 `json:"start_offset"`
	EndOffset   int64 `json:"end_offset"`
	LengthBytes int64 `json:"length_bytes"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <ntfs_volume_or_image> <file_path>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./finfo disk.img /Windows/System32/notepad.exe")
		fmt.Println("  ./finfo /dev/sda1 /Users/Alice/Documents/report.docx")
		fmt.Println("  ./finfo \\\\.\\C: /Windows/explorer.exe   # Windows (Administrator)")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	filePath := os.Args[2]

	f, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("failed to open volume: %v", err)
	}
	defer f.Close()

	volume, err := libntfs.Open(f)
	if err != nil {
		log.Fatalf("failed to parse NTFS volume: %v", err)
	}
	defer volume.Close()

	ntfsFile, err := volume.OpenPath(filePath)
	if err != nil {
		log.Fatalf("failed to open file path %q: %v", filePath, err)
	}

	entry, err := volume.GetMFTEntry(ntfsFile.EntryNumber())
	if err != nil {
		log.Fatalf("failed to get MFT entry %d: %v", ntfsFile.EntryNumber(), err)
	}

	report := FileInfoReport{
		VolumePath:      volumePath,
		RequestedPath:   filePath,
		NormalizedName:  ntfsFile.Name(),
		EntryNumber:     ntfsFile.EntryNumber(),
		IsDirectory:     ntfsFile.IsDirectory(),
		IsDeleted:       !entry.IsInUse(),
		Size:            ntfsFile.Size(),
		StartOffset:     0,
		EndOffset:       0,
		HasData:         ntfsFile.HasData(),
		ResidentData:    false,
		NonResidentData: false,
		ReadSupport:     ntfsFile.ReadSupport(),
		Fragments:       []FileFragmentReport{},
	}

	if info, err := ntfsFile.GetMetadata(); err == nil {
		report.CreatedAt = info.CreateTime.Format("2006-01-02T15:04:05Z07:00")
		report.ModifiedAt = info.ModifyTime.Format("2006-01-02T15:04:05Z07:00")
		report.MFTChangedAt = info.MFTChangeTime.Format("2006-01-02T15:04:05Z07:00")
		report.AccessedAt = info.AccessTime.Format("2006-01-02T15:04:05Z07:00")
	}

	dataAttr := entry.FindPrimaryDataAttribute()
	if dataAttr != nil {
		if dataAttr.Resident != nil {
			report.ResidentData = true
		}

		if dataAttr.NonResident != nil {
			report.NonResidentData = true
			bpc := int64(volume.BytesPerCluster())
			for _, run := range dataAttr.NonResident.DataRuns {
				if run.IsSparse {
					continue
				}

				start := run.StartCluster * bpc
				end := start + int64(run.LengthClusters)*bpc
				report.Fragments = append(report.Fragments, FileFragmentReport{
					StartOffset: start,
					EndOffset:   end,
					LengthBytes: end - start,
				})
			}

			if len(report.Fragments) > 0 {
				report.StartOffset = report.Fragments[0].StartOffset
				report.EndOffset = report.Fragments[len(report.Fragments)-1].EndOffset
				report.IsFragmented = len(report.Fragments) > 1
			}
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		log.Fatalf("failed to encode output: %v", err)
	}
}
