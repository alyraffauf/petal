package tui

import (
	"os"
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
	if m.h == nil || m.h.record.ID == "" || m.Cwd == "" || len(entries) == 0 {
		return
	}
	messages := make([]sessions.Message, 0, len(entries))
	for _, e := range entries {
		if e.Role == compactionRole {
			c := m.compactions[e.Text]
			if c == nil {
				continue
			}
			e = entry{"system", c.display}
		}
		messages = append(messages, sessions.Message{Role: e.Role, Body: e.Text})
	}
	if err := sessions.SaveTranscript(m.Cwd, m.h.record.ID, messages, replace); err != nil {
		m.recordNote("Could not index transcript · " + err.Error())
	}
}

const searchPageSize = 40

type transcriptSearch struct {
	input                        textinput.Model
	cwd, account, status         string
	hits                         []sessions.Hit
	index, offset, first, firstY int
	more                         bool
	revision                     int
	reader                       []entry
	anchor                       int
	backlog                      backlogViewport
	markdown                     markdownCache
}
type transcriptResults struct {
	page     *transcriptSearch
	revision int
	hits     []sessions.Hit
	err      error
	run      bool
}

func (p *picker) openTranscriptSearch() {
	cwd, err := os.Getwd()
	s := &transcriptSearch{cwd: cwd, account: p.cfg.PinnedAccount, input: p.search}
	if p.center.owner != nil {
		s.cwd = p.center.owner.Cwd
		s.account = p.center.owner.cfg.PinnedAccount
	}
	s.input.Reset()
	s.input.Placeholder = "Words or a quoted phrase"
	if err != nil && s.cwd == "" {
		s.status = err.Error()
	}
	p.transcripts = s
}
func (s *transcriptSearch) request(offset int) tea.Cmd {
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
	s.reader, s.anchor = entries, anchor
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
		return nil
	case tea.MouseClickMsg:
		if s.reader == nil && t.Button == tea.MouseLeft && t.Y >= s.firstY && t.Y < p.height-navFooterHeight(p.height) {
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
			return tea.Quit
		case "esc":
			if s.reader != nil {
				s.reader = nil
				s.markdown = markdownCache{}
			} else {
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
