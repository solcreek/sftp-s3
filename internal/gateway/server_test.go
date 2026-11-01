package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func TestNewValidatesConfig(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	valid := func() Config {
		return Config{Client: &s3.Client{}, Bucket: "b", Users: map[string]User{"u": {Password: "p"}}, HostKey: signer}
	}
	tests := map[string]func(*Config){
		"client":                   func(c *Config) { c.Client = nil },
		"bucket":                   func(c *Config) { c.Bucket = "" },
		"user":                     func(c *Config) { c.Users = nil },
		"MaxConns":                 func(c *Config) { c.MaxConns = -1 },
		"no password, hash or key": func(c *Config) { c.Users = map[string]User{"u": {}} },
		"both a password and a password hash": func(c *Config) {
			c.Users = map[string]User{"u": {Password: "p", PasswordHash: "$2a$04$abcdefghijklmnopqrstuuJ5Qo3b1T6p2Q9xWZ9b2nE0m5n7lWcm"}}
		},
		"password hash:": func(c *Config) { c.Users = map[string]User{"u": {PasswordHash: "$2a$not-a-hash"}} },
		"host key":       func(c *Config) { c.HostKey = nil },
	}
	for missing, mutate := range tests {
		cfg := valid()
		mutate(&cfg)
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("New without %s: err = %v, want it to mention %q", missing, err, missing)
		}
	}

	srv, err := New(valid())
	if err != nil {
		t.Fatal(err)
	}
	if srv.cfg.PartSize != 8<<20 || srv.cfg.MaxReorder != 64<<20 {
		t.Fatalf("defaults: part %d, reorder %d", srv.cfg.PartSize, srv.cfg.MaxReorder)
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ user, pass string }{
		{"alice", "wrong"},
		{"alice", ""},
		{"alice", "bob-pw"}, // another user's password
		{"mallory", "mallory-pw"},
	} {
		if c, err := e.sshDial(tc.user, tc.pass); err == nil {
			c.Close()
			t.Errorf("login %s/%q succeeded", tc.user, tc.pass)
		}
	}
	c, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatalf("valid login failed: %v", err)
	}
	c.Close()
}

func newKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPasswordHashLogin(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("carol-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.Users["carol"] = User{PasswordHash: string(hash)} })
	if c, err := e.sshDial("carol", "wrong"); err == nil {
		c.Close()
		t.Fatal("wrong password accepted")
	}
	if c, err := e.sshDial("carol", string(hash)); err == nil {
		c.Close()
		t.Fatal("the hash itself was accepted as the password")
	}
	c, err := e.sshDial("carol", "carol-pw")
	if err != nil {
		t.Fatalf("bcrypt login failed: %v", err)
	}
	c.Close()
}

func TestPublicKeyLogin(t *testing.T) {
	daveKey, otherKey := newKey(t), newKey(t)
	e := newEnv(t, func(c *Config) {
		c.Users["dave"] = User{Keys: []ssh.PublicKey{newKey(t).PublicKey(), daveKey.PublicKey()}}
	})

	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	c, err := e.sshDialKey("dave", daveKey)
	if err != nil {
		t.Fatalf("key login failed: %v", err)
	}
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "auth="+ssh.FingerprintSHA256(daveKey.PublicKey())) {
		if time.Now().After(deadline) {
			t.Fatalf("login log does not name the key:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	slog.SetDefault(prev)

	for name, dial := range map[string]func() (*ssh.Client, error){
		"another key":             func() (*ssh.Client, error) { return e.sshDialKey("dave", otherKey) },
		"dave's key as alice":     func() (*ssh.Client, error) { return e.sshDialKey("alice", daveKey) },
		"password for a key-only": func() (*ssh.Client, error) { return e.sshDial("dave", "") },
	} {
		if c, err := dial(); err == nil {
			c.Close()
			t.Errorf("%s: login accepted", name)
		}
	}
}

func TestFailedLoginIsDelayed(t *testing.T) {
	const delay = 300 * time.Millisecond
	e := newEnv(t, func(c *Config) { c.AuthFailDelay = delay })

	start := time.Now()
	if c, err := e.sshDial("alice", "wrong"); err == nil {
		c.Close()
		t.Fatal("wrong password accepted")
	}
	if took := time.Since(start); took < delay {
		t.Fatalf("failed login answered after %v, want at least %v", took, delay)
	}

	start = time.Now()
	c, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if took := time.Since(start); took >= delay {
		t.Errorf("successful login took %v; only failures should be delayed", took)
	}
}

func TestSilentConnectionsAreClosedAfterLoginTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LoginTimeout = 200 * time.Millisecond })
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetReadDeadline(time.Now().Add(5 * time.Second)) // test guard only

	start := time.Now()
	_, err = io.Copy(io.Discard, raw) // read the banner, then wait for the close
	if err != nil {
		t.Fatalf("server did not close the silent connection: %v", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Fatalf("closed after %v, before the login timeout", took)
	}
}

func TestLoggedInSessionsOutliveTheLoginTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LoginTimeout = 200 * time.Millisecond })
	c := e.sftp("alice")
	time.Sleep(400 * time.Millisecond)
	if _, err := c.ReadDir("/"); err != nil {
		t.Fatalf("session closed by the login timeout: %v", err)
	}
}

func TestConnectionLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxConns = 2 })
	var open []*ssh.Client
	for range 2 {
		c, err := e.sshDial("alice", "alice-pw")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		open = append(open, c)
	}
	if c, err := e.sshDial("alice", "alice-pw"); err == nil {
		c.Close()
		t.Fatal("third connection accepted with MaxConns = 2")
	}

	open[0].Close() // frees a slot once the server notices
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := e.sshDial("alice", "alice-pw")
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not released after a connection closed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConnectionLimitLogIsThrottled(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t, func(c *Config) { c.MaxConns = 1 })
	held, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for range 5 {
		if c, err := e.sshDial("alice", "alice-pw"); err == nil {
			c.Close()
			t.Fatal("connection accepted past the limit")
		}
	}
	if n := strings.Count(buf.String(), "connection limit reached"); n != 1 {
		t.Fatalf("logged the limit %d times for 5 rejections within %v, want 1:\n%s", n, limitWarnEvery, buf.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the server's goroutines to log into.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestOnlyTheSFTPSubsystemIsServed(t *testing.T) {
	e := newEnv(t)
	conn, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Run("cat /etc/passwd"); err == nil {
		t.Fatal("exec request was accepted")
	}
	sess.Close()

	sess, err = conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("shell"); err == nil {
		t.Fatal(`subsystem "shell" was accepted`)
	}
	sess.Close()

	if _, _, err := conn.OpenChannel("direct-tcpip", nil); err == nil {
		t.Fatal("port-forwarding channel was accepted")
	} else if oce := new(ssh.OpenChannelError); !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType {
		t.Fatalf("direct-tcpip rejected with %v, want UnknownChannelType", err)
	}

	// The same connection still serves sftp afterwards.
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp after rejected requests: %v", err)
	}
	defer c.Close()
	if _, err := c.ReadDir("/"); err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeFailureDoesNotStopTheServer(t *testing.T) {
	e := newEnv(t)
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	raw.Write([]byte("GET / HTTP/1.1\r\n\r\n")) // not SSH
	raw.Close()

	e.sftp("alice") // still accepting
}

func TestHandshakeLogLevel(t *testing.T) {
	tests := map[string]struct {
		err  error
		want slog.Level
	}{
		"closed before handshake (health check)": {io.EOF, slog.LevelDebug},
		"wrapped EOF":                            {fmt.Errorf("ssh: %w", io.EOF), slog.LevelDebug},
		"reset after our banner (Linux)":         {&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, slog.LevelDebug},
		"bad password":                           {errors.New("ssh: no auth passed yet"), slog.LevelInfo},
	}
	for name, tt := range tests {
		if got := handshakeLogLevel(tt.err); got != tt.want {
			t.Errorf("%s: level %v, want %v", name, got, tt.want)
		}
	}
}

func TestHealthCheckConnectionsAreNotLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t)
	raw, err := net.Dial("tcp", e.addr) // what an NLB TCP health check does
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	e.sftp("alice") // a later login proves the probe was handled

	if strings.Contains(buf.String(), "handshake failed") {
		t.Fatalf("health check logged at info:\n%s", buf.String())
	}
}

func TestIsSFTP(t *testing.T) {
	payload := func(s string) []byte { return ssh.Marshal(struct{ Name string }{s}) }
	if !isSFTP(payload("sftp")) {
		t.Error(`isSFTP("sftp") = false`)
	}
	for _, p := range [][]byte{payload("sftpx"), payload(""), nil, {0, 0, 0, 9, 's'}} {
		if isSFTP(p) {
			t.Errorf("isSFTP(%q) = true", p)
		}
	}
}

type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("accept: too many open files")
}

func TestServeReturnsAcceptErrors(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	srv, err := New(Config{Client: &s3.Client{}, Bucket: "b", Users: map[string]User{"u": {Password: "p"}}, HostKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(failingListener{}); err == nil || !strings.Contains(err.Error(), "too many open files") {
		t.Fatalf("Serve = %v, want the accept error", err)
	}
}
