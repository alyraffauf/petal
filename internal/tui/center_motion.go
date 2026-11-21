package tui

import (
	"fmt"
	"math"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
)

type centerMotion struct {
	started  time.Time
	arrivals map[string]time.Time
	pending  bool
	frame    int
}
type centerFrame struct{ owner *picker }

const flowerDuration = 950 * time.Millisecond
const arrivalDuration = 900 * time.Millisecond

func (m centerMotion) orchid() []string {
	elapsed := time.Since(m.started)
	if elapsed >= flowerDuration || !anim.Motion() {
		return orchidRows
	}
	return renderOrchid(float64(elapsed) / float64(flowerDuration))
}
func (m centerMotion) arrival(id string) float64 {
	start, ok := m.arrivals[id]
	if !ok || !anim.Motion() {
		return -1
	}
	t := float64(time.Since(start)) / float64(arrivalDuration)
	if t >= 1 {
		return -1
	}
	return t
}
func (p *picker) workingTide() bool {
	if p.center.owner != nil && p.center.owner.Busy {
		return true
	}
	for _, r := range p.saved {
		if childIsRunning(r.DelegationStatus) {
			return true
		}
	}
	return false
}
func (p *picker) motionTick() tea.Cmd {
	for id, t := range p.motion.arrivals {
		if time.Since(t) >= arrivalDuration {
			delete(p.motion.arrivals, id)
		}
	}
	if !p.history || p.motion.pending || !anim.Motion() || (time.Since(p.motion.started) >= flowerDuration && len(p.motion.arrivals) == 0 && !p.workingTide()) {
		return nil
	}
	p.motion.pending = true
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return centerFrame{p} })
}

var treeBands = func() [13]string {
	var tree [13]string
	for i := range tree {
		tree[i] = fmt.Sprintf("%d;%d;%d", 42+i, 31+i/2, 47+i/2)
	}
	return tree
}()

// The working ribbon moves between the same plum shadow and rose highlight
// as the motion study. Each half-cell row samples a different phase.
var tidePalette = func() [128]string {
	var colors [128]string
	for i := range colors {
		t := float64(i) / float64(len(colors)-1)
		colors[i] = fmt.Sprintf("%d;%d;%d", 42+int(math.Round(213*t)), 29+int(math.Round(145*t)), 52+int(math.Round(159*t)))
	}
	return colors
}()

func tideColor(position float64, frame int) string {
	wave := (1 + math.Cos(2*math.Pi*(position-float64(frame)/60))) / 2
	return tidePalette[int(math.Round(127*math.Pow(anim.InOutCubic(wave), 2.5)))]
}
