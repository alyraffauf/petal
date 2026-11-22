//go:build linux || darwin

package livesession

import "encoding/json"

const (
	journalEvents = 4096
	journalBytes  = 8 << 20
)

type journalEntry struct {
	event Event
	size  int
}

// All journal operations run under the live session lock. Readers pull by
// cursor, so publishing never queues a copy of the transcript per attachment.
type journal struct {
	entries []journalEntry
	start   int
	count   int
	bytes   int
	head    uint64
}

func (j *journal) floor() uint64 { return j.head - uint64(j.count) }

func (j *journal) skip(count uint64) {
	*j = journal{head: j.head + count}
}

func (j *journal) append(event Event) {
	event.Sequence = j.head + 1
	raw, _ := json.Marshal(event)
	size := len(raw)
	if size > maxBody {
		j.skip(1)
		return
	}
	if j.entries == nil {
		j.entries = make([]journalEntry, journalEvents)
	}
	j.head++
	for j.count != 0 && (j.count == len(j.entries) || j.bytes+size > journalBytes) {
		j.bytes -= j.entries[j.start].size
		j.entries[j.start] = journalEntry{}
		j.start = (j.start + 1) % len(j.entries)
		j.count--
	}

	index := (j.start + j.count) % len(j.entries)
	j.entries[index] = journalEntry{event, size}
	j.count++
	j.bytes += size
}

func (j *journal) after(after uint64) (Event, *Error) {
	if after < j.floor() {
		return Event{}, &Error{Code: Gap, Message: "event cursor fell behind retained history", Floor: j.floor()}
	}
	if after > j.head {
		return Event{}, failure(Invalid, "event cursor is ahead of the session")
	}
	if after == j.head {
		return Event{}, nil
	}
	return j.entries[(j.start+int(after-j.floor()))%len(j.entries)].event, nil
}
