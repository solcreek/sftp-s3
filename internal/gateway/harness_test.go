package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/s3client"
)

const testBucket = "test-bucket"

func TestMain(m *testing.M) {
	// Keep test output readable; the server logs every login and upload.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// env is one gateway serving on a loopback port, backed by an in-memory S3.
type env struct {
	t      *testing.T
	s3     *s3.Client
	addr   string
	hostPK ssh.PublicKey
	seen   *headerLog // request headers the store received; in-memory store only
	bucket string
	base   string // key prefix this test owns; "" on the in-memory store
}

type envOpt func(*Config)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	client, bucket, base := conformanceStore(t)
	var seen *headerLog
	if client == nil {
		client, bucket, seen = memoryStore(t)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Client:   client,
		Bucket:   bucket,
		Prefix:   base,
		Users:    map[string]string{"alice": "alice-pw", "bob": "bob-pw"},
		HostKey:  signer,
		PartSize: 5 << 20, // S3's minimum, so multi-part paths run on small files
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ln.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return &env{t: t, s3: client, addr: ln.Addr().String(), hostPK: signer.PublicKey(), seen: seen, bucket: bucket, base: base}
}

func (e *env) sshDial(user, pass string) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK),
	})
}

// sftp logs in and returns a client; the connection closes with the test.
func (e *env) sftp(user string, opts ...sftp.ClientOption) *sftp.Client {
	e.t.Helper()
	conn, err := e.sshDial(user, user+"-pw")
	if err != nil {
		e.t.Fatalf("ssh dial as %s: %v", user, err)
	}
	c, err := sftp.NewClient(conn, opts...)
	if err != nil {
		conn.Close()
		e.t.Fatalf("sftp client: %v", err)
	}
	e.t.Cleanup(func() { c.Close(); conn.Close() })
	return c
}

// object returns the body of the object at key (relative to the test's
// base prefix), or ok=false if it does not exist.
func (e *env) object(key string) (body []byte, ok bool) {
	e.t.Helper()
	out, err := e.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: &e.bucket, Key: aws.String(e.base + key)})
	if err != nil {
		return nil, false
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return b, true
}

// put writes an object (key relative to the base prefix) straight to the
// store, bypassing the gateway.
func (e *env) put(key string, body []byte) {
	e.t.Helper()
	_, err := e.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: &e.bucket, Key: aws.String(e.base + key), Body: bytesReader(body)})
	if err != nil {
		e.t.Fatal(err)
	}
}

// headerLog records which request header names a store has received.
type headerLog struct {
	mu    sync.Mutex
	names map[string]bool
}

func (h *headerLog) any(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n := range h.names {
		if strings.HasPrefix(strings.ToLower(n), prefix) {
			return true
		}
	}
	return false
}

// memoryStore starts an in-memory S3 (gofakes3) for this test.
func memoryStore(t *testing.T) (*s3.Client, string, *headerLog) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(testBucket); err != nil {
		t.Fatal(err)
	}
	seen := &headerLog{names: map[string]bool{}}
	handler := gofakes3.New(backend).Server()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		for n := range r.Header {
			seen.names[n] = true
		}
		seen.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fake.Close)
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(fake.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	}), testBucket, seen
}

// runID keeps concurrent conformance runs against one bucket apart.
var runID = time.Now().UTC().Format("20060102T150405") + "-" + strconv.Itoa(os.Getpid())

// conformanceStore returns a client for a real store when CONFORMANCE_BUCKET
// is set, built exactly as production builds it (internal/s3client), so the
// suite proves the settings a deployment would use:
//
//	CONFORMANCE_BUCKET    bucket to use (required to enable)
//	CONFORMANCE_ENDPOINT  S3_ENDPOINT equivalent; empty means AWS
//	CONFORMANCE_REGION, CONFORMANCE_PATH_STYLE, CONFORMANCE_CHECKSUM
//
// Credentials come from the SDK chain (AWS_PROFILE, AWS_ACCESS_KEY_ID, ...).
// Each test owns sftp-s3-conformance/<run>/<test>/ and deletes it afterwards.
func conformanceStore(t *testing.T) (*s3.Client, string, string) {
	t.Helper()
	bucket := os.Getenv("CONFORMANCE_BUCKET")
	if bucket == "" {
		return nil, "", ""
	}
	client, _, err := s3client.New(context.Background(), s3client.Options{
		Endpoint:  os.Getenv("CONFORMANCE_ENDPOINT"),
		Region:    os.Getenv("CONFORMANCE_REGION"),
		PathStyle: os.Getenv("CONFORMANCE_PATH_STYLE"),
		Checksum:  os.Getenv("CONFORMANCE_CHECKSUM"),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "sftp-s3-conformance/" + runID + "/" + strings.ReplaceAll(t.Name(), "/", "_") + "/"
	t.Cleanup(func() { deletePrefix(t, client, bucket, base) })
	return client, bucket, base
}

// deletePrefix removes every object and unfinished multipart upload under
// prefix, one request per object: multi-object delete is not available on
// every compatible store (GCS's XML API lacks it).
func deletePrefix(t *testing.T, c *s3.Client, bucket, prefix string) {
	ctx := context.Background()
	pg := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			t.Errorf("cleanup list %s: %v", prefix, err)
			return
		}
		for _, o := range out.Contents {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: o.Key}); err != nil {
				t.Errorf("cleanup delete %s: %v", *o.Key, err)
			}
		}
	}
	mu, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &bucket, Prefix: &prefix})
	if err != nil {
		return // not every store lists uploads; a lifecycle rule is the backstop
	}
	for _, u := range mu.Uploads {
		c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: u.Key, UploadId: u.UploadId})
	}
}
