package wfx

import (
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wfxs3/internal/s3store"
)

func TestActivityLogKeepsNewestEntries(t *testing.T) {
	var log activityLog
	for i := range activityCapacity + 50 {
		log.add(Activity{Op: strconv.Itoa(i)})
	}
	entries := log.newestFirst()
	if len(entries) != activityCapacity {
		t.Fatalf("kept %d activities, want %d", len(entries), activityCapacity)
	}
	if newest, oldest := entries[0].Op, entries[len(entries)-1].Op; newest != strconv.Itoa(activityCapacity+49) || oldest != "50" {
		t.Fatalf("unexpected order: newest %s, oldest %s", newest, oldest)
	}
}

func TestOperationsAreRecorded(t *testing.T) {
	backend := newFakeBackend()
	backend.entries["demo|"] = []s3store.Entry{{Name: "a.txt", Size: 1}}
	backend.objects["demo|a.txt"] = []byte("a")
	dir := t.TempDir()
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))

	// FindFirst reloads the unchanged file every time; only the first load
	// is worth an entry.
	_ = findNames(t, service, `\demo`)
	_ = findNames(t, service, `\demo`)
	if _, err := service.GetFile(`\demo\a.txt`, filepath.Join(dir, "a.txt"), CopyOverwrite); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetFile(`\demo\missing.txt`, filepath.Join(dir, "missing.txt"), 0); err == nil {
		t.Fatal("expected the missing object to fail")
	}
	if _, err := service.DeleteFile(`\demo\a.txt`); err != nil {
		t.Fatal(err)
	}

	entries := service.activity.newestFirst()
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Op+" "+entry.Status)
	}
	want := []string{"delete ok", "get not found", "get ok", "list ok", "list ok", "config ok"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if missing := entries[1]; !strings.Contains(missing.Err, `download \demo\missing.txt`) || missing.Path != `\demo\missing.txt` {
		t.Fatalf("the failed download lost its details: %+v", missing)
	}
	if download := entries[2]; download.Detail != "-> "+filepath.Join(dir, "a.txt")+" (overwrite)" || download.Err != "" {
		t.Fatalf("unexpected download entry: %+v", download)
	}
	if listing := entries[3]; listing.Detail != "(1 entry)" {
		t.Fatalf("unexpected listing entry: %+v", listing)
	}
}

func TestDebugReportHidesSecrets(t *testing.T) {
	service := New(newFakeBackend())
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, "[demo]\n"+
		"endpoint=https://user:hunter2@s3.example.test\n"+
		"bucket=bucket\n"+
		"prefix=base\n"+
		"access_key=AKIAIOSFODNN7EXAMPLE\n"+
		"secret_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"+
		"session_token=FwoGZXIvYXdzEXAMPLETOKEN\n")

	report := service.DebugReport(HostInfo{PluginNr: 3}, `\demo\docs\a.txt`)
	for _, secret := range []string{"wJalrXUtnFEMI", "FwoGZXIvYXdzEXAMPLETOKEN", "AKIAIOSFODNN7EXAMPLE", "hunter2"} {
		if strings.Contains(report, secret) {
			t.Fatalf("the report exposes %q:\n%s", secret, report)
		}
	}
	for _, want := range []string{"AKIA…MPLE", "s3://bucket/base/docs/a.txt", "https://user:xxxxx@s3.example.test (path-style)", "<set>", "#3"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not contain %q:\n%s", want, report)
		}
	}
}

func TestMaskAccessKey(t *testing.T) {
	tests := map[string]string{
		"":                     "<none>",
		"short":                "<set>",
		"AKIAIOSFODNN7EXAMPLE": "AKIA…MPLE",
	}
	for key, want := range tests {
		if got := maskAccessKey(key); got != want {
			t.Errorf("maskAccessKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestDebugReportShowsConfigError(t *testing.T) {
	service := New(newFakeBackend())
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
	_ = findNames(t, service, `\`)
	writeServiceConfig(t, service, "[demo]\nnot a setting\n")

	report := service.DebugReport(HostInfo{}, `\`)
	if !strings.Contains(report, "error: line 2: expected key=value (the plugin keeps the last configuration that loaded)") {
		t.Fatalf("the report does not show the load error:\n%s", report)
	}
	if !strings.Contains(report, "Profile demo") {
		t.Fatalf("the report dropped the last configuration that loaded:\n%s", report)
	}
}

func TestConnectionTestReportsEachProfile(t *testing.T) {
	backend := newFakeBackend()
	backend.probeErrs = map[string]error{"broken": errors.New("api error AccessDenied: Access Denied")}
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("broken", "https://s3.example.test")+profileConfig("demo", "https://s3.example.test"))

	done := service.StartConnectionTest()
	if done == nil {
		t.Fatal("the connection test did not start")
	}
	waitForConnectionTest(t, done)
	if service.ConnectionTestRunning() {
		t.Fatal("the connection test still runs after it finished")
	}

	results := service.test.results
	if len(results) != 2 {
		t.Fatalf("expected one result per profile, got %+v", results)
	}
	if broken := results[0]; broken.Profile != "broken" || broken.StatusCode != http.StatusForbidden || broken.RequestID != "REQ-broken" || !strings.Contains(broken.Err, "AccessDenied") {
		t.Fatalf("unexpected failed result: %+v", broken)
	}
	if demo := results[1]; demo.Profile != "demo" || demo.StatusCode != http.StatusOK || demo.Err != "" {
		t.Fatalf("unexpected successful result: %+v", demo)
	}
	report := service.DebugReport(HostInfo{}, `\`)
	for _, want := range []string{"HTTP 403  request REQ-broken", "api error AccessDenied", "HTTP 200  request REQ-demo", "test    error"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not contain %q:\n%s", want, report)
		}
	}
}

// Total Commander's callbacks may only run on its own thread, so the probes
// that run in the background must not invoke them.
func TestConnectionTestRunsWithoutCallbacks(t *testing.T) {
	backend := newFakeBackend()
	backend.probeRelease = make(chan struct{})
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
	var logs atomic.Int32
	service.SetCallbacks(Callbacks{Log: func(int, string) { logs.Add(1) }})

	done := service.StartConnectionTest()
	if done == nil {
		t.Fatal("the connection test did not start")
	}
	afterStart := logs.Load()
	if afterStart == 0 {
		t.Fatal("the configuration was not loaded before the probes started")
	}
	if !service.ConnectionTestRunning() {
		t.Fatal("the connection test is not reported as running")
	}
	if service.StartConnectionTest() != nil {
		t.Fatal("a second connection test started while the first was running")
	}
	if report := service.DebugReport(HostInfo{}, `\`); !strings.Contains(report, "Connection test (running for") {
		t.Fatalf("the report does not show the running test:\n%s", report)
	}
	close(backend.probeRelease)
	waitForConnectionTest(t, done)
	if got := logs.Load(); got != afterStart {
		t.Fatalf("the background probes invoked the log callback %d times", got-afterStart)
	}
}

func waitForConnectionTest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection test did not finish")
	}
}
