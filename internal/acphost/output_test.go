package acphost

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func BenchmarkTerminalOutputAtLimit(b *testing.B) {
	chunk := bytes.Repeat([]byte("x"), 8192)
	term := terminal{limit: 1 << 20}
	for range 128 {
		term.Write(chunk)
	}
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		term.Write(chunk)
	}
}

func FuzzTerminalOutputTail(f *testing.F) {
	f.Add("札幌→京都", uint8(8), uint8(2))
	f.Add("1234567", uint8(0), uint8(9))
	f.Add("☘ abc ☘ abc", uint8(7), uint8(2))
	f.Fuzz(func(t *testing.T, data string, limit, stride uint8) {
		if len(data) > 4096 {
			return
		}
		term := terminal{limit: int64(limit)}
		for end := 0; end < len(data); {
			next := min(len(data), end+int(stride)+1)
			term.Write([]byte(data[end:next]))
			want := data[:next]
			cut := len(want) > int(limit)
			if cut {
				want = want[len(want)-int(limit):]
				for len(want) > 0 && !utf8.RuneStart(want[0]) {
					want = want[1:]
				}
			}
			if got := term.output(); got != strings.ToValidUTF8(want, "�") || term.truncated != cut {
				t.Fatalf("limit %d after %d bytes: got %q truncated %v, want tail %q truncated %v", limit, next, got, term.truncated, want, cut)
			}
			end = next
		}
	})
}
