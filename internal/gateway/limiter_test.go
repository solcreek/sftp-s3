package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/ssh"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func tcp(s string) net.Addr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestClientKey(t *testing.T) {
	for _, tc := range []struct {
		addr net.Addr
		want string // "" means not tracked
	}{
		{tcp("203.0.113.7:22"), "203.0.113.7/32"},
		{tcp("[::ffff:203.0.113.7]:22"), "203.0.113.7/32"},
		{tcp("[2001:db8:1:2:3:4:5:6]:22"), "2001:db8:1:2::/64"},
		{tcp("[fe80::1%lo0]:22"), "fe80::/64"},
		{&net.UDPAddr{IP: net.ParseIP("198.51.100.1"), Port: 1}, "198.51.100.1/32"},
		{&net.UnixAddr{Name: "/tmp/sock", Net: "unix"}, ""},
	} {
		k, ok := clientKey(tc.addr)
		got := ""
		if ok {
			got = k.String()
		}
		if got != tc.want {
			t.Errorf("clientKey(%v) = %q, want %q", tc.addr, got, tc.want)
		}
	}
	if a, b := clientName(netip.MustParsePrefix("203.0.113.7/32")), clientName(netip.MustParsePrefix("2001:db8::/64")); a != "203.0.113.7" || b != "2001:db8::/64" {
		t.Errorf("clientName = %q, %q", a, b)
	}
}

func TestLimiterBansPastTheThreshold(t *testing.T) {
	logs := captureLogs(t)
	clk := newFakeClock()
	l := newLimiter(3, time.Minute, 10*time.Minute, time.Hour, 100, clk.now)
	bad, good := tcp("203.0.113.7:1000"), tcp("198.51.100.1:1000")

	for range 2 {
		l.fail(bad)
	}
	if !l.allow(bad) {
		t.Fatal("banned below the threshold")
	}
	l.fail(tcp("203.0.113.7:2000")) // another port: same client
	if l.allow(bad) || !l.banned(bad) {
		t.Fatal("not banned at the threshold")
	}
	if !l.allow(good) || l.banned(good) {
		t.Fatal("another client was banned too")
	}
	if !strings.Contains(logs.String(), "client banned") || !strings.Contains(logs.String(), "client=203.0.113.7 ") {
		t.Fatalf("ban not logged:\n%s", logs)
	}

	l.allow(bad) // a second refusal
	clk.advance(10*time.Minute - time.Second)
	if l.allow(bad) {
		t.Fatal("ban ended early")
	}
	clk.advance(time.Second)
	if !l.allow(bad) {
		t.Fatal("ban did not end")
	}
	if !strings.Contains(logs.String(), `unbanned" client=203.0.113.7 refused=3`) {
		t.Fatalf("unban not logged with the refusal count:\n%s", logs)
	}
}

func TestLimiterWindowSlides(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(3, time.Minute, time.Hour, time.Hour, 100, clk.now)
	a := tcp("203.0.113.7:1")
	l.fail(a) // t=0
	clk.advance(40 * time.Second)
	l.fail(a) // t=40s
	clk.advance(20 * time.Second)
	l.fail(a) // t=60s: the first has left the window
	if !l.allow(a) {
		t.Fatal("a failure older than the window was counted")
	}
	clk.advance(10 * time.Second)
	l.fail(a) // t=70s: three within the last minute
	if l.allow(a) {
		t.Fatal("three failures within the window did not ban")
	}

	// A record whose failures have all left the window starts afresh.
	b := tcp("198.51.100.1:1")
	l.fail(b)
	l.fail(b)
	clk.advance(time.Minute)
	l.fail(b)
	l.fail(b)
	if !l.allow(b) {
		t.Fatal("failures older than the window were counted")
	}
}

func TestLimiterIgnoresFailuresDuringABan(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(2, time.Minute, time.Minute, time.Hour, 100, clk.now)
	a := tcp("203.0.113.7:1")
	l.fail(a)
	l.fail(a)
	clk.advance(30 * time.Second)
	l.fail(a) // must not extend the ban or count towards the next
	clk.advance(30 * time.Second)
	if !l.allow(a) {
		t.Fatal("a failure during the ban extended it")
	}
	l.fail(a)
	if !l.allow(a) {
		t.Fatal("a failure during the ban counted towards the next one")
	}
}

func TestLimiterRepeatBansGrowUpToTheCap(t *testing.T) {
	logs := captureLogs(t)
	clk := newFakeClock()
	l := newLimiter(1, time.Minute, time.Minute, 5*time.Minute, 100, clk.now)
	a := tcp("203.0.113.7:1")
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		l.fail(a)
		clk.advance(want - time.Second)
		if l.allow(a) {
			t.Fatalf("ban shorter than %v", want)
		}
		clk.advance(time.Second)
		if !l.allow(a) {
			t.Fatalf("ban longer than %v", want)
		}
	}
	if !strings.Contains(logs.String(), "for=5m0s bans=5") {
		t.Fatalf("ban length and count not logged:\n%s", logs)
	}

	// Once maxBan has passed since the last ban ended, the record is
	// forgotten and the next ban starts short again.
	clk.advance(5 * time.Minute)
	l.fail(a)
	clk.advance(time.Minute)
	if !l.allow(a) {
		t.Fatal("an old offence still lengthened the ban")
	}
}

func TestLimiterBanLength(t *testing.T) {
	l := newLimiter(1, time.Minute, 15*time.Minute, 24*time.Hour, 1, time.Now)
	for n, want := range map[int]time.Duration{1: 15 * time.Minute, 2: 30 * time.Minute, 7: 16 * time.Hour, 8: 24 * time.Hour, 1000: 24 * time.Hour} {
		if got := l.banLength(n); got != want {
			t.Errorf("banLength(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	logs := captureLogs(t)
	clk := newFakeClock()
	l := newLimiter(2, time.Minute, time.Hour, time.Hour, 3, clk.now)
	banned := tcp("203.0.113.7:1")
	l.fail(banned)
	l.fail(banned)

	for i := range 1000 { // a spread of clients, one failure each
		l.fail(tcp(fmt.Sprintf("[2001:db8:%x::1]:1", i)))
		if len(l.hosts) > 3 {
			t.Fatalf("%d clients tracked, cap 3", len(l.hosts))
		}
	}
	if l.allow(banned) {
		t.Fatal("a flood of new clients pushed out a ban")
	}
	if !strings.Contains(logs.String(), "ban table full") {
		t.Fatalf("eviction not logged:\n%s", logs)
	}
}

func TestLimiterForgetsExpiredClients(t *testing.T) {
	logs := captureLogs(t)
	clk := newFakeClock()
	l := newLimiter(2, time.Minute, time.Minute, time.Hour, 100, clk.now)
	banned := tcp("203.0.113.7:1")
	l.fail(banned)
	l.fail(banned)
	l.fail(tcp("198.51.100.1:1"))

	clk.advance(time.Minute + time.Hour) // past both the window and the ban's memory
	l.fail(tcp("198.51.100.2:1"))
	if len(l.hosts) != 1 {
		t.Fatalf("%d clients tracked after the others expired, want 1", len(l.hosts))
	}
	// The ban ended unnoticed; the sweep still reports it.
	if !strings.Contains(logs.String(), `unbanned" client=203.0.113.7 refused=0`) {
		t.Fatalf("unban not logged by the sweep:\n%s", logs)
	}
}

func TestNilLimiterAllowsEverything(t *testing.T) {
	var l *limiter
	a := tcp("203.0.113.7:1")
	l.fail(a)
	if !l.allow(a) || l.banned(a) {
		t.Fatal("nil limiter refused a client")
	}
}

func TestLimiterIgnoresNonIPAddresses(t *testing.T) {
	l := newLimiter(1, time.Minute, time.Minute, time.Hour, 100, time.Now)
	a := &net.UnixAddr{Name: "/tmp/sock", Net: "unix"}
	l.fail(a)
	if !l.allow(a) || l.banned(a) || len(l.hosts) != 0 {
		t.Fatal("a non-IP address was tracked")
	}
}

func TestNewBanConfig(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	base := Config{Client: &s3.Client{}, Bucket: "b", Users: map[string]User{"u": {Password: "p"}}, HostKeys: []ssh.Signer{signer}}

	srv, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if l := srv.bans; l == nil || l.threshold != 10 || l.window != 10*time.Minute || l.ban != 15*time.Minute ||
		l.maxBan != 24*time.Hour || l.maxHosts != 10000 || l.now == nil {
		t.Fatalf("default limiter = %+v", srv.bans)
	}
	if srv.sshCfg.MaxAuthTries != 6 {
		t.Errorf("MaxAuthTries = %d, want 6", srv.sshCfg.MaxAuthTries)
	}

	off := base
	off.BanThreshold, off.MaxAuthTries = -1, 3
	if srv, err = New(off); err != nil || srv.bans != nil || srv.sshCfg.MaxAuthTries != 3 {
		t.Fatalf("BanThreshold -1: bans %v, MaxAuthTries %d, err %v", srv.bans, srv.sshCfg.MaxAuthTries, err)
	}

	for name, mod := range map[string]func(*Config){
		"negative MaxAuthTries":   func(c *Config) { c.MaxAuthTries = -1 },
		"negative window":         func(c *Config) { c.BanWindow = -time.Second },
		"negative duration":       func(c *Config) { c.BanDuration = -time.Second },
		"max shorter than a ban":  func(c *Config) { c.BanDuration, c.BanMaxDuration = time.Hour, time.Minute },
		"default max below a ban": func(c *Config) { c.BanDuration = 48 * time.Hour },
	} {
		c := base
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// banEnv is a gateway behind the PROXY protocol, so tests can connect as
// any client address, with a ban after 3 failures and a clock under test
// control.
func banEnv(t *testing.T, opts ...envOpt) (*env, *fakeClock) {
	clk := newFakeClock()
	opts = append([]envOpt{func(c *Config) {
		c.ProxyProtocol = true
		c.BanThreshold = 3
		c.BanDuration = time.Minute
		c.clock = clk.now
	}}, opts...)
	return newEnv(t, opts...), clk
}

// dialFrom connects as alice from client address src (via a PROXY header)
// with the given auth methods.
func (e *env) dialFrom(src string, auth ...ssh.AuthMethod) (*ssh.Client, error) {
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(proxyHeader(e.t, 2, src)); err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, e.addr, &ssh.ClientConfig{
		User:            "alice",
		Auth:            auth,
		HostKeyCallback: ssh.FixedHostKey(e.hostPK),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		raw.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func (e *env) loginFrom(src, pass string) error {
	c, err := e.dialFrom(src, ssh.Password(pass))
	if err == nil {
		c.Close()
	}
	return err
}

const attacker, bystander = "203.0.113.7:4000", "198.51.100.1:4000"

func TestRepeatedFailuresGetTheClientBanned(t *testing.T) {
	logs := captureLogs(t)
	e, clk := banEnv(t)
	for range 3 {
		if e.loginFrom(attacker, "wrong") == nil {
			t.Fatal("wrong password accepted")
		}
	}
	waitForLog(t, logs, `banned" client=203.0.113.7`)

	if err := e.loginFrom(attacker, "alice-pw"); err == nil {
		t.Fatal("correct password from a banned client accepted")
	} else if strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("banned client reached authentication: %v", err)
	}
	if err := e.loginFrom(bystander, "alice-pw"); err != nil {
		t.Fatalf("another client was refused: %v", err)
	}
	if err := e.loginFrom("[2001:db8::1]:1", "alice-pw"); err != nil {
		t.Fatalf("an IPv6 client was refused: %v", err)
	}
	if strings.Contains(logs.String(), "level=INFO msg=\"connection refused") {
		t.Fatalf("refused connection logged at info:\n%s", logs)
	}

	clk.advance(time.Minute)
	if err := e.loginFrom(attacker, "alice-pw"); err != nil {
		t.Fatalf("still refused after the ban ended: %v", err)
	}
	waitForLog(t, logs, `unbanned" client=203.0.113.7 refused=1`)
}

func TestIPv6ClientsAreBannedBySlash64(t *testing.T) {
	e, _ := banEnv(t)
	for i := range 3 {
		e.loginFrom(fmt.Sprintf("[2001:db8:0:1::%x]:1", i+1), "wrong")
	}
	if e.loginFrom("[2001:db8:0:1::ffff]:1", "alice-pw") == nil {
		t.Fatal("another address in the banned /64 was accepted")
	}
	if err := e.loginFrom("[2001:db8:0:2::1]:1", "alice-pw"); err != nil {
		t.Fatalf("a neighbouring /64 was refused: %v", err)
	}
}

func TestBanStopsALoginAlreadyUnderWay(t *testing.T) {
	e, _ := banEnv(t)
	tries := []string{"wrong", "wrong", "wrong", "alice-pw"}
	next := func() (string, error) { p := tries[0]; tries = tries[1:]; return p, nil }
	if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ssh.PasswordCallback(next), 4)); err == nil {
		c.Close()
		t.Fatal("correct password accepted after the connection's failures banned it")
	}
	if len(tries) != 0 {
		t.Fatalf("%d passwords left untried", len(tries))
	}
}

func TestBanStopsAKeyLoginAlreadyUnderWay(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewSignerFromKey(priv)
	e, _ := banEnv(t, func(c *Config) {
		c.Users = map[string]User{"alice": {Password: "alice-pw", Keys: []ssh.PublicKey{key.PublicKey()}}}
	})
	tries := []string{"wrong", "wrong", "wrong"}
	next := func() (string, error) { p := tries[0]; tries = tries[1:]; return p, nil }
	if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ssh.PasswordCallback(next), 3), ssh.PublicKeys(key)); err == nil {
		c.Close()
		t.Fatal("valid key accepted after the connection's failures banned it")
	}
	if c, err := e.dialFrom(bystander, ssh.PublicKeys(key)); err != nil {
		t.Fatalf("valid key refused from a client that is not banned: %v", err)
	} else {
		c.Close()
	}
}

func TestConnectionsWithoutLoginAttemptsAreNotCounted(t *testing.T) {
	e, _ := banEnv(t, func(c *Config) { c.BanThreshold = 1 })
	for range 5 {
		// An NLB health check: TCP connect and close, PROXY header or not.
		raw, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		raw.Close()
		raw, err = net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(proxyHeader(t, 2, attacker))
		raw.Close()
		// ssh-keyscan: a handshake, then no method but "none".
		if c, err := e.dialFrom(attacker); err == nil {
			c.Close()
			t.Fatal("login without credentials accepted")
		}
	}
	if err := e.loginFrom(attacker, "alice-pw"); err != nil {
		t.Fatalf("health checks and key scans got the client banned: %v", err)
	}
}

func TestRejectedKeysCountOncePerConnection(t *testing.T) {
	e, _ := banEnv(t, func(c *Config) { c.BanThreshold = 2 })
	var keys []ssh.Signer
	for range 5 {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(priv)
		keys = append(keys, s)
	}
	if c, err := e.dialFrom(attacker, ssh.PublicKeys(keys...)); err == nil {
		c.Close()
		t.Fatal("unknown key accepted")
	}
	if err := e.loginFrom(attacker, "alice-pw"); err != nil {
		t.Fatalf("five rejected keys in one connection counted more than once: %v", err)
	}
	if c, err := e.dialFrom(attacker, ssh.PublicKeys(keys...)); err == nil {
		c.Close()
	}
	if e.loginFrom(attacker, "alice-pw") == nil {
		t.Fatal("two failed connections did not reach a threshold of 2")
	}
}

func TestBansCanBeTurnedOff(t *testing.T) {
	e, _ := banEnv(t, func(c *Config) { c.BanThreshold = -1 })
	for range 10 {
		e.loginFrom(attacker, "wrong")
	}
	if err := e.loginFrom(attacker, "alice-pw"); err != nil {
		t.Fatalf("refused with bans off: %v", err)
	}
}

func TestMaxAuthTries(t *testing.T) {
	e, _ := banEnv(t, func(c *Config) { c.BanThreshold, c.MaxAuthTries = -1, 2 })
	tries := []string{"wrong", "wrong", "alice-pw"}
	next := func() (string, error) { p := tries[0]; tries = tries[1:]; return p, nil }
	if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ssh.PasswordCallback(next), 3)); err == nil {
		c.Close()
		t.Fatal("third try accepted with MaxAuthTries = 2")
	}
	if len(tries) != 1 {
		t.Fatalf("%d passwords left, want 1", len(tries))
	}
}
