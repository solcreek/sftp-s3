package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Audit events are ordinary log records at INFO. Each carries audit=<event>,
// the attribute a collector filters on, and has the event name as its
// message. Every event of one connection carries the same session, remote
// and (once known) user, so a session's events can be joined.
const (
	evLogin       = "login"
	evLoginFailed = "login_failed"
	evUpload      = "upload"
	evDownload    = "download"
	evMkdir       = "mkdir"
	evRemove      = "remove"
	evRmdir       = "rmdir"
	evRename      = "rename"
	evSetstat     = "setstat"
	evSessionEnd  = "session_end"
)

// maxLoggedName caps client-chosen strings (user names, auth method names)
// in logs: a client may send one as large as an SSH packet, and failed
// logins repeat it.
const maxLoggedName = 64

// loggedName returns s for a log record, cut to maxLoggedName bytes on a
// rune boundary, with the original length noted when cut.
func loggedName(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= maxLoggedName {
		return s
	}
	cut := maxLoggedName
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(%d bytes)", s[:cut], len(s))
}

// audit logs one audit event.
func audit(log *slog.Logger, event string, attrs ...any) {
	log.Info(event, append([]any{"audit", event}, attrs...)...)
}

// result gives an event's outcome: result=ok, or result=failed and the
// error's text. Errors reaching here come from S3 calls, the SFTP stream
// and the gateway itself; none of them carries a credential.
func result(err error) []any {
	if err == nil {
		return []any{"result", "ok"}
	}
	return []any{"result", "failed", "error", err.Error()}
}

// newSessionID returns a random id for one connection: 8 bytes, in hex.
func newSessionID() string {
	var b [8]byte
	rand.Read(b[:]) // never fails; see crypto/rand.Read
	return hex.EncodeToString(b[:])
}

// session is one logged-in connection.
type session struct {
	log      *slog.Logger // carries session, remote and user
	start    time.Time
	bytesIn  atomic.Int64 // upload payload
	bytesOut atomic.Int64 // download payload
}

func newSession(log *slog.Logger) *session {
	return &session{log: log, start: time.Now()}
}

// end logs session_end.
func (s *session) end() {
	audit(s.log, evSessionEnd, "duration_ms", time.Since(s.start).Milliseconds(),
		"bytes_in", s.bytesIn.Load(), "bytes_out", s.bytesOut.Load())
}

// transfer logs an upload or download of n bytes that began at start.
func (s *session) transfer(event, bucket, key string, n int64, start time.Time, err error) {
	attrs := []any{"bucket", bucket, "key", key, "bytes", n, "duration_ms", time.Since(start).Milliseconds()}
	audit(s.log, event, append(attrs, result(err)...)...)
}

// Reasons a login failed, as login_failed reports them.
var (
	errUnknownUser   = errors.New("unknown user")
	errWrongPassword = errors.New("wrong password")
)

// failReason is the reason login_failed gives for a rejected password.
func failReason(err error) string {
	switch {
	case errors.Is(err, errBanned):
		return "banned"
	case errors.Is(err, errUnknownUser):
		return "unknown user"
	}
	return "wrong password"
}
