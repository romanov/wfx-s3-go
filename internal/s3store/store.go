package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/example/wfxs3/internal/config"
	"github.com/example/wfxs3/internal/path"
)

// Entry is a single item returned by an S3 directory listing.
type Entry struct {
	Name         string
	Directory    bool
	Size         int64
	LastModified time.Time
}

// Object is a readable S3 object body and its metadata.
type Object struct {
	Body         io.ReadCloser
	Size         int64
	LastModified time.Time
}

// Backend is the small S3 surface used by the WFX layer and tests. Download
// returns an error wrapping os.ErrNotExist when the object does not exist.
type Backend interface {
	List(context.Context, config.Profile, string) ([]Entry, error)
	Head(context.Context, config.Profile, string) (bool, error)
	Download(context.Context, config.Profile, string) (Object, error)
	Upload(context.Context, config.Profile, string, io.Reader, int64) error
	Delete(context.Context, config.Profile, string) error
}

const (
	// dialTimeout bounds how long a wrong or unreachable endpoint can stall
	// before the plugin reports a failure.
	dialTimeout = 15 * time.Second
	// responseHeaderTimeout bounds the wait for response headers, which begins
	// only once the request body has been written. Transfers of any size are
	// therefore unaffected, while a server that accepts a connection and never
	// answers still fails promptly.
	responseHeaderTimeout = 30 * time.Second
)

// newHTTPClient builds the transport shared by every profile. The SDK's default
// client has no response-header timeout, so a silent endpoint would otherwise
// hang Total Commander indefinitely.
func newHTTPClient() aws.HTTPClient {
	return awshttp.NewBuildableClient().
		WithDialerOptions(func(dialer *net.Dialer) {
			dialer.Timeout = dialTimeout
		}).
		WithTransportOptions(func(transport *http.Transport) {
			transport.ResponseHeaderTimeout = responseHeaderTimeout
		})
}

// Store is an AWS SDK-backed Backend. Clients are cached by profile identity
// so HTTP connections can be reused while configuration changes invalidate the
// relevant key naturally.
type Store struct {
	mu         sync.Mutex
	clients    map[string]*s3.Client
	httpClient aws.HTTPClient
}

func New() *Store {
	return &Store{
		clients:    make(map[string]*s3.Client),
		httpClient: newHTTPClient(),
	}
}

func (s *Store) client(profile config.Profile) *s3.Client {
	region := strings.TrimSpace(profile.Region)
	if region == "" {
		region = config.DefaultRegion
	}
	key := strings.Join([]string{profile.Name, profile.Endpoint, region, profile.Bucket, profile.AccessKey, profile.SecretKey, profile.SessionToken, fmt.Sprint(profile.PathStyle)}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if client, ok := s.clients[key]; ok {
		return client
	}
	awsConfig := aws.Config{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(profile.AccessKey, profile.SecretKey, profile.SessionToken)),
		HTTPClient:  s.httpClient,
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(profile.Endpoint)
		options.UsePathStyle = profile.PathStyle
	})
	s.clients[key] = client
	return client
}

func (s *Store) List(ctx context.Context, profile config.Profile, relative string) ([]Entry, error) {
	client := s.client(profile)
	prefix := path.ListingPrefix(profile, relative)
	entries := make([]Entry, 0)
	seen := make(map[string]bool)
	var token *string
	for {
		output, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(profile.Bucket),
			Delimiter:         aws.String("/"),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		addDirectory := func(full string, lastModified *time.Time) {
			name := strings.TrimPrefix(full, prefix)
			name = strings.TrimSuffix(name, "/")
			if name == "" || strings.Contains(name, "/") || seen[name] {
				return
			}
			seen[name] = true
			entry := Entry{Name: name, Directory: true}
			if lastModified != nil {
				entry.LastModified = *lastModified
			}
			entries = append(entries, entry)
		}
		for _, common := range output.CommonPrefixes {
			addDirectory(aws.ToString(common.Prefix), nil)
		}
		for _, object := range output.Contents {
			full := aws.ToString(object.Key)
			name := strings.TrimPrefix(full, prefix)
			if strings.HasSuffix(name, "/") {
				// Some S3-compatible services return directory marker objects
				// in Contents instead of (or in addition to) CommonPrefixes.
				addDirectory(full, object.LastModified)
				continue
			}
			if name == "" || strings.Contains(name, "/") || seen[name] {
				continue
			}
			entry := Entry{Name: name, Size: aws.ToInt64(object.Size)}
			if object.LastModified != nil {
				entry.LastModified = *object.LastModified
			}
			seen[name] = true
			entries = append(entries, entry)
		}
		if !aws.ToBool(output.IsTruncated) || output.NextContinuationToken == nil {
			break
		}
		token = output.NextContinuationToken
	}
	return entries, nil
}

func (s *Store) Head(ctx context.Context, profile config.Profile, key string) (bool, error) {
	_, err := s.client(profile).HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(profile.Bucket),
		Key:    aws.String(path.ObjectKey(profile, key)),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) Download(ctx context.Context, profile config.Profile, key string) (Object, error) {
	output, err := s.client(profile).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(profile.Bucket),
		Key:    aws.String(path.ObjectKey(profile, key)),
	})
	if err != nil {
		if isNotFound(err) {
			return Object{}, fmt.Errorf("%w: %w", os.ErrNotExist, err)
		}
		return Object{}, err
	}
	object := Object{Body: output.Body, Size: aws.ToInt64(output.ContentLength)}
	if output.LastModified != nil {
		object.LastModified = *output.LastModified
	}
	return object, nil
}

func (s *Store) Upload(ctx context.Context, profile config.Profile, key string, body io.Reader, size int64) error {
	_, err := s.client(profile).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(profile.Bucket),
		Key:           aws.String(path.ObjectKey(profile, key)),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	return err
}

func (s *Store) Delete(ctx context.Context, profile config.Profile, key string) error {
	_, err := s.client(profile).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(profile.Bucket),
		Key:    aws.String(path.ObjectKey(profile, key)),
	})
	return err
}

func isNotFound(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusError interface{ HTTPStatusCode() int }
	if errors.As(err, &statusError) && statusError.HTTPStatusCode() == 404 {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		code := apiError.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "NoSuchBucket" || code == "404"
	}
	return false
}

// Keep the generated S3 types referenced here so upgrades that change the
// modeled listing shape fail at compile time instead of silently degrading.
var _ types.CommonPrefix
