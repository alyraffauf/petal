package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"petal/internal/sessions"
)

// Collect only completed subagent output. A parent can have several active
// children, each publishing independently when its turn reaches idle.
func (b *agentBrowser) completedEntries(entries []entry) []entry {
	for i := range b.children {
		c := &b.children[i]
		title := c.title
		if title == "" {
			title = c.id
		}
		for _, e := range c.transcript[c.indexed:] {
			entries = append(entries, entry{"subagent", "**" + title + "** · " + e.Role + "\n\n" + e.Text})
		}
		c.indexed = len(c.transcript)
	}
	for _, m := range b.mail {
		if m.display != "" && m.display != m.indexed {
			entries = append(entries, entry{"message", m.display})
			m.indexed = m.display
		}
	}
	return entries
}

func (m *terminal) indexMessages(entries []entry, replace bool) {
	if m.h == nil || m.h.daemon != nil || m.h.record.ID == "" || m.Cwd == "" || (len(entries) == 0 && !replace) {
		return
	}
	messages := make([]sessions.Message, 0, len(entries))
	for _, e := range entries {
		if e.Role == compactionRole {
			c := m.compactions[e.Text]
			if c == nil {
				continue
			}
			messages = append(messages, sessions.Message{Role: "compaction", Body: c.display, SourceID: e.Text})
			continue
		}
		if preview, ok := m.images[e.Text]; ok {
			e.Text = preview.caption
		}
		messages = append(messages, sessions.Message{Role: e.Role, Body: e.Text})
	}
	if err := sessions.SaveTranscript(m.Cwd, m.h.record.ID, messages, replace); err != nil {
		m.recordNote("Could not index transcript · " + err.Error())
	}
}

const searchPageSize = 40

type transcriptSearch struct {
	cancel                       context.CancelFunc
	lexical                      int
	input                        textinput.Model
	cwd, account, status         string
	hits                         []sessions.Hit
	index, offset, first, firstY int
	endY                         int
	more                         bool
	revision                     int
	reader                       []entry
	readerHit                    sessions.Hit
	anchor                       int
	backlog                      backlogViewport
	markdown                     markdownCache
}
type transcriptResults struct {
	semantic bool
	search   *sessions.ProjectSearch
	coverage sessions.HistoryCoverage
	final    bool
	page     *transcriptSearch
	revision int
	hits     []sessions.Hit
	err      error
	run      bool
}

func (p *picker) stopTranscriptSearch() {
	if s := p.transcripts; s != nil && s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (p *picker) openTranscriptSearch() {
	p.stopTranscriptSearch()
	cwd := p.center.cwd
	s := &transcriptSearch{cwd: cwd, account: p.cfg.PinnedAccount, input: p.search}
	if p.center.owner != nil {
		s.cwd = p.center.owner.Cwd
		s.account = p.center.owner.cfg.PinnedAccount
	}
	s.input.Reset()
	s.input.Placeholder = "Words or a quoted phrase"
	p.transcripts = s
}
func (s *transcriptSearch) request(offset int) tea.Cmd {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.revision++
	s.index, s.offset, s.hits, s.more = 0, offset, nil, false
	s.status = ""
	if strings.TrimSpace(s.input.Value()) == "" {
		return nil
	}
	s.status = "Searching…"
	msg := transcriptResults{page: s, revision: s.revision, run: true}
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return msg })
}
func (s *transcriptSearch) openHit() {
	if len(s.hits) == 0 {
		return
	}
	hit := s.hits[s.index]
	rows, err := sessions.Transcript(s.cwd, hit.Session)
	if err != nil {
		s.status = err.Error()
		return
	}
	entries := make([]entry, len(rows))
	anchor := -1
	for i, row := range rows {
		entries[i] = entry{row.Role, row.Body}
		if row.Rowid == hit.Row {
			anchor = i
		}
	}
	if anchor < 0 {
		s.status = "This transcript changed. Edit the query to refresh."
		return
	}
	s.reader, s.readerHit, s.anchor = entries, hit, anchor
	s.backlog = backlogViewport{}
	s.markdown = markdownCache{}
}
func (p *picker) transcriptInput(msg tea.Msg) tea.Cmd {
	s := p.transcripts
	switch t := msg.(type) {
	case transcriptResults:
		if t.page != s || t.revision != s.revision {
			return nil
		}
		if t.run {
			query, offset := s.input.Value(), s.offset
			return func() tea.Msg {
				t.run = false
				t.hits, t.err = sessions.Search(s.cwd, s.account, query, offset, searchPageSize+1)
				return t
			}
		}
		if t.semantic {
			if t.search == nil || t.err != nil {
				if len(s.hits) == 0 {
					s.status = "No matching messages"
				} else {
					s.status = ""
				}
				return nil
			}
			selected := int64(-1)
			if len(s.hits) > 0 {
				selected = s.hits[s.index].Row
			}
			s.hits = s.hits[:s.lexical]
			s.hits = append(s.hits, t.hits...)
			s.index = 0
			for i, h := range s.hits {
				if h.Row == selected {
					s.index = i
					break
				}
			}
			s.status = ""
			if len(s.hits) == 0 {
				s.status = "No matching messages"
			}
			if t.coverage.Indexed < t.coverage.Window && !t.final {
				s.status = fmt.Sprintf("Indexing messages · %d/%d", t.coverage.Indexed, t.coverage.Window)
				ctx, cancel := context.WithCancel(context.Background())
				if s.cancel != nil {
					s.cancel()
				}
				s.cancel = cancel
				legacyIndex := p.legacyIndex
				return func() tea.Msg {
					if legacyIndex {
						var more bool
						more, t.err = t.search.Backfill(ctx, time.Second)
						t.final = !more
						if t.err == nil {
							t.hits, t.coverage, t.err = t.search.Matches(ctx)
						}
						return t
					}
					select {
					case <-ctx.Done():
						t.err = ctx.Err()
					case <-time.After(time.Second):
						t.hits, t.coverage, t.err = t.search.Matches(ctx)
					}
					return t
				}
			}
			return nil
		}
		s.status = ""
		if t.err != nil {
			s.status = t.err.Error()
			return nil
		}
		s.more = len(t.hits) > searchPageSize
		s.hits = t.hits[:min(len(t.hits), searchPageSize)]
		if len(s.hits) == 0 {
			s.status = "No matching messages"
		}
		s.lexical = len(s.hits)
		if s.offset == 0 && p.cfg.History.Embeddings.Provider != "off" {
			ctx, cancel := context.WithCancel(context.Background())
			s.cancel = cancel
			cwd, account, query, settings := s.cwd, s.account, s.input.Value(), p.cfg.History.Embeddings
			if s.lexical == 0 {
				s.status = "Searching by meaning…"
			}
			return func() tea.Msg {
				t.semantic = true
				t.search = sessions.PrepareProjectSearch(ctx, cwd, account, query, settings)
				if t.search != nil {
					t.hits, t.coverage, t.err = t.search.Matches(ctx)
				}
				return t
			}
		}
		return nil
	case tea.MouseClickMsg:
		if s.reader == nil && t.Button == tea.MouseLeft && t.Y >= s.firstY && t.Y < s.endY {
			i := s.first + (t.Y-s.firstY)/2
			if i < len(s.hits) {
				s.index = i
				s.openHit()
			}
		}
		return nil
	case tea.MouseWheelMsg:
		d := 1
		if t.Button == tea.MouseWheelUp {
			d = -1
		} else if t.Button != tea.MouseWheelDown {
			return nil
		}
		if s.reader != nil {
			s.backlog.move(d * 3)
		} else {
			s.index = max(0, min(len(s.hits)-1, s.index+d))
		}
		return nil
	case tea.KeyPressMsg:
		switch t.String() {
		case "ctrl+c":
			p.stopTranscriptSearch()
			return tea.Quit
		case "esc":
			if s.reader != nil {
				s.reader = nil
				s.readerHit = sessions.Hit{}
				s.markdown = markdownCache{}
			} else {
				p.stopTranscriptSearch()
				p.transcripts = nil
			}
			return nil
		}
		if s.reader != nil {
			switch t.String() {
			case "up":
				s.backlog.move(-1)
			case "down":
				s.backlog.move(1)
			case "pgup":
				s.backlog.move(-max(1, p.height-5))
			case "pgdown":
				s.backlog.move(max(1, p.height-5))
			}
			return nil
		}
		switch t.String() {
		case "up":
			s.index = max(0, s.index-1)
			return nil
		case "down":
			s.index = max(0, min(len(s.hits)-1, s.index+1))
			return nil
		case "pgdown":
			if s.more {
				return s.request(s.offset + searchPageSize)
			}
			return nil
		case "pgup":
			if s.offset > 0 {
				return s.request(s.offset - searchPageSize)
			}
			return nil
		case "enter":
			s.openHit()
			return nil
		}
	case tea.PasteMsg:
		if s.reader != nil {
			return nil
		}
		t.Content = petalLabel(t.Content)
		msg = t
	}
	old := s.input.Value()
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	if old != s.input.Value() {
		return tea.Batch(cmd, s.request(0))
	}
	return cmd
}
