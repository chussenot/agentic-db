package state

import (
	"testing"
	"time"
)

func TestParseNameRoundTrip(t *testing.T) {
	cases := []struct {
		status Status
		slot   int
		level  int
	}{
		{Working, 1, 0},
		{Working, 20, 0},
		{Prompt, 7, 0},
		{Idle, 3, 0},
		{Idle, 12, 6},
		{Idle, 1, 4},
	}
	for _, c := range cases {
		name := Encode(c.status, c.slot, c.level)
		slot, status, level, ok := ParseName(name)
		if !ok {
			t.Errorf("ParseName(%q) not ok", name)
			continue
		}
		if slot != c.slot || status != c.status {
			t.Errorf("ParseName(%q) = slot %d status %q; want slot %d status %q",
				name, slot, status, c.slot, c.status)
		}
		// Level only meaningful for idle.
		if c.status == Idle && level != c.level {
			t.Errorf("ParseName(%q) level = %d; want %d", name, level, c.level)
		}
	}
}

func TestParseNameInvalid(t *testing.T) {
	for _, name := range []string{"", "cw", "cx3", "c3", "foo", "cw3x", "cwa", "ci3lx"} {
		if _, _, _, ok := ParseName(name); ok {
			t.Errorf("ParseName(%q) = ok; want not ok", name)
		}
	}
}

func TestEncodeForms(t *testing.T) {
	if got := EncodeWorking(3); got != "cw3" {
		t.Errorf("EncodeWorking(3) = %q; want cw3", got)
	}
	if got := EncodePrompt(3); got != "cp3" {
		t.Errorf("EncodePrompt(3) = %q; want cp3", got)
	}
	if got := EncodeIdle(3, 2); got != "ci3l2" {
		t.Errorf("EncodeIdle(3,2) = %q; want ci3l2", got)
	}
}

func TestDecayLevelBoundaries(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    int
	}{
		{-5 * time.Minute, 0},
		{0, 0},
		{59 * time.Second, 0},
		{1 * time.Minute, 1},
		{3*time.Minute + 59*time.Second, 1},
		{4 * time.Minute, 2},
		{9*time.Minute + 59*time.Second, 2},
		{10 * time.Minute, 3},
		{19 * time.Minute, 3},
		{20 * time.Minute, 4},
		{34 * time.Minute, 4},
		{35 * time.Minute, 5},
		{59 * time.Minute, 5},
		{60 * time.Minute, 6},
		{2 * time.Hour, 6},
	}
	for _, c := range cases {
		if got := DecayLevel(c.elapsed); got != c.want {
			t.Errorf("DecayLevel(%v) = %d; want %d", c.elapsed, got, c.want)
		}
	}
}

func TestMapEvent(t *testing.T) {
	cases := []struct {
		event        string
		wantStatus   Status
		changeStatus bool
		bumpTalk     bool
		del          bool
		known        bool
	}{
		{"SessionStart", Idle, true, true, false, true},
		{"UserPromptSubmit", Working, true, false, false, true},
		{"PostToolUse", Working, true, false, false, true},
		{"Notification", Prompt, true, false, false, true},
		{"Stop", Idle, true, true, false, true},
		{"SubagentStop", "", false, true, false, true},
		{"SessionEnd", "", false, false, true, true},
		{"Bogus", "", false, false, false, false},
	}
	for _, c := range cases {
		tr := MapEvent(c.event)
		if tr.Known != c.known {
			t.Errorf("MapEvent(%q).Known = %v; want %v", c.event, tr.Known, c.known)
		}
		if tr.ChangeStatus != c.changeStatus || (c.changeStatus && tr.NewStatus != c.wantStatus) {
			t.Errorf("MapEvent(%q) status = (%v,%q); want (%v,%q)",
				c.event, tr.ChangeStatus, tr.NewStatus, c.changeStatus, c.wantStatus)
		}
		if tr.BumpTalk != c.bumpTalk {
			t.Errorf("MapEvent(%q).BumpTalk = %v; want %v", c.event, tr.BumpTalk, c.bumpTalk)
		}
		if tr.Delete != c.del {
			t.Errorf("MapEvent(%q).Delete = %v; want %v", c.event, tr.Delete, c.del)
		}
	}
}

func TestRenderTables(t *testing.T) {
	if WorkingIcon() != " ●" || PromptIcon() != " ?" {
		t.Errorf("working/prompt icons wrong: %q %q", WorkingIcon(), PromptIcon())
	}
	if IdleIcon(0) != " ██" || IdleIcon(6) != " ░░" {
		t.Errorf("idle icon ends wrong: %q %q", IdleIcon(0), IdleIcon(6))
	}
	if IdleColor(0) != "#ffffff" || IdleColor(6) != "#3a3a3a" {
		t.Errorf("idle color ends wrong: %q %q", IdleColor(0), IdleColor(6))
	}
	// Clamp out-of-range.
	if IdleIcon(-1) != " ██" || IdleIcon(99) != " ░░" {
		t.Errorf("idle icon clamp wrong")
	}
}
