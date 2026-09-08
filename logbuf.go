package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level is the severity of a log message. The constants are ordered from
// most to least severe, and a message is emitted when its level is at or
// below the active threshold: LevelInfo shows errors, warnings and info,
// LevelDebug shows everything.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

// defaultLevel is what the app logs at when nothing selects a level: the
// connection lifecycle (resolve, auth, connect, listen, disconnect) without
// the per-connection forwarding chatter, which is LevelDebug.
const defaultLevel = LevelInfo

// levelNames lists every level from most to least severe. Doubles as the
// choices offered by the log window's level picker.
var levelNames = []string{"error", "warn", "info", "debug"}

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

// parseLevel maps a config or command-line spelling onto a Level.
func parseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	}
	return defaultLevel, fmt.Errorf("unknown log level %q (use %s)", s, strings.Join(levelNames, ", "))
}

// activeLevel is the threshold every leveled log call is tested against.
// Written by the config reloader and the log window, read by every tunnel
// goroutine, hence atomic.
var activeLevel atomic.Int32

// levelPinned records that -log-level set the level, which then outranks
// the config file for the rest of the session. Cleared when the user picks
// a level in the log window, since that choice is written to the config.
var levelPinned atomic.Bool

func init() {
	// atomic.Int32 zeroes to LevelError; the wanted default is LevelInfo.
	activeLevel.Store(int32(defaultLevel))
}

func setLogLevel(l Level) { activeLevel.Store(int32(l)) }

func logLevel() Level { return Level(activeLevel.Load()) }

// levelEnabled reports whether a message of level l passes the threshold.
func levelEnabled(l Level) bool { return l <= logLevel() }

// verbose mirrors the -v command-line flag. When false, no log output is
// written to stdout; the per-tunnel ring buffers still capture connection
// logs for the in-app log window. Package-level so log helpers and tests
// can flip it without threading it through every call site.
var verbose bool

// stdoutLog writes a single timestamped line to stdout iff verbose is set.
// Shared by appLog (app-wide events) and the per-tunnel logger.
func stdoutLog(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stdout, time.Now().Format("15:04:05")+"  "+format+"\n", args...)
}

// logFn writes one formatted line to a log sink. Call sites go through the
// leveled helpers below rather than calling the sink directly, so messages
// that don't pass the threshold are dropped before they reach a ring buffer
// — a chatty tunnel then can't evict the connection log the user is after.
type logFn func(format string, a ...any)

// at emits the line only if lv passes the active threshold. A nil logFn
// discards everything, so callers without a sink need no guard.
func (f logFn) at(lv Level, format string, a ...any) {
	if f == nil || !levelEnabled(lv) {
		return
	}
	f(format, a...)
}

// errorf reports a failure the user has to act on: a tunnel that would not
// open or close, a config file that would not load.
func (f logFn) errorf(format string, a ...any) { f.at(LevelError, format, a...) }

// warnf reports something that went wrong but was survivable: a key that
// would not parse, an unreachable agent, an unexpected disconnect.
func (f logFn) warnf(format string, a ...any) { f.at(LevelWarn, format, a...) }

// infof reports the connection lifecycle — one handful of lines per open.
func (f logFn) infof(format string, a ...any) { f.at(LevelInfo, format, a...) }

// debugf reports per-connection traffic, which scales with what the tunnel
// carries and is only wanted while diagnosing.
func (f logFn) debugf(format string, a ...any) { f.at(LevelDebug, format, a...) }

// appLog reports an application-wide event (startup, config errors, etc.).
// These events have no associated tunnel, so they go to stdout only — never
// to a per-tunnel buffer. Silent unless -v is given.
var appLog logFn = stdoutLog

// logBuffer is a thread-safe ring buffer for connection logs.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	onAdd func()
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max}
}

// Log appends a timestamped line. The optional onAdd callback fires after
// the buffer mutates, on the calling goroutine.
func (b *logBuffer) Log(format string, args ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	cb := b.onAdd
	b.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (b *logBuffer) SetOnAdd(f func()) {
	b.mu.Lock()
	b.onAdd = f
	b.mu.Unlock()
}

// Snapshot returns a copy of the current buffer.
func (b *logBuffer) Snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

func (b *logBuffer) Clear() {
	b.mu.Lock()
	b.lines = nil
	cb := b.onAdd
	b.mu.Unlock()
	if cb != nil {
		cb()
	}
}
