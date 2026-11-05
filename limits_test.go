package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

func TestLoadLoginLimits(t *testing.T) {
	var cfg gateway.Config
	if err := loadLoginLimits(envMap(nil), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BanThreshold != 0 || cfg.BanWindow != 0 || cfg.BanDuration != 0 || cfg.BanMaxDuration != 0 || cfg.MaxAuthTries != 0 || cfg.NoKeyboardInteractive {
		t.Fatalf("unset variables changed the config: %+v", cfg)
	}
	for v, off := range map[string]bool{"on": false, "off": true} {
		var c gateway.Config
		if err := loadLoginLimits(envMap(map[string]string{"SFTP_KEYBOARD_INTERACTIVE": v}), &c); err != nil || c.NoKeyboardInteractive != off {
			t.Errorf("SFTP_KEYBOARD_INTERACTIVE=%s: NoKeyboardInteractive %v, err %v", v, c.NoKeyboardInteractive, err)
		}
	}

	err := loadLoginLimits(envMap(map[string]string{
		"SFTP_BAN_THRESHOLD": "5", "SFTP_BAN_WINDOW": "2m", "SFTP_BAN_DURATION": "1h",
		"SFTP_BAN_MAX_DURATION": "48h", "SFTP_MAX_AUTH_TRIES": "3",
	}), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BanThreshold != 5 || cfg.BanWindow != 2*time.Minute || cfg.BanDuration != time.Hour ||
		cfg.BanMaxDuration != 48*time.Hour || cfg.MaxAuthTries != 3 {
		t.Fatalf("settings not applied: %+v", cfg)
	}

	if err := loadLoginLimits(envMap(map[string]string{"SFTP_BAN_THRESHOLD": "0"}), &cfg); err != nil || cfg.BanThreshold >= 0 {
		t.Fatalf("SFTP_BAN_THRESHOLD=0: threshold %d, err %v; want bans off (negative)", cfg.BanThreshold, err)
	}

	for name, v := range map[string]string{
		"SFTP_BAN_THRESHOLD":        "-1",
		"SFTP_BAN_WINDOW":           "10",
		"SFTP_BAN_DURATION":         "500ms",
		"SFTP_BAN_MAX_DURATION":     "forever",
		"SFTP_MAX_AUTH_TRIES":       "0",
		"SFTP_KEYBOARD_INTERACTIVE": "no",
	} {
		if err := loadLoginLimits(envMap(map[string]string{name: v}), &cfg); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: err = %v, want it to mention %s", name, v, err, name)
		}
	}
	for _, v := range []string{"x", "1001"} {
		if err := loadLoginLimits(envMap(map[string]string{"SFTP_BAN_THRESHOLD": v}), &cfg); err == nil {
			t.Errorf("SFTP_BAN_THRESHOLD=%s accepted", v)
		}
	}
	if err := loadLoginLimits(envMap(map[string]string{"SFTP_MAX_AUTH_TRIES": "many"}), &cfg); err == nil {
		t.Error("SFTP_MAX_AUTH_TRIES=many accepted")
	}
	// Durations are bounded, so doubling a repeat ban cannot overflow.
	for v, ok := range map[string]bool{"720h": true, "721h": false, "2562047h": false} {
		err := loadLoginLimits(envMap(map[string]string{"SFTP_BAN_MAX_DURATION": v}), &cfg)
		if (err == nil) != ok {
			t.Errorf("SFTP_BAN_MAX_DURATION=%s: err = %v, want accepted=%v", v, err, ok)
		}
	}
}

func TestLoadConfigAppliesLoginLimits(t *testing.T) {
	isolateAWS(t)
	env := map[string]string{"S3_BUCKET": "b", "SFTP_USERS": "a:b", "SFTP_BAN_THRESHOLD": "7"}
	cfg, err := loadConfig(context.Background(), envMap(env))
	if err != nil || cfg.BanThreshold != 7 {
		t.Fatalf("threshold %d, err %v", cfg.BanThreshold, err)
	}
	env["SFTP_BAN_WINDOW"] = "soon"
	if _, err := loadConfig(context.Background(), envMap(env)); err == nil || !strings.Contains(err.Error(), "SFTP_BAN_WINDOW") {
		t.Fatalf("err = %v, want it to mention SFTP_BAN_WINDOW", err)
	}
}
