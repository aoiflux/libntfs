package libntfs

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"
)

// TestCapabilitiesFormatConstants pins every field that is a property of NTFS
// rather than of a volume.
//
// The point of writing them all out is that a change to any one of them is a
// claim about the format, and should have to be made deliberately.
func TestCapabilitiesFormatConstants(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildReportFixtureImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer vol.Close()

	caps := vol.Capabilities()

	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"UnicodeNames", caps.UnicodeNames, true},
		{"CaseSensitive", caps.CaseSensitive, false},
		{"CreationTimes", caps.CreationTimes, true},
		{"ModificationTimes", caps.ModificationTimes, true},
		{"AccessTimes", caps.AccessTimes, true},
		{"MetadataChangeTimes", caps.MetadataChangeTimes, true},
		{"SubSecondTimestamps", caps.SubSecondTimestamps, true},
		{"TimezoneOffsets", caps.TimezoneOffsets, false},
		{"POSIXPermissions", caps.POSIXPermissions, false},
		{"SecurityDescriptors", caps.SecurityDescriptors, true},
		{"HardLinks", caps.HardLinks, true},
		{"SymbolicLinks", caps.SymbolicLinks, true},
		{"ExtendedAttributes", caps.ExtendedAttributes, true},
		{"AlternateDataStreams", caps.AlternateDataStreams, true},
		{"SparseFiles", caps.SparseFiles, true},
		{"Compression", caps.Compression, true},
		{"Encryption", caps.Encryption, true},
		{"StableFileIdentity", caps.StableFileIdentity, true},
		{"IdentityReuseCounter", caps.IdentityReuseCounter, true},
		{"DeletedEntriesSurvive", caps.DeletedEntriesSurvive, true},
		{"Journaled", caps.Journaled, true},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestCapabilitiesChangeJournalPresent covers a volume that has a usable
// $UsnJrnl.
func TestCapabilitiesChangeJournalPresent(t *testing.T) {
	// One sparse cluster ahead of the records, as a real $J has.
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, usnTestRecords(t)...), 1)

	if !vol.Capabilities().ChangeJournal {
		t.Error("ChangeJournal = false on a volume whose $UsnJrnl holds records")
	}

	present, err := vol.HasChangeJournal()
	if err != nil {
		t.Errorf("HasChangeJournal returned %v, want no error", err)
	}
	if !present {
		t.Error("HasChangeJournal = false on a volume whose $UsnJrnl holds records")
	}

	// Journaled is about $LogFile and must not move with it.
	if !vol.Capabilities().Journaled {
		t.Error("Journaled = false; every NTFS volume has $LogFile")
	}
}

// newVolumeWithoutUSNJournal builds a volume whose $Extend directory is present
// and readable but does not name $UsnJrnl.
//
// That is what USN logging switched off looks like: an ordinary NTFS volume in
// every respect but one. A volume with no $Extend at all would be a different
// thing — entry 11 is a reserved record every NTFS volume has, so its absence is
// damage rather than configuration, and probeChangeJournal reports it as such.
func newVolumeWithoutUSNJournal(t testing.TB) *Volume {
	t.Helper()

	const otherEntry = 24

	// The index names something other than $UsnJrnl, so the directory walk
	// succeeds and finds nothing.
	indexRoot := buildIndexRootValue(4096,
		buildIndexEntry(otherEntry, 1,
			buildFileNameAttrValue(MFTEntryExtend, 1, "$Quota", NamespaceWin32, 0, 0, 0)))

	extend := buildTestMFTRecord(t, testRecordOpts{
		flags:      MFTFlagInUse | MFTFlagDirectory,
		name:       "$Extend",
		namespace:  NamespaceWin32,
		extraAttrs: [][]byte{buildNamedResidentAttr(AttrTypeIndexRoot, 2, "$I30", indexRoot)},
	})

	other := buildTestMFTRecord(t, testRecordOpts{
		flags: MFTFlagInUse, name: "$Quota", namespace: NamespaceWin32,
	})

	image := buildTestNTFSImageAt(t, map[uint64][]byte{
		MFTEntryExtend: extend,
		otherEntry:     other,
	})

	vol, err := Open(&mockReaderAt{data: image})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { vol.Close() })

	return vol
}

// TestCapabilitiesChangeJournalAbsent is the test that makes ChangeJournal
// worth having.
//
// A field that were a format constant would report true here too, since the
// fixture is an ordinary NTFS volume in every respect but the one that matters:
// its $Extend directory does not name $UsnJrnl, which is what a volume whose USN
// logging was switched off looks like.
func TestCapabilitiesChangeJournalAbsent(t *testing.T) {
	vol := newVolumeWithoutUSNJournal(t)

	caps := vol.Capabilities()
	if caps.ChangeJournal {
		t.Error("ChangeJournal = true on a volume whose $Extend does not name $UsnJrnl")
	}

	// Everything else must be unmoved, or the field is reporting something
	// broader than the journal.
	if !caps.Journaled {
		t.Error("Journaled = false; $LogFile does not depend on $UsnJrnl")
	}
	if !caps.UnicodeNames || !caps.HardLinks || !caps.SparseFiles {
		t.Error("a missing change journal changed a format constant")
	}
}

// TestHasChangeJournalDistinguishesAbsentFromUnreadable covers the reason
// HasChangeJournal exists alongside the bool.
func TestHasChangeJournalDistinguishesAbsentFromUnreadable(t *testing.T) {
	absent := newVolumeWithoutUSNJournal(t)

	present, err := absent.HasChangeJournal()
	if present {
		t.Error("HasChangeJournal = true on a volume whose $Extend does not name $UsnJrnl")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("HasChangeJournal error = %v, want one wrapping ErrFileNotFound so "+
			"a caller can tell a deleted journal from a volume it could not read", err)
	}

	// A volume with no $Extend at all is damaged rather than configured, and
	// must not come back as a plain absence.
	damaged := newTestMFTVolume(t, [][]byte{
		buildTestMFTRecord(t, testRecordOpts{
			flags: MFTFlagInUse, name: "a.txt", namespace: NamespaceWin32,
		}),
	})
	damagedPresent, err := damaged.HasChangeJournal()
	if damagedPresent {
		t.Error("HasChangeJournal = true on a volume with no $Extend")
	}
	if err == nil {
		t.Error("HasChangeJournal on a volume with no $Extend returned no error")
	}
	if errors.Is(err, ErrFileNotFound) {
		t.Errorf("HasChangeJournal error = %v; an unreachable $Extend is damage, "+
			"not a journal that was never enabled", err)
	}

	// A closed volume is a volume that could not be asked, which is a third
	// answer again and must not be cached as an absence.
	closed := newUSNTestVolume(t, buildUSNJournal(t, 0, usnTestRecords(t)...), 1)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := closed.HasChangeJournal(); !errors.Is(err, ErrVolumeClosed) {
		t.Errorf("HasChangeJournal on a closed volume = %v, want ErrVolumeClosed", err)
	}
}

// TestHasChangeJournalCaches covers the cache, which exists because the answer
// costs a directory read and Capabilities gives no sign of that.
func TestHasChangeJournalCaches(t *testing.T) {
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, usnTestRecords(t)...), 1)

	first, err := vol.HasChangeJournal()
	if err != nil {
		t.Fatalf("HasChangeJournal failed: %v", err)
	}
	for range 3 {
		got, err := vol.HasChangeJournal()
		if err != nil {
			t.Fatalf("a repeated HasChangeJournal failed: %v", err)
		}
		if got != first {
			t.Fatalf("HasChangeJournal = %v on a repeat, want the cached %v", got, first)
		}
	}

	vol.changeJournalMu.RLock()
	cached := vol.changeJournalInit
	vol.changeJournalMu.RUnlock()
	if !cached {
		t.Error("the answer was not cached, so every call pays for a directory read")
	}
}

// TestCapabilitiesNilAndClosedVolume covers the two handles a caller can hold
// that cannot be read from.
func TestCapabilitiesNilAndClosedVolume(t *testing.T) {
	var nilVolume *Volume
	if got := nilVolume.Capabilities(); got != (Capabilities{}) {
		t.Errorf("Capabilities on a nil volume = %+v, want the zero value", got)
	}
	if _, err := nilVolume.HasChangeJournal(); err == nil {
		t.Error("HasChangeJournal on a nil volume returned no error")
	}

	vol, err := Open(&mockReaderAt{data: buildReportFixtureImage(t)})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// What NTFS can record does not depend on whether a handle is still open,
	// and the geometry accessors answer after Close too.
	caps := vol.Capabilities()
	if !caps.UnicodeNames || !caps.Journaled || !caps.IdentityReuseCounter {
		t.Errorf("Capabilities on a closed volume dropped its format constants: %+v", caps)
	}
	if caps.ChangeJournal {
		t.Error("ChangeJournal = true on a closed volume, which cannot have been read")
	}
}

// TestCapabilitiesJSONTagsAreSnakeCase keeps this file inside the convention the
// rest of the family uses, so a consumer deserialising reports from more than
// one of these libraries does not have to special-case NTFS.
func TestCapabilitiesJSONTagsAreSnakeCase(t *testing.T) {
	encoded, err := json.Marshal(Capabilities{})
	if err != nil {
		t.Fatalf("marshalling Capabilities failed: %v", err)
	}

	var keyed map[string]any
	if err := json.Unmarshal(encoded, &keyed); err != nil {
		t.Fatalf("unmarshalling Capabilities failed: %v", err)
	}

	camel := regexp.MustCompile(`[a-z][A-Z]`)
	for key := range keyed {
		if camel.MatchString(key) {
			t.Errorf("json key %q is camelCase, want snake_case", key)
		}
	}

	// A struct whose fields all lost their tags would pass the check above
	// vacuously, since Go's default key for an untagged field is the field name
	// and several of those are single words.
	if _, ok := keyed["unicode_names"]; !ok {
		t.Error("expected a unicode_names key; are the json tags present at all?")
	}
	if len(keyed) < 20 {
		t.Errorf("marshalled %d keys, want one per field", len(keyed))
	}
}
