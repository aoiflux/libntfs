package libntfs

import (
	"fmt"
	"sort"
)

// SDSStreamName is the $Secure stream holding security descriptors.
const SDSStreamName = "$SDS"

// maxSDSSize bounds how much of $SDS is read, guarding against a corrupt
// attribute claiming an implausible length.
const maxSDSSize = 256 << 20

// SecurityDescriptorByID returns the security descriptor with the given security
// ID, as referenced by StandardInformation.SecurityID.
//
// The $Secure:$SDS stream is read and indexed on first use, then cached for the
// life of the Volume. An unknown ID returns ErrSecurityDescriptorNotFound.
func (v *Volume) SecurityDescriptorByID(securityID uint32) (*SecurityDescriptor, error) {
	if err := v.ensureSecurityMap(); err != nil {
		return nil, err
	}

	v.securityMu.RLock()
	sd, ok := v.securityMap[securityID]
	v.securityMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: security ID %d", ErrSecurityDescriptorNotFound, securityID)
	}

	return sd, nil
}

// EachSecurityDescriptor calls fn for every descriptor in $Secure:$SDS, in
// ascending security ID order. If fn returns a non-nil error, iteration stops
// and that error is returned.
func (v *Volume) EachSecurityDescriptor(fn func(securityID uint32, sd *SecurityDescriptor) error) error {
	if err := v.ensureSecurityMap(); err != nil {
		return err
	}

	v.securityMu.RLock()
	ids := make([]uint32, 0, len(v.securityMap))
	for id := range v.securityMap {
		ids = append(ids, id)
	}
	descriptors := v.securityMap
	v.securityMu.RUnlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if err := fn(id, descriptors[id]); err != nil {
			return err
		}
	}

	return nil
}

// SecurityDescriptor returns the security descriptor governing this file.
//
// A $SECURITY_DESCRIPTOR attribute on the file itself takes precedence, which is
// how older NTFS volumes and some system files store it. Otherwise the file's
// StandardInformation.SecurityID is resolved through $Secure:$SDS.
func (f *File) SecurityDescriptor() (*SecurityDescriptor, error) {
	if f.volume.IsClosed() {
		return nil, ErrVolumeClosed
	}

	// Prefer a descriptor stored directly on the entry.
	if attr := f.entry.FindAttribute(AttrTypeSecurityDesc, ""); attr != nil {
		if attr.Resident != nil {
			return ParseSecurityDescriptor(attr.Resident.Value)
		}

		if attr.NonResident != nil {
			size := attr.NonResident.RealSize
			if size == 0 || size > maxSDSSize {
				return nil, fmt.Errorf("%w: $SECURITY_DESCRIPTOR is %d bytes",
					ErrInvalidSecurityDescriptor, size)
			}

			buf := make([]byte, size)
			if _, err := f.volume.ReadDataRuns(attr.NonResident.DataRuns, 0, buf); err != nil {
				return nil, fmt.Errorf("read $SECURITY_DESCRIPTOR: %w", err)
			}

			return ParseSecurityDescriptor(buf)
		}
	}

	si, err := f.entry.GetStandardInformation()
	if err != nil {
		return nil, fmt.Errorf("%w: no $SECURITY_DESCRIPTOR and no $STANDARD_INFORMATION",
			ErrSecurityDescriptorNotFound)
	}

	return f.volume.SecurityDescriptorByID(si.SecurityID)
}

// ensureSecurityMap loads and indexes $Secure:$SDS once per Volume.
func (v *Volume) ensureSecurityMap() error {
	if v.IsClosed() {
		return ErrVolumeClosed
	}

	v.securityMu.RLock()
	if v.securityInit {
		err := v.securityErr
		v.securityMu.RUnlock()
		return err
	}
	v.securityMu.RUnlock()

	v.securityMu.Lock()
	defer v.securityMu.Unlock()
	if v.securityInit {
		return v.securityErr
	}

	descriptors, err := v.loadSecurityDescriptors()
	v.securityMap = descriptors
	v.securityErr = err
	v.securityInit = true

	return err
}

// loadSecurityDescriptors reads $Secure:$SDS and parses every descriptor in it.
func (v *Volume) loadSecurityDescriptors() (map[uint32]*SecurityDescriptor, error) {
	entry, err := v.GetMFTEntry(MFTEntrySecure)
	if err != nil {
		return nil, fmt.Errorf("read $Secure: %w", err)
	}

	attr := entry.FindAttribute(AttrTypeData, SDSStreamName)
	if attr == nil {
		return nil, fmt.Errorf("%w: $Secure has no %s stream",
			ErrAttributeNotFound, SDSStreamName)
	}

	var sds []byte

	switch {
	case attr.Resident != nil:
		sds = attr.Resident.Value

	case attr.NonResident != nil:
		size := attr.NonResident.RealSize
		if size == 0 {
			return map[uint32]*SecurityDescriptor{}, nil
		}
		if size > maxSDSSize {
			return nil, fmt.Errorf("%w: %s is %d bytes", ErrInvalidSecurityDescriptor,
				SDSStreamName, size)
		}

		sds = make([]byte, size)
		n, err := v.ReadDataRuns(attr.NonResident.DataRuns, 0, sds)
		if err != nil {
			// A short read still leaves usable descriptors at the front.
			if n == 0 {
				return nil, fmt.Errorf("read %s: %w", SDSStreamName, err)
			}
			sds = sds[:n]
		}

	default:
		return nil, fmt.Errorf("%w: %s stream has no content",
			ErrInvalidSecurityDescriptor, SDSStreamName)
	}

	return parseSDSStream(sds), nil
}

// parseSDSStream walks $SDS and returns its descriptors keyed by security ID.
//
// $SDS repeats its contents in mirrored blocks, so entries are deduplicated by
// security ID rather than by modelling the block layout. Malformed entries are
// skipped rather than ending the walk, so damage in one region does not hide the
// descriptors around it.
func parseSDSStream(sds []byte) map[uint32]*SecurityDescriptor {
	descriptors := make(map[uint32]*SecurityDescriptor)

	for offset := 0; offset+SDSEntryHeaderSize <= len(sds); {
		securityID := ReadUint32LE(sds, offset+4)
		entryLength := int(ReadUint32LE(sds, offset+16))

		// A zero header marks unused space, which on a mirrored $SDS means the
		// remainder of this block is padding. Skip to the next 16-byte slot and
		// keep looking rather than stopping outright.
		if entryLength == 0 {
			offset = AlignUp(offset+1, 16)
			continue
		}

		if entryLength < SDSEntryHeaderSize || offset+entryLength > len(sds) {
			offset = AlignUp(offset+1, 16)
			continue
		}

		if _, seen := descriptors[securityID]; !seen {
			descriptorBytes := sds[offset+SDSEntryHeaderSize : offset+entryLength]
			if sd, err := ParseSecurityDescriptor(descriptorBytes); err == nil {
				descriptors[securityID] = sd
			}
		}

		offset = AlignUp(offset+entryLength, 16)
	}

	return descriptors
}
