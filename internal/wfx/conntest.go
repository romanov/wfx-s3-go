package wfx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/example/wfxs3/internal/config"
)

// probeTimeout bounds each profile's connection probe. It is a variable so
// that tests can shorten it.
var probeTimeout = 10 * time.Second

// ConnectionResult is the outcome of probing one profile.
type ConnectionResult struct {
	Profile    string
	Duration   time.Duration
	StatusCode int
	RequestID  string
	Err        string
}

// connectionTest is the state of the most recent connection test.
type connectionTest struct {
	running  bool
	started  time.Time
	finished time.Time
	err      string // why no profile could be probed
	results  []ConnectionResult
}

// StartConnectionTest probes every configured profile concurrently in the
// background and keeps the results for DebugReport. The returned channel is
// closed once the test has finished; it is nil when a test is already running.
//
// The configuration is reloaded here, on the caller's thread, because Total
// Commander's callbacks may only be invoked on its own thread. The probing
// goroutines touch nothing but the backend and in-memory state.
func (s *Service) StartConnectionTest() <-chan struct{} {
	s.mu.Lock()
	if s.test.running {
		s.mu.Unlock()
		return nil
	}
	s.test = connectionTest{running: true, started: time.Now()}
	s.mu.Unlock()

	done := make(chan struct{})
	if err := s.reloadConfig(false); err != nil {
		s.finishConnectionTest(nil, fmt.Sprintf("load %s: %v", s.ConfigPath(), err))
		close(done)
		return done
	}
	s.mu.RLock()
	profiles := make([]config.Profile, 0, len(s.config.Profiles))
	for _, name := range s.config.Names() {
		profile, _ := s.config.Profile(name)
		profiles = append(profiles, profile)
	}
	s.mu.RUnlock()

	go func() {
		defer close(done)
		results := make([]ConnectionResult, len(profiles))
		var wg sync.WaitGroup
		for i, profile := range profiles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = s.probe(profile)
			}()
		}
		wg.Wait()
		s.finishConnectionTest(results, "")
	}()
	return done
}

// ConnectionTestRunning reports whether a connection test is in progress.
func (s *Service) ConnectionTestRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.test.running
}

func (s *Service) probe(profile config.Profile) ConnectionResult {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	start := time.Now()
	response, err := s.backend.Probe(ctx, profile)
	result := ConnectionResult{
		Profile:    profile.Name,
		Duration:   time.Since(start),
		StatusCode: response.StatusCode,
		RequestID:  response.RequestID,
	}
	if err != nil {
		result.Err = err.Error()
	}
	detail := ""
	if result.StatusCode != 0 {
		detail = fmt.Sprintf("(HTTP %d)", result.StatusCode)
	}
	s.record("test", profile.Name, detail, start, resultStatus(err), err)
	return result
}

func (s *Service) finishConnectionTest(results []ConnectionResult, err string) {
	s.mu.Lock()
	s.test.running = false
	s.test.finished = time.Now()
	s.test.results = results
	s.test.err = err
	s.mu.Unlock()
}
