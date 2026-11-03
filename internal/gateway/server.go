// Package gateway serves SFTP over SSH with an S3 bucket as the filesystem.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Config describes one gateway. Users maps a login name to its
// credentials and location; it is the initial set, which Server.SetUsers
// replaces. A user without a Bucket of its own is confined
// to s3://Bucket/Prefix<user>/; Bucket may be empty when every user has one.
type Config struct {
	Client     *s3.Client
	Bucket     string
	Prefix     string
	Users      map[string]User
	HostKeys   []ssh.Signer
	PartSize   int64 // multipart part size; 0 means 8 MiB
	MaxReorder int64 // out-of-order bytes buffered per upload; 0 means 64 MiB

	// AuthFailDelay is how long a wrong password is held before the client
	// may try again; 0 means 1 s. With ssh's 6 tries per connection it slows
	// password guessing to a few attempts per second per connection.
	AuthFailDelay time.Duration

	// LoginTimeout bounds the SSH handshake plus authentication; 0 means
	// 30 s. Connections that never log in are closed instead of holding a
	// slot forever.
	LoginTimeout time.Duration

	// MaxConns caps concurrent connections, logged in or not; 0 means 32.
	// Further connections are closed at once. Size it to memory: a large
	// upload holds roughly 120 MiB (parts in flight plus the reorder buffer).
	MaxConns int

	// ProxyProtocol requires every connection to start with a PROXY
	// protocol header (v1 or v2), as sent by load balancers configured for
	// it, and logs the client address it carries. Connections without one
	// are closed. The listener must then be reachable only through the
	// proxy, or clients could claim any address.
	ProxyProtocol bool
}

// limitWarnEvery throttles the "connection limit reached" log.
const limitWarnEvery = 10 * time.Second

// Server accepts SSH connections and serves the sftp subsystem.
type Server struct {
	cfg    Config
	sshCfg *ssh.ServerConfig
	wg     sync.WaitGroup
	slots  chan struct{} // one per open connection
	users  atomic.Pointer[map[string]User]
}

// New validates cfg and builds a Server. It needs at least one host key and
// offers them all; give at most one per key type, since ssh keeps only the
// last key of a type.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("gateway: S3 client is required")
	case len(cfg.HostKeys) == 0:
		return nil, errors.New("gateway: host key is required")
	case slices.Contains(cfg.HostKeys, nil):
		return nil, errors.New("gateway: host key is nil")
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = 8 << 20
	}
	if cfg.MaxReorder == 0 {
		cfg.MaxReorder = 64 << 20
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 32
	}
	if cfg.MaxConns < 0 {
		return nil, errors.New("gateway: MaxConns must not be negative")
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = 30 * time.Second
	}
	if cfg.AuthFailDelay == 0 {
		cfg.AuthFailDelay = time.Second
	}
	s := &Server{cfg: cfg, slots: make(chan struct{}, cfg.MaxConns)}
	if err := s.SetUsers(cfg.Users); err != nil {
		return nil, err
	}
	auth := authenticator{users: s.currentUsers, delay: cfg.AuthFailDelay, locate: s.locate}
	s.sshCfg = &ssh.ServerConfig{
		PasswordCallback:  auth.password,
		PublicKeyCallback: auth.publicKey,
	}
	for _, k := range cfg.HostKeys {
		s.sshCfg.AddHostKey(k)
	}
	return s, nil
}

// SetUsers replaces the users, validated as New validates Config.Users.
// On error the current users stay. New logins see the new set at once;
// connections already logged in keep the bucket and root they got at login.
// It is safe to call while the server is serving.
func (s *Server) SetUsers(users map[string]User) error {
	if len(users) == 0 {
		return errors.New("gateway: at least one user is required")
	}
	for name, u := range users {
		if err := u.validate(); err != nil {
			return fmt.Errorf("gateway: user %q: %w", name, err)
		}
		if u.Bucket == "" && s.cfg.Bucket == "" {
			return fmt.Errorf("gateway: user %q has no bucket, and there is no default bucket", name)
		}
	}
	m := maps.Clone(users) // the caller may go on to change its map
	s.users.Store(&m)
	return nil
}

func (s *Server) currentUsers() map[string]User { return *s.users.Load() }

// Serve accepts connections until ln is closed, then waits for open
// sessions to finish. It returns nil when ln was closed.
func (s *Server) Serve(ln net.Listener) error {
	defer s.wg.Wait()
	var rejected int
	var lastWarn time.Time
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.slots <- struct{}{}:
		default:
			// Health checks and floods keep arriving while full: log at
			// most once per limitWarnEvery, with the count since last time.
			rejected++
			if time.Since(lastWarn) >= limitWarnEvery {
				slog.Warn("connection limit reached", "remote", conn.RemoteAddr(), "max", s.cfg.MaxConns, "rejected", rejected)
				lastWarn, rejected = time.Now(), 0
			}
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.slots }()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(s.cfg.LoginTimeout))
	if s.cfg.ProxyProtocol {
		pc, err := readProxyHeader(nc)
		if err != nil {
			slog.Log(context.Background(), handshakeLogLevel(err), "no valid PROXY protocol header", "remote", nc.RemoteAddr(), "err", err)
			return
		}
		nc = pc
	}
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.sshCfg)
	if err != nil {
		slog.Log(context.Background(), handshakeLogLevel(err), "handshake failed", "remote", nc.RemoteAddr(), "err", err)
		return
	}
	nc.SetDeadline(time.Time{}) // logged in: sessions may idle and transfer as long as they like
	defer sc.Close()
	method := "password"
	ext := sc.Permissions.Extensions
	if fp, ok := ext[extKeyFP]; ok {
		method = fp
	}
	fs := s.userFS(ext[extBucket], ext[extRoot])
	slog.Info("login", "user", sc.User(), "remote", nc.RemoteAddr(), "auth", method, "bucket", fs.bucket, "root", fs.root)
	go ssh.DiscardRequests(reqs)

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
// Such a peer shows up as EOF, or as a reset when it closes after our
// version banner arrived unread (Linux).
func handshakeLogLevel(err error) slog.Level {
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
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

// locate gives where a user's files live: their own bucket and root, or
// <Prefix><name>/ in the default bucket.
func (s *Server) locate(name string, u User) (bucket, root string) {
	if u.Bucket != "" {
		return u.Bucket, u.Root
	}
	return s.cfg.Bucket, s.cfg.Prefix + name + "/"
}

// userFS is a filesystem confined to s3://bucket/root.
func (s *Server) userFS(bucket, root string) *userFS {
	return &userFS{
		client: s.cfg.Client, bucket: bucket, root: root,
		partSize: s.cfg.PartSize, maxReorder: s.cfg.MaxReorder,
	}
}
