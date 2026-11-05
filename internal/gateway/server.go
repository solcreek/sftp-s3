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

	// BanThreshold is how many failed logins from one client (an IPv4
	// address or an IPv6 /64) within BanWindow get it banned: its
	// connections are then closed before the SSH handshake for BanDuration,
	// doubled on each repeat ban up to BanMaxDuration. 0 means 10 failures;
	// negative turns bans off. See watchAuth for what counts as a failure.
	BanThreshold   int
	BanWindow      time.Duration // 0 means 10 min
	BanDuration    time.Duration // 0 means 15 min
	BanMaxDuration time.Duration // 0 means 24 h; also how long past bans are remembered

	// MaxAuthTries caps login attempts per connection; 0 means 6.
	MaxAuthTries int

	// NoKeyboardInteractive stops offering keyboard-interactive, which
	// otherwise asks for the password as password login does.
	NoKeyboardInteractive bool

	// Version is reported by the sftp_s3_build_info metric.
	Version string

	banHosts int              // cap on clients tracked for bans; 0 means 10000
	clock    func() time.Time // for tests; nil means time.Now
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
	bans   *limiter // nil when bans are off

	metrics metrics
	serving atomic.Bool // Serve is accepting connections
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
	bans, err := newBans(&cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, slots: make(chan struct{}, cfg.MaxConns), bans: bans}
	if err := s.SetUsers(cfg.Users); err != nil {
		return nil, err
	}
	auth := authenticator{users: s.currentUsers, delay: cfg.AuthFailDelay, locate: s.locate}
	s.sshCfg = &ssh.ServerConfig{
		PasswordCallback:  auth.password,
		PublicKeyCallback: auth.publicKey,
		MaxAuthTries:      cfg.MaxAuthTries,
	}
	if !cfg.NoKeyboardInteractive {
		s.sshCfg.KeyboardInteractiveCallback = auth.keyboardInteractive
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

// Serving reports whether Serve is accepting connections: false before
// it starts, and from the moment ln is closed (shutdown has begun) while
// open sessions drain.
func (s *Server) Serving() bool { return s.serving.Load() }

// Serve accepts connections until ln is closed, then waits for open
// sessions to finish. It returns nil when ln was closed.
func (s *Server) Serve(ln net.Listener) error {
	defer s.wg.Wait()
	s.serving.Store(true)
	defer s.serving.Store(false) // runs before the wait above
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
		s.metrics.accepted()
		select {
		case s.slots <- struct{}{}:
		default:
			s.metrics.reject(rejectLimit)
			// Health checks and floods keep arriving while full: log at
			// most once per limitWarnEvery, with the count since last time.
			rejected++
			if time.Since(lastWarn) >= limitWarnEvery {
				slog.Warn("connection limit reached", "remote", conn.RemoteAddr().String(), "max", s.cfg.MaxConns, "rejected", rejected)
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
			slog.Log(context.Background(), handshakeLogLevel(err), "no valid PROXY protocol header", "remote", nc.RemoteAddr().String(), "err", err)
			return
		}
		nc = pc
	}
	remote := nc.RemoteAddr().String() // a string, so JSON logs show it as one
	if !s.bans.allow(nc.RemoteAddr()) {
		s.metrics.reject(rejectBanned)
		slog.Debug("connection refused: client is banned", "remote", remote)
		return
	}
	log := slog.With("session", newSessionID(), "remote", remote)
	sshCfg, authDone := s.watchAuth(nc.RemoteAddr(), log)
	sc, chans, reqs, err := ssh.NewServerConn(nc, sshCfg)
	authDone(err)
	if err != nil {
		log.Log(context.Background(), handshakeLogLevel(err), "handshake failed", "err", err)
		return
	}
	defer sc.Close()
	log = log.With("user", sc.User())
	ext := sc.Permissions.Extensions
	method, fp := "password", ext[extKeyFP]
	switch {
	case fp != "":
		method = "publickey"
	case ext[extMethod] != "":
		method = ext[extMethod]
	}
	// ssh caches public-key results per connection, so a key accepted before
	// a ban can still complete the login after it: check once more.
	if s.bans.banned(nc.RemoteAddr()) {
		audit(&s.metrics, log, evLoginFailed, "method", method, "reason", "banned")
		return
	}
	nc.SetDeadline(time.Time{}) // logged in: sessions may idle and transfer as long as they like
	sess := newSession(log, &s.metrics)
	fs := s.userFS(ext[extBucket], ext[extRoot], sess)
	attrs := []any{"method", method, "bucket", fs.bucket, "root", fs.root}
	if fp != "" {
		attrs = append(attrs, "fingerprint", fp)
	}
	audit(&s.metrics, log, evLogin, attrs...)
	defer sess.end()
	go ssh.DiscardRequests(reqs)

	var wg sync.WaitGroup
	defer wg.Wait() // before sess.end: open files are closed, and logged, first
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
			fs.sess.log.Info("sftp session ended", "err", err)
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

// userFS is a filesystem confined to s3://bucket/root, which logs its
// operations as sess's.
func (s *Server) userFS(bucket, root string, sess *session) *userFS {
	return &userFS{
		client: s.cfg.Client, bucket: bucket, root: root, sess: sess,
		partSize: s.cfg.PartSize, maxReorder: s.cfg.MaxReorder,
	}
}
