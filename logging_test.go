package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	for _, tc := range []struct {
		name, format, level string
		json                bool
		levels              []string // the levels of d, i, w that are written
	}{
		{name: "defaults", levels: []string{"INFO", "WARN"}},
		{name: "text", format: "text", levels: []string{"INFO", "WARN"}},
		{name: "json", format: "json", json: true, levels: []string{"INFO", "WARN"}},
		{name: "debug", level: "debug", levels: []string{"DEBUG", "INFO", "WARN"}},
		{name: "info", level: "info", levels: []string{"INFO", "WARN"}},
		{name: "warn json", format: "json", level: "warn", json: true, levels: []string{"WARN"}},
		{name: "error", level: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := newLogger(envMap(map[string]string{"SFTP_LOG_FORMAT": tc.format, "SFTP_LOG_LEVEL": tc.level}), &buf)
			if err != nil {
				t.Fatal(err)
			}
			log.Debug("d")
			log.Info("i")
			log.Warn("w")
			var got []string
			for _, l := range strings.Split(buf.String(), "\n") {
				if l == "" {
					continue
				}
				var rec struct{ Level string }
				if tc.json {
					if err := json.Unmarshal([]byte(l), &rec); err != nil {
						t.Fatalf("line %q is not JSON: %v", l, err)
					}
				} else {
					if !strings.HasPrefix(l, "time=") {
						t.Fatalf("line %q is not slog text", l)
					}
					rec.Level = strings.Fields(l)[1][len("level="):]
				}
				got = append(got, rec.Level)
			}
			if strings.Join(got, ",") != strings.Join(tc.levels, ",") {
				t.Fatalf("levels written = %v, want %v:\n%s", got, tc.levels, buf.String())
			}
		})
	}
}

func TestNewLoggerRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"SFTP_LOG_FORMAT": "JSON"}, `SFTP_LOG_FORMAT: "JSON" is not text or json`},
		{map[string]string{"SFTP_LOG_FORMAT": "logfmt"}, `SFTP_LOG_FORMAT: "logfmt" is not text or json`},
		{map[string]string{"SFTP_LOG_LEVEL": "trace"}, `SFTP_LOG_LEVEL: "trace" is not debug, info, warn or error`},
		{map[string]string{"SFTP_LOG_LEVEL": "INFO+2"}, `SFTP_LOG_LEVEL: "INFO+2" is not debug, info, warn or error`},
	} {
		if _, err := newLogger(envMap(tc.env), &bytes.Buffer{}); err == nil || err.Error() != tc.want {
			t.Errorf("%v: err = %v, want %q", tc.env, err, tc.want)
		}
	}
}
