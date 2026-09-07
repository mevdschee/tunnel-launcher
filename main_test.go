package main

import (
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"abcdefghij", "abcdefghij"},
		{"abcdefghijk", "abcdefghi…"},
		{"hello world", "hello wor…"},
	}
	for _, c := range cases {
		if got := truncate(c.in, 10); got != c.want {
			t.Errorf("truncate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName(&Desc{Name: "foo"}); got != "foo" {
		t.Errorf("got %q", got)
	}
}

func TestStatusGlyph(t *testing.T) {
	if got := statusGlyph(&Desc{Status: StatusOpen}); got != "●" {
		t.Errorf("open got %q", got)
	}
	if got := statusGlyph(&Desc{Status: StatusClosed}); got != "○" {
		t.Errorf("closed got %q", got)
	}
}

func TestAnyOpen(t *testing.T) {
	if anyOpen(nil) {
		t.Error("empty snapshot got true")
	}
	closed := map[string]Desc{"a": {Status: StatusClosed}, "b": {Status: StatusClosed}}
	if anyOpen(closed) {
		t.Error("all closed got true")
	}
	mixed := map[string]Desc{"a": {Status: StatusClosed}, "b": {Status: StatusOpen}}
	if !anyOpen(mixed) {
		t.Error("one open got false")
	}
}

func TestUptimeText_Closed(t *testing.T) {
	if got := uptimeText(&Desc{Status: StatusClosed}); got != "" {
		t.Errorf("got %q, want empty string for closed tunnel", got)
	}
}

func TestUptimeText_OpenSeconds(t *testing.T) {
	d := &Desc{Status: StatusOpen, LastConn: time.Now().Add(-(2*time.Minute + 5*time.Second))}
	got := uptimeText(d)
	if !strings.HasPrefix(got, "02m") {
		t.Errorf("got %q, want prefix '02m'", got)
	}
}

func TestUptimeText_OpenHours(t *testing.T) {
	d := &Desc{Status: StatusOpen, LastConn: time.Now().Add(-(3*time.Hour + 5*time.Minute))}
	got := uptimeText(d)
	if got != "03h05m" {
		t.Errorf("got %q", got)
	}
}

func TestUptimeText_OpenDays(t *testing.T) {
	d := &Desc{Status: StatusOpen, LastConn: time.Now().Add(-(2*24*time.Hour + 4*time.Hour))}
	got := uptimeText(d)
	if got != "02d04h" {
		t.Errorf("got %q", got)
	}
}

func TestModeString(t *testing.T) {
	if ModeLocal.String() != "local" {
		t.Error("local")
	}
	if ModeRemote.String() != "remote" {
		t.Error("remote")
	}
	if ModeSocks.String() != "socks" {
		t.Error("socks")
	}
}

// state -----------------------------------------------------------------

func mkState(entries ...tunnelEntry) *state {
	s := newState()
	s.setFile(&tunnelsFile{Tunnels: entries})
	return s
}

func TestState_LenAndConfigured(t *testing.T) {
	s := mkState(
		tunnelEntry{Name: "a", Forward: "-D 1080"},
		tunnelEntry{Name: "b", Forward: "-D 1081"},
	)
	if got := s.len(); got != 2 {
		t.Errorf("len = %d", got)
	}
	if !s.isConfigured(0) || !s.isConfigured(1) {
		t.Error("expected configured")
	}
	if s.isConfigured(2) {
		t.Error("idx 2 should not be configured")
	}
	if got := s.numConfigured(); got != 2 {
		t.Errorf("numConfigured = %d", got)
	}
}

func TestState_AddReplaceDelete(t *testing.T) {
	s := newState()
	s.setFile(&tunnelsFile{})
	s.addEntry(tunnelEntry{Name: "a", Forward: "-D 1080"})
	s.addEntry(tunnelEntry{Name: "b", Forward: "-D 1081"})
	if got := s.numConfigured(); got != 2 {
		t.Fatalf("numConfigured = %d", got)
	}
	s.replaceEntry(0, tunnelEntry{Name: "renamed", Forward: "-D 1080"})
	e, ok := s.entry(0)
	if !ok || e.Name != "renamed" {
		t.Fatalf("after replace: %+v ok=%v", e, ok)
	}
	s.deleteEntry(0)
	if got := s.numConfigured(); got != 1 {
		t.Fatalf("after delete: numConfigured = %d", got)
	}
	if e, _ := s.entry(0); e.Name != "b" {
		t.Errorf("after delete: head = %q", e.Name)
	}
}

func TestState_DeleteOutOfRangeIsNoOp(t *testing.T) {
	s := mkState(tunnelEntry{Name: "a", Forward: "-D 1080"})
	s.deleteEntry(-1)
	s.deleteEntry(99)
	if got := s.numConfigured(); got != 1 {
		t.Errorf("numConfigured = %d", got)
	}
}

func TestState_Move(t *testing.T) {
	// move is a swap, not a shift — the GUI only ever calls it with
	// adjacent indices, where swap and shift coincide.
	s := mkState(
		tunnelEntry{Name: "a", Forward: "-D 1080"},
		tunnelEntry{Name: "b", Forward: "-D 1081"},
		tunnelEntry{Name: "c", Forward: "-D 1082"},
	)
	if !s.move(0, 1) {
		t.Fatal("move 0→1 should succeed")
	}
	got := []string{}
	for i := 0; i < s.numConfigured(); i++ {
		e, _ := s.entry(i)
		got = append(got, e.Name)
	}
	want := []string{"b", "a", "c"}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("[%d] = %q, want %q", i, got[i], n)
		}
	}
	// Out-of-bounds and identity moves return false.
	if s.move(0, 0) {
		t.Error("self-move should be no-op")
	}
	if s.move(-1, 0) {
		t.Error("negative move should be no-op")
	}
	if s.move(0, 99) {
		t.Error("out-of-bounds move should be no-op")
	}
}

func TestState_AppFor(t *testing.T) {
	s := mkState(
		tunnelEntry{Name: "with-app", Forward: "-D 1080", App: "code ."},
		tunnelEntry{Name: "no-app", Forward: "-D 1081"},
	)
	cmd, ok := s.appFor("with-app")
	if !ok || cmd != "code ." {
		t.Errorf("with-app: %q ok=%v", cmd, ok)
	}
	if _, ok := s.appFor("no-app"); ok {
		t.Error("no-app should not have an app")
	}
	if _, ok := s.appFor("missing"); ok {
		t.Error("missing should not have an app")
	}
}

func TestState_DescsMergesRunningStatus(t *testing.T) {
	s := mkState(
		tunnelEntry{Name: "a", Forward: "-D 1080"},
		tunnelEntry{Name: "b", Forward: "-D 1081"},
	)
	now := time.Now()
	s.setRunning(map[string]Desc{
		"a": {Name: "a", Status: StatusOpen, LastConn: now},
	})
	descs := s.descs()
	if len(descs) != 2 {
		t.Fatalf("len = %d", len(descs))
	}
	if descs[0].Status != StatusOpen {
		t.Errorf("a status = %v, want open", descs[0].Status)
	}
	if !descs[0].LastConn.Equal(now) {
		t.Errorf("a LastConn = %v, want %v", descs[0].LastConn, now)
	}
	if descs[1].Status != StatusClosed {
		t.Errorf("b status = %v, want closed", descs[1].Status)
	}
}

func TestState_DescsAppendsExtraRunning(t *testing.T) {
	s := mkState(tunnelEntry{Name: "a", Forward: "-D 1080"})
	s.setRunning(map[string]Desc{
		"a":   {Name: "a", Status: StatusOpen},
		"x":   {Name: "x", Status: StatusOpen},
		"foo": {Name: "foo", Status: StatusOpen},
	})
	if got := s.len(); got != 3 {
		t.Errorf("len = %d, want 3", got)
	}
	descs := s.descs()
	if len(descs) != 3 {
		t.Fatalf("descs len = %d", len(descs))
	}
	if descs[0].Name != "a" {
		t.Errorf("[0] = %q, want a", descs[0].Name)
	}
	// extras come sorted after configured.
	if descs[1].Name != "foo" || descs[2].Name != "x" {
		t.Errorf("extras = %q,%q, want foo,x", descs[1].Name, descs[2].Name)
	}
}

func TestState_DescsSkipsBadEntries(t *testing.T) {
	s := mkState(
		tunnelEntry{Name: "good", Forward: "-D 1080"},
		tunnelEntry{Name: "bad", Forward: "garbage"},
	)
	descs := s.descs()
	if len(descs) != 1 || descs[0].Name != "good" {
		t.Errorf("descs = %+v, want only 'good'", descs)
	}
}

func TestState_SnapshotFileIsCopy(t *testing.T) {
	s := mkState(tunnelEntry{Name: "a", Forward: "-D 1080"})
	snap := s.snapshotFile()
	snap.Tunnels[0].Name = "mutated"

	if e, _ := s.entry(0); e.Name != "a" {
		t.Errorf("snapshot mutation leaked into state: name = %q", e.Name)
	}
}

func TestState_EntryOutOfRange(t *testing.T) {
	s := newState()
	if _, ok := s.entry(0); ok {
		t.Error("empty state should not return an entry")
	}
	if _, ok := s.entry(-1); ok {
		t.Error("negative idx should return false")
	}
}
