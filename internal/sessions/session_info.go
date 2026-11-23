package sessions

import (
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// ApplySessionInfo applies the ACP metadata patch, preserving fields the adapter omitted.
func ApplySessionInfo(record *Record, info acp.SessionSessionInfoUpdate) bool {
	previousTitle, previousUpdated := record.Title, record.Updated
	if !info.Title.IsUnset() {
		record.Title, _ = info.Title.Value()
	}
	if stamp, ok := info.UpdatedAt.Value(); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			record.Updated = parsed
		}
	} else if info.UpdatedAt.IsNull() {
		record.Updated = time.Time{}
	}
	return record.Title != previousTitle || !record.Updated.Equal(previousUpdated)
}
