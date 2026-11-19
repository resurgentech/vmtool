package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the MinIO-backed store.
//
// Endpoint is host:port (plain HTTP, the in-Docker-network case) or a URL
// with an http:// or https:// scheme and no path; only https:// turns TLS
// on. The secret key is never included in an error.
type S3Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3 is a BlobStore on a MinIO/S3 bucket. It is thin glue on minio-go.
type S3 struct {
	c      *minio.Client
	bucket string
}

// parseEndpoint splits Endpoint into minio's host and secure flag.
func parseEndpoint(ep string) (host string, secure bool, err error) {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return "", false, errors.New("blob: s3 endpoint is empty")
	}
	if !strings.Contains(ep, "://") {
		return ep, false, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return "", false, errors.New("blob: s3 endpoint is not a valid URL")
	}
	switch u.Scheme {
	case "http":
	case "https":
		secure = true
	default:
		return "", false, fmt.Errorf("blob: s3 endpoint scheme %q, want http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", false, errors.New("blob: s3 endpoint has no host")
	}
	if p := strings.Trim(u.Path, "/"); p != "" || u.RawQuery != "" {
		return "", false, errors.New("blob: s3 endpoint must not have a path or query")
	}
	return u.Host, secure, nil
}

// NewS3 validates c and builds the client. It makes no request.
func NewS3(c S3Config) (*S3, error) {
	host, secure, err := parseEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	if c.Bucket == "" {
		return nil, errors.New("blob: s3 bucket is empty")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("blob: s3 access key and secret key are required")
	}
	cl, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: s3 client: %w", err)
	}
	return &S3{c: cl, bucket: c.Bucket}, nil
}

// mapErr turns minio's not-found responses into ErrNotFound and leaves
// other errors alone. A HEAD has no body, so a bare 404 counts too.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var er minio.ErrorResponse
	if errors.As(err, &er) {
		switch er.Code {
		case "NoSuchKey", "NoSuchBucket", "NoSuchVersion", "NotFound":
			return ErrNotFound
		}
		if er.StatusCode == 404 {
			return ErrNotFound
		}
	}
	return err
}

func (s *S3) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("blob: check bucket: %w", err)
	}
	if ok {
		return nil
	}
	if err := s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		switch minio.ToErrorResponse(err).Code {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return nil // raced with another starter
		}
		return fmt.Errorf("blob: make bucket: %w", err)
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size < 0 {
		return fmt.Errorf("blob: negative size %d", size)
	}
	fr := newFixedReader(ctx, r, size)
	_, err := s.c.PutObject(ctx, s.bucket, key, fr, size, minio.PutObjectOptions{})
	if err == nil {
		return nil
	}
	if fr.short {
		return ErrShortBody
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	o, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Info{}, mapErr(err)
	}
	st, err := o.Stat() // GetObject is lazy; this is where a missing key shows
	if err != nil {
		_ = o.Close()
		return nil, Info{}, mapErr(err)
	}
	return o, Info{Size: st.Size, ModTime: st.LastModified}, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	st, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return Info{}, mapErr(err)
	}
	return Info{Size: st.Size, ModTime: st.LastModified}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	// RemoveObject on a missing key succeeds on S3; treat a 404 the same.
	if err := mapErr(s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

var _ BlobStore = (*S3)(nil)
