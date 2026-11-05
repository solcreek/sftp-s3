package gateway

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
)

// Metrics are counted in memory, per process, and written in the
// Prometheus text exposition format (version 0.0.4) by WriteMetrics.
//
// Every label value comes from a fixed, small set below, never from the
// client: no user names, keys, addresses or error texts.

// Label values. The last entry of each set is "other", which catches a
// value no entry names, so the set of series stays fixed whatever is passed.
var (
	loginMethods  = [...]string{"password", "publickey", "keyboard-interactive", "other"}
	failReasons   = [...]string{"unknown user", "wrong password", "banned", "no accepted method", "no accepted key", "other"}
	opEvents      = [...]string{evUpload, evDownload, evMkdir, evRemove, evRmdir, evRename, evSetstat}
	opResults     = [...]string{"ok", "failed"}
	rejectReasons = [...]string{"limit", "banned"}
)

// Indexes into rejectReasons.
const (
	rejectLimit = iota
	rejectBanned
)

// metrics holds a server's counters. Audit events feed it through audit, so
// the counts and the audit log cannot disagree; the connection counts are
// fed by Serve and handleConn. Hot paths only add to atomics.
type metrics struct {
	connections   atomic.Int64
	rejected      [len(rejectReasons)]atomic.Int64 // by rejectReasons
	sessions      atomic.Int64                     // logged in now
	logins        [len(loginMethods)]atomic.Int64  // by loginMethods
	loginFailures [len(failReasons)]atomic.Int64   // by failReasons
	ops           [len(opEvents)][len(opResults)]atomic.Int64
	bytesIn       atomic.Int64
	bytesOut      atomic.Int64
}

// index returns where v is in set, or the last index ("other").
func index(set []string, v string) int {
	if i := slices.Index(set, v); i >= 0 {
		return i
	}
	return len(set) - 1
}

// attr returns the value of key in attrs, alternating keys and values as
// slog takes them, or nil.
func attr(attrs []any, key string) any {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key {
			return attrs[i+1]
		}
	}
	return nil
}

// record counts one audit event with the attributes it is logged with.
func (m *metrics) record(event string, attrs []any) {
	str := func(key string) string { s, _ := attr(attrs, key).(string); return s }
	switch event {
	case evLogin:
		m.sessions.Add(1)
		m.logins[index(loginMethods[:], str("method"))].Add(1)
	case evSessionEnd:
		m.sessions.Add(-1)
	case evLoginFailed:
		m.loginFailures[index(failReasons[:], str("reason"))].Add(1)
	default:
		op := slices.Index(opEvents[:], event)
		if op < 0 {
			return
		}
		res := 0
		if str("result") != "ok" {
			res = 1
		}
		m.ops[op][res].Add(1)
		n, _ := attr(attrs, "bytes").(int64)
		switch event {
		case evUpload:
			m.bytesIn.Add(n)
		case evDownload:
			m.bytesOut.Add(n)
		}
	}
}

func (m *metrics) accepted() { m.connections.Add(1) }

func (m *metrics) reject(reason int) { m.rejected[reason].Add(1) }

// WriteMetrics writes the server's metrics to w in the Prometheus text
// exposition format, version 0.0.4 (Content-Type
// "text/plain; version=0.0.4; charset=utf-8").
func (s *Server) WriteMetrics(w io.Writer) error {
	m := &s.metrics
	var e expo
	e.family("sftp_s3_build_info", "gauge", "Always 1; the version label is the running version.")
	e.sample("sftp_s3_build_info", 1, "version", s.cfg.Version)

	e.family("sftp_s3_connections_total", "counter", "TCP connections accepted, including those then rejected.")
	e.sample("sftp_s3_connections_total", m.connections.Load())
	e.family("sftp_s3_connections_rejected_total", "counter", "Connections closed before the SSH handshake: limit (SFTP_MAX_CONNECTIONS reached) or banned (client is banned).")
	for i, r := range rejectReasons {
		e.sample("sftp_s3_connections_rejected_total", m.rejected[i].Load(), "reason", r)
	}
	e.family("sftp_s3_connections_open", "gauge", "Connections open now, logged in or not.")
	e.sample("sftp_s3_connections_open", int64(len(s.slots)))
	e.family("sftp_s3_connections_max", "gauge", "The connection limit (SFTP_MAX_CONNECTIONS).")
	e.sample("sftp_s3_connections_max", int64(s.cfg.MaxConns))

	e.family("sftp_s3_sessions_active", "gauge", "Logged-in connections now.")
	e.sample("sftp_s3_sessions_active", m.sessions.Load())
	e.family("sftp_s3_logins_total", "counter", "Successful logins (audit event login), by method.")
	for i, v := range loginMethods {
		e.sample("sftp_s3_logins_total", m.logins[i].Load(), "method", v)
	}
	e.family("sftp_s3_login_failures_total", "counter", "Failed logins (audit event login_failed), by reason.")
	for i, v := range failReasons {
		e.sample("sftp_s3_login_failures_total", m.loginFailures[i].Load(), "reason", v)
	}

	e.family("sftp_s3_operations_total", "counter", "File operations (audit events upload, download, mkdir, remove, rmdir, rename, setstat), by op and result.")
	for i, op := range opEvents {
		for j, r := range opResults {
			e.sample("sftp_s3_operations_total", m.ops[i][j].Load(), "op", op, "result", r)
		}
	}
	e.family("sftp_s3_bytes_received_total", "counter", "Upload payload bytes received over SFTP, counted when each upload ends.")
	e.sample("sftp_s3_bytes_received_total", m.bytesIn.Load())
	e.family("sftp_s3_bytes_sent_total", "counter", "Download payload bytes sent over SFTP, counted when each download ends.")
	e.sample("sftp_s3_bytes_sent_total", m.bytesOut.Load())

	total, now := s.bans.counts()
	e.family("sftp_s3_bans_total", "counter", "Clients banned for failed logins.")
	e.sample("sftp_s3_bans_total", total)
	e.family("sftp_s3_banned_clients", "gauge", "Clients (IPv4 addresses or IPv6 /64s) banned now.")
	e.sample("sftp_s3_banned_clients", int64(now))

	_, err := w.Write(e.buf.Bytes())
	return err
}

// expo builds a Prometheus text exposition (version 0.0.4).
type expo struct{ buf bytes.Buffer }

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

// family starts a metric family: its HELP and TYPE lines.
func (e *expo) family(name, typ, help string) {
	fmt.Fprintf(&e.buf, "# HELP %s %s\n# TYPE %s %s\n", name, helpEscaper.Replace(help), name, typ)
}

// sample writes one sample; labels alternate names and values.
func (e *expo) sample(name string, v int64, labels ...string) {
	e.buf.WriteString(name)
	if len(labels) > 0 {
		e.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			fmt.Fprintf(&e.buf, `%s="%s"`, labels[i], labelEscaper.Replace(labels[i+1]))
		}
		e.buf.WriteByte('}')
	}
	fmt.Fprintf(&e.buf, " %d\n", v)
}
