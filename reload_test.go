package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs sends the default logger to a buffer for this test.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadUsers(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	signer, err := ssh.ParsePrivateKey([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(bucket string) (gateway.Config, *gateway.Server) {
		cfg := gateway.Config{Client: &s3.Client{}, Bucket: bucket, Prefix: "p/", HostKeys: []ssh.Signer{signer},
			Users: map[string]gateway.User{"alice": {Password: "secret-alice-pw"}}}
		if bucket == "" {
			cfg.Users["alice"] = gateway.User{Password: "secret-alice-pw", Bucket: "own"}
		}
		srv, err := gateway.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, srv
	}
	dir := t.TempDir()
	usersFile := filepath.Join(dir, "users")
	env := map[string]string{"SFTP_USERS_FILE": usersFile}

	t.Run("line format", func(t *testing.T) {
		logs := captureLogs(t)
		cfg, srv := newServer("b")
		writeFile(t, usersFile, "alice:secret-alice-pw\nbob:$2a$10$abcdefghijklmnopqrstuuJ5Qo3b1T6p2Q9xWZ9b2nE0m5n7lWcm\n")
		n, err := reloadUsers(envMap(env), cfg, srv)
		if err != nil || n != 2 {
			t.Fatalf("reloadUsers = %d, %v; want 2 users", n, err)
		}
		if !strings.Contains(logs.String(), "plaintext password") || !strings.Contains(logs.String(), "user=alice") {
			t.Errorf("no plaintext warning for alice:\n%s", logs)
		}
		if strings.Contains(logs.String(), "secret-alice-pw") {
			t.Errorf("log shows a password:\n%s", logs)
		}
	})

	t.Run("JSON format uses S3_BUCKET and S3_PREFIX", func(t *testing.T) {
		logs := captureLogs(t)
		cfg, srv := newServer("b")
		writeFile(t, usersFile, `{"users": [{"username": "carol", "password_hash": "$2a$10$abcdefghijklmnopqrstuuJ5Qo3b1T6p2Q9xWZ9b2nE0m5n7lWcm"}]}`)
		if n, err := reloadUsers(envMap(env), cfg, srv); err != nil || n != 1 {
			t.Fatalf("reloadUsers = %d, %v; want 1 user", n, err)
		}
		if strings.Contains(logs.String(), "plaintext") {
			t.Errorf("plaintext warning for a hashed user:\n%s", logs)
		}
	})

	for name, tc := range map[string]struct {
		env    map[string]string
		bucket string
		body   string
		want   string
	}{
		"only SFTP_USERS":     {env: map[string]string{"SFTP_USERS": "alice:pw"}, bucket: "b", want: "SFTP_USERS"},
		"missing file":        {env: map[string]string{"SFTP_USERS_FILE": filepath.Join(dir, "missing")}, bucket: "b", want: "SFTP_USERS_FILE: open"},
		"unparsable file":     {env: env, bucket: "b", body: "alice\n", want: "is not user:credential"},
		"bad JSON":            {env: env, bucket: "b", body: `{"users": [{"username": "a", "password": "secret-pw"}]}`, want: "plaintext"},
		"no bucket for users": {env: env, bucket: "", body: "dave:secret-dave-pw\n", want: "no default bucket"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, srv := newServer(tc.bucket)
			if tc.body != "" {
				writeFile(t, usersFile, tc.body)
			}
			n, err := reloadUsers(envMap(tc.env), cfg, srv)
			if err == nil || n != 0 || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reloadUsers = %d, %v; want an error mentioning %q", n, err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error shows a password: %v", err)
			}
		})
	}
}

func TestLogReload(t *testing.T) {
	for _, tc := range []struct {
		n    int
		err  error
		want string
	}{
		{3, nil, `level=INFO msg="SIGHUP: users reloaded" users=3`},
		{0, errNoUsersFile, `level=WARN msg="SIGHUP: nothing to reload"`},
		{0, errors.New("users file: boom"), `level=ERROR msg="SIGHUP: users not reloaded; keeping the current users" err="users file: boom"`},
	} {
		logs := captureLogs(t)
		logReload(tc.n, tc.err)
		if !strings.Contains(logs.String(), tc.want) {
			t.Errorf("log = %q, want it to contain %q", logs, tc.want)
		}
	}
}

// waitFor polls cond until it holds, for up to 10 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
