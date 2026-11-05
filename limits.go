package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// loadLoginLimits applies SFTP_BAN_THRESHOLD (0 turns bans off),
// SFTP_BAN_WINDOW, SFTP_BAN_DURATION, SFTP_BAN_MAX_DURATION,
// SFTP_MAX_AUTH_TRIES and SFTP_KEYBOARD_INTERACTIVE (on or off) to cfg.
// Unset variables keep the gateway defaults.
// maxBanSetting bounds the ban durations: longer ones are more likely typos
// than intent, and doubling a bounded ban cannot overflow.
const maxBanSetting = 30 * 24 * time.Hour

func loadLoginLimits(getenv func(string) string, cfg *gateway.Config) error {
	if v := getenv("SFTP_BAN_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1000 {
			return fmt.Errorf("SFTP_BAN_THRESHOLD: %q must be an integer from 0 (bans off) to 1000", v)
		}
		cfg.BanThreshold = n
		if n == 0 {
			cfg.BanThreshold = -1
		}
	}
	for name, d := range map[string]*time.Duration{
		"SFTP_BAN_WINDOW":       &cfg.BanWindow,
		"SFTP_BAN_DURATION":     &cfg.BanDuration,
		"SFTP_BAN_MAX_DURATION": &cfg.BanMaxDuration,
	} {
		v := getenv(name)
		if v == "" {
			continue
		}
		var err error
		if *d, err = time.ParseDuration(v); err != nil || *d < time.Second || *d > maxBanSetting {
			return fmt.Errorf("%s: %q must be a duration from 1s to 30 days (720h), like 10m or 24h", name, v)
		}
	}
	if v := getenv("SFTP_MAX_AUTH_TRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return fmt.Errorf("SFTP_MAX_AUTH_TRIES: %q must be an integer from 1 to 100", v)
		}
		cfg.MaxAuthTries = n
	}
	switch v := getenv("SFTP_KEYBOARD_INTERACTIVE"); v {
	case "", "on":
	case "off":
		cfg.NoKeyboardInteractive = true
	default:
		return fmt.Errorf("SFTP_KEYBOARD_INTERACTIVE: %q must be on or off", v)
	}
	return nil
}
