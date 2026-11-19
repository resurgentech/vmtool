package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
)

// patterned returns n deterministic, non-repeating-looking bytes.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// TestMemoryContract runs the shared contract against the in-memory double.
func TestMemoryContract(t *testing.T) {
	runContract(t, func() BlobStore { return NewMemory() })
}

// runContract is the behaviour every BlobStore must show. It never touches
// a network; the S3 implementation is not run through it (no server in
// tests), so the double and S3 agree only through the documented errors.
func runContract(t *testing.T, mk func() BlobStore) {
	ctx := context.Background()

	t.Run("ensure bucket is idempotent", func(t *testing.T) {
		s := mk()
		for i := 0; i < 2; i++ {
			if err := s.EnsureBucket(ctx); err != nil {
				t.Fatalf("EnsureBucket #%d: %v", i, err)
			}
		}
	})

	t.Run("list returns keys under a prefix, sorted", func(t *testing.T) {
		s := mk()
		for _, k := range []string{"backups/store-b.db", "images/a", "backups/store-a.db", "backups/other.db", "backupsx/z"} {
			if err := s.Put(ctx, k, bytes.NewReader([]byte("x")), 1); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.List(ctx, "backups/store-")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if want := []string{"backups/store-a.db", "backups/store-b.db"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("List = %v, want %v", got, want)
		}
		none, err := s.List(ctx, "nothing/")
		if err != nil || len(none) != 0 {
			t.Fatalf("List none = %v, %v", none, err)
		}
	})

	t.Run("put get stat roundtrip multi MiB", func(t *testing.T) {
		s := mk()
		body := patterned(5<<20 + 123)
		if err := s.Put(ctx, "images/a.qcow2", bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		info, err := s.Stat(ctx, "images/a.qcow2")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Fatalf("Stat size = %d, want %d", info.Size, len(body))
		}
		rc, ginfo, err := s.Get(ctx, "images/a.qcow2")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		if ginfo.Size != int64(len(body)) {
			t.Fatalf("Get size = %d, want %d", ginfo.Size, len(body))
		}
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("Get body mismatch (err %v, %d bytes)", err, len(got))
		}
	})

	t.Run("put overwrites", func(t *testing.T) {
		s := mk()
		_ = s.Put(ctx, "k", strings.NewReader("one"), 3)
		if err := s.Put(ctx, "k", strings.NewReader("three"), 5); err != nil {
			t.Fatal(err)
		}
		rc, _, _ := s.Get(ctx, "k")
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "three" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		s := mk()
		if err := s.Put(ctx, "empty", strings.NewReader(""), 0); err != nil {
			t.Fatal(err)
		}
		info, err := s.Stat(ctx, "empty")
		if err != nil || info.Size != 0 {
			t.Fatalf("Stat = %+v, %v", info, err)
		}
	})

	t.Run("missing key is ErrNotFound", func(t *testing.T) {
		s := mk()
		if _, err := s.Stat(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat err = %v", err)
		}
		if rc, _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			if rc != nil {
				rc.Close()
			}
			t.Fatalf("Get err = %v", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		s := mk()
		_ = s.Put(ctx, "k", strings.NewReader("x"), 1)
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Stat(ctx, "k"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete Stat err = %v", err)
		}
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatalf("second Delete (missing key) = %v, want nil", err)
		}
	})

	t.Run("short body is ErrShortBody and stores nothing", func(t *testing.T) {
		s := mk()
		err := s.Put(ctx, "short", strings.NewReader("12345"), 10)
		if !errors.Is(err, ErrShortBody) {
			t.Fatalf("err = %v, want ErrShortBody", err)
		}
		if _, err := s.Stat(ctx, "short"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("object exists after short put: %v", err)
		}
	})

	t.Run("short put keeps the previous object", func(t *testing.T) {
		s := mk()
		_ = s.Put(ctx, "k", strings.NewReader("old"), 3)
		if err := s.Put(ctx, "k", strings.NewReader("ne"), 3); !errors.Is(err, ErrShortBody) {
			t.Fatalf("err = %v", err)
		}
		rc, _, _ := s.Get(ctx, "k")
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "old" {
			t.Fatalf("got %q, want old", got)
		}
	})

	t.Run("negative size refused", func(t *testing.T) {
		s := mk()
		if err := s.Put(ctx, "k", strings.NewReader("x"), -1); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("read error surfaces and stores nothing", func(t *testing.T) {
		s := mk()
		boom := errors.New("boom")
		r := io.MultiReader(strings.NewReader("abc"), errReader{boom})
		if err := s.Put(ctx, "k", r, 10); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if _, err := s.Stat(ctx, "k"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("object exists: %v", err)
		}
	})

	t.Run("cancelled context stops a put mid stream", func(t *testing.T) {
		s := mk()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		const total = 8 << 20
		var read int
		r := readFunc(func(p []byte) (int, error) {
			if read >= total {
				return 0, io.EOF
			}
			n := len(p)
			if n > total-read {
				n = total - read
			}
			read += n
			if read >= 1<<20 {
				cancel() // cancel after the first MiB went by
			}
			return n, nil
		})
		err := s.Put(cctx, "k", r, total)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if read >= total {
			t.Fatalf("put read the whole body (%d) despite cancel", read)
		}
		if _, err := s.Stat(ctx, "k"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("object exists after cancelled put: %v", err)
		}
	})

	t.Run("already cancelled context", func(t *testing.T) {
		s := mk()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Put(cctx, "k", strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("concurrent puts and gets", func(t *testing.T) {
		s := mk()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i%3)
				body := bytes.Repeat([]byte{byte(i)}, 100000)
				_ = s.Put(ctx, key, bytes.NewReader(body), int64(len(body)))
				if rc, _, err := s.Get(ctx, key); err == nil {
					_, _ = io.Copy(io.Discard, rc)
					rc.Close()
				}
				_, _ = s.Stat(ctx, key)
				_ = s.Delete(ctx, key)
			}(i)
		}
		wg.Wait()
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// --- offline tests for the S3 glue ---

func TestNewS3Validation(t *testing.T) {
	ok := S3Config{Endpoint: "minio-minio-1:9000", Bucket: "placeholder-bucket", AccessKey: "PLACEHOLDER-ak", SecretKey: "PLACEHOLDER-not-a-secret-1"}
	if _, err := NewS3(ok); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, mut := range map[string]func(*S3Config){
		"empty endpoint": func(c *S3Config) { c.Endpoint = "" },
		"empty bucket":   func(c *S3Config) { c.Bucket = "" },
		"bad scheme":     func(c *S3Config) { c.Endpoint = "ftp://hv1:9000" },
		"path in url":    func(c *S3Config) { c.Endpoint = "http://hv1:9000/x" },
		"empty host":     func(c *S3Config) { c.Endpoint = "http://" },
		"empty access":   func(c *S3Config) { c.AccessKey = "" },
		"empty secret":   func(c *S3Config) { c.SecretKey = "" },
	} {
		c := ok
		mut(&c)
		if _, err := NewS3(c); err == nil {
			t.Errorf("%s: want error", name)
		} else if strings.Contains(err.Error(), ok.SecretKey) {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in     string
		host   string
		secure bool
		bad    bool
	}{
		{"minio-minio-1:9000", "minio-minio-1:9000", false, false},
		{"http://hv1:9000", "hv1:9000", false, false},
		{"https://hv1:9000", "hv1:9000", true, false},
		{"https://hv1:9000/", "hv1:9000", true, false},
		{"", "", false, true},
		{"  ", "", false, true},
		{"gopher://hv1", "", false, true},
	} {
		host, secure, err := parseEndpoint(tc.in)
		if (err != nil) != tc.bad || host != tc.host || secure != tc.secure {
			t.Errorf("parseEndpoint(%q) = %q, %v, %v", tc.in, host, secure, err)
		}
	}
}

func TestMapErr(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NoSuchBucket", "NoSuchVersion"} {
		err := mapErr(minio.ErrorResponse{Code: code, StatusCode: 404})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s -> %v, want ErrNotFound", code, err)
		}
	}
	if err := mapErr(minio.ErrorResponse{Code: "NotFound", StatusCode: 404}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bare 404 (HEAD) -> %v", err)
	}
	other := minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}
	if err := mapErr(other); errors.Is(err, ErrNotFound) || err == nil {
		t.Errorf("AccessDenied -> %v", err)
	}
	if mapErr(nil) != nil {
		t.Error("nil must map to nil")
	}
	wrapped := fmt.Errorf("x: %w", minio.ErrorResponse{Code: "NoSuchKey"})
	if !errors.Is(mapErr(wrapped), ErrNotFound) {
		t.Error("wrapped ErrorResponse not mapped")
	}
}

func TestFixedReader(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		fr := newFixedReader(context.Background(), strings.NewReader("hello"), 5)
		got, err := io.ReadAll(fr)
		if err != nil || string(got) != "hello" || fr.short {
			t.Fatalf("got %q %v short=%v", got, err, fr.short)
		}
	})
	t.Run("longer source is cut at size", func(t *testing.T) {
		fr := newFixedReader(context.Background(), strings.NewReader("hello world"), 5)
		got, err := io.ReadAll(fr)
		if err != nil || string(got) != "hello" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("short", func(t *testing.T) {
		fr := newFixedReader(context.Background(), strings.NewReader("abc"), 5)
		_, err := io.ReadAll(fr)
		if !errors.Is(err, ErrShortBody) || !fr.short {
			t.Fatalf("err = %v short=%v", err, fr.short)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fr := newFixedReader(ctx, strings.NewReader("abc"), 3)
		if _, err := fr.Read(make([]byte, 3)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}
