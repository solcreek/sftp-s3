package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

func TestMetricsAddr(t *testing.T) {
	for _, tc := range []struct {
		addr, port string
		want       string // the error mentions it; "" means valid
	}{
		{"", "", ""},
		{":9090", "", ""},
		{"127.0.0.1:9090", "", ""},
		{"[::1]:9090", "", ""},
		{"localhost:9090", "", ""},
		{":0", "", ""},
		{":2222", "2022", ""},
		{"9090", "", "must be :port or host:port"},
		{":http", "", "must be :port or host:port"},
		{":70000", "", "must be :port or host:port"},
		{":-1", "", "must be :port or host:port"},
		{"127.0.0.1:", "", "must be :port or host:port"},
		{":2222", "", "is the SFTP port (PORT=2222)"},
		{"127.0.0.1:2022", "2022", "is the SFTP port (PORT=2022)"},
	} {
		got, err := metricsAddr(envMap(map[string]string{"SFTP_METRICS_ADDR": tc.addr, "PORT": tc.port}))
		switch {
		case tc.want == "" && (err != nil || got != tc.addr):
			t.Errorf("%q (PORT %q): %q, %v; want it accepted", tc.addr, tc.port, got, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "SFTP_METRICS_ADDR")):
			t.Errorf("%q (PORT %q): err = %v, want it to mention %q", tc.addr, tc.port, err, tc.want)
		}
	}
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestHTTPHandler(t *testing.T) {
	isolateAWS(t)
	keyPEM, _ := testKeyPEM(t)
	cfg, err := loadConfig(context.Background(), envMap(map[string]string{
		"S3_BUCKET": "b", "S3_ENDPOINT": "http://127.0.0.1:1", "SFTP_USERS": "a:b", "SFTP_HOST_KEY": keyPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := newHTTPHandler(srv)

	if w := get(t, h, "GET", "/healthz"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz before Serve: %d", w.Code)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	waitFor(t, srv.Serving)

	w := get(t, h, "GET", "/healthz")
	if w.Code != http.StatusOK || w.Body.String() != "ok\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("/healthz while serving: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w := get(t, h, "HEAD", "/healthz"); w.Code != http.StatusOK {
		t.Errorf("HEAD /healthz: %d", w.Code)
	}
	w = get(t, h, "GET", "/metrics")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" ||
		!strings.Contains(w.Body.String(), "\nsftp_s3_build_info{version=\"dev\"} 1\n") {
		t.Errorf("/metrics: %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/", http.StatusNotFound},
		{"GET", "/metrics/x", http.StatusNotFound},
		{"GET", "/healthz/", http.StatusNotFound},
		{"GET", "/debug/pprof/", http.StatusNotFound},
		{"POST", "/metrics", http.StatusMethodNotAllowed},
		{"DELETE", "/healthz", http.StatusMethodNotAllowed},
	} {
		if w := get(t, h, tc.method, tc.path); w.Code != tc.code {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, w.Code, tc.code)
		}
	}

	ln.Close() // shutdown begins
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	w = get(t, h, "GET", "/healthz")
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "not accepting connections\n" {
		t.Errorf("/healthz after shutdown: %d %q", w.Code, w.Body.String())
	}
}

func TestHTTPServerHasTimeouts(t *testing.T) {
	hs := newHTTPServer(http.NotFoundHandler())
	if hs.ReadHeaderTimeout <= 0 || hs.ReadTimeout <= 0 || hs.WriteTimeout <= 0 || hs.IdleTimeout <= 0 || hs.MaxHeaderBytes <= 0 {
		t.Errorf("server without limits: %+v", hs)
	}
}

// TestRunServesHealthAndMetrics runs the whole process: /healthz is 200
// while serving, 503 once the context is cancelled while a session is still
// open, and the HTTP listener closes when run returns.
func TestRunServesHealthAndMetrics(t *testing.T) {
	isolateAWS(t)
	backend := s3mem.New()
	if err := backend.CreateBucket("uploads"); err != nil {
		t.Fatal(err)
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	keyPEM, hostPub := testKeyPEM(t)
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, envMap(map[string]string{
			"PORT": "0", "S3_BUCKET": "uploads", "S3_ENDPOINT": store.URL,
			"SFTP_USERS": "alice:alice-pw", "SFTP_HOST_KEY": keyPEM,
			"SFTP_METRICS_ADDR": "127.0.0.1:0",
		}))
	}()
	sftpRE := regexp.MustCompile(`msg=listening .*addr=\S*:(\d+)`)
	httpRE := regexp.MustCompile(`msg="http listening" addr=(\S+)`)
	var sftpPort, httpAddr string
	waitFor(t, func() bool {
		s, h := sftpRE.FindStringSubmatch(logs.String()), httpRE.FindStringSubmatch(logs.String())
		if s != nil && h != nil {
			sftpPort, httpAddr = s[1], h[1]
		}
		return s != nil && h != nil
	})
	client := &http.Client{Timeout: 5 * time.Second}
	fetch := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get("http://" + httpAddr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	waitFor(t, func() bool { code, _ := fetch("/healthz"); return code == http.StatusOK })

	conn, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", sftpPort), &ssh.ClientConfig{
		User: "alice", Auth: []ssh.AuthMethod{ssh.Password("alice-pw")},
		HostKeyCallback: ssh.FixedHostKey(hostPub), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if code, body := fetch("/metrics"); code != http.StatusOK ||
		!strings.Contains(body, "\nsftp_s3_sessions_active 1\n") ||
		!strings.Contains(body, "\nsftp_s3_logins_total{method=\"password\"} 1\n") {
		t.Errorf("/metrics: %d\n%s", code, body)
	}

	cancel() // SIGTERM: stop accepting, drain alice's session
	waitFor(t, func() bool { code, _ := fetch("/healthz"); return code == http.StatusServiceUnavailable })
	select {
	case err := <-done:
		t.Fatalf("run returned with a session open: %v", err)
	default:
	}
	if s, err := conn.NewSession(); err != nil {
		t.Errorf("open session broke during the drain: %v", err)
	} else {
		s.Close()
	}

	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	if _, err := client.Get("http://" + httpAddr + "/healthz"); err == nil {
		t.Error("HTTP listener still open after run returned")
	}
}

func TestRunHTTPListenerErrors(t *testing.T) {
	isolateAWS(t)
	base := func(extra map[string]string) func(string) string {
		env := map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "PORT": "0"}
		for k, v := range extra {
			env[k] = v
		}
		return envMap(env)
	}
	// A regression would serve instead of failing: bound it, so the test fails
	// rather than hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run(ctx, base(map[string]string{"SFTP_METRICS_ADDR": "nonsense"})); err == nil || !strings.Contains(err.Error(), "SFTP_METRICS_ADDR") {
		t.Errorf("bad address: err = %v", err)
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	// A port that was free a moment ago, for SFTP: run must release it
	// when the HTTP listener fails.
	free, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	_, sftpPort, _ := net.SplitHostPort(free.Addr().String())
	free.Close()
	if err := run(ctx, base(map[string]string{"PORT": sftpPort, "SFTP_METRICS_ADDR": busy.Addr().String()})); err == nil || !strings.Contains(err.Error(), "SFTP_METRICS_ADDR") {
		t.Errorf("busy port: err = %v", err)
	}
	again, err := net.Listen("tcp", ":"+sftpPort)
	if err != nil {
		t.Fatalf("SFTP port still held after the HTTP listener failed: %v", err)
	}
	again.Close()
}

// Without SFTP_METRICS_ADDR there is no HTTP listener at all.
func TestRunWithoutMetricsAddr(t *testing.T) {
	isolateAWS(t)
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, envMap(map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "PORT": "0"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "msg=listening") || strings.Contains(logs.String(), "http listening") {
		t.Errorf("logs:\n%s", logs.String())
	}
}
