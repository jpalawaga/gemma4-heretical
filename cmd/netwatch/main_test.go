package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// round describes what each target does during one probe round.
type round struct{ primaryUp, secondaryUp bool }

var errDown = errors.New("timeout")

// scriptedProber replays rounds in order, then holds the final round forever so
// a stray extra iteration can't change the outcome. It closes done once the
// script is exhausted.
func scriptedProber(cfg config, rounds []round, done chan struct{}) (prober, *int) {
	var (
		idx            int
		cur            round
		once           sync.Once
		secondaryCalls int
	)
	return func(target string) error {
		switch target {
		case cfg.primary:
			if idx < len(rounds) {
				cur = rounds[idx]
				idx++
			}
			if idx >= len(rounds) {
				once.Do(func() { close(done) })
			}
			if cur.primaryUp {
				return nil
			}
		case cfg.secondary:
			secondaryCalls++
			if cur.secondaryUp {
				return nil
			}
		default:
			panic("unexpected target " + target)
		}
		return errDown
	}, &secondaryCalls
}

func testConfig() config {
	return config{
		primary:   "primary.test",
		secondary: "secondary.test",
		interval:  time.Millisecond,
		timeout:   time.Millisecond,
		failures:  3,
		recover:   "both",
	}
}

// events returns just the event names from the captured log, e.g.
// ["OUTAGE_START", "OUTAGE_END"].
func events(out string) []string {
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			got = append(got, fields[1])
		}
	}
	return got
}

func runScript(t *testing.T, cfg config, rounds []round) (string, int) {
	t.Helper()
	done := make(chan struct{})
	probe, secondaryCalls := scriptedProber(cfg, rounds, done)
	var buf bytes.Buffer

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		run(cfg, probe, log.New(&buf, "", 0), done)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the script was exhausted")
	}
	return buf.String(), *secondaryCalls
}

func TestOutageStartsAfterThresholdAndEndsOnRecovery(t *testing.T) {
	cfg := testConfig()
	rounds := []round{
		{true, true},   // healthy
		{false, false}, // 1
		{false, false}, // 2
		{false, false}, // 3 -> outage declared
		{false, false}, // still out
		{true, true},   // both back -> outage ends
		{true, true},
	}
	out, _ := runScript(t, cfg, rounds)
	got := events(out)
	want := []string{"OUTAGE_START", "OUTAGE_END"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v\nlog:\n%s", got, want, out)
	}
	if !strings.Contains(out, "OUTAGE_START both targets unreachable for 3 consecutive rounds") {
		t.Errorf("start line should name the threshold; got:\n%s", out)
	}
}

func TestBelowThresholdIsNotAnOutage(t *testing.T) {
	cfg := testConfig()
	rounds := []round{
		{false, false},
		{false, false}, // only 2 consecutive, threshold is 3
		{true, true},
		{false, false},
		{true, true}, // counter must have reset in between
	}
	out, _ := runScript(t, cfg, rounds)
	if got := events(out); len(got) != 0 {
		t.Fatalf("expected no events, got %v\nlog:\n%s", got, out)
	}
}

func TestSecondaryUpIsNotAnOutage(t *testing.T) {
	cfg := testConfig()
	rounds := []round{
		{false, true}, // primary down but the internet is fine
		{false, true},
		{false, true},
		{false, true},
	}
	out, calls := runScript(t, cfg, rounds)
	if got := events(out); len(got) != 0 {
		t.Fatalf("expected no events, got %v\nlog:\n%s", got, out)
	}
	if calls != len(rounds) {
		t.Errorf("secondary probed %d times, want %d (once per failed primary)", calls, len(rounds))
	}
}

func TestHealthyRoundsNeverProbeSecondary(t *testing.T) {
	cfg := testConfig()
	rounds := []round{{true, true}, {true, true}, {true, true}, {true, true}}
	out, calls := runScript(t, cfg, rounds)
	if calls != 0 {
		t.Errorf("secondary probed %d times while healthy, want 0", calls)
	}
	if got := events(out); len(got) != 0 {
		t.Fatalf("expected no events, got %v\nlog:\n%s", got, out)
	}
}

func TestRecoverBothWaitsForSecondary(t *testing.T) {
	cfg := testConfig()
	rounds := []round{
		{false, false},
		{false, false},
		{false, false}, // outage
		{true, false},  // primary back, secondary still down: stay in outage
		{true, false},
		{true, true}, // both back: outage ends
	}
	out, _ := runScript(t, cfg, rounds)
	got := events(out)
	if len(got) != 2 || got[0] != "OUTAGE_START" || got[1] != "OUTAGE_END" {
		t.Fatalf("events = %v, want [OUTAGE_START OUTAGE_END]\nlog:\n%s", got, out)
	}
	// The OUTAGE_END must come from the last round, not the first one where
	// only the primary recovered.
	if strings.Count(out, "OUTAGE_END") != 1 {
		t.Errorf("outage ended more than once:\n%s", out)
	}
}

func TestRecoverAnyEndsOutageOnEitherTarget(t *testing.T) {
	cfg := testConfig()
	cfg.recover = "any"
	rounds := []round{
		{false, false},
		{false, false},
		{false, false}, // outage
		{false, true},  // only the secondary is back; "any" should close it
		{false, true},
	}
	out, _ := runScript(t, cfg, rounds)
	got := events(out)
	if len(got) != 2 || got[0] != "OUTAGE_START" || got[1] != "OUTAGE_END" {
		t.Fatalf("events = %v, want [OUTAGE_START OUTAGE_END]\nlog:\n%s", got, out)
	}
}

func TestOpenOutageIsLoggedOnShutdown(t *testing.T) {
	cfg := testConfig()
	rounds := []round{
		{false, false},
		{false, false},
		{false, false}, // outage declared, script ends while still down
	}
	out, _ := runScript(t, cfg, rounds)
	got := events(out)
	if len(got) != 2 || got[0] != "OUTAGE_START" || got[1] != "OUTAGE_OPEN" {
		t.Fatalf("events = %v, want [OUTAGE_START OUTAGE_OPEN]\nlog:\n%s", got, out)
	}
}
