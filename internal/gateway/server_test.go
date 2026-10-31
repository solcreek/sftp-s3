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
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func TestNewValidatesConfig(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	valid := func() Config {
		return Config{Client: &s3.Client{}, Bucket: "b", Users: map[string]string{"u": "p"}, HostKey: signer}
	}
	tests := map[string]func(*Config){
		"client":   func(c *Config) { c.Client = nil },
		"bucket":   func(c *Config) { c.Bucket = "" },
		"user":     func(c *Config) { c.Users = nil },
		"host key": func(c *Config) { c.HostKey = nil },
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
	srv, err := New(Config{Client: &s3.Client{}, Bucket: "b", Users: map[string]string{"u": "p"}, HostKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(failingListener{}); err == nil || !strings.Contains(err.Error(), "too many open files") {
		t.Fatalf("Serve = %v, want the accept error", err)
	}
}
