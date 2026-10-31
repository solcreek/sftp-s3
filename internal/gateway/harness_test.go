package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
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
	prefix string
}

type envOpt func(*Config)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(testBucket); err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(fake.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(fake.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})

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
		Bucket:   testBucket,
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
	return &env{t: t, s3: client, addr: ln.Addr().String(), hostPK: signer.PublicKey(), prefix: cfg.Prefix}
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

// object returns the S3 object body at key, or ok=false if it does not exist.
func (e *env) object(key string) (body []byte, ok bool) {
	e.t.Helper()
	out, err := e.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(testBucket), Key: aws.String(key)})
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

// put writes an object straight to S3, bypassing the gateway.
func (e *env) put(key string, body []byte) {
	e.t.Helper()
	_, err := e.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testBucket), Key: aws.String(key), Body: bytesReader(body)})
	if err != nil {
		e.t.Fatal(err)
	}
}
