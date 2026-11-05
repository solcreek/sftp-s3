package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// samples maps a series, written as name{k="v",...} with labels sorted and
// values unescaped, to its value.
type samples map[string]float64

// series names one series as samples keys it; labels alternate names and values.
func series(name string, labels ...string) string {
	var kv []string
	for i := 0; i+1 < len(labels); i += 2 {
		kv = append(kv, labels[i]+"="+strconv.Quote(labels[i+1]))
	}
	slices.Sort(kv)
	return name + "{" + strings.Join(kv, ",") + "}"
}

var (
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*`)
)

// parseExposition checks text against the Prometheus text exposition format
// 0.0.4, as far as the gateway uses it (counters and gauges, no timestamps),
// and returns its samples. Each family needs HELP then TYPE before its
// samples, appears once, and counters are named *_total.
func parseExposition(text string) (samples, error) {
	if text == "" || !strings.HasSuffix(text, "\n") {
		return nil, errors.New("empty, or not ending in a newline")
	}
	out := samples{}
	declared := map[string]bool{}
	var family, help string
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(format string, a ...any) (samples, error) {
			return nil, fmt.Errorf("line %d %q: %s", n+1, line, fmt.Sprintf(format, a...))
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, text, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !metricName.MatchString(name) || declared[name] {
				return fail("bad or repeated family name")
			}
			if err := checkHelpEscapes(text); err != nil {
				return fail("%v", err)
			}
			help, family = name, ""
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(f) != 2 || f[0] != help {
				return fail("TYPE without its HELP line")
			}
			switch f[1] {
			case "counter":
				if !strings.HasSuffix(f[0], "_total") {
					return fail("counter not named *_total")
				}
			case "gauge":
			default:
				return fail("type %q", f[1])
			}
			family, declared[f[0]] = f[0], true
		case strings.HasPrefix(line, "#"):
			return fail("unknown comment")
		default:
			key, value, err := parseSample(line)
			if err != nil {
				return fail("%v", err)
			}
			if family == "" || !strings.HasPrefix(key, family+"{") {
				return fail("sample outside its family %q", family)
			}
			if _, dup := out[key]; dup {
				return fail("repeated series")
			}
			out[key] = value
		}
	}
	return out, nil
}

// checkHelpEscapes accepts only \\ and \n after a backslash.
func checkHelpEscapes(text string) error {
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if i+1 == len(text) || text[i+1] != '\\' && text[i+1] != 'n' {
			return fmt.Errorf("bad escape at byte %d of HELP", i)
		}
		i++
	}
	return nil
}

// parseSample parses `name{l="v",...} value` or `name value`.
func parseSample(line string) (key string, value float64, err error) {
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return "", 0, errors.New("no value")
	}
	name, rest := line[:i], line[i:]
	if !metricName.MatchString(name) {
		return "", 0, errors.New("bad metric name")
	}
	var labels []string
	if rest[0] == '{' {
		rest = rest[1:]
		for !strings.HasPrefix(rest, "}") {
			if len(labels) > 0 {
				if !strings.HasPrefix(rest, ",") {
					return "", 0, errors.New("labels not separated by commas")
				}
				rest = rest[1:]
			}
			l := labelName.FindString(rest)
			if l == "" || !strings.HasPrefix(rest[len(l):], `="`) {
				return "", 0, errors.New("bad label")
			}
			rest = rest[len(l)+2:]
			var v strings.Builder
			for {
				if rest == "" {
					return "", 0, errors.New("unterminated label value")
				}
				c := rest[0]
				rest = rest[1:]
				if c == '"' {
					break
				}
				if c == '\n' {
					return "", 0, errors.New("raw newline in label value")
				}
				if c == '\\' {
					if rest == "" {
						return "", 0, errors.New("unterminated label value")
					}
					switch rest[0] {
					case '\\', '"':
						c = rest[0]
					case 'n':
						c = '\n'
					default:
						return "", 0, fmt.Errorf("bad escape \\%c", rest[0])
					}
					rest = rest[1:]
				}
				v.WriteByte(c)
			}
			if slices.Contains(labels, l) {
				return "", 0, errors.New("repeated label")
			}
			labels = append(labels, l, v.String())
		}
		rest = rest[1:]
	}
	if !strings.HasPrefix(rest, " ") {
		return "", 0, errors.New("no space before the value")
	}
	value, err = strconv.ParseFloat(rest[1:], 64)
	if err != nil {
		return "", 0, err
	}
	return series(name, labels...), value, nil
}

func TestParseExpositionRejectsBadInput(t *testing.T) {
	good := "# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"v\"} 1\n"
	if s, err := parseExposition(good); err != nil || s[series("a_total", "l", "v")] != 1 {
		t.Fatalf("good input: %v %v", s, err)
	}
	for _, bad := range []string{
		"",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total 1",     // no final newline
		"# TYPE a_total counter\na_total 1\n",                         // no HELP
		"# HELP a_total Help.\n# TYPE a_total histogram\na_total 1\n", // unsupported type
		"# HELP a Help.\n# TYPE a counter\na 1\n",                     // counter without _total
		"# HELP a_total Help.\n# TYPE a_total counter\nb_total 1\n",   // sample of another family
		"a_total 1\n", // undeclared
		"# HELP a_total Help.\n# TYPE a_total counter\na_total 1\na_total 2\n",
		"# HELP a_total Bad \\t escape.\n# TYPE a_total counter\na_total 1\n",
		"# HELP a_total Trailing \\\n# TYPE a_total counter\na_total 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\n# HELP a_total Again.\n",
		"# HELP a_total Help.\n# TYPE a_total counter\n# other\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total x\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"v\"}1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"v\" k=\"w\"} 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=v} 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"v} 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"v\\",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"\\t\"} 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"a\",l=\"b\"} 1\n",
		"# HELP a_total Help.\n# TYPE a_total counter\na_total{l=\"a\nb\"} 1\n",
		"# HELP 1a_total Help.\n# TYPE 1a_total counter\n",
		"# HELP a_total Help.\n# TYPE a_total counter\n1a_total 1\n",
	} {
		if _, err := parseExposition(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// scrape returns s's metrics, checked against the exposition format.
func scrape(t *testing.T, s *Server) samples {
	t.Helper()
	var b strings.Builder
	if err := s.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	m, err := parseExposition(b.String())
	if err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	return m
}

// expect asserts that each series in want (series key -> value) has that value.
func expect(t *testing.T, got samples, want map[string]float64) {
	t.Helper()
	for _, k := range slices.Sorted(maps.Keys(want)) {
		v, ok := got[k]
		if !ok {
			t.Errorf("%s missing", k)
		} else if v != want[k] {
			t.Errorf("%s = %v, want %v", k, v, want[k])
		}
	}
}

func TestExpositionEscapes(t *testing.T) {
	var e expo
	e.family("x_total", "counter", "back\\slash\nnewline")
	e.sample("x_total", 7, "a", "q\"uote\\back\nnl", "b", "plain")
	want := "# HELP x_total back\\\\slash\\nnewline\n# TYPE x_total counter\n" +
		`x_total{a="q\"uote\\back\nnl",b="plain"} 7` + "\n"
	if got := e.buf.String(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	s, err := parseExposition(e.buf.String())
	if err != nil {
		t.Fatal(err)
	}
	expect(t, s, map[string]float64{series("x_total", "a", "q\"uote\\back\nnl", "b", "plain"): 7})
}

// Every series exists from the start, at zero, so dashboards and alerts
// need no absent() guards; the label sets are fixed.
func TestMetricsStartAtZero(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Version = `1.2.3"x` })
	got := scrape(t, e.srv)
	want := map[string]float64{
		series("sftp_s3_build_info", "version", `1.2.3"x`): 1,
		series("sftp_s3_connections_total"):                0,
		series("sftp_s3_connections_open"):                 0,
		series("sftp_s3_connections_max"):                  32,
		series("sftp_s3_sessions_active"):                  0,
		series("sftp_s3_bytes_received_total"):             0,
		series("sftp_s3_bytes_sent_total"):                 0,
		series("sftp_s3_bans_total"):                       0,
		series("sftp_s3_banned_clients"):                   0,
	}
	for _, r := range rejectReasons {
		want[series("sftp_s3_connections_rejected_total", "reason", r)] = 0
	}
	for _, m := range loginMethods {
		want[series("sftp_s3_logins_total", "method", m)] = 0
	}
	for _, r := range failReasons {
		want[series("sftp_s3_login_failures_total", "reason", r)] = 0
	}
	for _, op := range opEvents {
		for _, r := range opResults {
			want[series("sftp_s3_operations_total", "op", op, "result", r)] = 0
		}
	}
	expect(t, got, want)
	if len(got) != len(want) {
		t.Errorf("%d series, want %d: %v", len(got), len(want), got)
	}
}

// countEvents counts audit events by a label, as the metrics do: the keys
// are "<event>" or "<event>/<attr value>".
func countEvents(evs []event) map[string]float64 {
	n := map[string]float64{}
	for _, ev := range evs {
		name := ev["audit"].(string)
		n[name]++
		for _, a := range []string{"method", "reason", "result"} {
			if v, ok := ev[a].(string); ok {
				n[name+"/"+v]++
			}
		}
		if b, ok := ev["bytes"].(float64); ok {
			n[name+"/bytes"] += b
		}
		if b, ok := ev["bytes_in"].(float64); ok {
			n["bytes_in"] += b
		}
		if b, ok := ev["bytes_out"].(float64); ok {
			n["bytes_out"] += b
		}
	}
	return n
}

// A scripted run through logins, file operations, failures and a ban: the
// metrics move as expected and agree with the audit log.
func TestMetricsFollowAScriptedSession(t *testing.T) {
	logs := captureJSON(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	carolKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) {
		c.BanThreshold = 3
		c.Users["carol"] = User{Keys: []ssh.PublicKey{carolKey.PublicKey()}}
	})

	c, conn := e.session()
	expect(t, scrape(t, e.srv), map[string]float64{
		series("sftp_s3_sessions_active"):                    1,
		series("sftp_s3_connections_open"):                   1,
		series("sftp_s3_logins_total", "method", "password"): 1,
	})
	f, err := c.Create("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := c.Open("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(r); err != nil || string(b) != "hello" {
		t.Fatalf("download %q, %v", b, err)
	}
	r.Close()
	for _, err := range []error{
		c.Mkdir("/d"),
		c.Rename("/a.txt", "/d/b.txt"),
		c.Chmod("/d/b.txt", 0o644),
		c.Remove("/d/b.txt"),
		c.RemoveDirectory("/d"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c.Mkdir("/") == nil {
		t.Fatal("mkdir / succeeded")
	}
	c.Close()
	conn.Close()

	carol, err := e.sshDialKey("carol", carolKey)
	if err != nil {
		t.Fatal(err)
	}
	carol.Close()

	// Three failures ban the client; its next connection is refused.
	for _, try := range [][2]string{{"bob", "wrong"}, {"nobody", "x"}, {"bob", "wrong"}} {
		if c, err := e.sshDial(try[0], try[1]); err == nil {
			c.Close()
			t.Fatalf("%s logged in with a wrong password", try[0])
		}
	}
	if c, err := e.sshDial("alice", "alice-pw"); err == nil {
		c.Close()
		t.Fatal("banned client logged in")
	}

	evs := stopAndCollect(t, e, logs)
	got := scrape(t, e.srv)
	expect(t, got, map[string]float64{
		series("sftp_s3_connections_total"):                                   6,
		series("sftp_s3_connections_rejected_total", "reason", "banned"):      1,
		series("sftp_s3_connections_rejected_total", "reason", "limit"):       0,
		series("sftp_s3_connections_open"):                                    0,
		series("sftp_s3_sessions_active"):                                     0,
		series("sftp_s3_logins_total", "method", "password"):                  1,
		series("sftp_s3_logins_total", "method", "publickey"):                 1,
		series("sftp_s3_login_failures_total", "reason", "wrong password"):    2,
		series("sftp_s3_login_failures_total", "reason", "unknown user"):      1,
		series("sftp_s3_operations_total", "op", "upload", "result", "ok"):    1,
		series("sftp_s3_operations_total", "op", "download", "result", "ok"):  1,
		series("sftp_s3_operations_total", "op", "mkdir", "result", "ok"):     1,
		series("sftp_s3_operations_total", "op", "mkdir", "result", "failed"): 1,
		series("sftp_s3_operations_total", "op", "rename", "result", "ok"):    1,
		series("sftp_s3_operations_total", "op", "setstat", "result", "ok"):   1,
		series("sftp_s3_operations_total", "op", "remove", "result", "ok"):    1,
		series("sftp_s3_operations_total", "op", "rmdir", "result", "ok"):     1,
		series("sftp_s3_bytes_received_total"):                                5,
		series("sftp_s3_bytes_sent_total"):                                    5,
		series("sftp_s3_bans_total"):                                          1,
		series("sftp_s3_banned_clients"):                                      1,
	})

	// The same numbers, derived from the audit log.
	n := countEvents(evs)
	agree := map[string]float64{
		series("sftp_s3_sessions_active"):      n[evLogin] - n[evSessionEnd],
		series("sftp_s3_bytes_received_total"): n[evUpload+"/bytes"],
		series("sftp_s3_bytes_sent_total"):     n[evDownload+"/bytes"],
	}
	if n["bytes_in"] != n[evUpload+"/bytes"] || n["bytes_out"] != n[evDownload+"/bytes"] {
		t.Errorf("session_end bytes disagree with the transfers: %v", n)
	}
	for _, m := range loginMethods {
		agree[series("sftp_s3_logins_total", "method", m)] = n[evLogin+"/"+m]
	}
	for _, r := range failReasons {
		agree[series("sftp_s3_login_failures_total", "reason", r)] = n[evLoginFailed+"/"+r]
	}
	for _, op := range opEvents {
		for _, r := range opResults {
			agree[series("sftp_s3_operations_total", "op", op, "result", r)] = n[op+"/"+r]
		}
	}
	expect(t, got, agree)
}

func TestMetricsCountConnectionLimitRejections(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxConns = 1 })
	held, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for range 3 {
		if c, err := e.sshDial("alice", "alice-pw"); err == nil {
			c.Close()
			t.Fatal("connection accepted past the limit")
		}
	}
	expect(t, scrape(t, e.srv), map[string]float64{
		series("sftp_s3_connections_total"):                             4,
		series("sftp_s3_connections_rejected_total", "reason", "limit"): 3,
		series("sftp_s3_connections_open"):                              1,
		series("sftp_s3_connections_max"):                               1,
		series("sftp_s3_sessions_active"):                               1,
	})
}

// Values outside a label set count as "other", so the series stay fixed.
func TestMetricsRecordOther(t *testing.T) {
	var m metrics
	m.record(evLogin, []any{"method", "gssapi-with-mic"})
	m.record(evLoginFailed, []any{"reason", "something new"})
	m.record(evLoginFailed, []any{"dangling"})
	m.record("not an event", []any{"result", "ok"})
	m.record(evUpload, []any{"result", "failed"}) // no bytes
	// A value equal to an attribute name (here a user named "reason") is
	// not taken for that attribute.
	m.record(evLoginFailed, []any{"user", "reason", "reason", "banned"})
	if m.logins[index(loginMethods[:], "other")].Load() != 1 ||
		m.loginFailures[index(failReasons[:], "other")].Load() != 2 ||
		m.loginFailures[index(failReasons[:], "banned")].Load() != 1 ||
		m.ops[0][1].Load() != 1 || m.bytesIn.Load() != 0 {
		t.Fatalf("metrics %+v", &m)
	}
	var total int64
	for i := range m.ops {
		for j := range m.ops[i] {
			total += m.ops[i][j].Load()
		}
	}
	if total != 1 {
		t.Errorf("an unknown event was counted: %d ops", total)
	}
}

func TestBanCounts(t *testing.T) {
	if total, now := (*limiter)(nil).counts(); total != 0 || now != 0 {
		t.Errorf("bans off: counts = %d, %d", total, now)
	}
	clk := newFakeClock()
	l := newLimiter(1, time.Minute, time.Minute, time.Hour, 100, clk.now)
	l.fail(tcp("203.0.113.1:1"))
	l.fail(tcp("203.0.113.2:1"))
	if total, now := l.counts(); total != 2 || now != 2 {
		t.Errorf("counts = %d, %d; want 2, 2", total, now)
	}
	clk.advance(time.Minute) // both bans end
	if total, now := l.counts(); total != 2 || now != 0 {
		t.Errorf("after the bans: counts = %d, %d; want 2, 0", total, now)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestWriteMetricsReportsWriteErrors(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.WriteMetrics(errWriter{}); err == nil {
		t.Fatal("write error not returned")
	}
}

// Serving is false before Serve and once the listener closes, while open
// sessions still drain.
func TestServing(t *testing.T) {
	e := newEnv(t)
	idle, err := New(e.srv.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if idle.Serving() {
		t.Error("Serving before Serve")
	}
	c, conn := e.session()
	defer conn.Close()
	if !e.srv.Serving() {
		t.Fatal("not Serving while accepting")
	}
	e.ln.Close() // shutdown begins
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.Serving() {
		if time.Now().After(deadline) {
			t.Fatal("still Serving after the listener closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.Getwd(); err != nil {
		t.Fatalf("open session broke while draining: %v", err)
	}
	c.Close()
	conn.Close()
	e.stop()
	if e.srv.Serving() {
		t.Error("Serving after Serve returned")
	}
}
