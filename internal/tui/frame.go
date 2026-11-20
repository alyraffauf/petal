package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// frame reserves the same outer margin for chat and navigation. The child
// receives dimensions and pointer positions relative to the content area.
type frame struct {
	tea.Model
	background       string
	width, left, top int
}

func (f *frame) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch event := msg.(type) {
	case tea.WindowSizeMsg:
		f.width = event.Width
		f.left, f.top = 0, 0
		if event.Width >= 40 {
			f.left = 2
		}
		if event.Height >= 16 {
			f.top = 1
		}
		event.Width -= 2 * f.left
		event.Height -= 2 * f.top
		msg = event
	case tea.MouseClickMsg:
		event.X -= f.left
		event.Y -= f.top
		if event.X < 0 || event.Y < 0 || event.X >= f.width-2*f.left {
			return f, nil
		}
		msg = event
	case tea.MouseWheelMsg:
		event.X -= f.left
		event.Y -= f.top
		msg = event
	}
	var cmd tea.Cmd
	f.Model, cmd = f.Model.Update(msg)
	return f, cmd
}

func (f *frame) View() tea.View {
	v := f.Model.View()
	if f.left == 0 && f.top == 0 {
		return v
	}
	if f.left > 0 {
		side := richBand("", f.left, petalText, f.background, false)
		v.Content = side + strings.ReplaceAll(v.Content, "\n", side+"\n"+side) + side
	}
	if f.top > 0 {
		row := richBand("", f.width, petalText, f.background, false)
		v.Content = row + "\n" + v.Content + "\n" + row
	}
	if v.Cursor != nil {
		cursor := *v.Cursor
		cursor.X += f.left
		cursor.Y += f.top
		v.Cursor = &cursor
	}
	return v
}
