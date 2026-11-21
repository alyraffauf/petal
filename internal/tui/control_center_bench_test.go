package tui

import (
	"fmt"
	"petal/internal/routing"
	"petal/internal/sessions"
	"testing"
)

func BenchmarkAuditCenter(b *testing.B) {
	for _, n := range []int{10, 400, 4000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := newPicker(routing.Config{})
			p.width, p.height, p.history = 100, 30, true
			for i := range n {
				p.saved = append(p.saved, sessions.Record{ID: fmt.Sprint(i), Title: "Investigate routing behaviour", Model: "test", Account: "work"})
			}
			p.centerView()
			for b.Loop() {
				p.motion.frame++
				p.centerView()
			}
		})
	}
}
func BenchmarkAuditGraph(b *testing.B) {
	for _, n := range []int{400, 4000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := newPicker(routing.Config{})
			p.width, p.height, p.history = 100, 30, true
			for i := range n {
				p.saved = append(p.saved, sessions.Record{ID: fmt.Sprint(i), DelegatedParent: "original-parent", DelegationStatus: "completed", Title: "Research"})
			}
			p.saved = append(p.saved, sessions.Record{ID: "original-parent", Title: "Parent"})
			for b.Loop() {
				p.hasGraph()
			}
		})
	}
}
