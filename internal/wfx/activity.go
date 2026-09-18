package wfx

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Activity is one finished plugin operation, kept in memory for the debug
// dialog. Total Commander shows its log window only for plugins that report a
// connection, which this one does not, so these results are otherwise
// invisible.
type Activity struct {
	Time     time.Time
	Op       string
	Path     string
	Detail   string
	Duration time.Duration
	Status   string
	Err      string
}

// activityCapacity is how many recent activities the debug dialog can show.
const activityCapacity = 100

// activityLog is a fixed-size ring of the most recent activities. It has its
// own lock so that recording never waits for, or deadlocks with, Service.mu.
type activityLog struct {
	mu      sync.Mutex
	entries [activityCapacity]Activity
	next    int
	count   int
}

func (l *activityLog) add(entry Activity) {
	l.mu.Lock()
	l.entries[l.next] = entry
	l.next = (l.next + 1) % len(l.entries)
	if l.count < len(l.entries) {
		l.count++
	}
	l.mu.Unlock()
}

// newestFirst returns a copy of the recorded activities, most recent first.
func (l *activityLog) newestFirst() []Activity {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]Activity, 0, l.count)
	for i := 1; i <= l.count; i++ {
		result = append(result, l.entries[(l.next-i+len(l.entries))%len(l.entries)])
	}
	return result
}

// record adds a finished operation to the activity log. It never invokes Total
// Commander's callbacks, so any goroutine may call it.
func (s *Service) record(op, path, detail string, start time.Time, status string, err error) {
	entry := Activity{
		Time:     start,
		Op:       op,
		Path:     path,
		Detail:   detail,
		Duration: time.Since(start),
		Status:   status,
	}
	if err != nil {
		entry.Err = err.Error()
	}
	s.activity.add(entry)
}

// recordTransfer adds a finished download or upload to the activity log.
func (s *Service) recordTransfer(op, source, target string, flags int, start time.Time, status int, err error) {
	detail := "-> " + target
	if names := copyFlagNames(flags); names != "" {
		detail += " (" + names + ")"
	}
	if errors.Is(err, ErrUserAbort) {
		err = nil // the status already says so
	}
	s.record(op, source, detail, start, transferStatus(status), err)
}

// resultStatus is the activity status of an operation that only succeeds or
// fails.
func resultStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// transferStatus names a WFX file operation result.
func transferStatus(code int) string {
	switch code {
	case FileOK:
		return "ok"
	case FileExists, FileExistsResumeAllowed:
		return "exists"
	case FileNotFound:
		return "not found"
	case FileReadError:
		return "read error"
	case FileWriteError:
		return "write error"
	case FileUserAbort:
		return "cancelled"
	case FileNotSupported:
		return "not supported"
	}
	return fmt.Sprintf("status %d", code)
}

func copyFlagNames(flags int) string {
	var names []string
	if flags&CopyOverwrite != 0 {
		names = append(names, "overwrite")
	}
	if flags&CopyResume != 0 {
		names = append(names, "resume")
	}
	if flags&CopyMove != 0 {
		names = append(names, "move")
	}
	return strings.Join(names, ", ")
}

func countNoun(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", count, plural)
}
