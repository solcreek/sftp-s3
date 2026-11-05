package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

// captureJSON sends the default logger, as JSON from debug level up, to a
// buffer for the test's duration, as SFTP_LOG_FORMAT=json would.
func captureJSON(t *testing.T) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type event map[string]any

// records parses every log line; each must be a JSON object.
func records(t *testing.T, logs *syncBuffer) []event {
	t.Helper()
	var out []event
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

// auditEvents returns the audit events logged so far, oldest first.
func auditEvents(t *testing.T, logs *syncBuffer) []event {
	t.Helper()
	var out []event
	for _, ev := range records(t, logs) {
		if _, ok := ev["audit"]; ok {
			out = append(out, ev)
		}
	}
	return out
}

// stopAndCollect stops e's server, which waits for every connection to end
// and log, and returns all audit events.
func stopAndCollect(t *testing.T, e *env, logs *syncBuffer) []event {
	t.Helper()
	e.stop()
	return auditEvents(t, logs)
}

// named returns the events named name.
func named(evs []event, name string) []event {
	var out []event
	for _, ev := range evs {
		if ev["audit"] == name {
			out = append(out, ev)
		}
	}
	return out
}

// only returns the one event named name, failing unless there is exactly one.
func only(t *testing.T, evs []event, name string) event {
	t.Helper()
	found := named(evs, name)
	if len(found) != 1 {
		t.Fatalf("%d %s events, want 1: %v", len(found), name, evs)
	}
	return found[0]
}

// eventNames lists the events' names in order.
func eventNames(evs []event) []any {
	var out []any
	for _, ev := range evs {
		out = append(out, ev["audit"])
	}
	return out
}

// common are the attributes of every audit event.
var common = []string{"time", "level", "msg", "audit", "session", "remote", "user"}

// check asserts that ev is a well-formed audit event whose attributes are
// exactly those in common plus want's, with want's values (nil: any value).
func check(t *testing.T, ev event, want map[string]any) {
	t.Helper()
	keys := slices.Clone(common)
	for k := range want {
		keys = append(keys, k)
	}
	var got []string
	for k := range ev {
		got = append(got, k)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	slices.Sort(got)
	if !slices.Equal(got, keys) {
		t.Errorf("%v event has attributes %v, want %v", ev["audit"], got, keys)
	}
	if ev["level"] != "INFO" || ev["msg"] != ev["audit"] {
		t.Errorf("%v event: level %v, msg %v; want INFO and the event name", ev["audit"], ev["level"], ev["msg"])
	}
	for _, k := range []string{"session", "remote", "user"} {
		if s, ok := ev[k].(string); !ok || s == "" {
			t.Errorf("%v event: %s = %#v, want a non-empty string", ev["audit"], k, ev[k])
		}
	}
	for k, v := range want {
		if v != nil && ev[k] != v {
			t.Errorf("%v event: %s = %#v, want %#v", ev["audit"], k, ev[k], v)
		}
	}
}

// noSecrets fails if any of secrets appears in the logs.
func noSecrets(t *testing.T, logs *syncBuffer, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a secret (%q) reached the log:\n%s", s, logs.String())
		}
	}
}

func TestAuditPasswordLoginAndSessionEnd(t *testing.T) {
	logs := captureJSON(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("carol-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.Users["carol"] = User{PasswordHash: string(hash)} })
	conn, err := e.sshDial("carol", "carol-secret")
	if err != nil {
		t.Fatal(err)
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadDir("/"); err != nil { // listings are not audited
		t.Fatal(err)
	}
	c.Close()
	conn.Close()

	evs := stopAndCollect(t, e, logs)
	if fmt.Sprint(eventNames(evs)) != "[login session_end]" {
		t.Fatalf("events %v, want login and session_end", evs)
	}
	login := only(t, evs, evLogin)
	check(t, login, map[string]any{"user": "carol", "method": "password", "bucket": e.bucket, "root": e.base + "carol/"})
	if !strings.HasPrefix(login["remote"].(string), "127.0.0.1:") {
		t.Errorf("remote = %v", login["remote"])
	}
	if s := login["session"].(string); len(s) != 16 || strings.Trim(s, "0123456789abcdef") != "" {
		t.Errorf("session = %q, want 16 hex digits", s)
	}
	end := only(t, evs, evSessionEnd)
	check(t, end, map[string]any{"user": "carol", "duration_ms": nil, "bytes_in": 0.0, "bytes_out": 0.0})
	if _, ok := end["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %#v, want a number", end["duration_ms"])
	}
	for _, k := range []string{"session", "remote"} {
		if end[k] != login[k] {
			t.Errorf("session_end %s = %v, login's = %v", k, end[k], login[k])
		}
	}
	noSecrets(t, logs, "carol-secret", string(hash))
}

func TestAuditSessionIDsDiffer(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	for range 2 {
		c, err := e.sshDial("alice", "alice-pw")
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	logins := named(stopAndCollect(t, e, logs), evLogin)
	if len(logins) != 2 || logins[0]["session"] == logins[1]["session"] {
		t.Fatalf("logins %v, want two with different session ids", logins)
	}
}

func TestAuditPublicKeyLogin(t *testing.T) {
	logs := captureJSON(t)
	key := newKey(t)
	e := newEnv(t, func(c *Config) { c.Users["dave"] = User{Keys: []ssh.PublicKey{key.PublicKey()}} })
	c, err := e.sshDialKey("dave", key)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	evs := stopAndCollect(t, e, logs)
	if fmt.Sprint(eventNames(evs)) != "[login session_end]" {
		t.Fatalf("events %v, want login and session_end", evs)
	}
	check(t, evs[0], map[string]any{"user": "dave", "method": "publickey",
		"fingerprint": ssh.FingerprintSHA256(key.PublicKey()), "bucket": e.bucket, "root": e.base + "dave/"})
}

// checkFailed checks a login_failed event.
func checkFailed(t *testing.T, ev event, user, method, reason string) {
	t.Helper()
	check(t, ev, map[string]any{"user": user, "method": method, "reason": reason})
}

// keyboardInteractiveOnce makes a client try keyboard-interactive (in these
// tests, with the server not offering it) and then give up.
func keyboardInteractiveOnce(ctx *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
	if slices.Contains(ctx.TriedMethods, "keyboard-interactive") {
		return nil, errors.New("giving up")
	}
	return ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil }), nil
}

// answer is keyboard-interactive answering each question with pass.
func answer(pass string) ssh.AuthMethod {
	return ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = pass
		}
		return answers, nil
	})
}

func TestAuditFailedLogins(t *testing.T) {
	daveKey := newKey(t)
	for _, tc := range []struct {
		name   string
		user   string
		auth   []ssh.AuthMethod
		cb     func(*ssh.ClientAuthContext) (ssh.AuthMethod, error)
		noKI   bool // the server does not offer keyboard-interactive
		method string
		reason string
		n      int // login_failed events
	}{
		{name: "wrong keyboard-interactive answer", user: "alice", auth: []ssh.AuthMethod{answer("not-alice-pw")},
			method: "keyboard-interactive", reason: "wrong password", n: 1},
		{name: "each wrong keyboard-interactive answer", user: "alice", auth: []ssh.AuthMethod{ssh.RetryableAuthMethod(answer("not-alice-pw"), 3)},
			method: "keyboard-interactive", reason: "wrong password", n: 3},
		{name: "keyboard-interactive for an unknown user", user: "mallory", auth: []ssh.AuthMethod{answer("not-alice-pw")},
			method: "keyboard-interactive", reason: "unknown user", n: 1},
		{name: "keyboard-interactive for a key-only user", user: "dave", auth: []ssh.AuthMethod{answer("not-alice-pw")},
			method: "keyboard-interactive", reason: "wrong password", n: 1},
		{name: "wrong password", user: "alice", auth: []ssh.AuthMethod{ssh.Password("not-alice-pw")},
			method: "password", reason: "wrong password", n: 1},
		{name: "each wrong password", user: "alice", auth: []ssh.AuthMethod{ssh.RetryableAuthMethod(ssh.Password("not-alice-pw"), 3)},
			method: "password", reason: "wrong password", n: 3},
		{name: "unknown user", user: "mallory", auth: []ssh.AuthMethod{ssh.Password("not-alice-pw")},
			method: "password", reason: "unknown user", n: 1},
		{name: "password for a key-only user", user: "dave", auth: []ssh.AuthMethod{ssh.Password("not-alice-pw")},
			method: "password", reason: "wrong password", n: 1},
		{name: "no accepted key, however many", user: "dave", auth: []ssh.AuthMethod{ssh.PublicKeys(newKey(t), newKey(t), newKey(t))},
			method: "publickey", reason: "no accepted key", n: 1},
		{name: "key, then a method not offered", user: "dave", auth: []ssh.AuthMethod{ssh.PublicKeys(newKey(t))},
			cb: func(ctx *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
				if len(ctx.TriedMethods) < 2 { // none, then the key from Auth
					return nil, nil
				}
				return keyboardInteractiveOnce(ctx)
			},
			noKI: true, method: "publickey,keyboard-interactive", reason: "no accepted key", n: 1},
		{name: "no accepted method", user: "alice", cb: keyboardInteractiveOnce,
			noKI: true, method: "keyboard-interactive", reason: "no accepted method", n: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureJSON(t)
			e := newEnv(t, func(c *Config) {
				c.Users["dave"] = User{Keys: []ssh.PublicKey{daveKey.PublicKey()}}
				c.NoKeyboardInteractive = tc.noKI
			})
			c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: tc.user, Auth: tc.auth, AuthCallback: tc.cb,
				HostKeyCallback: ssh.FixedHostKey(e.hostPK)})
			if err == nil {
				c.Close()
				t.Fatal("login accepted")
			}
			evs := stopAndCollect(t, e, logs)
			if len(evs) != tc.n || len(named(evs, evLoginFailed)) != tc.n {
				t.Fatalf("events %v, want %d login_failed", evs, tc.n)
			}
			for _, ev := range evs {
				checkFailed(t, ev, tc.user, tc.method, tc.reason)
			}
			noSecrets(t, logs, "not-alice-pw", "alice-pw")
		})
	}
}

// A wrong password, then a key: the password is logged, and no summary
// login_failed follows for the key.
func TestAuditWrongPasswordThenKeyIsLoggedOnce(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong"), ssh.PublicKeys(newKey(t))},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK)})
	if err == nil {
		c.Close()
		t.Fatal("login accepted")
	}
	evs := stopAndCollect(t, e, logs)
	if len(evs) != 1 {
		t.Fatalf("events %v, want one login_failed", evs)
	}
	checkFailed(t, evs[0], "alice", "password", "wrong password")
}

// A wrong password, then the right one: login_failed, then login, with the
// same session.
func TestAuditWrongThenRightPassword(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	tries := []string{"wrong", "alice-pw"}
	next := func() (string, error) { p := tries[0]; tries = tries[1:]; return p, nil }
	c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: "alice",
		Auth:            []ssh.AuthMethod{ssh.RetryableAuthMethod(ssh.PasswordCallback(next), 2)},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK)})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	evs := stopAndCollect(t, e, logs)
	if fmt.Sprint(eventNames(evs)) != "[login_failed login session_end]" {
		t.Fatalf("events %v", evs)
	}
	if evs[0]["session"] != evs[1]["session"] {
		t.Errorf("login_failed and login of one connection have sessions %v and %v", evs[0]["session"], evs[1]["session"])
	}
}

func TestAuditSilentConnectionsAreNotLogged(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if evs := stopAndCollect(t, e, logs); len(evs) != 0 {
		t.Fatalf("events %v, want none", evs)
	}
}

func TestAuditBannedLogins(t *testing.T) {
	t.Run("password during the ban", func(t *testing.T) {
		logs := captureJSON(t)
		e, _ := banEnv(t, func(c *Config) { c.BanThreshold = 2 })
		if c, err := e.dialFrom(attacker, ssh.RetryableAuthMethod(ssh.Password("wrong"), 3)); err == nil {
			c.Close()
			t.Fatal("login accepted")
		}
		// Refused before the handshake: not an audit event.
		if e.loginFrom(attacker, "alice-pw") == nil {
			t.Fatal("banned client logged in")
		}
		var reasons []any
		for _, ev := range stopAndCollect(t, e, logs) {
			checkFailed(t, ev, "alice", "password", ev["reason"].(string))
			reasons = append(reasons, ev["reason"])
		}
		if fmt.Sprint(reasons) != "[wrong password wrong password banned]" {
			t.Fatalf("login_failed reasons %v", reasons)
		}
	})
	t.Run("key during the ban", func(t *testing.T) {
		logs := captureJSON(t)
		e, _ := banEnv(t, func(c *Config) { c.BanThreshold = 1 })
		// The key is offered only once another connection from the same
		// client has got it banned.
		keys := ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			if e.loginFrom(attacker, "wrong") == nil {
				return nil, errors.New("wrong password accepted")
			}
			return []ssh.Signer{newKey(t)}, nil
		})
		if c, err := e.dialFrom(attacker, keys); err == nil {
			c.Close()
			t.Fatal("login accepted")
		}
		evs := stopAndCollect(t, e, logs)
		if len(evs) != 2 {
			t.Fatalf("events %v, want two login_failed", evs)
		}
		checkFailed(t, evs[0], "alice", "password", "wrong password")
		checkFailed(t, evs[1], "alice", "publickey", "banned")
	})
}

// A login accepted before its client was banned, refused by the check after
// the handshake, is a login_failed with reason banned, and nothing more.
func TestAuditBanArrivingDuringALogin(t *testing.T) {
	logs := captureJSON(t)
	client, bucket, _ := memoryStore(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	srv, err := New(Config{Client: client, Bucket: bucket, HostKeys: []ssh.Signer{signer},
		Users: map[string]User{"alice": {Password: "alice-pw"}}, ProxyProtocol: true, BanThreshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	pass := srv.sshCfg.PasswordCallback // before Serve, as in TestBanArrivingDuringALoginIsHonoured
	srv.sshCfg.PasswordCallback = func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		perms, err := pass(c, p)
		if err == nil {
			srv.bans.fail(c.RemoteAddr())
		}
		return perms, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	e := &env{t: t, addr: ln.Addr().String(), hostPK: signer.PublicKey()}
	if c, err := e.dialFrom(attacker, ssh.Password("alice-pw")); err == nil {
		c.Close()
	}
	ln.Close()
	if err := <-served; err != nil { // Serve waits for the connection
		t.Fatal(err)
	}
	evs := auditEvents(t, logs)
	if len(evs) != 1 {
		t.Fatalf("events %v, want one login_failed", evs)
	}
	checkFailed(t, evs[0], "alice", "password", "banned")
	if evs[0]["remote"] != attacker {
		t.Errorf("remote = %v, want %s", evs[0]["remote"], attacker)
	}
}

func TestFailReason(t *testing.T) {
	for err, want := range map[error]string{
		errBanned:                             "banned",
		fmt.Errorf("x: %w", errUnknownUser):   "unknown user",
		fmt.Errorf("x: %w", errWrongPassword): "wrong password",
	} {
		if got := failReason(err); got != want {
			t.Errorf("failReason(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestLoggedName(t *testing.T) {
	long := strings.Repeat("a", 63) + "é" + strings.Repeat("b", 100) // é straddles byte 64
	for in, want := range map[string]string{
		"alice":                 "alice",
		strings.Repeat("x", 64): strings.Repeat("x", 64),
		strings.Repeat("x", 65): strings.Repeat("x", 64) + "...(65 bytes)",
		long:                    strings.Repeat("a", 63) + "...(165 bytes)",
		"bad\xffutf8":           "bad�utf8",
	} {
		if got := loggedName(in); got != want {
			t.Errorf("loggedName(%.20q...) = %q, want %q", in, got, want)
		}
	}
}

// A client-chosen user name is capped in every record that carries it.
func TestAuditCapsLongUserNames(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	huge := strings.Repeat("u", 30000)
	if c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: huge,
		Auth:            []ssh.AuthMethod{ssh.Password("x"), ssh.PublicKeys(newKey(t))},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK)}); err == nil {
		c.Close()
		t.Fatal("login accepted")
	}
	evs := stopAndCollect(t, e, logs)
	if len(named(evs, evLoginFailed)) == 0 {
		t.Fatalf("no login_failed in %v", evs)
	}
	if strings.Contains(logs.String(), strings.Repeat("u", maxLoggedName+1)) {
		t.Fatal("a user name longer than the cap reached the log")
	}
	for _, ev := range named(evs, evLoginFailed) {
		if want := strings.Repeat("u", maxLoggedName) + "...(30000 bytes)"; ev["user"] != want {
			t.Errorf("user = %.80v, want the capped name", ev["user"])
		}
	}
}
