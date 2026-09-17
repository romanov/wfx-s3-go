package wfx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/wfxs3/internal/config"
	"github.com/example/wfxs3/internal/s3store"
)

type fakeBackend struct {
	entries map[string][]s3store.Entry
	objects map[string][]byte
	deleted []string
	uploads map[string][]byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		entries: make(map[string][]s3store.Entry),
		objects: make(map[string][]byte),
		uploads: make(map[string][]byte),
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
	data, ok := f.objects[objectID(profile, key)]
	if !ok {
		return s3store.Object{}, os.ErrNotExist
	}
	return s3store.Object{Body: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data)), LastModified: time.Unix(1700000000, 0)}, nil
}

func (f *fakeBackend) Upload(_ context.Context, profile config.Profile, key string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.uploads[objectID(profile, key)] = data
	f.objects[objectID(profile, key)] = data
	return nil
}

func (f *fakeBackend) Delete(_ context.Context, profile config.Profile, key string) error {
	id := objectID(profile, key)
	delete(f.objects, id)
	f.deleted = append(f.deleted, id)
	return nil
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
	status, err := service.GetFile(`\demo\hello.txt`, local, CopyMove)
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
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodHead:
			response.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			puts++
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
	if len(percents) == 0 || percents[len(percents)-1] != 100 {
		t.Fatalf("expected a 100%% completion callback, got %v", percents)
	}
}

func TestProgressRewindReanchorsReporting(t *testing.T) {
	var percents []int
	progress := newProgress(Callbacks{Progress: func(_, _ string, percent int) bool {
		percents = append(percents, percent)
		return false
	}}, "source", "target", 100)

	// The SDK drains the body to hash it, rewinds, then streams it for real.
	progress.update(progress.done + 100)
	progress.rewind(0)
	progress.update(progress.done + 50)

	if len(percents) == 0 || percents[len(percents)-1] != 50 {
		t.Fatalf("expected progress to resume at 50%% after the rewind, got %v", percents)
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
