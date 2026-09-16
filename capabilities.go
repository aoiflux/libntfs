package libntfs

import (
	"errors"
	"fmt"
)

// Capabilities reports what an NTFS volume can record, as distinct from what it
// happens to record.
//
// It exists so a consumer can tell "this format does not keep that" from "that
// was absent here". Without it, a pass comparing two readings of a volume has no
// way to know which absences are facts about the file system and which are facts
// about the files, and a report merging several file systems shows NTFS files as
// having lost POSIX permissions they never had.
//
// Most of these are properties of NTFS and identical for every volume. They are
// fields rather than documentation so a caller can branch on a value instead of
// on a string naming the file system. Each field's comment says which of two
// kinds it is — a property of the format, or something read from this volume —
// because the distinction is the entire point of the type. Only ChangeJournal is
// the second kind.
type Capabilities struct {
	// UnicodeNames reports that names are stored as UTF-16 code units. NTFS
	// always does, in $FILE_NAME. Format.
	UnicodeNames bool `json:"unicode_names"`

	// CaseSensitive reports whether name comparison distinguishes case.
	//
	// Format, but with a caveat worth reading. NTFS stores names in a POSIX
	// namespace that can hold two names differing only in case, so the on-disk
	// format permits sensitivity. Whether any given directory is treated that
	// way is decided by the running Windows kernel — the obcaseinsensitive
	// registry value, and a per-directory flag — and none of that is recorded in
	// a way an image can be read for. This library folds case when resolving
	// paths and stream names, so false describes both Windows' default and what
	// libntfs itself does. It is not a reading of this volume, and a consumer
	// must not treat it as evidence that two names differing only in case cannot
	// coexist here.
	CaseSensitive bool `json:"case_sensitive"`

	// CreationTimes, ModificationTimes, AccessTimes and MetadataChangeTimes
	// report which timestamps a record carries. NTFS has all four in
	// $STANDARD_INFORMATION, and a second copy of the same four in $FILE_NAME
	// that most tools do not rewrite — see [NTFSTimestamps]. Format.
	//
	// MetadataChangeTimes is the ctime analogue, the moment the record's own
	// metadata last changed. Its presence is why a rename on NTFS leaves a
	// timestamp behind, which on a format without one it would not.
	CreationTimes       bool `json:"creation_times"`
	ModificationTimes   bool `json:"modification_times"`
	AccessTimes         bool `json:"access_times"`
	MetadataChangeTimes bool `json:"metadata_change_times"`

	// SubSecondTimestamps reports that timestamps carry a fraction of a second.
	// NTFS records FILETIME in 100-nanosecond units, so every timestamp does.
	// Format.
	SubSecondTimestamps bool `json:"sub_second_timestamps"`

	// TimezoneOffsets reports that a timestamp can be anchored to UTC by an
	// offset stored beside it. NTFS stores no such offset because it needs none:
	// FILETIME is absolute UTC already. False here therefore means "no offset is
	// recorded", not "the reading is unanchored". Format.
	TimezoneOffsets bool `json:"timezone_offsets"`

	// POSIXPermissions reports whether records carry Unix ownership and mode.
	// NTFS does not; its model is the access control list, which
	// SecurityDescriptors reports instead. Format.
	//
	// WSL stores uid, gid and mode on individual files in the $LXUID, $LXGID and
	// $LXMOD extended attributes, readable through [File.LinuxAttributes]. That
	// is a property of particular files rather than of the volume, so it is
	// covered by ExtendedAttributes and does not make this true.
	POSIXPermissions bool `json:"posix_permissions"`

	// SecurityDescriptors reports that the volume carries access control lists,
	// which NTFS keeps in $Secure:$SDS and this library parses. It is the NTFS
	// counterpart of POSIXPermissions and the reason that field is false.
	// Format.
	SecurityDescriptors bool `json:"security_descriptors"`

	// HardLinks reports that one file's data can be reached by several names,
	// which NTFS supports by holding several $FILE_NAME attributes in one
	// record. Format.
	//
	// [NTFSFile.HardLinkCount] reports how many a record has; this library does
	// not enumerate the other paths, so a count above one means a path is one of
	// several rather than naming which.
	HardLinks bool `json:"hard_links"`

	// SymbolicLinks reports that the volume can hold links resolved by name,
	// which NTFS implements as reparse points — symlinks, mount points and WSL's
	// LX_SYMLINK, all decoded by [File.ReparsePoint]. Format.
	SymbolicLinks bool `json:"symbolic_links"`

	// ExtendedAttributes reports that a record can carry named attributes beyond
	// the standard set, which NTFS holds in $EA and $EA_INFORMATION. Format, and
	// unconditionally so: unlike a format where attributes need a feature flag
	// or a separate tree, every NTFS volume can hold them.
	ExtendedAttributes bool `json:"extended_attributes"`

	// AlternateDataStreams reports that a file can hold more than one $DATA
	// stream, each independently named, sized and laid out. NTFS can, and it is
	// the feature most of the other file systems in this family have no analogue
	// for — a consumer comparing file sizes across volumes will miss content
	// entirely unless it reads [NTFSFile.Streams]. Format.
	AlternateDataStreams bool `json:"alternate_data_streams"`

	// SparseFiles reports that a stream can contain holes that occupy no
	// clusters. NTFS can, and [FileFragment] marks them. Format.
	SparseFiles bool `json:"sparse_files"`

	// Compression reports that a stream can be stored compressed, which NTFS
	// does per compression unit with LZNT1. This library decompresses it.
	// Format.
	Compression bool `json:"compression"`

	// Encryption reports that a stream can be stored encrypted, which NTFS
	// supports through EFS. Format — and the one capability here this library
	// detects but cannot act on: an encrypted stream is reported as such and its
	// layout is readable, but the content is not decrypted and never will be
	// without key material that is not on the volume.
	Encryption bool `json:"encryption"`

	// StableFileIdentity reports that the volume records an identity for a file
	// that a rename does not change. NTFS does: the MFT record number. Format.
	StableFileIdentity bool `json:"stable_file_identity"`

	// IdentityReuseCounter reports that a reused identity can be recognised as
	// reused. NTFS increments a record's sequence number each time the record is
	// taken by a new file, so it can. Format.
	//
	// It is stated separately from StableFileIdentity because the pair is what
	// makes two readings of a volume diffable: on the record number alone, a
	// deleted file whose record was reused reads as a modified one. See
	// [NTFSFile] for the same point at the level of a report entry.
	IdentityReuseCounter bool `json:"identity_reuse_counter"`

	// DeletedEntriesSurvive reports that deleting a file leaves evidence behind
	// rather than erasing it. On NTFS the MFT record keeps its content and is
	// only marked not-in-use, and the directory index keeps the entry in slack,
	// so names, sizes, timestamps and run lists all outlive the deletion. This
	// library surfaces both — see MFTEntry.IsInUse and DirEntry.Deleted. Format.
	DeletedEntriesSurvive bool `json:"deleted_entries_survive"`

	// Journaled reports that the volume keeps a metadata journal this library
	// can read. Every NTFS volume has $LogFile, so this is a format constant and
	// not a reading of the volume — it is true even where the log holds nothing
	// of interest. [Volume.EachLogRecord] is what reads it.
	//
	// It is deliberately not the same question as ChangeJournal. $LogFile
	// records what the file system did to its own structures; $UsnJrnl records
	// which files changed. A consumer wanting the second must read
	// ChangeJournal, and one that reads this field instead will believe a
	// change journal is present on every NTFS volume it ever sees.
	Journaled bool `json:"journaled"`

	// ChangeJournal reports whether $UsnJrnl exists on this volume and holds
	// records.
	//
	// This is the one field here that is a reading of the volume rather than a
	// property of NTFS. USN logging can be switched off and the journal deleted,
	// which leaves a perfectly ordinary volume with no record of which files
	// changed, so a consumer planning to diff two readings needs to know before
	// it starts.
	//
	// False means only that this volume was not seen to have a usable journal.
	// Because [Volume.Capabilities] returns no error, an unreadable $Extend
	// index reads the same as an absent journal, which on a partially written
	// image is a real possibility. Use [Volume.HasChangeJournal] where the
	// difference matters.
	ChangeJournal bool `json:"change_journal"`
}

// Capabilities returns what this volume can record.
//
// Every field but ChangeJournal is fixed by NTFS itself and costs nothing.
// ChangeJournal is read from the volume on the first call and cached, so the
// first call reads the $Extend directory and later ones do not.
//
// A nil volume reports the zero value. A closed one reports the format
// constants with ChangeJournal false, rather than nothing at all: the geometry
// accessors answer after Close and it would be surprising for this to be the
// exception, and what NTFS can record does not depend on whether a handle is
// still open.
func (v *Volume) Capabilities() Capabilities {
	if v == nil {
		return Capabilities{}
	}

	caps := Capabilities{
		UnicodeNames: true,
		// The false values here are written out rather than left to the zero
		// value, because each one is an answer this type exists to give.
		CaseSensitive: false,

		CreationTimes:       true,
		ModificationTimes:   true,
		AccessTimes:         true,
		MetadataChangeTimes: true,
		SubSecondTimestamps: true,
		TimezoneOffsets:     false,

		POSIXPermissions:    false,
		SecurityDescriptors: true,

		HardLinks:            true,
		SymbolicLinks:        true,
		ExtendedAttributes:   true,
		AlternateDataStreams: true,

		SparseFiles: true,
		Compression: true,
		Encryption:  true,

		StableFileIdentity:    true,
		IdentityReuseCounter:  true,
		DeletedEntriesSurvive: true,

		Journaled: true,
	}

	// An error here is indistinguishable from absence in a bool, which is what
	// HasChangeJournal exists to resolve.
	caps.ChangeJournal, _ = v.HasChangeJournal()

	return caps
}

// HasChangeJournal reports whether $UsnJrnl is present on this volume and holds
// records, and says why not when it is not.
//
// It is [Capabilities.ChangeJournal] with the error kept. A journal that was
// never enabled, one that was deleted, and an $Extend directory too damaged to
// read all produce false, and only this method tells them apart — the first two
// return an error wrapping [ErrFileNotFound], and anything else is a volume that
// could not be asked rather than one that answered no.
//
// A journal that exists but is empty, or whose $J is still wholly sparse,
// reports false with no error: it is present and has nothing in it, which is the
// same thing [Volume.EachUSNRecord] would find.
//
// The answer is cached after the first call, except when the volume was closed,
// so calling it repeatedly costs one directory read in total. It is safe for
// concurrent use.
func (v *Volume) HasChangeJournal() (bool, error) {
	if v == nil {
		return false, ErrVolumeClosed
	}

	if v.IsClosed() {
		// Not cached: a closed handle says nothing about the volume, and pinning
		// this answer would make a later question on a reopened volume wrong.
		return false, ErrVolumeClosed
	}

	v.changeJournalMu.RLock()
	if v.changeJournalInit {
		present, err := v.changeJournal, v.changeJournalErr
		v.changeJournalMu.RUnlock()
		return present, err
	}
	v.changeJournalMu.RUnlock()

	v.changeJournalMu.Lock()
	defer v.changeJournalMu.Unlock()
	if v.changeJournalInit {
		return v.changeJournal, v.changeJournalErr
	}

	present, err := v.probeChangeJournal()
	if errors.Is(err, ErrVolumeClosed) {
		// Racing with Close. Same reasoning as above: do not pin it.
		return false, err
	}

	v.changeJournal = present
	v.changeJournalErr = err
	v.changeJournalInit = true

	return present, err
}

// probeChangeJournal opens $Extend\$UsnJrnl:$J and decides whether it is worth
// reading.
func (v *Volume) probeChangeJournal() (bool, error) {
	journal, err := v.OpenUSNJournal()
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			// Genuinely absent, which is an answer rather than a failure.
			return false, fmt.Errorf("%s is not present on this volume: %w",
				USNJournalPath, err)
		}
		return false, err
	}

	// A journal whose $J is empty, or whose allocation is still entirely the
	// sparse leading region, carries no records. Reporting it as present would
	// promise a consumer a history that is not there.
	size := journal.Size()
	if size <= 0 {
		return false, nil
	}
	if usnFirstAllocatedOffset(journal, v.BytesPerCluster()) >= size {
		return false, nil
	}

	return true, nil
}
