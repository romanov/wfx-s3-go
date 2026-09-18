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

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"github.com/example/wfxs3/internal/config"
)

func TestStoreUsesPathStyleAndS3Operations(t *testing.T) {
	var uploaded []byte
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
	if err := store.Upload(context.Background(), profile, "upload.txt", bytes.NewReader([]byte("uploaded")), 8); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "uploaded" {
		t.Fatalf("unexpected uploaded body: %q", uploaded)
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
