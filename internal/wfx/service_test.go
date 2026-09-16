package wfx

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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
