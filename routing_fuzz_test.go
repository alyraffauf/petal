package main

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

type routingProbeKind uint8

const (
	routingMeasured routingProbeKind = iota
	routingUnmetered
	routingInvalid
	routingFailed
)

type routingCandidate struct {
	name       string
	eligible   bool
	cooling    bool
	capacity   float64
	probe      routingProbeKind
	percentage float64
}

func FuzzAutoRouting(f *testing.F) {
	f.Setenv("XDG_STATE_HOME", f.TempDir())
	for _, name := range []string{"0", "1", "2", "3", "4", "5"} {
		rememberRateLimit(name+"-cooling", &acp.RequestError{Code: 429})
	}
	// Each record encodes eligibility, capacity, probe kind, and percentage.
	f.Add(byte(0), []byte{0, 0, 0, 90, 0, 5, 0, 30})
	f.Add(byte(0), []byte{0, 2, 7, 0, 0, 1, 0, 100})
	f.Add(byte(0), []byte{0, 0, 0, 90, 0, 0, 0, 90})
	f.Add(byte(0), []byte{1, 0, 0, 100, 2, 0, 0, 100, 0, 0, 1, 0})
	f.Add(byte(240), []byte{2, 0, 3, 0, 0, 0, 0, 100})
	f.Add(byte(240), []byte{4, 0, 0, 100})
	f.Add(byte(241), []byte{4, 0, 0, 100})
	f.Add(byte(0), []byte{1, 0, 0, 100, 0, 0, 2, 0})
	f.Add(byte(0), []byte{})

	f.Fuzz(func(t *testing.T, pin byte, records []byte) {
		c := config{model: "requested", Accounts: make(map[string]account)}
		var candidates []routingCandidate
		for offset := 0; offset+4 <= len(records) && len(candidates) < 6; offset += 4 {
			candidate, a := makeRoutingCandidate(len(candidates), records[offset:offset+4])
			candidates = append(candidates, candidate)
			c.Accounts[candidate.name] = a
		}
		want := routingWinners(candidates)
		if pin >= 240 {
			index := int(pin-240) % (len(candidates) + 1)
			c.forced = "missing"
			validPin := index < len(candidates) && candidates[index].eligible
			if index < len(candidates) {
				c.forced = candidates[index].name
			}
			if err := c.validate(); (err == nil) != validPin {
				t.Fatalf("validate pin %q returned %v, want valid=%v", c.forced, err, validPin)
			}
			if !validPin {
				return
			}
			want = map[string]bool{c.forced: true}
		}
		got, err := c.choose(context.Background())
		if len(want) == 0 {
			if err == nil || got != "" {
				t.Fatalf("expected no route, got %q (%v)", got, err)
			}
		} else if err != nil || !want[got] {
			t.Fatalf("got %q (%v), want one of %v, pin %q, records %v", got, err, want, c.forced, records)
		}
	})
}

func makeRoutingCandidate(index int, record []byte) (routingCandidate, account) {
	candidate := routingCandidate{
		name: strconv.Itoa(index), eligible: record[0]&5 == 0, cooling: record[0]&2 != 0,
		capacity: 1,
	}
	if candidate.cooling {
		candidate.name += "-cooling"
	}
	model := "requested"
	if !candidate.eligible {
		model = "unrelated"
	}
	a := account{Models: map[string][]string{model: {"agent"}}}
	if record[0]&4 != 0 {
		a.Models["requested"] = nil
	}
	switch record[1] {
	case 0:
	case 1:
		candidate.capacity = math.SmallestNonzeroFloat64
	case 2:
		candidate.capacity = math.MaxFloat64
	case 3:
		candidate.capacity = math.Nextafter(1, 2)
	default:
		candidate.capacity = float64(record[1]-2) / 4
	}
	if record[1] != 0 {
		a.Capacity = &candidate.capacity
	}
	switch record[2] % 8 {
	case 1:
		candidate.probe = routingUnmetered
	case 2:
		candidate.probe = routingInvalid
		invalid := []string{"NaN", "101", "-1", "Inf", "unknown", ""}
		a.Usage = []string{"/bin/sh", "-c", `printf '%s' "$1"`, "probe", invalid[int(record[3])%len(invalid)]}
	case 3:
		candidate.probe = routingFailed
		a.Usage = []string{"/bin/sh", "-c", "exit 1"}
	default:
		percentage := float64(record[3] % 101)
		switch record[2] % 8 {
		case 6:
			percentage = float64(record[3]) / 10
		case 7:
			percentage = math.SmallestNonzeroFloat64
		}
		candidate.percentage = percentage
		a.Usage = []string{"/bin/sh", "-c", `printf '%s' "$1"`, "probe", strconv.FormatFloat(percentage, 'g', -1, 64)}
	}
	return candidate, a
}

func routingWinners(candidates []routingCandidate) map[string]bool {
	// A measured candidate wins if no eligible candidate has a larger exact
	// product. Omitting the common divisor of 100 preserves the ordering.
	measured := make(map[string]*big.Rat)
	unmetered := make(map[string]bool)
	for _, candidate := range candidates {
		if !candidate.eligible || candidate.cooling {
			continue
		}
		switch candidate.probe {
		case routingUnmetered:
			unmetered[candidate.name] = true
		case routingMeasured:
			if candidate.percentage > 0 {
				capacity := new(big.Rat).SetFloat64(candidate.capacity)
				percentage := new(big.Rat).SetFloat64(candidate.percentage)
				measured[candidate.name] = capacity.Mul(capacity, percentage)
			}
		}
	}
	if len(measured) == 0 {
		return unmetered
	}
	winners := make(map[string]bool)
	for name, score := range measured {
		winners[name] = true
		for _, other := range measured {
			if other.Cmp(score) > 0 {
				delete(winners, name)
				break
			}
		}
	}
	return winners
}
