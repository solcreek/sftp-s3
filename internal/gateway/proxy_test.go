package gateway

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
	"golang.org/x/crypto/ssh"
)

// captureLogs sends the default logger to a buffer for the test's duration.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialWithHeader logs in as alice over a connection that starts with header
// (nil: no header).
func (e *env) dialWithHeader(header []byte) (*ssh.Client, error) {
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		return nil, err
	}
	if header != nil {
		if _, err := raw.Write(header); err != nil {
			return nil, err
		}
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, e.addr, &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("alice-pw")},
		HostKeyCallback: ssh.FixedHostKey(e.hostPK),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		raw.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func proxyHeader(t *testing.T, version byte, src string) []byte {
	t.Helper()
	srcAddr, _ := net.ResolveTCPAddr("tcp", src)
	dst, _ := net.ResolveTCPAddr("tcp", "10.0.0.1:22")
	b, err := proxyproto.HeaderProxyFromAddrs(version, srcAddr, dst).Format()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProxyProtocolReportsTheClient(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version byte
		src     string
	}{
		{"v1", 1, "203.0.113.7:4242"},
		{"v2", 2, "203.0.113.8:4343"},
		{"v2 ipv6", 2, "[2001:db8::9]:4444"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			e := newEnv(t, func(c *Config) { c.ProxyProtocol = true })
			c, err := e.dialWithHeader(proxyHeader(t, tc.version, tc.src))
			if err != nil {
				t.Fatalf("login behind a PROXY %s header: %v", tc.name, err)
			}
			c.Close()
			waitForLog(t, logs, "remote="+tc.src)
		})
	}
}

func TestProxyProtocolLocalHeaderKeepsTheProxyAddress(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, func(c *Config) { c.ProxyProtocol = true })
	local, err := (&proxyproto.Header{Version: 2, Command: proxyproto.LOCAL}).Format()
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.dialWithHeader(local)
	if err != nil {
		t.Fatalf("login behind a LOCAL header: %v", err)
	}
	c.Close()
	waitForLog(t, logs, "remote=127.0.0.1:")
}

func TestProxyProtocolIsRequiredWhenOn(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, func(c *Config) { c.ProxyProtocol = true })
	if c, err := e.dialWithHeader(nil); err == nil {
		c.Close()
		t.Fatal("login without a PROXY header was accepted")
	}
	waitForLog(t, logs, "level=INFO msg=\"no valid PROXY protocol header\"")
}

// With the option off, a client cannot claim another address: SSH ignores
// text lines before its version line, so a v1 header is skipped, and the
// connection's own address is logged.
func TestProxyProtocolOffCannotSpoofTheClient(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t)
	if c, err := e.dialWithHeader(proxyHeader(t, 1, "203.0.113.7:4242")); err == nil {
		c.Close()
	}
	if c, err := e.dialWithHeader(proxyHeader(t, 2, "203.0.113.8:4343")); err == nil {
		c.Close()
		t.Error("a binary v2 header did not break the handshake")
	}
	e.sftp("alice") // a final login, so both attempts above were logged
	if strings.Contains(logs.String(), "203.0.113.") {
		t.Fatalf("a client-supplied address reached the log:\n%s", logs.String())
	}
}

func TestProxyProtocolHealthCheckIsQuiet(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, func(c *Config) { c.ProxyProtocol = true })
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	e.sshDialHeader(t)
	if strings.Contains(logs.String(), "level=INFO msg=\"no valid PROXY") {
		t.Fatalf("a connect-and-close probe was logged at info:\n%s", logs.String())
	}
}

// sshDialHeader logs in once with a header, proving earlier probes were handled.
func (e *env) sshDialHeader(t *testing.T) {
	t.Helper()
	c, err := e.dialWithHeader(proxyHeader(t, 2, "203.0.113.1:1000"))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestProxyProtocolHeaderReadIsBoundedByLoginTimeout(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.ProxyProtocol = true
		c.LoginTimeout = 200 * time.Millisecond
	})
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetReadDeadline(time.Now().Add(5 * time.Second)) // test guard only
	start := time.Now()
	if _, err := io.Copy(io.Discard, raw); err != nil {
		t.Fatalf("server did not close a connection that sent no header: %v", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Fatalf("closed after %v, before the login timeout", took)
	}
}

func TestReadProxyHeaderKeepsTheBytesAfterIt(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		client.Write(append(proxyHeader(t, 2, "198.51.100.2:5555"), "SSH-2.0-test\r\n"...))
	}()
	pc, err := readProxyHeader(server)
	if err != nil {
		t.Fatal(err)
	}
	if got := pc.RemoteAddr().String(); got != "198.51.100.2:5555" {
		t.Fatalf("RemoteAddr = %s", got)
	}
	buf := make([]byte, 14)
	if _, err := io.ReadFull(pc, buf); err != nil || string(buf) != "SSH-2.0-test\r\n" {
		t.Fatalf("bytes after the header: %q, %v", buf, err)
	}
}
