package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"github.com/example/wfxs3/internal/config"
)

func TestStoreUsesPathStyleAndS3Operations(t *testing.T) {
	var uploaded []byte
	var putHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/bucket") {
			http.Error(response, "expected path-style request", http.StatusBadRequest)
			return
		}
		key := strings.TrimPrefix(request.URL.Path, "/bucket/")
		switch request.Method {
		case http.MethodGet:
			if request.URL.Query().Get("list-type") == "2" {
				response.Header().Set("Content-Type", "application/xml")
				_, _ = fmt.Fprint(response, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>base/</Prefix><KeyCount>3</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><CommonPrefixes><Prefix>base/docs/</Prefix></CommonPrefixes><Contents><Key>base/empty/</Key><LastModified>2024-01-02T03:04:05Z</LastModified><ETag>"marker"</ETag><Size>0</Size><StorageClass>STANDARD</StorageClass></Contents><Contents><Key>base/readme.txt</Key><LastModified>2024-01-02T03:04:05Z</LastModified><ETag>"etag"</ETag><Size>5</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`)
				return
			}
			_, _ = response.Write([]byte("hello"))
		case http.MethodHead:
			if key == "missing" {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			response.Header().Set("Content-Length", "5")
		case http.MethodPut:
			var err error
			putHeader = request.Header.Clone()
			uploaded, err = io.ReadAll(request.Body)
			if err != nil {
				http.Error(response, err.Error(), http.StatusInternalServerError)
			}
		case http.MethodDelete:
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	profile := config.Profile{
		Name:      "demo",
		Endpoint:  server.URL,
		Region:    "us-east-1",
		Bucket:    "bucket",
		Prefix:    "base/",
		AccessKey: "access",
		SecretKey: "secret",
		PathStyle: true,
	}
	store := New()
	entries, err := store.List(context.Background(), profile, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Name != "docs" || !entries[0].Directory || entries[1].Name != "empty" || !entries[1].Directory || entries[2].Name != "readme.txt" || entries[2].Size != 5 {
		t.Fatalf("unexpected listing: %+v", entries)
	}

	exists, err := store.Head(context.Background(), profile, "readme.txt")
	if err != nil || !exists {
		t.Fatalf("head failed: %v %v", exists, err)
	}
	object, err := store.Download(context.Background(), profile, "readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil || string(data) != "hello" {
		t.Fatalf("download failed: %q %v", data, err)
	}
	modTime := time.Unix(1700000000, 123456789)
	input := UploadInput{
		Body:        bytes.NewReader([]byte("uploaded")),
		Size:        8,
		ContentType: "text/plain; charset=utf-8",
		ModTime:     modTime,
	}
	if err := store.Upload(context.Background(), profile, "upload.txt", input); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "uploaded" {
		t.Fatalf("unexpected uploaded body: %q", uploaded)
	}
	if got := putHeader.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got, want := putHeader.Get("X-Amz-Meta-Mtime"), formatModTime(modTime); got != want {
		t.Errorf("X-Amz-Meta-Mtime = %q, want %q", got, want)
	}
	if err := store.Delete(context.Background(), profile, "upload.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDownloadReportsMissingObjectAsNotExist(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		code     string
		notExist bool
	}{
		{name: "missing key", status: http.StatusNotFound, code: "NoSuchKey", notExist: true},
		{name: "access denied", status: http.StatusForbidden, code: "AccessDenied", notExist: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/xml")
				response.WriteHeader(test.status)
				_, _ = fmt.Fprintf(response, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>test</Message></Error>`, test.code)
			}))
			defer server.Close()
			profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKey: "a", SecretKey: "s", PathStyle: true}

			_, err := New().Download(context.Background(), profile, "file.txt")
			if err == nil {
				t.Fatal("expected a download error")
			}
			if got := errors.Is(err, os.ErrNotExist); got != test.notExist {
				t.Fatalf("errors.Is(err, os.ErrNotExist) = %v, want %v: %v", got, test.notExist, err)
			}
		})
	}
}

func TestStoreDefaultsEmptyRegion(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		response.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	profile := config.Profile{
		Name:      "demo",
		Endpoint:  server.URL,
		Bucket:    "bucket",
		AccessKey: "access",
		SecretKey: "secret",
	}

	exists, err := New().Head(context.Background(), profile, "object.txt")
	if err != nil || exists {
		t.Fatalf("expected a not-found result, got %v %v", exists, err)
	}
	if !strings.Contains(authorization, "/"+config.DefaultRegion+"/s3/aws4_request") {
		t.Fatalf("authorization does not use default region %q: %q", config.DefaultRegion, authorization)
	}
}

func TestStoreClientIdentityIncludesConnectionSettings(t *testing.T) {
	base := config.Profile{
		Name:         "demo",
		Endpoint:     "https://s3.example.test",
		Region:       "us-east-1",
		Bucket:       "bucket",
		AccessKey:    "access",
		SecretKey:    "secret",
		SessionToken: "token",
		PathStyle:    true,
	}
	store := New()
	original := store.client(base)

	variants := []config.Profile{
		{Name: base.Name, Endpoint: "https://other.example.test", Region: base.Region, Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: base.SecretKey, SessionToken: base.SessionToken, PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: "eu-west-1", Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: base.SecretKey, SessionToken: base.SessionToken, PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: base.Region, Bucket: "other-bucket", AccessKey: base.AccessKey, SecretKey: base.SecretKey, SessionToken: base.SessionToken, PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: base.Region, Bucket: base.Bucket, AccessKey: "other-access", SecretKey: base.SecretKey, SessionToken: base.SessionToken, PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: base.Region, Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: "other-secret", SessionToken: base.SessionToken, PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: base.Region, Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: base.SecretKey, SessionToken: "other-token", PathStyle: base.PathStyle},
		{Name: base.Name, Endpoint: base.Endpoint, Region: base.Region, Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: base.SecretKey, SessionToken: base.SessionToken, PathStyle: false},
	}
	for _, variant := range variants {
		if got := store.client(variant); got == original {
			t.Fatalf("connection settings reused the original client: %+v", variant)
		}
	}
}

func TestStoreObjectPathEscapesKeys(t *testing.T) {
	var escapedPath string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		escapedPath = request.URL.EscapedPath()
		response.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "x", Bucket: "bucket", AccessKey: "a", SecretKey: "s", PathStyle: true}
	exists, err := New().Head(context.Background(), profile, "a b.txt")
	if err != nil || exists {
		t.Fatalf("expected a not-found result, got %v %v", exists, err)
	}
	if escapedPath != "/bucket/a%20b.txt" {
		t.Fatalf("unexpected escaped request path: %s", escapedPath)
	}
}

func TestStoreProbeReportsStatusAndRequestID(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query = request.URL.Query()
		response.Header().Set("Content-Type", "application/xml")
		response.Header().Set("x-amz-request-id", "REQ200")
		_, _ = fmt.Fprint(response, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>base/</Prefix><KeyCount>0</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()
	profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", Prefix: "base/", AccessKey: "a", SecretKey: "s", PathStyle: true}

	result, err := New().Probe(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || result.RequestID != "REQ200" {
		t.Fatalf("unexpected probe result: %+v", result)
	}
	if query.Get("list-type") != "2" || query.Get("max-keys") != "1" || query.Get("prefix") != "base/" || query.Get("delimiter") != "/" {
		t.Fatalf("the probe is not a one-key listing of the profile prefix: %v", query)
	}
}

func TestStoreProbeReportsErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/xml")
		response.Header().Set("x-amz-request-id", "REQ403")
		response.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(response, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>REQ403</RequestId></Error>`)
	}))
	defer server.Close()
	profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKey: "a", SecretKey: "s", PathStyle: true}

	result, err := New().Probe(context.Background(), profile)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("expected an access-denied error, got %v", err)
	}
	if result.StatusCode != http.StatusForbidden || result.RequestID != "REQ403" {
		t.Fatalf("unexpected probe result: %+v", result)
	}
}

func TestStoreHTTPClientAppliesTimeouts(t *testing.T) {
	store := New()
	client, ok := store.httpClient.(*awshttp.BuildableClient)
	if !ok {
		t.Fatalf("expected a buildable HTTP client, got %T", store.httpClient)
	}
	if got := client.GetDialer().Timeout; got != dialTimeout {
		t.Errorf("dial timeout = %s, want %s", got, dialTimeout)
	}
	if got := client.GetTransport().ResponseHeaderTimeout; got != responseHeaderTimeout {
		t.Errorf("response header timeout = %s, want %s", got, responseHeaderTimeout)
	}
}

func TestStoreUploadOmitsUnsetContentTypeAndModTime(t *testing.T) {
	var putHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		putHeader = request.Header.Clone()
		_, _ = io.ReadAll(request.Body)
	}))
	defer server.Close()
	profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKey: "a", SecretKey: "s", PathStyle: true}

	input := UploadInput{Body: bytes.NewReader([]byte("body")), Size: 4}
	if err := New().Upload(context.Background(), profile, "file.bin", input); err != nil {
		t.Fatal(err)
	}
	// With nothing set the SDK supplies application/octet-stream itself, so an
	// unrecognised extension behaves exactly as it did before Content-Type
	// detection existed. This pins that the plugin adds nothing of its own.
	if got := putHeader.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want the SDK default", got)
	}
	for name := range putHeader {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			t.Errorf("unexpected user metadata header %q", name)
		}
	}
}

func TestStoreDownloadReadsModTimeMetadata(t *testing.T) {
	const lastModified = "Thu, 02 Jan 2025 03:04:05 GMT"
	tests := []struct {
		name     string
		metadata string
		want     time.Time
	}{
		{"rclone style", "1700000000.123456789", time.Unix(1700000000, 123456789)},
		{"s3fs style", "1700000000", time.Unix(1700000000, 0)},
		{"absent", "", time.Time{}},
		{"unreadable", "not-a-time", time.Time{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Last-Modified", lastModified)
				if test.metadata != "" {
					// Written in the wire form; the SDK strips the prefix and
					// lowercases the rest, which is what Download relies on.
					response.Header().Set("x-amz-meta-mtime", test.metadata)
				}
				_, _ = response.Write([]byte("hello"))
			}))
			defer server.Close()
			profile := config.Profile{Name: "demo", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", AccessKey: "a", SecretKey: "s", PathStyle: true}

			object, err := New().Download(context.Background(), profile, "file.txt")
			if err != nil {
				t.Fatal(err)
			}
			_ = object.Body.Close()
			if !object.ModTime.Equal(test.want) {
				t.Errorf("ModTime = %v, want %v", object.ModTime.UTC(), test.want.UTC())
			}
			// A bad or missing source time stamp must not disturb the object's
			// own, which is the fallback the download path uses next.
			want, err := time.Parse(http.TimeFormat, lastModified)
			if err != nil {
				t.Fatal(err)
			}
			if !object.LastModified.Equal(want) {
				t.Errorf("LastModified = %v, want %v", object.LastModified.UTC(), want)
			}
		})
	}
}
