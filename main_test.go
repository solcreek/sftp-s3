package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// isolateAWS keeps the SDK's default chain from reading the developer's
// ~/.aws files or real credentials.
func isolateAWS(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func testKeyPEM(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk)), sshPub
}

func TestLoadConfig(t *testing.T) {
	isolateAWS(t)
	keyPEM, pub := testKeyPEM(t)
	cfg, err := loadConfig(context.Background(), envMap(map[string]string{
		"S3_BUCKET":     "uploads",
		"S3_PREFIX":     "sftp/",
		"S3_ENDPOINT":   "http://127.0.0.1:9000",
		"SFTP_USERS":    "alice:pw",
		"SFTP_HOST_KEY": keyPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "uploads" || cfg.Prefix != "sftp/" || cfg.Users["alice"].Password != "pw" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if string(cfg.HostKeys[0].PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("host key not taken from SFTP_HOST_KEY")
	}
	opts := cfg.Client.Options()
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "http://127.0.0.1:9000" || !opts.UsePathStyle {
		t.Fatalf("S3_ENDPOINT not applied: endpoint %v, path style %v", opts.BaseEndpoint, opts.UsePathStyle)
	}

	if opts.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatal("custom endpoint did not get compatible checksum mode")
	}
	if cfg.PartSize != 0 {
		t.Fatalf("PartSize = %d, want 0 (gateway default)", cfg.PartSize)
	}

	cfg, err = loadConfig(context.Background(), envMap(map[string]string{
		"S3_BUCKET": "b", "SFTP_USERS": "a:b", "S3_ENDPOINT": "https://x.example",
		"S3_REGION": "auto", "S3_PATH_STYLE": "false", "S3_CHECKSUM": "when_supported", "S3_PART_SIZE_MB": "16",
		"SFTP_MAX_CONNECTIONS": "4", "SFTP_PROXY_PROTOCOL": "on",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.Client.Options(); o.Region != "auto" || o.UsePathStyle ||
		o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenSupported || cfg.PartSize != 16<<20 || cfg.MaxConns != 4 || !cfg.ProxyProtocol {
		t.Fatalf("overrides not applied: region %q path %v checksum %v part %d", o.Region, o.UsePathStyle, o.RequestChecksumCalculation, cfg.PartSize)
	}

	// Without S3_ENDPOINT the client talks to AWS with virtual-hosted style.
	cfg, err = loadConfig(context.Background(), envMap(map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b"}))
	if err != nil {
		t.Fatal(err)
	}
	if opts := cfg.Client.Options(); opts.BaseEndpoint != nil || opts.UsePathStyle {
		t.Fatalf("unexpected endpoint override: %v, path style %v", opts.BaseEndpoint, opts.UsePathStyle)
	}
}

func TestLoadConfigFromFiles(t *testing.T) {
	isolateAWS(t)
	dir := t.TempDir()
	keyPEM, pub := testKeyPEM(t)
	authKey, _ := authorizedKey(t, "a comment, with a comma")
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := loadConfig(context.Background(), envMap(map[string]string{
		"S3_BUCKET":          "b",
		"S3_REGION":          "us-east-1",
		"SFTP_USERS_FILE":    write("users", "alice:"+authKey+"\nbob:pw\n"),
		"SFTP_HOST_KEY_FILE": write("host_key", keyPEM),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Users["alice"].Keys) != 1 || cfg.Users["bob"].Password != "pw" {
		t.Fatalf("users = %+v", cfg.Users)
	}
	if string(cfg.HostKeys[0].PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("host key not read from SFTP_HOST_KEY_FILE")
	}

	for name, env := range map[string]map[string]string{
		"set SFTP_USERS or SFTP_USERS_FILE, not both": {"SFTP_USERS": "a:b", "SFTP_USERS_FILE": write("u2", "a:b")},
		"SFTP_USERS_FILE: open":                       {"SFTP_USERS_FILE": filepath.Join(dir, "missing")},
		"SFTP_HOST_KEY_FILE: open":                    {"SFTP_USERS": "a:b", "SFTP_HOST_KEY_FILE": filepath.Join(dir, "missing")},
		"set SFTP_HOST_KEY or SFTP_HOST_KEY_FILE":     {"SFTP_USERS": "a:b", "SFTP_HOST_KEY": keyPEM, "SFTP_HOST_KEY_FILE": write("k2", keyPEM)},
	} {
		env["S3_BUCKET"], env["S3_REGION"] = "b", "us-east-1"
		if _, err := loadConfig(context.Background(), envMap(env)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to mention %q", err, name)
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	isolateAWS(t)
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no bucket", map[string]string{"SFTP_USERS": "a:b"}, "S3_BUCKET"},
		{"no users", map[string]string{"S3_BUCKET": "b"}, "SFTP_USERS"},
		{"bad host key", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "SFTP_HOST_KEY": "x"}, "SFTP_HOST_KEY"},
		{"part size too small", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "S3_PART_SIZE_MB": "4"}, "S3_PART_SIZE_MB"},
		{"part size not a number", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "S3_PART_SIZE_MB": "8M"}, "S3_PART_SIZE_MB"},
		{"zero connections", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "SFTP_MAX_CONNECTIONS": "0"}, "SFTP_MAX_CONNECTIONS"},
		{"connections not a number", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "SFTP_MAX_CONNECTIONS": "many"}, "SFTP_MAX_CONNECTIONS"},
		{"bad proxy protocol", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "SFTP_PROXY_PROTOCOL": "v2"}, "SFTP_PROXY_PROTOCOL"},
		{"bad checksum mode", map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "S3_CHECKSUM": "off"}, "S3_CHECKSUM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadConfig(context.Background(), envMap(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	isolateAWS(t)
	base := map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b"}

	t.Run("invalid config", func(t *testing.T) {
		if err := run(context.Background(), envMap(map[string]string{})); err == nil {
			t.Fatal("run with no config succeeded")
		}
	})

	t.Run("port in use", func(t *testing.T) {
		// Same wildcard address run uses: on macOS, holding only 127.0.0.1:N
		// does not stop another process binding [::]:N.
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		env := map[string]string{"PORT": port}
		for k, v := range base {
			env[k] = v
		}
		if err := run(context.Background(), envMap(env)); err == nil {
			t.Fatal("run on a busy port succeeded")
		}
	})

	t.Run("stops when the context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		env := map[string]string{"PORT": "0"}
		for k, v := range base {
			env[k] = v
		}
		if err := run(ctx, envMap(env)); err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	})
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		args     []string
		check    bool
		code     int
		done     bool
		out, err string
	}{
		{args: nil, code: 0, done: false},
		{args: []string{"-check"}, check: true, code: 0, done: false},
		{args: []string{"-version"}, code: 0, done: true, out: "sftp-s3 dev\n"},
		{args: []string{"--version"}, code: 0, done: true, out: "sftp-s3 dev\n"},
		{args: []string{"-h"}, code: 0, done: true, err: "environment variables"},
		{args: []string{"-port", "22"}, code: 2, done: true, err: "flag provided but not defined"},
		{args: []string{"serve"}, code: 2, done: true, err: `unexpected argument "serve"`},
	}
	for _, tt := range tests {
		var out, errOut strings.Builder
		check, code, done := parseFlags(tt.args, strings.NewReader(""), &out, &errOut)
		if check != tt.check || code != tt.code || done != tt.done || out.String() != tt.out || !strings.Contains(errOut.String(), tt.err) {
			t.Errorf("parseFlags(%q) = %d, %v, stdout %q, stderr %q", tt.args, code, done, out.String(), errOut.String())
		}
	}
}

func TestHashPassword(t *testing.T) {
	var out, errOut strings.Builder
	_, code, done := parseFlags([]string{"-hash-password"}, strings.NewReader("correct horse\n"), &out, &errOut)
	if code != 0 || !done {
		t.Fatalf("exit %d, done %v, stderr %q", code, done, errOut.String())
	}
	hash := strings.TrimSpace(out.String())
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse")) != nil {
		t.Fatalf("%q is not a bcrypt hash of the input (trailing newline must be dropped)", hash)
	}
	users, err := parseUsers("alice:" + hash)
	if err != nil || users["alice"].PasswordHash != hash {
		t.Fatalf("hash not usable in SFTP_USERS: %v %+v", err, users)
	}

	out.Reset()
	errOut.Reset()
	if _, code, _ := parseFlags([]string{"-hash-password"}, strings.NewReader("\n"), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "empty password") {
		t.Fatalf("empty input: exit %d, stderr %q", code, errOut.String())
	}
}

func TestRunCheck(t *testing.T) {
	isolateAWS(t)
	backend := s3mem.New()
	if err := backend.CreateBucket("uploads"); err != nil {
		t.Fatal(err)
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	keyPEM, _ := testKeyPEM(t)
	authKey, _ := authorizedKey(t, "")
	env := map[string]string{
		"S3_BUCKET":     "uploads",
		"S3_ENDPOINT":   store.URL,
		"SFTP_USERS":    "alice:" + authKey + ",bob:pw",
		"SFTP_HOST_KEY": keyPEM,
	}

	var out strings.Builder
	if err := runCheck(context.Background(), envMap(env), &out); err != nil {
		t.Fatalf("check failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"users: 2 (1 keys, 0 hashed passwords, 1 plaintext passwords)", "host key: SHA256:", "bucket: uploads reachable", "ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	env["S3_BUCKET"] = "missing"
	if err := runCheck(context.Background(), envMap(env), io.Discard); err == nil || !strings.Contains(err.Error(), `bucket "missing"`) {
		t.Errorf("missing bucket: err = %v", err)
	}
	env["S3_BUCKET"], env["SFTP_USERS"] = "uploads", "carol:$2a$not-a-hash"
	if err := runCheck(context.Background(), envMap(env), io.Discard); err == nil || !strings.Contains(err.Error(), "password hash") {
		t.Errorf("bad hash: err = %v", err)
	}
	delete(env, "SFTP_USERS")
	if err := runCheck(context.Background(), envMap(env), io.Discard); err == nil || !strings.Contains(err.Error(), "SFTP_USERS") {
		t.Errorf("no users: err = %v", err)
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "2222") != "2222" || orDefault("22", "2222") != "22" {
		t.Fatal("orDefault")
	}
}

func TestResolveVersion(t *testing.T) {
	mod := func(v string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: v}} }
	for _, tc := range []struct {
		release string
		info    *debug.BuildInfo
		want    string
	}{
		{"0.1.0", mod("v0.1.0"), "0.1.0"},  // GoReleaser
		{"0.2.0", mod("(devel)"), "0.2.0"}, // ldflags win
		{"dev", mod("v0.1.0"), "0.1.0"},    // go install …@v0.1.0
		{"dev", mod("v0.1.1-0.20261102010203-abcdef123456+dirty"), "0.1.1-0.20261102010203-abcdef123456+dirty"},
		{"dev", mod("(devel)"), "dev"},
		{"dev", mod(""), "dev"},
		{"dev", nil, "dev"},
	} {
		if got := resolveVersion(tc.release, tc.info); got != tc.want {
			t.Errorf("resolveVersion(%q, %v) = %q, want %q", tc.release, tc.info, got, tc.want)
		}
	}
}

// A JSON users file can spread users over buckets with no S3_BUCKET at all;
// -check then reaches every one of them.
func TestRunCheckJSONUsers(t *testing.T) {
	isolateAWS(t)
	backend := s3mem.New()
	for _, b := range []string{"alpha", "beta"} {
		if err := backend.CreateBucket(b); err != nil {
			t.Fatal(err)
		}
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	keyPEM, _ := testKeyPEM(t)
	users := filepath.Join(t.TempDir(), "users.json")
	write := func(s string) {
		if err := os.WriteFile(users, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"users": [
	  {"username": "alice", "password_hash": "` + testHash + `", "bucket": "alpha", "prefix": ""},
	  {"username": "bob", "password_hash": "` + testHash + `", "bucket": "beta", "prefix": "in/"}]}`)
	env := map[string]string{"S3_ENDPOINT": store.URL, "SFTP_USERS_FILE": users, "SFTP_HOST_KEY": keyPEM}

	var out strings.Builder
	if err := runCheck(context.Background(), envMap(env), &out); err != nil {
		t.Fatalf("check failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"users: 2 (0 keys, 2 hashed passwords, 0 plaintext passwords)", "bucket: alpha reachable\nbucket: beta reachable\nok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	write(`{"users": [{"username": "carol", "password_hash": "` + testHash + `", "bucket": "gamma"}]}`)
	if err := runCheck(context.Background(), envMap(env), io.Discard); err == nil || !strings.Contains(err.Error(), `bucket "gamma"`) {
		t.Errorf("missing bucket: err = %v", err)
	}
	write(`{"users": [{"username": "carol", "password_hash": "` + testHash + `"}]}`)
	if err := runCheck(context.Background(), envMap(env), io.Discard); err == nil || !strings.Contains(err.Error(), "S3_BUCKET") {
		t.Errorf("no bucket anywhere: err = %v", err)
	}
}
