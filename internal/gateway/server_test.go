package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"

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
