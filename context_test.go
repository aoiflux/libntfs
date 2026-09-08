package libntfs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The journal and log walks are the long operations on a real volume — $J
// routinely runs to gigabytes and $LogFile is commonly 64 MB — so each has a
// ...Context variant. These tests pin the two halves of what that is worth: a
// cancelled context stops the walk, and an uncancelled one changes nothing.
//
// Cancellation is checked at an interval rather than per record: per chunk for
// the volume walks, per page for the log, and every usnCancelCheckInterval
// steps for the journal buffer. An already-cancelled context is therefore what
// most of these assert on; a walk that has begun stops at the next checkpoint,
// not mid-record.

// TestEachUSNRecordContextCancellation covers the buffer walk. The callback
// count is the real assertion: walkUSNRecords primes its step counter so the
// first iteration checks, and without that priming only journals longer than
// usnCancelCheckInterval steps would cancel at all.
func TestEachUSNRecordContextCancellation(t *testing.T) {
	journal := buildUSNJournal(t, 0, usnTestRecords(t)...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seen := 0
	err := EachUSNRecordContext(ctx, journal, func(*USNRecord) error {
		seen++
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen != 0 {
		t.Fatalf("a cancelled walk delivered %d records", seen)
	}
}

func TestVolumeEachUSNRecordContextCancellation(t *testing.T) {
	vol := newUSNTestVolume(t, buildUSNJournal(t, 0, usnTestRecords(t)...), 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seen := 0
	err := vol.EachUSNRecordContext(ctx, func(*USNRecord) error {
		seen++
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen != 0 {
		t.Fatalf("a cancelled walk delivered %d records", seen)
	}
}

func TestEachLogRecordContextCancellation(t *testing.T) {
	image := buildLogFileImage(t, buildLogRecord(0x600, LogRecordTypeClient,
		buildNTFSLogData(LogOpInitializeFileRecordSegment, LogOpNoop, 5, nil, []byte("x"), nil)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seen := 0
	err := EachLogRecordContext(ctx, image, func(*LogRecord) error {
		seen++
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen != 0 {
		t.Fatalf("a cancelled walk delivered %d records", seen)
	}
}

func TestVolumeEachLogRecordContextCancellation(t *testing.T) {
	vol := newLogTestVolume(t, buildLogFileImage(t, buildLogRecord(0x600, LogRecordTypeClient,
		buildNTFSLogData(LogOpInitializeFileRecordSegment, LogOpNoop, 5, nil, []byte("x"), nil))))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seen := 0
	err := vol.EachLogRecordContext(ctx, func(*LogRecord) error {
		seen++
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen != 0 {
		t.Fatalf("a cancelled walk delivered %d records", seen)
	}
}

// TestEachLogRecordContextStopsMidWalk cancels from inside the callback rather
// than before the call, which is what a caller with a deadline actually does.
// The log is walked a page at a time, so records already on the current page
// are still delivered; what must not happen is the walk running to the end.
func TestEachLogRecordContextStopsMidWalk(t *testing.T) {
	// Enough records to span several pages, or there is no later page boundary
	// for the walk to stop at.
	const records = 200

	var stream []byte
	for i := range records {
		stream = append(stream, buildLogRecord(uint64(0x600+i), LogRecordTypeClient,
			buildNTFSLogData(LogOpNoop, LogOpNoop, int64(i), nil, nil, nil))...)
	}

	image := buildLogFileImage(t, stream)

	want := 0
	if err := EachLogRecord(image, func(*LogRecord) error {
		want++
		return nil
	}); err != nil {
		t.Fatalf("EachLogRecord failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := 0
	err := EachLogRecordContext(ctx, image, func(*LogRecord) error {
		seen++
		cancel()
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen == 0 {
		t.Fatal("expected the records before the cancellation to be delivered")
	}
	if seen >= want {
		t.Fatalf("cancelled walk delivered %d of %d records; it ran to the end", seen, want)
	}
}

// TestContextVariantsWalkToCompletion is the other half of cancellation: the
// ...Context methods are the implementations the plain ones delegate to, so an
// uncancelled context must walk exactly as far as no context at all.
func TestContextVariantsWalkToCompletion(t *testing.T) {
	journal := buildUSNJournal(t, 0, usnTestRecords(t)...)
	logImage := buildLogFileImage(t, buildLogRecord(0x600, LogRecordTypeClient,
		buildNTFSLogData(LogOpInitializeFileRecordSegment, LogOpNoop, 5, nil, []byte("x"), nil)))

	usnVol := newUSNTestVolume(t, journal, 1)
	logVol := newLogTestVolume(t, logImage)

	// Each pair is the same walk twice: without a context, and with one. Both
	// halves count what they were handed, and the counts must agree.
	walks := []struct {
		name  string
		plain func(count func()) error
		ctx   func(ctx context.Context, count func()) error
	}{
		{
			name:  "EachUSNRecord",
			plain: func(count func()) error { return EachUSNRecord(journal, usnCounter(count)) },
			ctx: func(ctx context.Context, count func()) error {
				return EachUSNRecordContext(ctx, journal, usnCounter(count))
			},
		},
		{
			name:  "Volume.EachUSNRecord",
			plain: func(count func()) error { return usnVol.EachUSNRecord(usnCounter(count)) },
			ctx: func(ctx context.Context, count func()) error {
				return usnVol.EachUSNRecordContext(ctx, usnCounter(count))
			},
		},
		{
			name:  "EachLogRecord",
			plain: func(count func()) error { return EachLogRecord(logImage, logCounter(count)) },
			ctx: func(ctx context.Context, count func()) error {
				return EachLogRecordContext(ctx, logImage, logCounter(count))
			},
		},
		{
			name:  "Volume.EachLogRecord",
			plain: func(count func()) error { return logVol.EachLogRecord(logCounter(count)) },
			ctx: func(ctx context.Context, count func()) error {
				return logVol.EachLogRecordContext(ctx, logCounter(count))
			},
		},
	}

	for _, walk := range walks {
		t.Run(walk.name, func(t *testing.T) {
			plain := 0
			if err := walk.plain(func() { plain++ }); err != nil {
				t.Fatalf("%s failed: %v", walk.name, err)
			}
			if plain == 0 {
				t.Fatalf("%s delivered nothing; the fixture is wrong", walk.name)
			}

			withCtx := 0
			if err := walk.ctx(context.Background(), func() { withCtx++ }); err != nil {
				t.Fatalf("%sContext failed: %v", walk.name, err)
			}

			if withCtx != plain {
				t.Fatalf("%sContext delivered %d records, %s delivered %d",
					walk.name, withCtx, walk.name, plain)
			}
		})
	}
}

func usnCounter(count func()) func(*USNRecord) error {
	return func(*USNRecord) error {
		count()
		return nil
	}
}

func logCounter(count func()) func(*LogRecord) error {
	return func(*LogRecord) error {
		count()
		return nil
	}
}

// TestContextVariantsAcceptNilContext guards the nil check each variant opens
// with. Passing a nil context is a caller bug, but panicking on it part-way
// through a gigabyte-scale journal walk is a worse answer than treating it as
// no deadline.
func TestContextVariantsAcceptNilContext(t *testing.T) {
	journal := buildUSNJournal(t, 0, usnTestRecords(t)...)
	logImage := buildLogFileImage(t, buildLogRecord(0x600, LogRecordTypeClient,
		buildNTFSLogData(LogOpNoop, LogOpNoop, 0, nil, nil, nil)))

	//nolint:staticcheck // Passing nil is exactly what is under test.
	if err := EachUSNRecordContext(nil, journal, func(*USNRecord) error { return nil }); err != nil {
		t.Fatalf("EachUSNRecordContext(nil) failed: %v", err)
	}

	//nolint:staticcheck // As above.
	if err := EachLogRecordContext(nil, logImage, func(*LogRecord) error { return nil }); err != nil {
		t.Fatalf("EachLogRecordContext(nil) failed: %v", err)
	}

	usnVol := newUSNTestVolume(t, journal, 1)
	//nolint:staticcheck // As above.
	if err := usnVol.EachUSNRecordContext(nil, func(*USNRecord) error { return nil }); err != nil {
		t.Fatalf("Volume.EachUSNRecordContext(nil) failed: %v", err)
	}

	logVol := newLogTestVolume(t, logImage)
	//nolint:staticcheck // As above.
	if err := logVol.EachLogRecordContext(nil, func(*LogRecord) error { return nil }); err != nil {
		t.Fatalf("Volume.EachLogRecordContext(nil) failed: %v", err)
	}
}

// TestUSNRecordJSONTags pins the wire names. A change report serialises journal
// evidence straight from these records, so the key names are API: renaming one
// silently breaks every consumer parsing the output, and removing the parallel
// DTOs callers used to need is the whole reason the struct carries tags.
func TestUSNRecordJSONTags(t *testing.T) {
	record := USNRecord{
		MajorVersion:    2,
		MinorVersion:    0,
		USN:             0x1234,
		FileReference:   42,
		FileSequence:    7,
		ParentReference: 5,
		ParentSequence:  3,
		Timestamp:       time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		Reason:          USNReasonRenameNewName,
		SourceInfo:      1,
		SecurityID:      2,
		FileAttributes:  0x20,
		Name:            "evidence.txt",
	}

	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{
		"major_version", "minor_version", "usn",
		"file_reference", "file_sequence", "parent_reference", "parent_sequence",
		"timestamp", "reason", "source_info", "security_id", "file_attributes", "name",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON key %q in %s", key, raw)
		}
	}

	// An upper-case letter in a key means a field serialising under its Go name,
	// which is how a newly added field with no tag would show up.
	for key := range fields {
		if hasUpper(key) {
			t.Errorf("field %q is not snake_case, so it carries no JSON tag", key)
		}
	}

	// The identity pairs are what a report and a journal are joined on, so check
	// the values survive the round trip rather than only the key names.
	var decoded USNRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("round-trip Unmarshal failed: %v", err)
	}

	if decoded.FileReference != record.FileReference || decoded.FileSequence != record.FileSequence ||
		decoded.ParentReference != record.ParentReference || decoded.ParentSequence != record.ParentSequence {
		t.Fatalf("identity did not survive the round trip: %+v", decoded)
	}
	if !decoded.Timestamp.Equal(record.Timestamp) {
		t.Fatalf("timestamp round-tripped as %v, want %v", decoded.Timestamp, record.Timestamp)
	}
}

func hasUpper(s string) bool {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}

	return false
}
