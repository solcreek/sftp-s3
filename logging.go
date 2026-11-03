package main

import (
	"fmt"
	"io"
	"log/slog"
)

// logLevels are the values SFTP_LOG_LEVEL accepts.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// newLogger builds the process logger, writing to w, from SFTP_LOG_FORMAT
// (text, the default, or json) and SFTP_LOG_LEVEL (debug, info, the
// default, warn or error).
func newLogger(getenv func(string) string, w io.Writer) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if v := getenv("SFTP_LOG_LEVEL"); v != "" {
		level, ok := logLevels[v]
		if !ok {
			return nil, fmt.Errorf("SFTP_LOG_LEVEL: %q is not debug, info, warn or error", v)
		}
		opts.Level = level
	}
	switch v := getenv("SFTP_LOG_FORMAT"); v {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("SFTP_LOG_FORMAT: %q is not text or json", v)
	}
}
