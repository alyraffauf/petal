package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"petal/internal/sessions"
)

func forwardUpdate[T any](ctx context.Context, h *harness, raw json.RawMessage) (any, *acp.RequestError) {
	var params struct{ Update T }
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, acp.NewInvalidParams(err.Error())
	}
	select {
	case h.events <- params.Update:
	case <-ctx.Done():
	}
	return nil, nil
}

func (m *terminal) saveSessionInfo() {
	info := m.pendingInfo
	if m.h.record.ID == "" || info.Title.IsUnset() && info.UpdatedAt.IsUnset() {
		return
	}
	m.pendingInfo = acp.SessionSessionInfoUpdate{}
	if err := m.h.updateRecord(func(record *sessions.Record) bool {
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
	}); err != nil {
		m.recordNote("Could not save session details: " + err.Error())
	}
}

func usageDetails(u *acp.SessionUsageUpdate) string {
	s := fmt.Sprintf("Context · %d / %d tokens", u.Used, u.Size)
	if u.Cost != nil {
		s += fmt.Sprintf("\n\nSession cost · %g %s", u.Cost.Amount, u.Cost.Currency)
	}
	return s
}

const compactionRole = "compaction-ref"

type compaction struct {
	status          acp.CompactionStatus
	summary         strings.Builder
	detail, display string
	meta            acp.Meta
}

func (m *terminal) compaction(id string) *compaction {
	if m.compactions == nil {
		m.compactions = make(map[string]*compaction)
	}
	if c := m.compactions[id]; c != nil {
		return c
	}
	c := &compaction{status: acp.CompactionStatusInProgress, display: "**Compacting context**"}
	m.compactions[id] = c
	m.Messages = append(m.Messages, entry{compactionRole, id})
	if m.Busy || m.connection == connecting {
		m.pending.push(event{role: compactionRole, text: id})
	}
	return c
}

func (c *compaction) append(block acp.ContentBlock) {
	c.writeBlock(block)
	if c.status != acp.CompactionStatusInProgress {
		c.refresh()
	}
}

func (c *compaction) writeBlock(block acp.ContentBlock) {
	if block.Text != nil {
		c.summary.WriteString(block.Text.Text)
		return
	}
	raw, err := json.Marshal(block)
	if err == nil {
		c.summary.WriteString(contentText(raw))
	}
}

func (c *compaction) update(u acp.SessionCompactionUpdate) {
	c.status = u.Status
	if !u.Error.IsUnset() {
		c.detail, _ = u.Error.Value()
	}
	if !u.Meta.IsUnset() {
		c.meta, _ = u.Meta.Value()
	}
	if !u.Summary.IsUnset() {
		c.summary.Reset()
		blocks, _ := u.Summary.Value()
		for _, block := range blocks {
			c.writeBlock(block)
		}
	}
	c.refresh()
}

func (c *compaction) refresh() {
	if c.status == acp.CompactionStatusInProgress {
		c.display = "**Compacting context**"
		return
	}
	c.display = strings.TrimSpace("**Context compaction** · " + string(c.status) + "\n\n" + c.detail + "\n\n" + c.summary.String())
}
