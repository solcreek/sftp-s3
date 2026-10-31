package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
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

func TestHostKey(t *testing.T) {
	keyPEM, pub := testKeyPEM(t)
	s, err := hostKey(keyPEM)
	if err != nil {
		t.Fatalf("hostKey(valid PEM): %v", err)
	}
	if string(s.PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("hostKey returned a different key than the one supplied")
	}

	if _, err := hostKey("not a key"); err == nil || !strings.Contains(err.Error(), "SFTP_HOST_KEY") {
		t.Fatalf("hostKey(garbage) = %v, want an SFTP_HOST_KEY error", err)
	}

	a, err := hostKey("")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hostKey("")
	if a.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("ephemeral key type %s, want ed25519", a.PublicKey().Type())
	}
	if string(a.PublicKey().Marshal()) == string(b.PublicKey().Marshal()) {
		t.Fatal("two ephemeral keys are identical")
	}
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
	if cfg.Bucket != "uploads" || cfg.Prefix != "sftp/" || cfg.Users["alice"] != "pw" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if string(cfg.HostKey.PublicKey().Marshal()) != string(pub.Marshal()) {
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
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.Client.Options(); o.Region != "auto" || o.UsePathStyle ||
		o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenSupported || cfg.PartSize != 16<<20 {
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

func TestParseUsers(t *testing.T) {
	got, err := parseUsers(" alice:s3cret , bob:pa:ss ,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["alice"] != "s3cret" || got["bob"] != "pa:ss" {
		t.Fatalf("parseUsers = %v", got)
	}

	for in, want := range map[string]string{
		"":                  "SFTP_USERS must be",
		" , ":               "SFTP_USERS must be",
		"alice":             "not user:password",
		"alice:":            "not user:password",
		":pw":               "not user:password",
		"alice:pa,ss":       "not user:password", // comma inside a password
		"../evil:pw":        "may not contain",
		"a.b:pw":            "may not contain",
		"alice:one,alice:2": "listed twice",
	} {
		if _, err := parseUsers(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseUsers(%q) = %v, want error containing %q", in, err, want)
		}
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "2222") != "2222" || orDefault("22", "2222") != "22" {
		t.Fatal("orDefault")
	}
}
