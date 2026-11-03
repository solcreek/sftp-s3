// Command sftp-s3 is an SFTP server whose filesystem is an S3 bucket.
//
// Each user is confined to s3://$S3_BUCKET/$S3_PREFIX<user>/.
// Uploads stream straight into an S3 multipart upload; nothing touches disk.
package main

import (
	"bufio"
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
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/solcreek/sftp-s3/internal/gateway"
	"github.com/solcreek/sftp-s3/internal/s3client"
)

// version is set at release time: -ldflags "-X main.version=1.2.3".
// Other builds fall back to the module version Go records in the binary,
// so `go install github.com/solcreek/sftp-s3@v1.2.3` reports 1.2.3.
var version = "dev"

func init() {
	info, _ := debug.ReadBuildInfo()
	version = resolveVersion(version, info)
}

// resolveVersion prefers the release version, then the module version.
// A build from a source checkout reports "(devel)" or a pseudo-version.
func resolveVersion(release string, info *debug.BuildInfo) string {
	if release != "dev" || info == nil {
		return release
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return release
	}
	return strings.TrimPrefix(v, "v")
}

func main() {
	check, code, done := parseFlags(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if done {
		os.Exit(code)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if check {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runCheck(ctx, os.Getenv, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "check failed:", err)
			os.Exit(1)
		}
		return
	}
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
// variables only; done reports that the process should exit with code, and
// check that it should validate the configuration instead of serving.
func parseFlags(args []string, stdin io.Reader, stdout, stderr io.Writer) (check bool, code int, done bool) {
	fs := flag.NewFlagSet("sftp-s3", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: sftp-s3 [-check | -version | -hash-password]\n\nAn SFTP server backed by S3, configured through environment variables:\nhttps://github.com/solcreek/sftp-s3#configuration\n\n")
		fs.PrintDefaults()
	}
	showVersion := fs.Bool("version", false, "print the version and exit")
	checkOnly := fs.Bool("check", false, "validate the configuration and reach the bucket, then exit")
	hashPassword := fs.Bool("hash-password", false, "read a password from stdin, print its bcrypt hash for SFTP_USERS, and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0, true
		}
		return false, 2, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return false, 2, true
	}
	if *showVersion {
		fmt.Fprintln(stdout, "sftp-s3", version)
		return false, 0, true
	}
	if *hashPassword {
		hash, err := hashFromInput(stdin, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "sftp-s3:", err)
			return false, 1, true
		}
		fmt.Fprintln(stdout, hash)
		return false, 0, true
	}
	return *checkOnly, 0, false
}

// hashFromInput reads one password, without echo when stdin is a terminal,
// and returns its bcrypt hash.
func hashFromInput(stdin io.Reader, stderr io.Writer) (string, error) {
	var pass []byte
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		p, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		pass = p
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pass = []byte(strings.TrimRight(line, "\r\n"))
	}
	if len(pass) == 0 {
		return "", errors.New("empty password")
	}
	hash, err := bcrypt.GenerateFromPassword(pass, bcrypt.DefaultCost)
	return string(hash), err
}

// runCheck loads the configuration as the server would, then checks that
// the bucket is reachable with the credentials found.
func runCheck(ctx context.Context, getenv func(string) string, out io.Writer) error {
	cfg, err := loadConfig(ctx, getenv)
	if err != nil {
		return err
	}
	if _, err := gateway.New(cfg); err != nil {
		return err
	}
	var keys, hashed, plain int
	for _, u := range cfg.Users {
		keys += len(u.Keys)
		if u.PasswordHash != "" {
			hashed++
		}
		if u.Password != "" {
			plain++
		}
	}
	fmt.Fprintf(out, "users: %d (%d keys, %d hashed passwords, %d plaintext passwords)\n", len(cfg.Users), keys, hashed, plain)
	fmt.Fprintf(out, "host key: %s\n", ssh.FingerprintSHA256(cfg.HostKey.PublicKey()))
	for _, b := range buckets(cfg) {
		if _, err := cfg.Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &b}); err != nil {
			return fmt.Errorf("bucket %q: %w", b, err)
		}
		fmt.Fprintf(out, "bucket: %s reachable\n", b)
	}
	fmt.Fprintln(out, "ok")
	return nil
}

// buckets lists, sorted, the buckets the users live in.
func buckets(cfg gateway.Config) []string {
	var bs []string
	for _, u := range cfg.Users {
		b := u.Bucket
		if b == "" {
			b = cfg.Bucket
		}
		if !slices.Contains(bs, b) {
			bs = append(bs, b)
		}
	}
	slices.Sort(bs)
	return bs
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
	slog.Info("listening", "version", version, "addr", ln.Addr().String(), "buckets", strings.Join(buckets(cfg), ","),
		"fingerprint", ssh.FingerprintSHA256(cfg.HostKey.PublicKey()))
	go func() {
		<-ctx.Done()
		slog.Info("shutting down; waiting for open sessions")
		ln.Close()
	}()
	return srv.Serve(ln)
}

// secretEnv returns the value of name, or the contents of the file named by
// name_FILE. Setting both is an error, so it is always clear which one won.
func secretEnv(getenv func(string) string, name string) (string, error) {
	v, path := getenv(name), getenv(name+"_FILE")
	switch {
	case path == "":
		return v, nil
	case v != "":
		return "", fmt.Errorf("set %s or %s_FILE, not both", name, name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return string(b), nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// loadConfig builds the gateway config from environment variables:
// SFTP_USERS (line or JSON format, see loadUsers), S3_BUCKET (unless every
// JSON user names a bucket), and optionally S3_PREFIX, SFTP_HOST_KEY,
// (SFTP_USERS and SFTP_HOST_KEY may instead be read from a file named by
// SFTP_USERS_FILE / SFTP_HOST_KEY_FILE, the Docker and Kubernetes secrets
// convention),
// S3_PART_SIZE_MB, SFTP_MAX_CONNECTIONS, SFTP_PROXY_PROTOCOL, and the store settings S3_ENDPOINT, S3_REGION,
// S3_PATH_STYLE, S3_CHECKSUM (see internal/s3client). Credentials come from
// the AWS SDK's default chain (AWS_ACCESS_KEY_ID/..., profiles, IAM roles).
func loadConfig(ctx context.Context, getenv func(string) string) (gateway.Config, error) {
	var cfg gateway.Config
	cfg.Bucket = getenv("S3_BUCKET")
	cfg.Prefix = getenv("S3_PREFIX")
	var err error
	users, err := secretEnv(getenv, "SFTP_USERS")
	if err != nil {
		return cfg, err
	}
	if cfg.Users, err = loadUsers(users, cfg.Bucket, cfg.Prefix); err != nil {
		return cfg, err
	}
	for _, u := range cfg.Users {
		if u.Bucket == "" && cfg.Bucket == "" {
			return cfg, errors.New("S3_BUCKET is required")
		}
	}
	for _, name := range plaintextUsers(cfg.Users) {
		slog.Warn("plaintext password; store a bcrypt hash instead (sftp-s3 -hash-password)", "user", name)
	}
	key, err := secretEnv(getenv, "SFTP_HOST_KEY")
	if err != nil {
		return cfg, err
	}
	if cfg.HostKey, err = hostKey(key); err != nil {
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
	switch v := getenv("SFTP_PROXY_PROTOCOL"); v {
	case "", "off":
	case "on":
		cfg.ProxyProtocol = true
	default:
		return cfg, fmt.Errorf("SFTP_PROXY_PROTOCOL: %q is not on or off", v)
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
