package gateway

import (
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func (e *env) dialInteractive(user string, auth ssh.AuthMethod) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK),
	})
}

func TestKeyboardInteractiveLogin(t *testing.T) {
	logs := captureJSON(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("carol-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.Users["carol"] = User{PasswordHash: string(hash)} })

	var asked [][]string
	var echoed [][]bool
	recordQuestions := func(pass string) ssh.AuthMethod {
		return ssh.KeyboardInteractive(func(_, _ string, q []string, echo []bool) ([]string, error) {
			asked, echoed = append(asked, q), append(echoed, echo)
			return []string{pass}, nil
		})
	}
	for user, pass := range map[string]string{"alice": "alice-pw", "carol": "carol-pw"} {
		c, err := e.dialInteractive(user, recordQuestions(pass))
		if err != nil {
			t.Fatalf("%s: keyboard-interactive login failed: %v", user, err)
		}
		c.Close()
	}
	for i := range asked {
		if len(asked[i]) != 1 || asked[i][0] != "Password: " || echoed[i][0] {
			t.Fatalf("questions %q, echo %v; want one hidden \"Password: \"", asked[i], echoed[i])
		}
	}
	for _, bad := range []struct{ user, pass string }{
		{"alice", "wrong"}, {"alice", ""}, {"alice", "bob-pw"}, {"carol", string(hash)}, {"mallory", "x"},
	} {
		if c, err := e.dialInteractive(bad.user, answer(bad.pass)); err == nil {
			c.Close()
			t.Errorf("%s/%q accepted", bad.user, bad.pass)
		}
	}

	evs := stopAndCollect(t, e, logs)
	logins := named(evs, evLogin)
	if len(logins) != 2 {
		t.Fatalf("login events %v, want 2", logins)
	}
	for _, ev := range logins {
		if ev["method"] != "keyboard-interactive" {
			t.Errorf("login event %v, want method keyboard-interactive", ev)
		}
	}
	expect(t, scrape(t, e.srv), map[string]float64{
		series("sftp_s3_logins_total", "method", "keyboard-interactive"):   2,
		series("sftp_s3_logins_total", "method", "password"):               0,
		series("sftp_s3_login_failures_total", "reason", "wrong password"): 4,
		series("sftp_s3_login_failures_total", "reason", "unknown user"):   1,
	})
	noSecrets(t, logs, "alice-pw", "carol-pw", "bob-pw")
}

func TestKeyboardInteractiveFailureIsDelayed(t *testing.T) {
	const delay = 300 * time.Millisecond
	e := newEnv(t, func(c *Config) { c.AuthFailDelay = delay })
	start := time.Now()
	if c, err := e.dialInteractive("alice", answer("wrong")); err == nil {
		c.Close()
		t.Fatal("wrong answer accepted")
	}
	if took := time.Since(start); took < delay {
		t.Fatalf("failed login answered after %v, want at least %v", took, delay)
	}
}

func TestKeyboardInteractiveFailuresGetTheClientBanned(t *testing.T) {
	e, _ := banEnv(t)
	for range 3 {
		if c, err := e.dialFrom(attacker, answer("wrong")); err == nil {
			c.Close()
			t.Fatal("wrong answer accepted")
		}
	}
	for name, auth := range map[string]ssh.AuthMethod{"password": ssh.Password("alice-pw"), "keyboard-interactive": answer("alice-pw")} {
		if c, err := e.dialFrom(attacker, auth); err == nil {
			c.Close()
			t.Fatalf("%s: correct password from a banned client accepted", name)
		}
	}
	c, err := e.dialFrom(bystander, answer("alice-pw"))
	if err != nil {
		t.Fatalf("another client was refused: %v", err)
	}
	c.Close()
}

// The ban lands during the connection, so only the callback can refuse the
// login that follows; it does so without asking for the password again.
func TestKeyboardInteractiveBanStopsALoginAlreadyUnderWay(t *testing.T) {
	e, _ := banEnv(t)
	tries := []string{"wrong", "wrong", "wrong", "alice-pw"}
	ki := ssh.KeyboardInteractive(func(_, _ string, q []string, _ []bool) ([]string, error) {
		p := tries[0]
		tries = tries[1:]
		return []string{p}, nil
	})
	if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ki, 4)); err == nil {
		c.Close()
		t.Fatal("right answer accepted after the connection's failures banned it")
	}
	if len(tries) != 1 {
		t.Fatalf("%d answers left; want the right one never asked for", len(tries))
	}
}

func TestKeyboardInteractiveCountsTowardMaxAuthTries(t *testing.T) {
	e, _ := banEnv(t, func(c *Config) { c.BanThreshold, c.MaxAuthTries = -1, 2 })
	tries := []string{"wrong", "wrong", "alice-pw"}
	ki := ssh.KeyboardInteractive(func(_, _ string, q []string, _ []bool) ([]string, error) {
		p := tries[0]
		tries = tries[1:]
		return []string{p}, nil
	})
	if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ki, 3)); err == nil {
		c.Close()
		t.Fatal("third try accepted with MaxAuthTries = 2")
	}
	if len(tries) != 1 {
		t.Fatalf("%d answers left, want 1", len(tries))
	}
}

func TestKeyboardInteractiveCanBeTurnedOff(t *testing.T) {
	if newEnv(t).srv.sshCfg.KeyboardInteractiveCallback == nil {
		t.Fatal("keyboard-interactive is not offered by default")
	}
	e := newEnv(t, func(c *Config) { c.NoKeyboardInteractive = true })
	if e.srv.sshCfg.KeyboardInteractiveCallback != nil {
		t.Fatal("keyboard-interactive offered while off")
	}
	if c, err := e.dialInteractive("alice", answer("alice-pw")); err == nil {
		c.Close()
		t.Fatal("keyboard-interactive accepted while off")
	}
	c, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatalf("password login with keyboard-interactive off: %v", err)
	}
	c.Close()
}

type connMeta struct{ user string }

func (m connMeta) User() string        { return m.user }
func (connMeta) SessionID() []byte     { return nil }
func (connMeta) ClientVersion() []byte { return nil }
func (connMeta) ServerVersion() []byte { return nil }
func (connMeta) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (connMeta) LocalAddr() net.Addr   { return &net.TCPAddr{} }

// ssh refuses an answer count that does not match the questions before the
// callback sees it; the callback must not rely on that.
func TestKeyboardInteractiveNeedsExactlyOneAnswer(t *testing.T) {
	const delay = 50 * time.Millisecond
	a := authenticator{
		users:  func() map[string]User { return map[string]User{"alice": {Password: "alice-pw"}} },
		delay:  delay,
		locate: func(string, User) (string, string) { return "b", "alice/" },
	}
	for _, answers := range [][]string{nil, {"alice-pw", "alice-pw"}} {
		ask := func(string, string, []string, []bool) ([]string, error) { return answers, nil }
		start := time.Now()
		if _, err := a.keyboardInteractive(connMeta{"alice"}, ask); !errors.Is(err, errWrongPassword) {
			t.Errorf("%d answers: err %v, want a wrong password", len(answers), err)
		}
		if took := time.Since(start); took < delay {
			t.Errorf("%d answers: refused after %v, want the %v failure delay", len(answers), took, delay)
		}
	}
	broken := errors.New("connection reset")
	ask := func(string, string, []string, []bool) ([]string, error) { return nil, broken }
	if _, err := a.keyboardInteractive(connMeta{"alice"}, ask); !errors.Is(err, broken) {
		t.Errorf("a failed prompt: err %v, want it passed on", err)
	}
	ask = func(string, string, []string, []bool) ([]string, error) { return []string{"alice-pw"}, nil }
	p, err := a.keyboardInteractive(connMeta{"alice"}, ask)
	if err != nil || p.Extensions[extMethod] != "keyboard-interactive" || p.Extensions[extRoot] != "alice/" {
		t.Fatalf("right answer: %+v, %v", p, err)
	}
}
