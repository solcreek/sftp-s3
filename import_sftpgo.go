package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

// The parts of an SFTPGo data dump (GET /api/v2/dumpdata?output-data=1, or
// `sftpgo dumpdata`) that the import reads. Everything else, notably the
// encrypted S3 access_secret, is left undecoded: it is never read, so it
// cannot leak into the output or a warning.
type sftpgoDump struct {
	Users *[]sftpgoUser `json:"users"`
}

type sftpgoUser struct {
	Username       string              `json:"username"`
	Status         int                 `json:"status"`
	Password       string              `json:"password"`
	PublicKeys     []string            `json:"public_keys"`
	Permissions    map[string][]string `json:"permissions"`
	QuotaSize      int64               `json:"quota_size"`
	QuotaFiles     int64               `json:"quota_files"`
	UploadXfer     int64               `json:"upload_data_transfer"`
	DownloadXfer   int64               `json:"download_data_transfer"`
	TotalXfer      int64               `json:"total_data_transfer"`
	ExpirationDate int64               `json:"expiration_date"`
	VirtualFolders []struct {
		Name        string `json:"name"`
		VirtualPath string `json:"virtual_path"`
	} `json:"virtual_folders"`
	Groups []struct {
		Name string `json:"name"`
	} `json:"groups"`
	Filters struct {
		AllowedIP          []string `json:"allowed_ip"`
		DeniedIP           []string `json:"denied_ip"`
		DeniedLoginMethods []string `json:"denied_login_methods"`
		DeniedProtocols    []string `json:"denied_protocols"`
		TwoFactorProtocols []string `json:"two_factor_protocols"`
		// Only whether TOTP is on, and for which protocols; the secret
		// next to these fields is not decoded.
		TOTP struct {
			Enabled   bool     `json:"enabled"`
			Protocols []string `json:"protocols"`
		} `json:"totp_config"`
	} `json:"filters"`
	Filesystem struct {
		Provider int `json:"provider"`
		S3       struct {
			Bucket    string `json:"bucket"`
			KeyPrefix string `json:"key_prefix"`
			Region    string `json:"region"`
			Endpoint  string `json:"endpoint"`
		} `json:"s3config"`
	} `json:"filesystem"`
}

// sftpgoS3Provider is SFTPGo's filesystem provider number for S3.
const sftpgoS3Provider = 1

// importedUser is one entry of the JSON users file (see usersFile). Prefix
// is always written: "" is the bucket root in SFTPGo and must stay the
// bucket root here, where an absent prefix would mean <S3_PREFIX><user>/.
type importedUser struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"password_hash,omitempty"`
	PublicKeys   []string `json:"public_keys,omitempty"`
	Bucket       string   `json:"bucket"`
	Prefix       string   `json:"prefix"`
}

type importedFile struct {
	Users []importedUser `json:"users"`
}

const importUsage = "Usage: sftp-s3 import-sftpgo <dump.json>\n\nConverts the users of an SFTPGo data dump (sftpgo dumpdata, or\nGET /api/v2/dumpdata?output-data=1) to a JSON users file on stdout.\nWarnings go to stderr.\n"

// importSFTPGo runs the import-sftpgo subcommand and returns its exit code:
// 0 with the users file on stdout, 1 when the dump cannot be read or has no
// user to import, 2 on a usage error.
func importSFTPGo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprint(stderr, importUsage)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprint(stderr, importUsage)
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "sftp-s3 import-sftpgo:", err)
		return 1
	}
	out, err := convertSFTPGo(data, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "sftp-s3 import-sftpgo:", err)
		return 1
	}
	stdout.Write(out)
	return 0
}

// convertSFTPGo turns an SFTPGo dump into a JSON users file, in the dump's
// order. Users it cannot import are skipped, and settings it cannot carry
// over are dropped, each with a line on warn. Messages name users and
// settings, never credentials.
func convertSFTPGo(data []byte, warn io.Writer) ([]byte, error) {
	var dump sftpgoDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, fmt.Errorf("not an SFTPGo dump: %w", err)
	}
	if dump.Users == nil {
		return nil, errors.New(`not an SFTPGo dump: no "users"`)
	}
	var (
		out     importedFile
		regions = map[string][]string{} // region -> imported users
		ends    = map[string][]string{} // endpoint -> imported users
		seen    = map[string]bool{}     // every name in the dump so far
	)
	for _, su := range *dump.Users {
		// SFTPGo keeps names unique, but a hand-edited dump might not; one
		// repeat would make the users file refuse to load as a whole.
		if seen[su.Username] {
			fmt.Fprintf(warn, "skipped user %q: listed twice in the dump; the first entry is kept\n", su.Username)
			continue
		}
		seen[su.Username] = true
		u, ok := convertSFTPGoUser(su, warn)
		if !ok {
			continue
		}
		out.Users = append(out.Users, u)
		regions[su.Filesystem.S3.Region] = append(regions[su.Filesystem.S3.Region], u.Username)
		ends[su.Filesystem.S3.Endpoint] = append(ends[su.Filesystem.S3.Endpoint], u.Username)
	}
	if len(out.Users) == 0 {
		return nil, errors.New("no user to import")
	}
	warnSplit(warn, "region", "S3_REGION", regions)
	warnSplit(warn, "endpoint", "S3_ENDPOINT", ends)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out) // cannot fail: only strings and slices of them
	return buf.Bytes(), nil
}

// convertSFTPGoUser converts one user, or reports why it was skipped.
func convertSFTPGoUser(su sftpgoUser, warn io.Writer) (importedUser, bool) {
	name := su.Username
	skip := func(why string) (importedUser, bool) {
		fmt.Fprintf(warn, "skipped user %q: %s\n", name, why)
		return importedUser{}, false
	}
	note := func(format string, a ...any) {
		fmt.Fprintf(warn, "warning: user %q: "+format+"\n", append([]any{name}, a...)...)
	}
	s3 := su.Filesystem.S3
	switch {
	case su.Status != 1:
		return skip("disabled in SFTPGo")
	case su.ExpirationDate > 0 && su.ExpirationDate < time.Now().UnixMilli():
		return skip("expired in SFTPGo")
	case slices.Contains(su.Filters.DeniedProtocols, "SSH"):
		return skip("SFTP (protocol SSH) is denied in SFTPGo")
	case slices.Contains(su.Filters.TwoFactorProtocols, "SSH") ||
		su.Filters.TOTP.Enabled && slices.Contains(su.Filters.TOTP.Protocols, "SSH"):
		// The gateway has no second factor. A user who has set one up
		// for SSH would lose it; one who is required to but has not
		// cannot log in to SFTPGo over SSH at all.
		return skip("two-factor authentication over SSH is required in SFTPGo")
	case su.Filesystem.Provider != sftpgoS3Provider:
		return skip(fmt.Sprintf("storage provider %d is not S3", su.Filesystem.Provider))
	case s3.Bucket == "":
		return skip("no S3 bucket")
	case s3.KeyPrefix != "" && (!strings.HasSuffix(s3.KeyPrefix, "/") || strings.HasPrefix(s3.KeyPrefix, "/")):
		return skip(fmt.Sprintf("key prefix %q does not end in '/' or starts with '/'", s3.KeyPrefix))
	}
	u := importedUser{Username: name, Bucket: s3.Bucket, Prefix: s3.KeyPrefix}
	// A login method SFTPGo denies stays denied by leaving its credential
	// out. "password" denies it on every protocol, "password-over-SSH" on
	// SFTP only; the gateway offers neither keyboard-interactive nor the
	// multi-step methods, so denying those changes nothing here.
	denied := su.Filters.DeniedLoginMethods
	if su.Password != "" {
		switch {
		case slices.Contains(denied, "password") || slices.Contains(denied, "password-over-SSH"):
			note("password login is denied in SFTPGo; password not copied")
		case isBcrypt(su.Password):
			u.PasswordHash = su.Password
		default:
			note("password is not a bcrypt hash; dropped (set a new one with sftp-s3 -hash-password)")
		}
	}
	if len(su.PublicKeys) > 0 && slices.Contains(denied, "publickey") {
		note("public key login is denied in SFTPGo; public keys not copied")
		su.PublicKeys = nil
	}
	for i, k := range su.PublicKeys {
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil {
			note("public key %d is unreadable; dropped", i+1)
			continue
		}
		u.PublicKeys = append(u.PublicKeys, k)
	}
	if u.PasswordHash == "" && len(u.PublicKeys) == 0 {
		return skip("no usable password or public key")
	}
	if err := checkImported(u); err != nil {
		return skip(err.Error())
	}

	if len(su.VirtualFolders) > 0 {
		var paths []string
		for _, f := range su.VirtualFolders {
			paths = append(paths, f.VirtualPath)
		}
		note("virtual folders not carried over: %s", strings.Join(paths, ", "))
	}
	if su.QuotaSize != 0 || su.QuotaFiles != 0 || su.UploadXfer != 0 || su.DownloadXfer != 0 || su.TotalXfer != 0 {
		note("quotas not carried over")
	}
	if len(su.Filters.AllowedIP) > 0 || len(su.Filters.DeniedIP) > 0 {
		note("IP filters not carried over; the user can log in from any address")
	}
	if !fullAccess(su.Permissions) {
		note("permissions other than full access not carried over; the user gets full access to their prefix")
	}
	if len(su.Groups) > 0 {
		var groups []string
		for _, g := range su.Groups {
			groups = append(groups, g.Name)
		}
		note("member of groups %s; settings from groups not carried over", strings.Join(groups, ", "))
	}
	if su.ExpirationDate != 0 {
		note("expiration date not carried over; the account does not expire")
	}
	return u, true
}

// checkImported runs one converted user through the users file parser, so
// a user the server would refuse (say, an unusual name) is skipped rather
// than making the whole file unloadable.
func checkImported(u importedUser) error {
	data, _ := json.Marshal(importedFile{Users: []importedUser{u}})
	_, err := parseUsersJSON(data, "", "")
	if err != nil {
		// Drop the "users file: user 1 (name)" context; the caller names the user.
		msg := err.Error()
		if _, rest, ok := strings.Cut(msg, "): "); ok {
			msg = rest
		}
		return errors.New(msg)
	}
	return nil
}

// isBcrypt reports whether s is a well-formed bcrypt hash. SFTPGo can also
// hold argon2id, PBKDF2 and other hashes, which the gateway cannot check.
func isBcrypt(s string) bool {
	if credentialKind(s) != "hash" {
		return false
	}
	_, err := bcrypt.Cost([]byte(s))
	return err == nil
}

// fullAccess reports whether SFTPGo permissions grant everything everywhere,
// which is all the gateway offers.
func fullAccess(perms map[string][]string) bool {
	all := perms["/"]
	return len(perms) == 1 && len(all) == 1 && all[0] == "*"
}

// warnSplit warns when imported users are spread over several values of
// setting (region or endpoint): the gateway has one S3 client, configured
// by env. The most common value is suggested; users on the others are named.
func warnSplit(warn io.Writer, setting, env string, byValue map[string][]string) {
	if len(byValue) < 2 {
		return
	}
	values := slices.Sorted(maps.Keys(byValue))
	common := values[0]
	for _, v := range values {
		if len(byValue[v]) > len(byValue[common]) {
			common = v
		}
	}
	for _, v := range values {
		if v == common {
			continue
		}
		shown := v
		if shown == "" {
			shown = "(default)"
		}
		for _, name := range byValue[v] {
			fmt.Fprintf(warn, "warning: user %q: S3 %s %s differs from the others (%s); the gateway uses one %s for all users\n",
				name, setting, shown, orDefault(common, "(default)"), env)
		}
	}
}
