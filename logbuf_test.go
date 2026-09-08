package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLogBuffer_LogAndSnapshot(t *testing.T) {
	b := newLogBuffer(10)
	b.Log("hello %s", "world")
	b.Log("second")

	lines := b.Snapshot()
	if len(lines) != 2 {
		t.Fatalf("len = %d", len(lines))
	}
	if !strings.HasSuffix(lines[0], "hello world") {
		t.Errorf("[0] = %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "second") {
		t.Errorf("[1] = %q", lines[1])
	}
}

func TestLogBuffer_RingTruncation(t *testing.T) {
	b := newLogBuffer(3)
	for i := 0; i < 10; i++ {
		b.Log("line %d", i)
	}
	lines := b.Snapshot()
	if len(lines) != 3 {
		t.Fatalf("len = %d", len(lines))
	}
	for i, want := range []string{"line 7", "line 8", "line 9"} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("[%d] = %q, want suffix %q", i, lines[i], want)
		}
	}
}

func TestLogBuffer_SnapshotIsCopy(t *testing.T) {
	b := newLogBuffer(10)
	b.Log("first")
	s1 := b.Snapshot()
	b.Log("second")
	s2 := b.Snapshot()

	if len(s1) != 1 {
		t.Errorf("snapshot 1 len = %d, want 1 (mutation leaked into snapshot)", len(s1))
	}
	if len(s2) != 2 {
		t.Errorf("snapshot 2 len = %d, want 2", len(s2))
	}
}

func TestLogBuffer_OnAddCallback(t *testing.T) {
	b := newLogBuffer(10)
	var calls atomic.Int32
	b.SetOnAdd(func() { calls.Add(1) })

	b.Log("a")
	b.Log("b")
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}

	// Clear must also fire the callback.
	b.Clear()
	if got := calls.Load(); got != 3 {
		t.Errorf("after clear, calls = %d, want 3", got)
	}
	if len(b.Snapshot()) != 0 {
		t.Errorf("clear did not empty buffer")
	}
}

func TestLogBuffer_OnAddCanBeDisabled(t *testing.T) {
	b := newLogBuffer(10)
	var calls atomic.Int32
	b.SetOnAdd(func() { calls.Add(1) })
	b.Log("once")
	b.SetOnAdd(nil)
	b.Log("not counted")
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// Concurrent writers must not race on b.lines or b.onAdd. Rely on the
// race detector to catch regressions.
func TestLogBuffer_ConcurrentLog(t *testing.T) {
	b := newLogBuffer(1000)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Log("g%d-%d", i, j)
			}
		}(i)
	}
	wg.Wait()
	if got := len(b.Snapshot()); got != 800 {
		t.Errorf("len = %d, want 800", got)
	}
}

// The onAdd callback fires while NOT holding the buffer mutex, so a
// callback that calls Snapshot must not deadlock.
func TestLogBuffer_OnAddCanCallSnapshot(t *testing.T) {
	b := newLogBuffer(10)
	done := make(chan int, 1)
	b.SetOnAdd(func() {
		done <- len(b.Snapshot())
	})
	b.Log("hello")
	if got := <-done; got != 1 {
		t.Errorf("snapshot inside onAdd got len %d, want 1", got)
	}
}

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
	}{
		{"error", LevelError},
		{"warn", LevelWarn},
		{"warning", LevelWarn},
		{"info", LevelInfo},
		{"debug", LevelDebug},
		{"  DEBUG  ", LevelDebug},
	} {
		got, err := parseLevel(tc.in)
		if err != nil {
			t.Errorf("parseLevel(%q) = error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if rt := got.String(); rt != tc.want.String() {
			t.Errorf("parseLevel(%q).String() = %q, want %q", tc.in, rt, tc.want.String())
		}
	}
}

func TestParseLevel_Unknown(t *testing.T) {
	got, err := parseLevel("chatty")
	if err == nil {
		t.Fatalf("parseLevel(chatty) = %v, want error", got)
	}
	if got != defaultLevel {
		t.Errorf("level on error = %v, want the default %v", got, defaultLevel)
	}
}

// withLevel sets the process-wide level for one test and restores it after.
func withLevel(t *testing.T, l Level) {
	t.Helper()
	prev := logLevel()
	setLogLevel(l)
	t.Cleanup(func() { setLogLevel(prev) })
}

// The default must keep the connection lifecycle and drop the
// per-connection forwarding chatter.
func TestLogLevel_DefaultKeepsInfoDropsDebug(t *testing.T) {
	withLevel(t, defaultLevel)
	var got []string
	log := logFn(func(format string, a ...any) {
		got = append(got, fmt.Sprintf(format, a...))
	})

	log.errorf("open failed")
	log.warnf("disconnected")
	log.infof("ssh connection established")
	log.debugf("forward 127.0.0.1:61369 ↔ 127.0.0.1:1080")

	want := []string{"open failed", "disconnected", "ssh connection established"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestLogLevel_Thresholds(t *testing.T) {
	for _, tc := range []struct {
		level Level
		want  []string
	}{
		{LevelError, []string{"e"}},
		{LevelWarn, []string{"e", "w"}},
		{LevelInfo, []string{"e", "w", "i"}},
		{LevelDebug, []string{"e", "w", "i", "d"}},
	} {
		withLevel(t, tc.level)
		var got []string
		log := logFn(func(format string, a ...any) { got = append(got, format) })
		log.errorf("e")
		log.warnf("w")
		log.infof("i")
		log.debugf("d")
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("at %v: lines = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// Filtering happens before the sink, so debug traffic can't evict the
// connection log from a tunnel's ring buffer.
func TestLogLevel_FiltersBeforeBuffer(t *testing.T) {
	withLevel(t, LevelInfo)
	b := newLogBuffer(10)
	log := logFn(b.Log)

	log.infof("ssh connection established")
	for i := 0; i < 50; i++ {
		log.debugf("forward %d", i)
	}

	lines := b.Snapshot()
	if len(lines) != 1 {
		t.Fatalf("buffered %d lines, want 1: %q", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "ssh connection established") {
		t.Errorf("kept line = %q", lines[0])
	}
}

// A nil sink is the "no logger" case (resolveHost calls through it), and
// must not panic at any level.
func TestLogLevel_NilLoggerIsSafe(t *testing.T) {
	withLevel(t, LevelDebug)
	var log logFn
	log.errorf("e")
	log.warnf("w")
	log.infof("i")
	log.debugf("d")
}
