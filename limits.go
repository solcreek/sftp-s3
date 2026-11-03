package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// loadLoginLimits applies SFTP_BAN_THRESHOLD (0 turns bans off),
// SFTP_BAN_WINDOW, SFTP_BAN_DURATION, SFTP_BAN_MAX_DURATION and
// SFTP_MAX_AUTH_TRIES to cfg. Unset variables keep the gateway defaults.
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
		if *d, err = time.ParseDuration(v); err != nil || *d < time.Second {
			return fmt.Errorf("%s: %q must be a duration of at least 1s, like 10m or 24h", name, v)
		}
	}
	if v := getenv("SFTP_MAX_AUTH_TRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return fmt.Errorf("SFTP_MAX_AUTH_TRIES: %q must be an integer from 1 to 100", v)
		}
		cfg.MaxAuthTries = n
	}
	return nil
}
