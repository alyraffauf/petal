package main

import (
	"encoding/json"
	"testing"
)

func FuzzCodexQuotaWindows(f *testing.F) {
	f.Add(int16(0), int16(88), uint8(0))
	f.Add(int16(101), int16(1), uint8(0))
	f.Add(int16(100), int16(0), uint8(2))
	f.Add(int16(13), int16(49), uint8(3))
	f.Fuzz(func(t *testing.T, primary, secondary int16, flags uint8) {
		windows := map[string]any{}
		used := []int16{}
		for i, value := range []int16{primary, secondary} {
			name := []string{"primary", "secondary"}[i]
			if flags&(1<<i) == 0 {
				windows[name] = map[string]int16{"usedPercent": value}
				used = append(used, value)
			} else if flags&(4<<i) != 0 {
				windows[name] = nil
			}
		}
		input := map[string]any{"rateLimits": windows}
		blocked := flags&16 != 0
		if blocked {
			input["ordinaryUsageAllowed"] = false
		}
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := codexRemaining(data)
		if blocked {
			if actual != 0 || err != nil {
				t.Fatalf("blocked account: %g, %v", actual, err)
			}
			return
		}
		var highest int16 = -1
		for _, value := range used {
			if value < 0 || value > 100 {
				if err == nil {
					t.Fatalf("accepted invalid windows: %s", data)
				}
				return
			}
			highest = max(highest, value)
		}
		want := -1.0
		if highest >= 0 {
			want = float64(100 - highest)
		}
		if actual != want || err != nil {
			t.Fatalf("windows %s: got %g, %v; want %g", data, actual, err, want)
		}
	})
}
