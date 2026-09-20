package wfx

import (
	"os"
	"time"
)

// setFileTime is os.Chtimes, which maps to SetFileTime on Windows. Unlike
// syscall.SetFileTime it leaves this package free of cgo and of any build
// constraint, so it still runs under the platform-neutral tests. Tests replace
// it to exercise a stamping failure.
var setFileTime = os.Chtimes

// Chtimes converts through time.UnixNano, which is only defined between 1678
// and 2262; outside that range the value wraps to a plausible looking wrong
// date. Stay a year inside each end rather than work out the exact instant.
const (
	earliestStampYear = 1679
	latestStampYear   = 2261
)

// modTimeCandidate is a possible time stamp for a downloaded file, labelled
// with where it came from so the activity log can name it.
type modTimeCandidate struct {
	source string
	value  time.Time
}

// firstUsableTime returns the first candidate a local file can actually carry.
// Callers pass candidates in descending order of authority. An unusable value
// is skipped rather than ending the search, so a nonsense x-amz-meta-mtime
// falls through to the object's own time stamp instead of leaving the file
// stamped with the download time. The zero candidate means none was usable.
func firstUsableTime(candidates ...modTimeCandidate) modTimeCandidate {
	for _, candidate := range candidates {
		if usableModTime(candidate.value) {
			return candidate
		}
	}
	return modTimeCandidate{}
}

// usableModTime reports whether a time stamp is present and representable.
func usableModTime(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	year := value.Year()
	return year >= earliestStampYear && year <= latestStampYear
}
