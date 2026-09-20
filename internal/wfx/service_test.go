package wfx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/wfxs3/internal/config"
	"github.com/example/wfxs3/internal/s3store"
)

type fakeBackend struct {
	entries     map[string][]s3store.Entry
	objects     map[string][]byte
	deleted     []string
	uploads     map[string][]byte
	downloadErr error

	// uploadInputs keeps everything but the body of the last upload of each
	// object, so tests can check the metadata that went with it.
	uploadInputs map[string]s3store.UploadInput
	// The time stamps Download reports. newFakeBackend gives LastModified a
	// value because every real object has one; ModTime starts zero because
	// only objects uploaded by an mtime-aware client carry it.
	downloadLastModified time.Time
	downloadModTime      time.Time

	probeErrs    map[string]error // by profile name
	probeRelease chan struct{}    // when set, Probe waits until it is closed
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		entries:              make(map[string][]s3store.Entry),
		objects:              make(map[string][]byte),
		uploads:              make(map[string][]byte),
		uploadInputs:         make(map[string]s3store.UploadInput),
		downloadLastModified: time.Unix(1700000000, 0),
	}
}

func objectID(profile config.Profile, key string) string {
	return profile.Name + "|" + key
}

func (f *fakeBackend) List(_ context.Context, profile config.Profile, relative string) ([]s3store.Entry, error) {
	return append([]s3store.Entry(nil), f.entries[objectID(profile, relative)]...), nil
}

func (f *fakeBackend) Head(_ context.Context, profile config.Profile, key string) (bool, error) {
	_, ok := f.objects[objectID(profile, key)]
	return ok, nil
}

func (f *fakeBackend) Download(_ context.Context, profile config.Profile, key string) (s3store.Object, error) {
	if f.downloadErr != nil {
		return s3store.Object{}, f.downloadErr
	}
	data, ok := f.objects[objectID(profile, key)]
	if !ok {
		return s3store.Object{}, os.ErrNotExist
	}
	return s3store.Object{
		Body:         io.NopCloser(bytes.NewReader(data)),
		Size:         int64(len(data)),
		LastModified: f.downloadLastModified,
		ModTime:      f.downloadModTime,
	}, nil
}

func (f *fakeBackend) Upload(_ context.Context, profile config.Profile, key string, input s3store.UploadInput) error {
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return err
	}
	f.uploads[objectID(profile, key)] = data
	f.objects[objectID(profile, key)] = data
	input.Body = nil // consumed above; the body is in f.uploads
	f.uploadInputs[objectID(profile, key)] = input
	return nil
}

func (f *fakeBackend) Delete(_ context.Context, profile config.Profile, key string) error {
	id := objectID(profile, key)
	delete(f.objects, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeBackend) Probe(ctx context.Context, profile config.Profile) (s3store.ProbeResult, error) {
	if f.probeRelease != nil {
		select {
		case <-f.probeRelease:
		case <-ctx.Done():
			return s3store.ProbeResult{}, ctx.Err()
		}
	}
	if err := f.probeErrs[profile.Name]; err != nil {
		return s3store.ProbeResult{StatusCode: http.StatusForbidden, RequestID: "REQ-" + profile.Name}, err
	}
	return s3store.ProbeResult{StatusCode: http.StatusOK, RequestID: "REQ-" + profile.Name}, nil
}

func newTestService(t *testing.T, backend *fakeBackend) config.Profile {
	t.Helper()
	dir := t.TempDir()
	ini := filepath.Join(dir, "wincmd.ini")
	content := "[demo]\nendpoint=https://s3.example.test\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"
	if err := os.WriteFile(filepath.Join(dir, "wfxs3.ini"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	service := New(backend)
	service.SetDefaultIniName(ini)
	return config.Profile{Name: "demo", Endpoint: "https://s3.example.test", Region: "us-east-1", Bucket: "bucket", AccessKey: "access", SecretKey: "secret", PathStyle: true}
}

func writeServiceConfig(t *testing.T, service *Service, contents string) {
	t.Helper()
	if err := os.WriteFile(service.ConfigPath(), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func profileConfig(name, endpoint string) string {
	return "[" + name + "]\nendpoint=" + endpoint + "\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"
}

func findNames(t *testing.T, service *Service, remote string) []string {
	t.Helper()
	token, first, err := service.FindFirst(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer service.FindClose(token)

	names := []string{first.Name}
	for {
		entry, ok, err := service.FindNext(token)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return names
		}
		names = append(names, entry.Name)
	}
}

func TestFindFirstReloadsConfigDespiteUnchangedStamp(t *testing.T) {
	service := New(newFakeBackend())
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	initial := profileConfig("old", "https://s3.example.test")
	replacement := profileConfig("new", "https://s3.example.test")
	if len(initial) != len(replacement) {
		t.Fatalf("test configs must have equal sizes: %d != %d", len(initial), len(replacement))
	}
	writeServiceConfig(t, service, initial)
	before, err := os.Stat(service.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	if names := findNames(t, service, `\`); len(names) != 1 || names[0] != "old" {
		t.Fatalf("unexpected initial profiles: %v", names)
	}
	writeServiceConfig(t, service, replacement)
	if err := os.Chtimes(service.ConfigPath(), before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if names := findNames(t, service, `\`); len(names) != 1 || names[0] != "new" {
		t.Fatalf("configuration was not reloaded: %v", names)
	}
}

func TestFindFirstReloadsConfigForSavedSubdirectory(t *testing.T) {
	backend := newFakeBackend()
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("old", "https://s3.example.test"))
	backend.entries["old|"] = []s3store.Entry{{Name: "old.txt"}}
	backend.entries["new|"] = []s3store.Entry{{Name: "new.txt"}}

	token, _, err := service.FindFirst(`\old`)
	if err != nil {
		t.Fatal(err)
	}
	service.FindClose(token)
	writeServiceConfig(t, service, profileConfig("new", "https://s3.example.test"))
	if _, _, err := service.FindFirst(`\old`); err == nil {
		t.Fatal("removed profile was still available after configuration reload")
	}
	token, first, err := service.FindFirst(`\new`)
	if err != nil {
		t.Fatal(err)
	}
	service.FindClose(token)
	if first.Name != "new.txt" {
		t.Fatalf("unexpected new profile entry: %+v", first)
	}
}

func TestConfigUsesTotalCommanderSettingsDirectory(t *testing.T) {
	settingsDir := t.TempDir()
	pluginDir := t.TempDir()
	service := New(newFakeBackend())
	service.SetDefaultIniName(filepath.Join(settingsDir, "wincmd.ini"))
	if err := os.WriteFile(filepath.Join(pluginDir, "wfxs3.ini"), []byte(profileConfig("old", "https://s3.example.test")), 0600); err != nil {
		t.Fatal(err)
	}
	writeServiceConfig(t, service, profileConfig("current", "https://s3.example.test"))

	if got, want := service.ConfigPath(), filepath.Join(settingsDir, "wfxs3.ini"); got != want {
		t.Fatalf("unexpected config path: got %q, want %q", got, want)
	}
	if names := findNames(t, service, `\`); len(names) != 1 || names[0] != "current" {
		t.Fatalf("loaded profiles from the wrong directory: %v", names)
	}
}

func TestConfigLoadLogsPathAndProfilesWithoutSecrets(t *testing.T) {
	service := New(newFakeBackend())
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
	var messageType int
	var message string
	service.SetCallbacks(Callbacks{Log: func(gotType int, gotMessage string) {
		messageType = gotType
		message = gotMessage
	}})

	_ = findNames(t, service, `\`)
	if messageType != MessageDetails {
		t.Fatalf("unexpected config log type: %d", messageType)
	}
	if !strings.Contains(message, service.ConfigPath()) || !strings.Contains(message, "demo") {
		t.Fatalf("config log omitted path or profile: %q", message)
	}
	if strings.Contains(message, "secret") || strings.Contains(message, "access") {
		t.Fatalf("config log exposed credentials: %q", message)
	}
}

func TestFindFirstAndNext(t *testing.T) {
	backend := newFakeBackend()
	profile := newTestService(t, backend)
	backend.entries[objectID(profile, "")] = []s3store.Entry{
		{Name: "z.txt", Size: 3},
		{Name: "docs", Directory: true},
	}
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	if err := os.WriteFile(service.ConfigPath(), []byte("[demo]\nendpoint=https://s3.example.test\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token, first, err := service.FindFirst(`\demo`)
	if err != nil {
		t.Fatal(err)
	}
	defer service.FindClose(token)
	if first.Name != "docs" || !first.Directory {
		t.Fatalf("unexpected first entry: %+v", first)
	}
	second, ok, err := service.FindNext(token)
	if err != nil || !ok || second.Name != "z.txt" {
		t.Fatalf("unexpected second entry: %+v %v %v", second, ok, err)
	}
	_, ok, err = service.FindNext(token)
	if err != nil || ok {
		t.Fatalf("expected end of listing: %v %v", ok, err)
	}
}

func TestGetFileSupportsOverwriteAndMove(t *testing.T) {
	backend := newFakeBackend()
	profile := newTestService(t, backend)
	backend.objects[objectID(profile, "hello.txt")] = []byte("hello")
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	if err := os.WriteFile(service.ConfigPath(), []byte("[demo]\nendpoint=https://s3.example.test\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "hello.txt")
	status, err := service.GetFile(`\demo\hello.txt`, local, CopyMove, time.Time{})
	if err != nil || status != FileOK {
		t.Fatalf("download failed: %d %v", status, err)
	}
	data, err := os.ReadFile(local)
	if err != nil || string(data) != "hello" {
		t.Fatalf("unexpected local data: %q %v", data, err)
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != objectID(profile, "hello.txt") {
		t.Fatalf("remote object was not moved: %#v", backend.deleted)
	}
}

func TestPutFileRejectsExistingObjectWithoutOverwrite(t *testing.T) {
	backend := newFakeBackend()
	profile := newTestService(t, backend)
	backend.objects[objectID(profile, "hello.txt")] = []byte("old")
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	if err := os.WriteFile(service.ConfigPath(), []byte("[demo]\nendpoint=https://s3.example.test\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(local, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := service.PutFile(local, `\demo\hello.txt`, 0)
	if err != nil || status != FileExists {
		t.Fatalf("expected exists result, got %d %v", status, err)
	}
	if len(backend.uploads) != 0 {
		t.Fatal("object was uploaded despite missing overwrite flag")
	}
}

func TestPutFileReportsCompletionForEmptyFile(t *testing.T) {
	backend := newFakeBackend()
	dir := t.TempDir()
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	if err := os.WriteFile(service.ConfigPath(), []byte("[demo]\nendpoint=https://s3.example.test\nregion=us-east-1\nbucket=bucket\naccess_key=access\nsecret_key=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(local, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var percents []int
	service.SetCallbacks(Callbacks{Progress: func(_, _ string, percent int) bool {
		percents = append(percents, percent)
		return false
	}})
	status, err := service.PutFile(local, `\demo\empty.txt`, 0)
	if err != nil || status != FileOK {
		t.Fatalf("upload failed: %d %v", status, err)
	}
	if len(percents) < 2 || percents[len(percents)-1] != 100 {
		t.Fatalf("expected a 100%% completion callback, got %v", percents)
	}
}

// TestPutFileUploadsThroughPlainHTTPEndpoint covers a regression: the upload
// body was wrapped in a reader that exposed only io.Reader, which hid the
// underlying file's io.Seeker. For an http:// endpoint the AWS SDK signs the
// real payload hash, so it reads the body and then rewinds it, and every upload
// failed with "failed to compute payload hash: ... request stream is not
// seekable". https:// endpoints use UNSIGNED-PAYLOAD and never noticed.
func TestPutFileUploadsThroughPlainHTTPEndpoint(t *testing.T) {
	payload := []byte(strings.Repeat("s3 over plain http\n", 512))
	var uploaded []byte
	var putHeader http.Header
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodHead:
			response.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			puts++
			putHeader = request.Header.Clone()
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(response, err.Error(), http.StatusInternalServerError)
				return
			}
			uploaded = body
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	service := New(s3store.New())
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", server.URL))

	local := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(local, payload, 0600); err != nil {
		t.Fatal(err)
	}

	var percents []int
	service.SetCallbacks(Callbacks{Progress: func(_, _ string, percent int) bool {
		percents = append(percents, percent)
		return false
	}})

	status, err := service.PutFile(local, `\demo\payload.txt`, 0)
	if err != nil || status != FileOK {
		t.Fatalf("upload failed: %d %v", status, err)
	}
	if !bytes.Equal(uploaded, payload) {
		t.Fatalf("uploaded %d bytes, want %d", len(uploaded), len(payload))
	}
	if puts != 1 {
		t.Fatalf("expected exactly one PutObject request, got %d", puts)
	}
	// The headers the real SDK put on the wire, rather than what the fake
	// backend was handed. The test server does not check the signature, so this
	// shows the request was built and sent, not that it was signed correctly.
	if got, want := putHeader.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if putHeader.Get("X-Amz-Meta-Mtime") == "" {
		t.Error("X-Amz-Meta-Mtime was not sent")
	}
	if len(percents) == 0 || percents[len(percents)-1] != 100 {
		t.Fatalf("expected a 100%% completion callback, got %v", percents)
	}
}

func TestCountingReaderTracksReplays(t *testing.T) {
	var done atomic.Int64
	reader := &countingReader{reader: bytes.NewReader(make([]byte, 100)), done: &done}

	// The SDK drains the body to hash it, rewinds, then streams it for real.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, reader, 50); err != nil {
		t.Fatal(err)
	}
	if got := done.Load(); got != 50 {
		t.Fatalf("expected the replayed body to count 50 bytes, got %d", got)
	}
}

func TestFindFirstFailsWhenEndpointStopsResponding(t *testing.T) {
	previous := metadataTimeout
	metadataTimeout = 2 * time.Second
	t.Cleanup(func() { metadataTimeout = previous })

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	service := New(s3store.New())
	service.SetDefaultIniName(filepath.Join(t.TempDir(), "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", server.URL))

	start := time.Now()
	_, _, err := service.FindFirst(`\demo`)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the listing to fail while the endpoint withheld its response")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("listing blocked for %s despite the metadata timeout", elapsed)
	}
}

func newHTTPService(t *testing.T, endpoint string) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	service := New(s3store.New())
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", endpoint))
	return service, dir
}

func setTransferTimings(t *testing.T, idle, interval time.Duration) {
	t.Helper()
	previousIdle, previousInterval := transferIdleTimeout, progressInterval
	transferIdleTimeout, progressInterval = idle, interval
	t.Cleanup(func() { transferIdleTimeout, progressInterval = previousIdle, previousInterval })
}

// stallingDownloadServer answers every GET with headers and the first 64 KiB
// of a 1 MiB body, then goes silent. stalled is closed once a response has
// stopped sending.
func stallingDownloadServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	stalled := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Length", strconv.Itoa(1<<20))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(make([]byte, 64<<10))
		response.(http.Flusher).Flush()
		once.Do(func() { close(stalled) })
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server, stalled
}

// A body that stops arriving used to block GetFile, and with it Total
// Commander, forever: nothing bounded the read once the headers had arrived.
func TestGetFileStallFailsAfterIdleTimeout(t *testing.T) {
	setTransferTimings(t, time.Second, 10*time.Millisecond)
	server, _ := stallingDownloadServer(t)
	service, dir := newHTTPService(t, server.URL)
	local := filepath.Join(dir, "stalled.bin")

	start := time.Now()
	status, err := service.GetFile(`\demo\stalled.bin`, local, 0, time.Time{})
	if status != FileReadError || !errors.Is(err, ErrTransferStalled) {
		t.Fatalf("expected a stalled download, got %d %v", status, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("stalled download took %s to fail", elapsed)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".wfxs3-download-*")); len(leftovers) != 0 {
		t.Fatalf("temporary download files were left behind: %v", leftovers)
	}
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a partial download was kept: %v", err)
	}
}

// Cancel must work while the network is stalled, which requires consulting
// the progress callback even when no bytes arrive.
func TestGetFileCancelDuringStall(t *testing.T) {
	setTransferTimings(t, time.Minute, 10*time.Millisecond)
	server, stalled := stallingDownloadServer(t)
	service, dir := newHTTPService(t, server.URL)
	var cancelledAt atomic.Int64
	go func() {
		select {
		case <-stalled:
			time.Sleep(500 * time.Millisecond)
			cancelledAt.Store(time.Now().UnixNano())
		case <-time.After(10 * time.Second):
		}
	}()
	service.SetCallbacks(Callbacks{Progress: func(_, _ string, _ int) bool {
		return cancelledAt.Load() != 0 // the user has clicked Cancel
	}})

	status, err := service.GetFile(`\demo\stalled.bin`, filepath.Join(dir, "stalled.bin"), 0, time.Time{})
	if status != FileUserAbort || !errors.Is(err, ErrUserAbort) {
		t.Fatalf("expected a user abort, got %d %v", status, err)
	}
	if latency := time.Since(time.Unix(0, cancelledAt.Load())); latency > 2*time.Second {
		t.Fatalf("Cancel took %s to take effect during the stall", latency)
	}
}

// Cancel during an upload used to surface as a failed read of the request
// body. The AWS SDK retries those as connection errors, rewinding the file and
// sending it again, so an upload the user had cancelled could still complete.
func TestPutFileCancelIsNotRetried(t *testing.T) {
	setTransferTimings(t, time.Minute, 10*time.Millisecond)
	streaming := make(chan struct{})
	release := make(chan struct{})
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodHead:
			response.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			if puts.Add(1) == 1 {
				_, _ = io.CopyN(io.Discard, request.Body, 1<<20)
				close(streaming)
				<-release
				return
			}
			// A retried upload would complete here.
			_, _ = io.Copy(io.Discard, request.Body)
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	service, dir := newHTTPService(t, server.URL)
	local := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(local, make([]byte, 32<<20), 0600); err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Bool
	service.SetCallbacks(Callbacks{Progress: func(_, _ string, _ int) bool {
		select {
		case <-streaming:
			// Report Cancel only once, as a host that does not repeat it would.
			return cancelled.CompareAndSwap(false, true)
		default:
			return false
		}
	}})

	start := time.Now()
	status, err := service.PutFile(local, `\demo\large.bin`, 0)
	if status != FileUserAbort || !errors.Is(err, ErrUserAbort) {
		t.Fatalf("expected a user abort, got %d %v", status, err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("the cancelled upload was sent %d times", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Cancel took %s", elapsed)
	}
}

func TestPutFileStallFailsAfterIdleTimeout(t *testing.T) {
	setTransferTimings(t, time.Second, 10*time.Millisecond)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodHead {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		<-release // accept the upload but never read its body or answer
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	service, dir := newHTTPService(t, server.URL)
	local := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(local, make([]byte, 8<<20), 0600); err != nil {
		t.Fatal(err)
	}

	status, err := service.PutFile(local, `\demo\large.bin`, 0)
	if status != FileWriteError || !errors.Is(err, ErrTransferStalled) {
		t.Fatalf("expected a stalled upload, got %d %v", status, err)
	}
}

func TestGetFileOverwrite(t *testing.T) {
	tests := []struct {
		name     string
		flags    int
		readOnly bool
		status   int
		content  string
	}{
		{name: "existing file without overwrite", status: FileExists, content: "old"},
		{name: "overwrite", flags: CopyOverwrite, status: FileOK, content: "hello"},
		{name: "overwrite read-only file", flags: CopyOverwrite, readOnly: true, status: FileOK, content: "hello"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			backend.objects[objectID(config.Profile{Name: "demo"}, "hello.txt")] = []byte("hello")
			dir := t.TempDir()
			service := New(backend)
			service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
			writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
			local := filepath.Join(dir, "hello.txt")
			mode := os.FileMode(0o600)
			if test.readOnly {
				mode = 0o400
			}
			if err := os.WriteFile(local, []byte("old"), mode); err != nil {
				t.Fatal(err)
			}

			status, err := service.GetFile(`\demo\hello.txt`, local, test.flags, time.Time{})
			if err != nil || status != test.status {
				t.Fatalf("unexpected result: %d %v", status, err)
			}
			if data, err := os.ReadFile(local); err != nil || string(data) != test.content {
				t.Fatalf("local file holds %q (%v), want %q", data, err, test.content)
			}
		})
	}
}

func TestGetFileMapsDownloadErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "missing object", err: fmt.Errorf("%w: NoSuchKey", os.ErrNotExist), status: FileNotFound},
		{name: "access denied", err: errors.New("api error AccessDenied: Access Denied"), status: FileReadError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			backend.downloadErr = test.err
			dir := t.TempDir()
			service := New(backend)
			service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
			writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))

			status, err := service.GetFile(`\demo\file.txt`, filepath.Join(dir, "file.txt"), 0, time.Time{})
			if status != test.status || !errors.Is(err, test.err) {
				t.Fatalf("got %d %v, want status %d", status, err, test.status)
			}
		})
	}
}

// TestGetFileStampsDownloadedFile covers the priority chain: the source time
// stamp the object carries beats the object's own, which beats what Total
// Commander read from the listing.
func TestGetFileStampsDownloadedFile(t *testing.T) {
	var (
		// Whole milliseconds, so the value survives NTFS, ext4 and APFS alike.
		metaTime   = time.Unix(1500000000, 500000000)
		objectTime = time.Unix(1600000000, 250000000)
		listedTime = time.Unix(1400000000, 750000000)
		// Chtimes converts through time.UnixNano and cannot carry this.
		unrepresentable = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	)
	tests := []struct {
		name         string
		modTime      time.Time
		lastModified time.Time
		remote       time.Time
		want         time.Time
	}{
		{"source time stamp wins", metaTime, objectTime, listedTime, metaTime},
		{"falls back to the object time stamp", time.Time{}, objectTime, listedTime, objectTime},
		{"falls back to what Total Commander passed", time.Time{}, time.Time{}, listedTime, listedTime},
		// An unusable candidate is skipped rather than ending the search, so a
		// nonsense source time stamp does not cost the file a good one.
		{"skips an unrepresentable candidate", unrepresentable, objectTime, listedTime, objectTime},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			backend.objects[objectID(config.Profile{Name: "demo"}, "hello.txt")] = []byte("hello")
			backend.downloadModTime = test.modTime
			backend.downloadLastModified = test.lastModified
			dir := t.TempDir()
			service := New(backend)
			service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
			writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
			local := filepath.Join(dir, "hello.txt")

			status, err := service.GetFile(`\demo\hello.txt`, local, 0, test.remote)
			if err != nil || status != FileOK {
				t.Fatalf("unexpected result: %d %v", status, err)
			}
			info, err := os.Stat(local)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(test.want) {
				t.Errorf("local time stamp = %v, want %v", info.ModTime().UTC(), test.want.UTC())
			}
		})
	}
}

func TestGetFileLeavesTimeStampAloneWhenNoneIsKnown(t *testing.T) {
	backend := newFakeBackend()
	backend.objects[objectID(config.Profile{Name: "demo"}, "hello.txt")] = []byte("hello")
	backend.downloadLastModified = time.Time{}
	dir := t.TempDir()
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
	local := filepath.Join(dir, "hello.txt")

	before := time.Now()
	status, err := service.GetFile(`\demo\hello.txt`, local, 0, time.Time{})
	if err != nil || status != FileOK {
		t.Fatalf("unexpected result: %d %v", status, err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to stamp with, so the file keeps the time it was written.
	if info.ModTime().Before(before.Add(-time.Minute)) {
		t.Errorf("local time stamp = %v, want roughly now", info.ModTime())
	}
}

// TestGetFileSurvivesTimeStampFailure pins the best-effort policy: a complete
// and correct download is not reported as a failure because its time stamp
// could not be set.
func TestGetFileSurvivesTimeStampFailure(t *testing.T) {
	original := setFileTime
	setFileTime = func(string, time.Time, time.Time) error { return errors.New("denied") }
	t.Cleanup(func() { setFileTime = original })

	backend := newFakeBackend()
	backend.objects[objectID(config.Profile{Name: "demo"}, "hello.txt")] = []byte("hello")
	dir := t.TempDir()
	service := New(backend)
	service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
	writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
	local := filepath.Join(dir, "hello.txt")

	status, err := service.GetFile(`\demo\hello.txt`, local, 0, time.Time{})
	if err != nil || status != FileOK {
		t.Fatalf("unexpected result: %d %v", status, err)
	}
	if data, err := os.ReadFile(local); err != nil || string(data) != "hello" {
		t.Fatalf("local file holds %q (%v), want %q", data, err, "hello")
	}
	// The temp file still has to be cleaned up by the rename.
	matches, err := filepath.Glob(filepath.Join(dir, ".wfxs3-download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temp files left behind: %v (%v)", matches, err)
	}
	// Silent would be wrong: the activity log is where someone looks to find
	// out why the time stamp is not what they expected.
	var recorded *Activity
	for _, entry := range service.activity.newestFirst() {
		if entry.Op == "mtime" {
			recorded = &entry
			break
		}
	}
	if recorded == nil {
		t.Fatal("the failed time stamp was not recorded")
	}
	if recorded.Status != "error" || !strings.Contains(recorded.Err, "denied") || recorded.Path != local {
		t.Errorf("unexpected mtime entry: %+v", *recorded)
	}
}

func TestPutFileSendsContentTypeAndModTime(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		key         string
		contentType string
	}{
		{"known extension", "site.css", `\demo\site.css`, "text/css; charset=utf-8"},
		// Nothing recognises this, so the plugin says nothing and the SDK's own
		// application/octet-stream default stands.
		{"unknown extension", "blob.unknownextension", `\demo\blob.unknownextension`, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			dir := t.TempDir()
			service := New(backend)
			service.SetDefaultIniName(filepath.Join(dir, "wincmd.ini"))
			writeServiceConfig(t, service, profileConfig("demo", "https://s3.example.test"))
			local := filepath.Join(dir, test.file)
			if err := os.WriteFile(local, []byte("body"), 0600); err != nil {
				t.Fatal(err)
			}
			modTime := time.Unix(1500000000, 500000000)
			if err := os.Chtimes(local, modTime, modTime); err != nil {
				t.Fatal(err)
			}

			status, err := service.PutFile(local, test.key, 0)
			if err != nil || status != FileOK {
				t.Fatalf("upload failed: %d %v", status, err)
			}
			input, ok := backend.uploadInputs[objectID(config.Profile{Name: "demo"}, strings.TrimPrefix(test.key, `\demo\`))]
			if !ok {
				t.Fatalf("no upload recorded: %v", backend.uploadInputs)
			}
			if input.ContentType != test.contentType {
				t.Errorf("ContentType = %q, want %q", input.ContentType, test.contentType)
			}
			if !input.ModTime.Equal(modTime) {
				t.Errorf("ModTime = %v, want %v", input.ModTime.UTC(), modTime.UTC())
			}
		})
	}
}
