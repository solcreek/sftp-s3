// Command sftp-s3 is an SFTP server whose filesystem is an S3 bucket.
//
// Each user is confined to s3://$S3_BUCKET/$S3_PREFIX<user>/.
// Uploads stream straight into an S3 multipart upload; nothing touches disk.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
	"github.com/solcreek/sftp-s3/internal/s3client"
)

// version is set at release time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if code, done := parseFlags(os.Args[1:], os.Stdout, os.Stderr); done {
		os.Exit(code)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	// Deploys send SIGTERM: stop accepting, let in-flight uploads finish.
	// The platform's kill timeout bounds how long that can take.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// parseFlags handles the command line. Configuration itself is environment
// variables only; done reports that the process should exit with code.
func parseFlags(args []string, stdout, stderr io.Writer) (code int, done bool) {
	fs := flag.NewFlagSet("sftp-s3", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: sftp-s3 [-version]\n\nAn SFTP server backed by S3, configured through environment variables:\nhttps://github.com/solcreek/sftp-s3#configuration\n\n")
		fs.PrintDefaults()
	}
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2, true
	}
	if *showVersion {
		fmt.Fprintln(stdout, "sftp-s3", version)
		return 0, true
	}
	return 0, false
}

func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := loadConfig(ctx, getenv)
	if err != nil {
		return err
	}
	srv, err := gateway.New(cfg)
	if err != nil {
		return err
	}
	addr := ":" + orDefault(getenv("PORT"), "2222")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	slog.Info("listening", "version", version, "addr", ln.Addr().String(), "bucket", cfg.Bucket,
		"fingerprint", ssh.FingerprintSHA256(cfg.HostKey.PublicKey()))
	go func() {
		<-ctx.Done()
		slog.Info("shutting down; waiting for open sessions")
		ln.Close()
	}()
	return srv.Serve(ln)
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// loadConfig builds the gateway config from environment variables:
// S3_BUCKET, SFTP_USERS, and optionally S3_PREFIX, SFTP_HOST_KEY,
// S3_PART_SIZE_MB, SFTP_MAX_CONNECTIONS, and the store settings S3_ENDPOINT, S3_REGION,
// S3_PATH_STYLE, S3_CHECKSUM (see internal/s3client). Credentials come from
// the AWS SDK's default chain (AWS_ACCESS_KEY_ID/..., profiles, IAM roles).
func loadConfig(ctx context.Context, getenv func(string) string) (gateway.Config, error) {
	var cfg gateway.Config
	cfg.Bucket = getenv("S3_BUCKET")
	if cfg.Bucket == "" {
		return cfg, errors.New("S3_BUCKET is required")
	}
	cfg.Prefix = getenv("S3_PREFIX")
	var err error
	passwords, err := parseUsers(getenv("SFTP_USERS"))
	if err != nil {
		return cfg, err
	}
	cfg.Users = make(map[string]gateway.User, len(passwords))
	for name, pw := range passwords {
		cfg.Users[name] = gateway.User{Password: pw}
	}
	if cfg.HostKey, err = hostKey(getenv("SFTP_HOST_KEY")); err != nil {
		return cfg, err
	}
	if mb := getenv("S3_PART_SIZE_MB"); mb != "" {
		n, err := strconv.Atoi(mb)
		if err != nil || n < 5 || n > 5120 {
			return cfg, fmt.Errorf("S3_PART_SIZE_MB: %q must be an integer from 5 to 5120", mb)
		}
		cfg.PartSize = int64(n) << 20
	}
	if v := getenv("SFTP_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return cfg, fmt.Errorf("SFTP_MAX_CONNECTIONS: %q must be an integer from 1 to 10000", v)
		}
		cfg.MaxConns = n
	}
	client, st, err := s3client.New(ctx, s3client.Options{
		Endpoint:  getenv("S3_ENDPOINT"),
		Region:    getenv("S3_REGION"),
		PathStyle: getenv("S3_PATH_STYLE"),
		Checksum:  getenv("S3_CHECKSUM"),
	})
	if err != nil {
		return cfg, err
	}
	cfg.Client = client
	slog.Info("s3", "endpoint", orDefault(st.Endpoint, "aws"), "region", st.Region,
		"path_style", st.PathStyle, "checksum", st.Checksum)
	return cfg, nil
}

// parseUsers parses "alice:secret,bob:hunter2". Any malformed entry is an
// error rather than skipped, so a typo cannot silently lock a user out or
// truncate a password containing a comma. Names may not contain "/" or "."
// since they become S3 key prefixes.
func parseUsers(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		u, p, ok := strings.Cut(pair, ":")
		switch {
		case !ok || u == "" || p == "":
			return nil, fmt.Errorf("SFTP_USERS: entry for %q is not user:password", u)
		case strings.ContainsAny(u, "/."):
			return nil, fmt.Errorf("SFTP_USERS: user %q may not contain '/' or '.'", u)
		case m[u] != "":
			return nil, fmt.Errorf("SFTP_USERS: user %q is listed twice", u)
		}
		m[u] = p
	}
	if len(m) == 0 {
		return nil, errors.New("SFTP_USERS must be user:pass[,user:pass...]")
	}
	return m, nil
}

// hostKey parses a PEM private key, or generates an ephemeral ed25519 key when
// pemData is empty. Production must pin a key: an ephemeral one changes on
// every deploy and clients refuse to connect ("HOST IDENTIFICATION HAS CHANGED").
func hostKey(pemData string) (ssh.Signer, error) {
	if pemData != "" {
		s, err := ssh.ParsePrivateKey([]byte(pemData))
		if err != nil {
			return nil, fmt.Errorf("SFTP_HOST_KEY: %w", err)
		}
		return s, nil
	}
	slog.Warn("SFTP_HOST_KEY not set; generating an ephemeral ed25519 host key")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(pem.EncodeToMemory(blk))
}
