package sessions

import (
	"slices"
	"testing"
)

// Run histories with and without stable adapter IDs. Keyed deliveries are
// idempotent. Anonymous replays consume each prior occurrence at most once.
func FuzzTranscriptArchiveReplay(f *testing.F) {
	f.Add(false, []byte{2, 0, 0, 4, 131, 0, 4, 0, 0, 128, 4})
	f.Add(false, []byte{129, 1, 5, 1, 1, 5, 130, 5, 1, 5})
	f.Add(true, []byte{0, 1, 128, 9, 129, 1, 9})
	// Replay the same 17 keyed messages on either side of the short-replay cutoff.
	trace := []byte{16}
	messages := []byte("abcdefghijklmnopq")
	trace = append(trace, messages...)
	trace = append(trace, 128+15)
	trace = append(trace, messages[:16]...)
	trace = append(trace, 128+16)
	f.Add(true, append(trace, messages...))
	f.Fuzz(func(t *testing.T, keyed bool, trace []byte) {
		if len(trace) < 2 || len(trace) > 64 {
			return
		}
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		dir := t.TempDir()
		if _, err := Create(dir, Record{ID: "s", Model: "m"}); err != nil {
			t.Fatal(err)
		}
		var archive []Message
		var references []int64
		for len(trace) > 1 {
			header := trace[0]
			size := min(1+int(header%32), len(trace)-1)
			prior := slices.Clone(archive)
			batch := make([]Message, 0, size)
			for _, code := range trace[1 : size+1] {
				m := Message{
					Role: []string{"user", "assistant"}[code>>2&1],
					Body: []string{"a", "b", "界", ""}[code&3],
				}
				if keyed {
					m.SourceID = string('A' + rune(code>>3))
				}
				batch = append(batch, m)
				if keyed && slices.Contains(archive, m) {
					continue
				}
				if !keyed && header&128 != 0 {
					if i := slices.Index(prior, m); i >= 0 {
						prior = slices.Delete(prior, i, i+1)
						continue
					}
				}
				archive = append(archive, m)
			}
			if err := SaveTranscript(dir, "s", batch, header&128 != 0); err != nil {
				t.Fatal(err)
			}
			rows, err := Transcript(dir, "s")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(archive) {
				t.Fatalf("got %d messages, want %d", len(rows), len(archive))
			}
			for i, row := range rows {
				if row.Role != archive[i].Role || row.Body != archive[i].Body {
					t.Fatalf("row %d = %+v, want %+v", i, row, archive[i])
				}
				if i == len(references) {
					references = append(references, row.Rowid)
				} else if row.Rowid != references[i] {
					t.Fatalf("row %d reference changed", i)
				}
			}
			trace = trace[1+size:]
		}
	})
}
