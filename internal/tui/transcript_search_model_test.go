package tui

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"petal/internal/sessions"
)

type pickerSearchFixture struct {
	cwd  string
	hits []sessions.Hit
}

func newPickerSearchFixture(t testing.TB) pickerSearchFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var messages []sessions.Message
	for i := range 45 {
		body := fmt.Sprintf("fixture mark%02d", i)
		if i == 0 {
			body += " marigold"
		}
		messages = append(messages, sessions.Message{Role: "assistant", Body: body})
	}
	if err := sessions.SaveTranscript(cwd, "fixture", messages, false); err != nil {
		t.Fatal(err)
	}
	hits, err := sessions.Search(cwd, "", "fixture", 0, len(messages))
	if err != nil || len(hits) != len(messages) {
		t.Fatal(hits, err)
	}
	for i := range hits {
		hits[i].Title = fmt.Sprintf("result%02d", i)
		hits[i].Snippet = hits[i].Title
	}
	return pickerSearchFixture{cwd, hits}
}

func (fixture pickerSearchFixture) replay(t *testing.T, trace []byte) {
	t.Helper()
	p := picker{width: 78, height: 9, search: textinput.New()}
	p.search.Focus()
	p.center.cwd = fixture.cwd
	p.openTranscriptSearch()
	defer func() {
		if p.transcripts != nil && p.transcripts.cancel != nil {
			p.transcripts.cancel()
		}
	}()

	// The model tracks request identity separately from delivery tokens,
	// selection by position in the expected ranking, and the reader by hit ID.
	var jobs []transcriptResults
	current, page, cursor := -1, 0, 0
	var keywords, ranking []sessions.Hit
	var reading *sessions.Hit
	hasNext, opened := false, true
	send := func(msg tea.Msg) tea.Cmd {
		_, cmd := p.Update(msg)
		return cmd
	}
	key := func(code rune) tea.Cmd {
		return send(tea.KeyPressMsg{Code: code})
	}
	request := func(cmd tea.Cmd, target int) {
		t.Helper()
		if cmd == nil {
			t.Fatal("missing search command")
		}
		current, page, cursor = len(jobs), target, 0
		keywords, ranking, hasNext = nil, nil, false
		s := p.transcripts
		jobs = append(jobs, transcriptResults{page: s, revision: s.revision})
	}

	for step, operation := range trace[:min(len(trace), 96)] {
		var cmd tea.Cmd
		wantCommand := false
		arg := int(operation >> 4)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("step %d after %x: %s", step, trace[:step+1], fmt.Sprintf(format, args...))
		}
		switch operation & 15 {
		case 0: // Edit the query, or reopen the picker.
			if !opened {
				p.openTranscriptSearch()
				opened = true
			}
			cmd = send(tea.PasteMsg{Content: "q"})
			if reading == nil {
				request(cmd, 0)
				wantCommand = true
			}
		case 1, 2: // Complete a pending job, potentially after a newer request.
			if len(jobs) == 0 {
				continue
			}
			job := current
			if operation&15 == 2 || job < 0 {
				job = arg % len(jobs)
			}
			msg := jobs[job]
			if msg.page == nil {
				continue
			}
			jobs[job].page = nil
			active := opened && job == current
			switch {
			case !msg.semantic:
				switch arg % 4 {
				case 0:
					msg.hits = fixture.hits[:2:2]
				case 1:
					msg.hits = fixture.hits[:41:41]
				case 3:
					msg.err = errors.New("keyword failed")
				}
				if active && msg.err == nil {
					keywords = msg.hits[:min(40, len(msg.hits))]
					ranking = slices.Clone(keywords)
					hasNext = len(msg.hits) > 40
					wantCommand = page == 0
					if wantCommand {
						jobs[job] = transcriptResults{page: msg.page, revision: msg.revision, semantic: true}
					}
				}
			default:
				if arg != 15 {
					msg.search = &sessions.ProjectSearch{}
					msg.final = arg >= 8
					msg.coverage = sessions.HistoryCoverage{Indexed: 1, Window: 2}
					tail := slices.Clone(fixture.hits[41:45])
					for i := range tail {
						tail[i].Semantic = true
					}
					if arg&1 != 0 {
						slices.Reverse(tail)
					}
					msg.hits = tail[:arg%5]
					if active {
						var selected sessions.Hit
						if len(ranking) > 0 {
							selected = ranking[cursor]
						}
						ranking = slices.Concat(keywords, msg.hits)
						cursor = max(0, slices.Index(ranking, selected))
						wantCommand = !msg.final
						if wantCommand {
							jobs[job] = transcriptResults{page: msg.page, revision: msg.revision, semantic: true}
						}
					}
				}
			}
			cmd = send(msg)
		case 3, 4, 11, 12: // Move selection or scroll inside the reader.
			if !opened {
				continue
			}
			down := operation&1 == 0
			if operation&15 >= 11 {
				button := tea.MouseWheelUp
				if down {
					button = tea.MouseWheelDown
				}
				cmd = send(tea.MouseWheelMsg{Button: button})
			} else if down {
				cmd = key(tea.KeyDown)
			} else {
				cmd = key(tea.KeyUp)
			}
			if reading == nil {
				if down && cursor+1 < len(ranking) {
					cursor++
				} else if !down && cursor > 0 {
					cursor--
				}
			}
		case 5, 6: // Page navigation is local to the reader when open.
			if !opened {
				continue
			}
			if operation&15 == 5 {
				cmd = key(tea.KeyPgDown)
				if reading == nil && hasNext {
					request(cmd, page+40)
					wantCommand = true
				}
			} else {
				cmd = key(tea.KeyPgUp)
				if reading == nil && page > 0 {
					request(cmd, page-40)
					wantCommand = true
				}
			}
		case 7:
			if opened {
				cmd = key(tea.KeyEnter)
				if reading == nil && len(ranking) > 0 {
					hit := ranking[cursor]
					reading = &hit
				}
			}
		case 8:
			if opened {
				cmd = key(tea.KeyEscape)
				if reading != nil {
					reading = nil
				} else {
					opened = false
					current, page, cursor = -1, 0, 0
					keywords, ranking, hasNext = nil, nil, false
				}
			}
		case 9:
			if opened {
				cmd = send(tea.WindowSizeMsg{Width: 78, Height: 3 + arg})
			}
		case 10: // Hit-test the terminal rows, including blank rows and footer.
			if !opened {
				continue
			}
			rows := strings.Split(ansi.Strip(p.View().Content), "\n")
			if reading == nil && arg < len(rows) {
				for i, hit := range ranking {
					if strings.Contains(rows[arg], hit.Title) {
						cursor = i
						reading = &hit
						break
					}
				}
			}
			cmd = send(tea.MouseClickMsg{Button: tea.MouseLeft, Y: arg})
		case 13:
			if reading != nil {
				frame := ansi.Strip(p.View().Content)
				if p.height >= 4 && !strings.Contains(frame, reading.Title) {
					fail("reader lost %s in frame %q", reading.Title, frame)
				}
			}
		}
		if (cmd != nil) != wantCommand {
			fail("command present = %t, want %t", cmd != nil, wantCommand)
		}
		s := p.transcripts
		if (s != nil) != opened {
			fail("search open = %t, want %t", s != nil, opened)
		}
		if !opened {
			continue
		}
		if s.offset != page || s.more != hasNext || s.index != cursor || !slices.Equal(s.hits, ranking) {
			fail("offset/more/index/hit count = %d/%t/%d/%d, want %d/%t/%d/%d",
				s.offset, s.more, s.index, len(s.hits), page, hasNext, cursor, len(ranking))
		}
		if (s.reader != nil) != (reading != nil) {
			fail("reader open = %t, want %t", s.reader != nil, reading != nil)
		}
	}
}

var pickerSearchTraces = [][]byte{
	{0, 0x21, 0x31, 4, 7, 0x41, 13, 5, 8, 0x81},
	{0, 0x21, 0x31, 4, 0x41, 7, 0x51, 13, 8},
	{0, 0x11, 0x7a, 5, 6, 2, 0x11, 5, 0x11},
	{0, 0, 2, 8, 0, 0x11, 0x81},
	{0, 8, 0, 2, 0x11},
}

func TestTranscriptSearchModel(t *testing.T) {
	fixture := newPickerSearchFixture(t)
	for i, trace := range pickerSearchTraces {
		t.Run(fmt.Sprint(i), func(t *testing.T) { fixture.replay(t, trace) })
	}
	for seed := uint64(0); seed < 128; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			random := rand.New(rand.NewPCG(seed, 0))
			trace := make([]byte, 64)
			for i := range trace {
				trace[i] = byte(random.Uint32())
			}
			fixture.replay(t, trace)
		})
	}
}

func FuzzTranscriptSearchModel(f *testing.F) {
	fixture := newPickerSearchFixture(f)
	for _, trace := range pickerSearchTraces {
		f.Add(trace)
	}
	f.Fuzz(func(t *testing.T, trace []byte) { fixture.replay(t, trace) })
}

func TestTranscriptSearchReaderScrollModel(t *testing.T) {
	fixture := newPickerSearchFixture(t)
	s := &transcriptSearch{cwd: fixture.cwd, hits: fixture.hits[20:21]}
	p := picker{width: 78, height: 9, transcripts: s}
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Every fixture message occupies one row, with a blank row between messages.
	// The reader has six body rows at 78x9.
	var transcriptRows []string
	for i := range fixture.hits {
		if i > 0 {
			transcriptRows = append(transcriptRows, "")
		}
		transcriptRows = append(transcriptRows, fmt.Sprintf("fixture mark%02d", i))
	}
	top := 2 * 24
	verify := func() {
		t.Helper()
		rows := strings.Split(ansi.Strip(p.View().Content), "\n")
		for i, want := range transcriptRows[top : top+6] {
			if want == "" {
				if strings.Contains(rows[i+2], "fixture mark") {
					t.Fatalf("expected a separator at transcript row %d, got %q", top+i, rows[i+2])
				}
			} else if !strings.Contains(rows[i+2], want) {
				t.Fatalf("expected %q at transcript row %d, got %q", want, top+i, rows[i+2])
			}
		}
	}
	verify()

	moves := []struct {
		event tea.Msg
		delta int
	}{
		{tea.KeyPressMsg{Code: tea.KeyUp}, -1},
		{tea.KeyPressMsg{Code: tea.KeyDown}, 1},
		{tea.KeyPressMsg{Code: tea.KeyPgUp}, -4},
		{tea.KeyPressMsg{Code: tea.KeyPgDown}, 4},
		{tea.MouseWheelMsg{Button: tea.MouseWheelUp}, -3},
		{tea.MouseWheelMsg{Button: tea.MouseWheelDown}, 3},
	}
	random := rand.New(rand.NewPCG(0, 0))
	for i := range 200 {
		// Drive through both ends as well as alternating scroll directions.
		move := moves[random.IntN(len(moves))]
		if i < 24 {
			move = moves[3]
		} else if i < 48 {
			move = moves[2]
		}
		p.Update(move.event)
		top = min(max(0, top+move.delta), len(transcriptRows)-6)
		verify()
	}
}
