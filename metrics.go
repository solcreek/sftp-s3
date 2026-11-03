package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// metricsAddr returns SFTP_METRICS_ADDR, the address of the HTTP listener
// for /healthz and /metrics, or "" when it is unset (no listener at all).
// It must be host:port or :port with a numeric port, and not the SFTP port.
func metricsAddr(getenv func(string) string) (string, error) {
	addr := getenv("SFTP_METRICS_ADDR")
	if addr == "" {
		return "", nil
	}
	_, p, err := net.SplitHostPort(addr)
	port, perr := strconv.Atoi(p)
	if err != nil || perr != nil || port < 0 || port > 65535 {
		return "", fmt.Errorf("SFTP_METRICS_ADDR: %q must be :port or host:port with a port from 0 to 65535, like :9090 or 127.0.0.1:9090", addr)
	}
	sftpPort := orDefault(getenv("PORT"), "2222")
	if n, err := strconv.Atoi(sftpPort); err == nil && n == port && port != 0 {
		return "", fmt.Errorf("SFTP_METRICS_ADDR: %q is the SFTP port (PORT=%s); give the HTTP listener a port of its own", addr, sftpPort)
	}
	return addr, nil
}

// newHTTPHandler serves GET (and HEAD) /healthz and /metrics for srv.
// Anything else is 404, or 405 for another method on those paths.
//
// /healthz answers 200 "ok" while srv accepts SFTP connections and 503 once
// shutdown has begun (open sessions draining), so a load balancer or a
// readiness probe stops sending new clients. It does not call S3.
func newHTTPHandler(srv *gateway.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !srv.Serving() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "not accepting connections\n")
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		srv.WriteMetrics(w) // an error means the scraper went away
	})
	return mux
}

// newHTTPServer wraps h with timeouts, so slow or idle clients cannot hold
// connections open: the listener has no authentication and no connection
// limit of its own.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
}

// serveHTTP listens on addr and serves h until stop is called. It returns
// the address it bound, which differs from addr for port 0.
func serveHTTP(addr string, h http.Handler) (stop func(), bound string, err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("SFTP_METRICS_ADDR: %w", err)
	}
	hs := newHTTPServer(h)
	go hs.Serve(ln)
	return func() { hs.Close() }, ln.Addr().String(), nil
}
