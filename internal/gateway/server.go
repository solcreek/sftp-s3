// Package gateway serves SFTP over SSH with an S3 bucket as the filesystem.
package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Config describes one gateway. Users maps a login name to its password;
// each user is confined to s3://Bucket/Prefix<user>/.
type Config struct {
	Client     *s3.Client
	Bucket     string
	Prefix     string
	Users      map[string]string
	HostKey    ssh.Signer
	PartSize   int64 // multipart part size; 0 means 8 MiB
	MaxReorder int64 // out-of-order bytes buffered per upload; 0 means 64 MiB
}

// Server accepts SSH connections and serves the sftp subsystem.
type Server struct {
	cfg    Config
	sshCfg *ssh.ServerConfig
	wg     sync.WaitGroup
}

// New validates cfg and builds a Server.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("gateway: S3 client is required")
	case cfg.Bucket == "":
		return nil, errors.New("gateway: bucket is required")
	case len(cfg.Users) == 0:
		return nil, errors.New("gateway: at least one user is required")
	case cfg.HostKey == nil:
		return nil, errors.New("gateway: host key is required")
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = 8 << 20
	}
	if cfg.MaxReorder == 0 {
		cfg.MaxReorder = 64 << 20
	}
	users := cfg.Users
	sshCfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			want, ok := users[c.User()]
			if ok && subtle.ConstantTimeCompare([]byte(want), pass) == 1 {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("auth failed for %q", c.User())
		},
	}
	sshCfg.AddHostKey(cfg.HostKey)
	return &Server{cfg: cfg, sshCfg: sshCfg}, nil
}

// Serve accepts connections until ln is closed, then waits for open
// sessions to finish. It returns nil when ln was closed.
func (s *Server) Serve(ln net.Listener) error {
	defer s.wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.sshCfg)
	if err != nil {
		slog.Log(context.Background(), handshakeLogLevel(err), "handshake failed", "remote", nc.RemoteAddr(), "err", err)
		return
	}
	defer sc.Close()
	slog.Info("login", "user", sc.User(), "remote", nc.RemoteAddr())
	go ssh.DiscardRequests(reqs)

	fs := s.userFS(sc.User())
	var wg sync.WaitGroup
	defer wg.Wait()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveSession(ch, chReqs, fs)
		}()
	}
}

// handshakeLogLevel keeps connections that close before saying anything
// (load balancer TCP health checks, port scanners) out of the default log;
// they arrive every few seconds behind an AWS NLB or GCP passthrough LB.
func handshakeLogLevel(err error) slog.Level {
	if errors.Is(err, io.EOF) {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// serveSession answers channel requests; only the "sftp" subsystem is accepted.
func serveSession(ch ssh.Channel, reqs <-chan *ssh.Request, fs *userFS) {
	defer ch.Close()
	for req := range reqs {
		ok := req.Type == "subsystem" && isSFTP(req.Payload)
		req.Reply(ok, nil)
		if !ok {
			continue
		}
		go ssh.DiscardRequests(reqs)
		srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: fs, FilePut: fs, FileCmd: fs, FileList: fs})
		if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
			slog.Info("sftp session ended", "root", fs.root, "err", err)
		}
		srv.Close()
		return
	}
}

// isSFTP reports whether a subsystem request payload (an SSH string) names "sftp".
func isSFTP(payload []byte) bool {
	var p struct{ Name string }
	return ssh.Unmarshal(payload, &p) == nil && p.Name == "sftp"
}

func (s *Server) userFS(user string) *userFS {
	return &userFS{
		client: s.cfg.Client, bucket: s.cfg.Bucket, root: s.cfg.Prefix + user + "/",
		partSize: s.cfg.PartSize, maxReorder: s.cfg.MaxReorder,
	}
}
