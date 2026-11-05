package gateway

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// limiter counts failed logins per client and bans clients that fail too
// often. A client is an IPv4 address or an IPv6 /64 (one host usually holds
// a whole /64, so banning single IPv6 addresses would not stop it).
//
// A nil *limiter allows everything and records nothing: bans are off.
//
// State lives in memory, per process; every replica bans on its own.
type limiter struct {
	threshold int           // failures within window that start a ban
	window    time.Duration // how far back failures are counted
	ban       time.Duration // first ban; each repeat ban doubles it
	maxBan    time.Duration // cap on a ban, and how long repeat bans are remembered
	maxHosts  int           // cap on tracked clients
	now       func() time.Time

	mu        sync.Mutex
	hosts     map[netip.Prefix]*host
	nextSweep time.Time
	total     int64 // bans so far, of every client
}

type host struct {
	failures []time.Time // within the window, oldest first
	until    time.Time   // end of the current or last ban
	banned   bool        // banned, and the unban not logged yet
	bans     int         // bans so far, for the growing ban length
	refused  int         // connections refused during the current ban
	expires  time.Time   // when nothing about this client matters any more
}

// newBans fills in cfg's ban and MaxAuthTries defaults, checks them, and
// returns the limiter they describe (nil when bans are off).
func newBans(cfg *Config) (*limiter, error) {
	if cfg.MaxAuthTries < 0 {
		return nil, errors.New("gateway: MaxAuthTries must not be negative")
	}
	if cfg.MaxAuthTries == 0 {
		cfg.MaxAuthTries = 6
	}
	if cfg.BanThreshold < 0 {
		return nil, nil
	}
	if cfg.BanThreshold == 0 {
		cfg.BanThreshold = 10
	}
	if cfg.BanWindow == 0 {
		cfg.BanWindow = 10 * time.Minute
	}
	if cfg.BanDuration == 0 {
		cfg.BanDuration = 15 * time.Minute
	}
	if cfg.BanMaxDuration == 0 {
		cfg.BanMaxDuration = 24 * time.Hour
	}
	if cfg.banHosts == 0 {
		cfg.banHosts = 10000
	}
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	switch {
	case cfg.BanWindow < 0 || cfg.BanDuration < 0:
		return nil, errors.New("gateway: BanWindow and BanDuration must not be negative")
	case cfg.BanMaxDuration < cfg.BanDuration:
		return nil, errors.New("gateway: BanMaxDuration must not be shorter than BanDuration")
	}
	return newLimiter(cfg.BanThreshold, cfg.BanWindow, cfg.BanDuration, cfg.BanMaxDuration, cfg.banHosts, cfg.clock), nil
}

func newLimiter(threshold int, window, ban, maxBan time.Duration, maxHosts int, now func() time.Time) *limiter {
	return &limiter{
		threshold: threshold, window: window, ban: ban, maxBan: maxBan, maxHosts: maxHosts, now: now,
		hosts: make(map[netip.Prefix]*host),
	}
}

// clientKey returns the prefix a remote address is tracked by, or false for
// an address that is not IP (it is then never banned).
func clientKey(a net.Addr) (netip.Prefix, bool) {
	var ip netip.Addr
	if t, ok := a.(*net.TCPAddr); ok {
		ip, _ = netip.AddrFromSlice(t.IP)
	} else if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		ip = ap.Addr()
	}
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return netip.Prefix{}, false
	}
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	p, _ := ip.Prefix(bits)
	return p, true
}

// clientName is how a client appears in logs: an IPv4 address, or an IPv6 /64.
func clientName(p netip.Prefix) string {
	if p.Addr().Is4() {
		return p.Addr().String()
	}
	return p.String()
}

// allow reports whether a new connection from a may proceed. Refusals are
// counted and reported in the unban log line rather than logged one by one.
func (l *limiter) allow(a net.Addr) bool {
	if l == nil {
		return true
	}
	k, ok := clientKey(a)
	if !ok {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[k]
	if h == nil || !l.bannedNow(k, h, l.now()) {
		return true
	}
	h.refused++
	return false
}

// banned reports whether a is banned, without counting a refusal.
func (l *limiter) banned(a net.Addr) bool {
	if l == nil {
		return false
	}
	k, ok := clientKey(a)
	if !ok {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hosts[k]
	return h != nil && l.bannedNow(k, h, l.now())
}

// bannedNow reports whether h is banned at now, logging the unban when a
// ban is found to have ended. l.mu must be held.
func (l *limiter) bannedNow(k netip.Prefix, h *host, now time.Time) bool {
	if !h.banned {
		return false
	}
	if now.Before(h.until) {
		return true
	}
	slog.Info("client unbanned", "client", clientName(k), "refused", h.refused)
	h.banned, h.refused = false, 0
	return false
}

// fail records a failed login from a, and bans it past the threshold.
// Failures during a ban are ignored: they neither extend it nor count
// towards the next one.
func (l *limiter) fail(a net.Addr) {
	if l == nil {
		return
	}
	k, ok := clientKey(a)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h := l.hosts[k]
	if h != nil && !now.Before(h.expires) { // stale: start afresh
		l.bannedNow(k, h, now) // logs the unban if it went unnoticed
		h = nil
	}
	if h == nil {
		l.makeRoom(now)
		h = &host{}
		l.hosts[k] = h
	}
	if l.bannedNow(k, h, now) {
		return
	}
	cut := 0
	for cut < len(h.failures) && !h.failures[cut].After(now.Add(-l.window)) {
		cut++
	}
	h.failures = append(h.failures[cut:], now)
	h.expires = later(h.expires, now.Add(l.window))
	if len(h.failures) < l.threshold {
		return
	}
	h.bans++
	l.total++
	d := l.banLength(h.bans)
	h.failures, h.banned, h.until = nil, true, now.Add(d)
	// Remember the ban for maxBan after it ends, so a repeat offence gets
	// a longer one.
	h.expires = h.until.Add(l.maxBan)
	slog.Warn("client banned", "client", clientName(k), "failures", l.threshold, "within", l.window, "for", d, "bans", h.bans)
}

// counts returns how many bans there have been, and how many clients are
// banned now. A nil *limiter (bans off) has none.
func (l *limiter) counts() (total int64, now int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	for _, h := range l.hosts {
		if h.banned && t.Before(h.until) {
			now++
		}
	}
	return l.total, now
}

// banLength is the length of the nth ban: ban, doubled for each earlier
// ban, at most maxBan.
func (l *limiter) banLength(n int) time.Duration {
	d := l.ban
	for i := 1; i < n && d < l.maxBan; i++ {
		d *= 2
	}
	return min(d, l.maxBan)
}

// makeRoom keeps the table bounded before a new client is added: it drops
// clients whose state has expired (at most once per window, or when the
// table is full), and if the table is still full, the client that would
// expire first. l.mu must be held.
func (l *limiter) makeRoom(now time.Time) {
	if len(l.hosts) < l.maxHosts && now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(l.window)
	for k, h := range l.hosts {
		if !now.Before(h.expires) {
			l.bannedNow(k, h, now) // logs the unban if it went unnoticed
			delete(l.hosts, k)
		}
	}
	if len(l.hosts) < l.maxHosts {
		return
	}
	var oldest netip.Prefix
	var first time.Time
	for k, h := range l.hosts {
		if first.IsZero() || h.expires.Before(first) {
			oldest, first = k, h.expires
		}
	}
	slog.Warn("ban table full; forgetting a client early", "client", clientName(oldest), "max", l.maxHosts)
	delete(l.hosts, oldest)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// watchAuth returns the SSH configuration for one connection from remote,
// whose AuthLogCallback feeds both the ban counting and the audit log
// (through log, which carries the connection's session and remote). It
// refuses every login once remote is banned, even one already under way.
// Call done with the handshake's error.
//
// Counted as failures, and logged as login_failed: each wrong password
// (keyboard-interactive asks for the same password, and counts the same), and
// a connection that ends without logging in after trying at least one
// method, if no wrong password was counted for it already. Rejected public
// keys are not counted one by one (clients offer every key they hold), and
// connections that never try to log in (load balancer health checks, port
// scans) are not counted. With bans off (s.bans nil) only the log remains.
func (s *Server) watchAuth(remote net.Addr, log *slog.Logger) (cfg *ssh.ServerConfig, done func(error)) {
	c := *s.sshCfg // what ssh.NewServerConn does too
	var (
		methods        []string // tried, in order, without repeats
		wrongPasswords int
		banned         bool
		user           string // the last user name tried
	)
	c.AuthLogCallback = func(m ssh.ConnMetadata, method string, err error) {
		if method == "none" { // a client asking which methods exist
			return
		}
		user = loggedName(m.User())
		method = loggedName(method)
		if !slices.Contains(methods, method) {
			methods = append(methods, method)
		}
		banned = banned || errors.Is(err, errBanned)
		asksPassword := method == "password" || (method == methodKeyboardInteractive && c.KeyboardInteractiveCallback != nil)
		if asksPassword && err != nil {
			wrongPasswords++
			s.bans.fail(remote)
			audit(&s.metrics, log, evLoginFailed, "user", user, "method", method, "reason", failReason(err))
		}
	}
	password, publicKey, interactive := c.PasswordCallback, c.PublicKeyCallback, c.KeyboardInteractiveCallback
	c.PasswordCallback = func(m ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if s.bans.banned(remote) {
			return nil, errBanned
		}
		return password(m, pass)
	}
	if interactive != nil {
		c.KeyboardInteractiveCallback = func(m ssh.ConnMetadata, ask ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			if s.bans.banned(remote) {
				return nil, errBanned
			}
			return interactive(m, ask)
		}
	}
	c.PublicKeyCallback = func(m ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if s.bans.banned(remote) {
			return nil, errBanned
		}
		return publicKey(m, key)
	}
	return &c, func(err error) {
		if err == nil || len(methods) == 0 || wrongPasswords > 0 {
			return
		}
		s.bans.fail(remote)
		reason := "no accepted method"
		switch {
		case banned:
			reason = "banned"
		case slices.Contains(methods, "publickey"):
			reason = "no accepted key"
		}
		audit(&s.metrics, log, evLoginFailed, "user", user, "method", strings.Join(methods, ","), "reason", reason)
	}
}

var errBanned = errors.New("client is banned")
