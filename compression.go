package libntfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

const ntfsSubBlockUncompressedDataSize = 4096

type dataRunClusterIterator struct {
	runs        []DataRun
	runIdx      int
	runRemain   uint64
	runCluster  int64
	runIsSparse bool
	initialized bool
}

func newDataRunClusterIterator(runs []DataRun) *dataRunClusterIterator {
	return &dataRunClusterIterator{runs: runs}
}

func (it *dataRunClusterIterator) loadNextRun() bool {
	for it.runIdx < len(it.runs) {
		run := it.runs[it.runIdx]
		it.runIdx++
		if run.LengthClusters == 0 {
			continue
		}

		it.runRemain = run.LengthClusters
		it.runCluster = run.StartCluster
		it.runIsSparse = run.IsSparse
		it.initialized = true
		return true
	}

	it.initialized = false
	return false
}

func (it *dataRunClusterIterator) Next() (cluster int64, sparse bool, ok bool) {
	if !it.initialized || it.runRemain == 0 {
		if !it.loadNextRun() {
			return 0, false, false
		}
	}

	cluster = it.runCluster
	sparse = it.runIsSparse
	ok = true

	it.runRemain--
	if !it.runIsSparse {
		it.runCluster++
	}

	return
}

func readAtFull(r io.ReaderAt, p []byte, off int64) error {
	read := 0
	for read < len(p) {
		n, err := r.ReadAt(p[read:], off+int64(read))
		if n > 0 {
			read += n
		}
		if err != nil {
			if err == io.EOF && read == len(p) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}

	return nil
}

func (v *Volume) readSingleCluster(cluster int64, dst []byte) error {
	if cluster < 0 || uint64(cluster) >= v.clusterCount {
		return ErrInvalidCluster
	}

	off := int64(cluster) * int64(v.bytesPerCluster)
	if err := readAtFull(v.reader, dst, off); err != nil {
		return v.ioErrorAt("read", off, len(dst), err)
	}

	return nil
}

func ntfsDecompressCompressionUnit(compData []byte, unitSize int) ([]byte, error) {
	if unitSize <= 0 {
		return nil, fmt.Errorf("invalid compression unit size: %d", unitSize)
	}

	out := make([]byte, unitSize)
	uncompIdx := 0
	clIdx := 0

	for clIdx+1 < len(compData) {
		subBlockHeader := binary.LittleEndian.Uint16(compData[clIdx:])

		if subBlockHeader == 0 {
			for uncompIdx < unitSize {
				out[uncompIdx] = 0
				uncompIdx++
			}
			break
		}

		blockSize := int(subBlockHeader&0x0FFF) + 3
		if blockSize == 3 {
			break
		}

		blockEnd := clIdx + blockSize
		if blockEnd > len(compData) {
			blockEnd = len(compData)
		}

		isCompressedBlock := (subBlockHeader & 0x8000) != 0
		blockStartUncomp := uncompIdx
		clIdx += 2

		if isCompressedBlock && (blockSize-2 != ntfsSubBlockUncompressedDataSize) {
			for clIdx < blockEnd {
				header := compData[clIdx]
				clIdx++

				for token := 0; token < 8 && clIdx < blockEnd; token++ {
					if (header & 0x01) == 0 {
						if uncompIdx >= unitSize {
							return nil, fmt.Errorf("symbol token writes past output buffer")
						}
						out[uncompIdx] = compData[clIdx]
						uncompIdx++
						clIdx++
					} else {
						if clIdx+1 >= blockEnd {
							return nil, fmt.Errorf("phrase token extends past compression block")
						}

						pheader := binary.LittleEndian.Uint16(compData[clIdx:])
						clIdx += 2

						shift := 0
						for i := uncompIdx - blockStartUncomp - 1; i >= 0x10; i >>= 1 {
							shift++
							if shift > 12 {
								return nil, fmt.Errorf("invalid phrase token shift")
							}
						}

						offset := int(pheader>>(12-shift)) + 1
						length := int(pheader&(0x0FFF>>shift)) + 2

						start := uncompIdx - offset
						end := start + length
						if start < 0 || start >= unitSize {
							return nil, fmt.Errorf("invalid phrase token offset")
						}

						for start <= end && uncompIdx < unitSize {
							if start < 0 || start >= unitSize {
								return nil, fmt.Errorf("phrase token copy out of bounds")
							}
							out[uncompIdx] = out[start]
							uncompIdx++
							start++
						}
					}
					header >>= 1
				}
			}
		} else {
			for clIdx < blockEnd {
				if uncompIdx >= unitSize {
					return nil, fmt.Errorf("uncompressed block writes past output buffer")
				}
				out[uncompIdx] = compData[clIdx]
				uncompIdx++
				clIdx++
			}
		}
	}

	return out, nil
}

// compressionUnitBytes returns the byte size of a compression unit of attr on
// this volume, or an error wrapping ErrInvalidCompressionUnit when the field is
// outside the range NTFS can mean.
//
// The compression-unit field is log2 of the clusters per unit, not the count:
// Windows writes 4, meaning 1<<4 = 16 clusters, which is 64 KiB at the usual
// 4 KiB cluster. The shift is bounded before it is taken, so a damaged field
// can neither overflow it nor size a unit buffer no volume could hold.
func (v *Volume) compressionUnitBytes(attr *NonResidentAttribute) (int64, error) {
	if cu := attr.CompressionUnit; cu == 0 || cu > maxCompressionUnitShift {
		return 0, fmt.Errorf("%w: compression unit 2^%d clusters",
			ErrInvalidCompressionUnit, cu)
	}

	compUnitClusters := 1 << attr.CompressionUnit

	unitSizeBytes := int64(compUnitClusters) * int64(v.bytesPerCluster)
	if unitSizeBytes <= 0 || unitSizeBytes > maxCompressionUnitBytes {
		return 0, fmt.Errorf("%w: unit of %d clusters of %d bytes exceeds %d",
			ErrInvalidCompressionUnit, compUnitClusters, v.bytesPerCluster,
			maxCompressionUnitBytes)
	}

	return unitSizeBytes, nil
}

// compressionUnitError reports why the compressed stream attr of record
// entryNum cannot be read on this volume, or nil when it can.
//
// The tolerant reader takes any compression unit that sizes a unit it can
// hold. The Linux driver refuses any value but 4 on a compressed attribute,
// and Options.Strict does the same, with a [*StrictError]. File.ReadAt,
// File.ReadSupport and File.Streams all ask this, so they agree.
func (v *Volume) compressionUnitError(entryNum uint64, attr *NonResidentAttribute) error {
	if _, err := v.compressionUnitBytes(attr); err != nil {
		return err
	}

	if v.strict && attr.CompressionUnit != StandardCompressionUnit {
		return &StrictError{
			Record: entryNum, VCN: -1, Check: "compression-unit",
			Err: fmt.Errorf("%w: compression unit %d, want %d",
				ErrInvalidCompressionUnit, attr.CompressionUnit, StandardCompressionUnit),
		}
	}

	return nil
}

// applyCompressionSupport marks a compressed stream unreadable when
// compressionUnitError refuses it. A stream already unreadable for another
// reason, such as encryption, keeps that reason.
func (v *Volume) applyCompressionSupport(support *FileReadSupport, entryNum uint64, attr *Attribute) {
	if !support.Readable || attr == nil || attr.NonResident == nil || !attr.IsCompressed() {
		return
	}

	if err := v.compressionUnitError(entryNum, attr.NonResident); err != nil {
		support.Readable = false
		support.BlockingError = err
	}
}

// readCompressedData decodes len(out) bytes of the compressed stream attr from
// offset. It checks the compression unit's range itself; the strict check is
// compressionUnitError's, which File.ReadAt applies before calling this.
func (v *Volume) readCompressedData(attr *NonResidentAttribute, offset int64, out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}

	if attr == nil {
		return 0, fmt.Errorf("missing non-resident attribute")
	}

	unitSizeBytes, err := v.compressionUnitBytes(attr)
	if err != nil {
		return 0, err
	}
	compUnitClusters := 1 << attr.CompressionUnit

	startUnit := offset / unitSizeBytes
	endUnit := (offset + int64(len(out)) - 1) / unitSizeBytes

	it := newDataRunClusterIterator(attr.DataRuns)
	clustersToSkip := startUnit * int64(compUnitClusters)
	for skipped := int64(0); skipped < clustersToSkip; skipped++ {
		if _, _, ok := it.Next(); !ok {
			return 0, io.EOF
		}
	}

	unitClusterMeta := make([]struct {
		cluster int64
		sparse  bool
	}, compUnitClusters)

	clusterBuf := make([]byte, v.bytesPerCluster)
	unitReadOffset := offset - (startUnit * unitSizeBytes)
	bytesWritten := 0

	for unit := startUnit; unit <= endUnit; unit++ {
		clustersInThisUnit := compUnitClusters
		for i := 0; i < compUnitClusters; i++ {
			cluster, sparse, ok := it.Next()
			if !ok {
				clustersInThisUnit = i
				break
			}
			unitClusterMeta[i] = struct {
				cluster int64
				sparse  bool
			}{cluster: cluster, sparse: sparse}
		}

		if clustersInThisUnit == 0 {
			break
		}

		allSparse := true
		for i := 0; i < clustersInThisUnit; i++ {
			if !unitClusterMeta[i].sparse {
				allSparse = false
				break
			}
		}

		unitData := make([]byte, int64(clustersInThisUnit)*int64(v.bytesPerCluster))
		if allSparse {
			for i := range unitData {
				unitData[i] = 0
			}
		} else {
			isCompressedUnit := unitClusterMeta[clustersInThisUnit-1].sparse
			if isCompressedUnit {
				compData := make([]byte, 0, len(unitData))
				for i := 0; i < clustersInThisUnit; i++ {
					if unitClusterMeta[i].sparse {
						break
					}
					if err := v.readSingleCluster(unitClusterMeta[i].cluster, clusterBuf); err != nil {
						return bytesWritten, err
					}
					compData = append(compData, clusterBuf...)
				}

				fullUnitSize := int(unitSizeBytes)
				if clustersInThisUnit < compUnitClusters {
					fullUnitSize = int(int64(clustersInThisUnit) * int64(v.bytesPerCluster))
				}

				decompressed, err := ntfsDecompressCompressionUnit(compData, int(fullUnitSize))
				if err != nil {
					return bytesWritten, fmt.Errorf("failed to decompress compression unit %d: %w", unit, err)
				}
				copy(unitData, decompressed)
			} else {
				for i := 0; i < clustersInThisUnit; i++ {
					start := i * int(v.bytesPerCluster)
					end := start + int(v.bytesPerCluster)
					if unitClusterMeta[i].sparse {
						for j := start; j < end; j++ {
							unitData[j] = 0
						}
						continue
					}
					if err := v.readSingleCluster(unitClusterMeta[i].cluster, unitData[start:end]); err != nil {
						return bytesWritten, err
					}
				}
			}
		}

		if unitReadOffset >= int64(len(unitData)) {
			unitReadOffset -= int64(len(unitData))
			continue
		}

		remainingDst := len(out) - bytesWritten
		toCopy := len(unitData) - int(unitReadOffset)
		if toCopy > remainingDst {
			toCopy = remainingDst
		}

		copy(out[bytesWritten:bytesWritten+toCopy], unitData[unitReadOffset:int(unitReadOffset)+toCopy])
		bytesWritten += toCopy
		unitReadOffset = 0

		if bytesWritten >= len(out) {
			break
		}
	}

	if bytesWritten == 0 {
		return 0, io.EOF
	}

	return bytesWritten, nil
}
