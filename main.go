// sftp-s3 (Go POC): an SFTP server whose filesystem is an S3 bucket.
//
// Each user is confined to s3://$S3_BUCKET/$S3_PREFIX<user>/.
// Uploads stream straight into an S3 multipart upload; nothing touches disk.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}
	users, err := parseUsers(os.Getenv("SFTP_USERS"))
	if err != nil {
		return err
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if ep := os.Getenv("S3_ENDPOINT"); ep != "" { // MinIO / R2 / etc.
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = true
		}
	})

	signer, err := hostKey(os.Getenv("SFTP_HOST_KEY"))
	if err != nil {
		return err
	}
	sshCfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			want, ok := users[c.User()]
			if ok && subtle.ConstantTimeCompare([]byte(want), pass) == 1 {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("auth failed for %q", c.User())
		},
	}
	sshCfg.AddHostKey(signer)

	addr := ":" + env("PORT", "2222")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", addr, "bucket", bucket,
		"fingerprint", ssh.FingerprintSHA256(signer.PublicKey()))

	fs := &s3fs{client: client, bucket: bucket, prefix: os.Getenv("S3_PREFIX"),
		partSize: 8 << 20, maxReorder: 64 << 20}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn, sshCfg, fs)
	}
}

// SFTP_USERS="alice:secret,bob:hunter2"
func parseUsers(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		u, p, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok || u == "" || p == "" || strings.ContainsAny(u, "/.") {
			continue
		}
		m[u] = p
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("SFTP_USERS must be user:pass[,user:pass...]")
	}
	return m, nil
}

// hostKey loads a PEM private key from the env var, or generates an ephemeral one.
// Production must pin a key: an ephemeral one changes on every deploy and
// clients will refuse to connect ("REMOTE HOST IDENTIFICATION HAS CHANGED").
func hostKey(pemData string) (ssh.Signer, error) {
	if pemData != "" {
		return ssh.ParsePrivateKey([]byte(pemData))
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

func handleConn(nc net.Conn, cfg *ssh.ServerConfig, fs *s3fs) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		slog.Info("handshake failed", "remote", nc.RemoteAddr(), "err", err)
		return
	}
	defer sc.Close()
	slog.Info("login", "user", sc.User(), "remote", nc.RemoteAddr())
	go ssh.DiscardRequests(reqs)

	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					go serveSFTP(ch, fs.forUser(sc.User()), sc.User())
				}
			}
		}()
	}
}

func serveSFTP(ch ssh.Channel, ufs *userFS, user string) {
	defer ch.Close()
	h := sftp.Handlers{FileGet: ufs, FilePut: ufs, FileCmd: ufs, FileList: ufs}
	srv := sftp.NewRequestServer(ch, h)
	if err := srv.Serve(); err != nil && err.Error() != "EOF" {
		slog.Info("sftp session ended", "user", user, "err", err)
	}
	srv.Close()
}
