package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

const sftpgoFixture = "testdata/sftpgo-dump.json"

// runImport runs `sftp-s3 import-sftpgo args...` as main would.
func runImport(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	_, code, done := parseFlags(append([]string{"import-sftpgo"}, args...), strings.NewReader(""), &out, &errOut)
	if !done {
		t.Fatal("import-sftpgo did not finish the command line")
	}
	return code, out.String(), errOut.String()
}

// fixtureSecrets lists every string in the fixture that must never reach
// the output: the encrypted S3 secrets and their keys, the access keys, and
// the admin's hash.
func fixtureSecrets(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(sftpgoFixture)
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Users []struct {
			Filesystem struct {
				S3 struct {
					AccessKey    string            `json:"access_key"`
					AccessSecret map[string]string `json:"access_secret"`
				} `json:"s3config"`
			} `json:"filesystem"`
		} `json:"users"`
		Admins []struct {
			Password string `json:"password"`
		} `json:"admins"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"fixture-secret", "fixture-admin", "Secretbox", "access_secret", "access_key"}
	for _, u := range dump.Users {
		s3 := u.Filesystem.S3
		for _, v := range []string{s3.AccessKey, s3.AccessSecret["payload"], s3.AccessSecret["key"]} {
			if v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	for _, a := range dump.Admins {
		secrets = append(secrets, a.Password)
	}
	if len(secrets) < 5+3*4+1 {
		t.Fatalf("fixture secrets not found: %q", secrets)
	}
	return secrets
}

func TestImportSFTPGoFixture(t *testing.T) {
	code, out, errOut := runImport(t, sftpgoFixture)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}

	users, err := loadUsers(out, "", "")
	if err != nil {
		t.Fatalf("output does not load: %v\n%s", err, out)
	}
	if len(users) != 3 {
		t.Fatalf("imported %d users, want alice, bob, erin:\n%s", len(users), out)
	}
	for name, want := range map[string]struct {
		bucket, root string
		keys         int
	}{
		"alice": {"cust-alpha", "", 0},
		"bob":   {"cust-beta", "incoming/", 1},
		"erin":  {"cust-delta", "erin/", 0},
	} {
		u := users[name]
		if u.Bucket != want.bucket || u.Root != want.root || len(u.Keys) != want.keys || !isBcrypt(u.PasswordHash) {
			t.Errorf("%s = bucket %q root %q, %d keys, hash %v; want %+v", name, u.Bucket, u.Root, len(u.Keys), isBcrypt(u.PasswordHash), want)
		}
	}
	// The bucket root must be explicit: without "prefix" it would become
	// <S3_PREFIX>alice/.
	if !strings.Contains(out, `"prefix": ""`) {
		t.Errorf("alice's empty prefix is not written out:\n%s", out)
	}
	if users, _ := loadUsers(out, "other", "sftp/"); users["alice"].Root != "" || users["alice"].Bucket != "cust-alpha" {
		t.Errorf("S3_BUCKET / S3_PREFIX moved alice: %+v", users["alice"])
	}
	if strings.Index(out, "alice") > strings.Index(out, "bob") || strings.Index(out, "bob") > strings.Index(out, "erin") {
		t.Errorf("users are not in dump order:\n%s", out)
	}
	if again := func() string { _, o, _ := runImport(t, sftpgoFixture); return o }(); again != out {
		t.Error("output differs between runs")
	}

	for _, want := range []string{
		`skipped user "carol": disabled`,
		`skipped user "dave": storage provider 0 is not S3`,
		`user "erin": quotas not carried over`,
		`user "erin": IP filters not carried over`,
		`user "erin": permissions other than full access`,
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(errOut), "\n") {
		if strings.Contains(line, "alice") || strings.Contains(line, "bob") {
			t.Errorf("unexpected warning about a plain user: %s", line)
		}
	}
	for _, s := range fixtureSecrets(t) {
		if strings.Contains(out, s) || strings.Contains(errOut, s) {
			t.Errorf("output contains the fixture secret %q", s)
		}
	}
}

// The converted users log in to a real gateway with their SFTPGo
// passwords and land in their own bucket and prefix.
func TestImportedUsersLogIn(t *testing.T) {
	isolateAWS(t)
	_, out, _ := runImport(t, sftpgoFixture)
	usersFile := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(usersFile, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := s3mem.New()
	for _, b := range []string{"cust-alpha", "cust-beta", "cust-delta"} {
		if err := backend.CreateBucket(b); err != nil {
			t.Fatal(err)
		}
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	keyPEM, _ := testKeyPEM(t)
	cfg, err := loadConfig(context.Background(), envMap(map[string]string{
		"S3_ENDPOINT": store.URL, "SFTP_USERS_FILE": usersFile, "SFTP_HOST_KEY": keyPEM,
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthFailDelay = 1 // keep the wrong-password case fast
	srv, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() { ln.Close(); <-served })

	dial := func(user, pass string) (*ssh.Client, error) {
		return ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.Password(pass)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
	}
	for _, tc := range []struct{ user, pass, bucket, key string }{
		{"alice", "alice-pass", "cust-alpha", "hello.txt"},
		{"bob", "bob-pass", "cust-beta", "incoming/hello.txt"},
		{"erin", "erin-pass", "cust-delta", "erin/hello.txt"},
	} {
		t.Run(tc.user, func(t *testing.T) {
			conn, err := dial(tc.user, tc.pass)
			if err != nil {
				t.Fatalf("login with the SFTPGo password: %v", err)
			}
			defer conn.Close()
			c, err := sftp.NewClient(conn)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			f, err := c.Create("/hello.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("hi")); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.HeadObject(tc.bucket, tc.key); err != nil {
				t.Fatalf("upload is not at s3://%s/%s: %v", tc.bucket, tc.key, err)
			}
		})
	}
	for _, tc := range []struct{ user, pass string }{{"alice", "bob-pass"}, {"carol", "carol-pass"}, {"dave", "dave-pass"}} {
		if conn, err := dial(tc.user, tc.pass); err == nil {
			conn.Close()
			t.Errorf("%s logged in with %s", tc.user, tc.pass)
		}
	}
}

func TestImportSFTPGoCommandLine(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
		err  string
	}{
		{"no file", nil, 2, "Usage: sftp-s3 import-sftpgo"},
		{"two files", []string{"a.json", "b.json"}, 2, "Usage: sftp-s3 import-sftpgo"},
		{"help", []string{"-h"}, 0, "Usage: sftp-s3 import-sftpgo"},
		{"missing file", []string{filepath.Join(dir, "missing.json")}, 1, "missing.json"},
		{"not JSON", []string{write("bad.json", "SQLite format 3\x00")}, 1, "not an SFTPGo dump"},
		{"no user left", []string{write("none.json", `{"users": [{"username": "carol", "status": 0}]}`)}, 1, "no user to import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runImport(t, tc.args...)
			if code != tc.code || out != "" || !strings.Contains(errOut, tc.err) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and %q", code, out, errOut, tc.code, tc.err)
			}
		})
	}
	// The usage of the server itself mentions the subcommand.
	var errOut strings.Builder
	parseFlags([]string{"-h"}, strings.NewReader(""), &strings.Builder{}, &errOut)
	if !strings.Contains(errOut.String(), "import-sftpgo") {
		t.Errorf("sftp-s3 -h does not mention import-sftpgo:\n%s", errOut.String())
	}
}

// s3User is a minimal enabled SFTPGo user on S3, with extra JSON fields.
// encoding/json lets a repeated key win, merging repeated objects field by
// field, so extra can override what is set here.
func s3User(name, extra string) string {
	if extra != "" {
		extra = ", " + extra
	}
	return `{"username": "` + name + `", "status": 1, "password": "` + testHash + `",
	  "permissions": {"/": ["*"]},
	  "filesystem": {"provider": 1, "s3config": {"bucket": "b", "region": "r1"}}` + extra + `}`
}

func TestConvertSFTPGo(t *testing.T) {
	key, _ := authorizedKey(t, "k@host")
	fs := func(s3 string) string { return `"filesystem": {"provider": 1, "s3config": {` + s3 + `}}` }
	tests := []struct {
		name    string
		users   []string
		want    map[string]string   // user -> expected fragment of their output entry
		without map[string][]string // user -> credentials ("hash", "keys") they must not have
		warn    []string
	}{
		{
			name:  "argon2 password dropped, keys kept",
			users: []string{s3User("a", `"password": "$argon2id$v=19$m=65536,t=1,p=2$c2FsdA$aGFzaA", "public_keys": ["`+key+`"]`)},
			want:  map[string]string{"a": `"public_keys": [`},
			warn:  []string{`user "a": password is not a bcrypt hash; dropped`},
		},
		{
			name:  "argon2 password and no key",
			users: []string{s3User("a", `"password": "$argon2id$v=19$m=65536,t=1,p=2$c2FsdA$aGFzaA"`), s3User("ok", "")},
			want:  map[string]string{"ok": `"username": "ok"`},
			warn:  []string{`user "a": password is not a bcrypt hash`, `skipped user "a": no usable password or public key`},
		},
		{
			name:  "malformed bcrypt",
			users: []string{s3User("a", `"password": "$2a$xx", "public_keys": ["`+key+`"]`)},
			want:  map[string]string{"a": `"public_keys"`},
			warn:  []string{`user "a": password is not a bcrypt hash`},
		},
		{
			name:  "no password at all",
			users: []string{s3User("a", `"password": "", "public_keys": ["`+key+`"]`)},
			want:  map[string]string{"a": `"public_keys"`},
		},
		{
			name:  "unreadable key",
			users: []string{s3User("a", `"public_keys": ["ssh-ed25519 garbage", "`+key+`"]`)},
			want:  map[string]string{"a": key},
			warn:  []string{`user "a": public key 1 is unreadable; dropped`},
		},
		{
			name:  "S3 without a bucket",
			users: []string{s3User("a", fs(`"bucket": "", "key_prefix": "x/"`)), s3User("ok", "")},
			want:  map[string]string{"ok": `"bucket": "b"`},
			warn:  []string{`skipped user "a": no S3 bucket`},
		},
		{
			name: "bad prefixes",
			users: []string{
				s3User("a", fs(`"bucket": "b", "key_prefix": "x"`)),
				s3User("c", fs(`"bucket": "b", "key_prefix": "/x/"`)),
				s3User("ok", fs(`"bucket": "b", "key_prefix": "x/y/"`)),
			},
			want: map[string]string{"ok": `"prefix": "x/y/"`},
			warn: []string{`skipped user "a": key prefix "x" does not end`, `skipped user "c": key prefix "/x/"`},
		},
		{
			name:  "name the server refuses",
			users: []string{s3User("a b", ""), s3User("ok", "")},
			want:  map[string]string{"ok": `"username": "ok"`},
			warn:  []string{`skipped user "a b": a username may not`},
		},
		{
			name: "other providers",
			users: []string{
				`{"username": "g", "status": 1, "password": "` + testHash + `", "filesystem": {"provider": 2}}`,
				s3User("ok", ""),
			},
			want: map[string]string{"ok": `"username": "ok"`},
			warn: []string{`skipped user "g": storage provider 2 is not S3`},
		},
		{
			// SFTPGo refuses login once expiration_date (ms since the
			// epoch) has passed; a future date is only warned about.
			name: "expired",
			users: []string{
				s3User("old", `"expiration_date": 1000`),
				s3User("lastyear", `"expiration_date": `+strconv.FormatInt(time.Now().AddDate(-1, 0, 0).UnixMilli(), 10)),
				s3User("later", `"expiration_date": `+strconv.FormatInt(time.Now().AddDate(1, 0, 0).UnixMilli(), 10)),
			},
			want: map[string]string{"later": `"later"`},
			warn: []string{
				`skipped user "old": expired in SFTPGo`,
				`skipped user "lastyear": expired in SFTPGo`,
				`user "later": expiration date not carried over`,
			},
		},
		{
			name: "SSH denied",
			users: []string{
				s3User("ftponly", `"filters": {"denied_protocols": ["SSH"]}`),
				s3User("noftp", `"filters": {"denied_protocols": ["FTP", "DAV"]}`),
			},
			want: map[string]string{"noftp": `"noftp"`},
			warn: []string{`skipped user "ftponly": SFTP (protocol SSH) is denied in SFTPGo`},
		},
		{
			// The gateway has no second factor: a user who needs one over
			// SSH would lose it, so they are skipped.
			name: "second factor",
			users: []string{
				s3User("totp", `"filters": {"totp_config": {"enabled": true, "config_name": "Default", "protocols": ["SSH", "HTTP"], "secret": {}}}`),
				s3User("required", `"filters": {"two_factor_protocols": ["SSH"]}`),
				s3User("webtotp", `"filters": {"totp_config": {"enabled": true, "protocols": ["HTTP"]}, "two_factor_protocols": ["HTTP"]}`),
				s3User("off", `"filters": {"totp_config": {"enabled": false, "protocols": ["SSH"]}}`),
			},
			want: map[string]string{"webtotp": `"webtotp"`, "off": `"off"`},
			warn: []string{
				`skipped user "totp": two-factor authentication over SSH is required in SFTPGo`,
				`skipped user "required": two-factor authentication over SSH is required in SFTPGo`,
			},
		},
		{
			// A login method SFTPGo denies is kept denied by not copying
			// its credential; a user left with none is skipped.
			name: "denied login methods",
			users: []string{
				s3User("keyonly", `"public_keys": ["`+key+`"], "filters": {"denied_login_methods": ["password"]}`),
				s3User("nosshpw", `"public_keys": ["`+key+`"], "filters": {"denied_login_methods": ["password-over-SSH"]}`),
				s3User("pwonly", `"public_keys": ["`+key+`"], "filters": {"denied_login_methods": ["publickey"]}`),
				s3User("kbd", `"filters": {"denied_login_methods": ["keyboard-interactive"]}`),
				s3User("none", `"public_keys": ["`+key+`"], "filters": {"denied_login_methods": ["password", "publickey"]}`),
				s3User("nopw", `"password": "", "filters": {"denied_login_methods": ["publickey"]}`),
			},
			want:    map[string]string{"keyonly": `"public_keys"`, "nosshpw": `"public_keys"`, "pwonly": `"password_hash"`, "kbd": `"password_hash"`},
			without: map[string][]string{"keyonly": {"hash"}, "nosshpw": {"hash"}, "pwonly": {"keys"}},
			warn: []string{
				`user "keyonly": password login is denied in SFTPGo; password not copied`,
				`user "nosshpw": password login is denied`,
				`user "pwonly": public key login is denied in SFTPGo; public keys not copied`,
				`user "none": password login is denied`,
				`user "none": public key login is denied`,
				`skipped user "none": no usable password or public key`,
				`skipped user "nopw": no usable password or public key`,
			},
		},
		{
			name: "settings not carried over",
			users: []string{s3User("a", `"virtual_folders": [{"name": "shared", "virtual_path": "/shared"}, {"name": "x", "virtual_path": "/x"}],
			  "quota_files": 10, "groups": [{"name": "staff", "type": 1}, {"name": "ops", "type": 2}],
			  "filters": {"denied_ip": ["192.0.2.1/32"]}, "expiration_date": 1893456000000,
			  "permissions": {"/": ["*"], "/ro": ["list", "download"]}`)},
			want: map[string]string{"a": `"username": "a"`},
			warn: []string{
				`user "a": virtual folders not carried over: /shared, /x`,
				`user "a": quotas not carried over`,
				`user "a": IP filters not carried over`,
				`user "a": permissions other than full access`,
				`user "a": member of groups staff, ops`,
				`user "a": expiration date not carried over`,
			},
		},
		{
			name: "transfer quotas",
			users: []string{
				s3User("u", `"upload_data_transfer": 1`), s3User("d", `"download_data_transfer": 1`), s3User("t", `"total_data_transfer": 1`),
			},
			want: map[string]string{"u": `"u"`, "d": `"d"`, "t": `"t"`},
			warn: []string{`user "u": quotas`, `user "d": quotas`, `user "t": quotas`},
		},
		{
			name: "regions and endpoints differ",
			users: []string{
				s3User("a", fs(`"bucket": "b", "region": "r1"`)),
				s3User("b", fs(`"bucket": "b", "region": "r1"`)),
				s3User("c", fs(`"bucket": "b", "region": "r2", "endpoint": "https://s3.example"`)),
			},
			want: map[string]string{"a": `"a"`, "b": `"b"`, "c": `"c"`},
			warn: []string{
				`user "c": S3 region r2 differs from the others (r1); the gateway uses one S3_REGION`,
				`user "c": S3 endpoint https://s3.example differs from the others ((default)); the gateway uses one S3_ENDPOINT`,
			},
		},
		{
			name: "most users on a custom endpoint",
			users: []string{
				s3User("a", fs(`"bucket": "b", "endpoint": "https://s3.example"`)),
				s3User("b", fs(`"bucket": "b", "endpoint": "https://s3.example"`)),
				s3User("c", fs(`"bucket": "b"`)),
			},
			want: map[string]string{"a": `"a"`, "b": `"b"`, "c": `"c"`},
			warn: []string{`user "c": S3 endpoint (default) differs from the others (https://s3.example)`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warn strings.Builder
			out, err := convertSFTPGo([]byte(`{"users": [`+strings.Join(tt.users, ",")+`]}`), &warn)
			if err != nil {
				t.Fatalf("err = %v, warnings:\n%s", err, warn.String())
			}
			users, err := loadUsers(string(out), "", "")
			if err != nil {
				t.Fatalf("output does not load: %v\n%s", err, out)
			}
			if len(users) != len(tt.want) {
				t.Errorf("imported %d users, want %d:\n%s", len(users), len(tt.want), out)
			}
			var f importedFile
			if err := json.Unmarshal(out, &f); err != nil {
				t.Fatal(err)
			}
			for _, u := range f.Users {
				frag, ok := tt.want[u.Username]
				if !ok || !strings.Contains(string(out), frag) {
					t.Errorf("user %q: want %q in\n%s", u.Username, frag, out)
				}
				for _, w := range tt.without[u.Username] {
					if (w == "hash" && u.PasswordHash != "") || (w == "keys" && len(u.PublicKeys) > 0) {
						t.Errorf("user %q: has %s, want none", u.Username, w)
					}
				}
			}
			lines := strings.Split(strings.TrimSpace(warn.String()), "\n")
			if warn.Len() == 0 {
				lines = nil
			}
			if len(lines) != len(tt.warn) {
				t.Errorf("%d warnings, want %d:\n%s", len(lines), len(tt.warn), warn.String())
			}
			for _, w := range tt.warn {
				if !strings.Contains(warn.String(), w) {
					t.Errorf("warnings lack %q:\n%s", w, warn.String())
				}
			}
		})
	}
}

func TestConvertSFTPGoErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`[`, "not an SFTPGo dump"},
		{`{"admins": []}`, `no "users"`},
		{`{"users": null}`, `no "users"`},
		{`{"users": []}`, "no user to import"},
		{`{"users": [` + s3User("a", `"status": 0`) + `]}`, "no user to import"},
	} {
		if _, err := convertSFTPGo([]byte(tc.in), &strings.Builder{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("convertSFTPGo(%s) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

// Messages name users and settings; a password that is not a bcrypt hash
// (it might even be plaintext) never shows up in them.
func TestConvertSFTPGoKeepsPasswordsOut(t *testing.T) {
	key, _ := authorizedKey(t, "")
	var warn strings.Builder
	out, err := convertSFTPGo([]byte(`{"users": [`+s3User("a", `"password": "hunter2-plain", "public_keys": ["`+key+`"]`)+`]}`), &warn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out)+warn.String(), "hunter2") {
		t.Fatalf("password leaked:\n%s\n%s", out, warn.String())
	}
}
