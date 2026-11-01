package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// parseUsers parses SFTP_USERS: entries of user:credential, separated by
// commas, or by newlines when the value has several lines (a file, where
// key comments may contain commas). Blank lines and lines starting with #
// are ignored. A credential is
//
//   - a public key in authorized_keys format ("ssh-ed25519 AAAA... comment"),
//   - a bcrypt hash ("$2a$...", "$2b$...", "$2y$..."), or
//   - anything else: a plaintext password.
//
// A user may appear on several entries, e.g. one per key, but with at most
// one password. Any malformed entry is an error rather than skipped, so a
// typo cannot silently lock a user out. Errors name the entry, never the
// credential. Names may not contain "/" or "." since they become S3 key
// prefixes.
func parseUsers(s string) (map[string]gateway.User, error) {
	sep := ","
	if strings.Contains(s, "\n") {
		sep = "\n"
	}
	users := map[string]gateway.User{}
	for i, entry := range strings.Split(s, sep) {
		entry = strings.TrimSpace(entry)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		where := fmt.Sprintf("SFTP_USERS entry %d", i+1)
		name, cred, ok := strings.Cut(entry, ":")
		name, cred = strings.TrimSpace(name), strings.TrimSpace(cred)
		switch {
		case !ok || name == "" || cred == "":
			return nil, fmt.Errorf("%s is not user:credential", where)
		case strings.ContainsAny(name, "/. \t"):
			return nil, fmt.Errorf("%s: user %q may not contain '/', '.' or spaces", where, name)
		}
		u := users[name]
		switch kind := credentialKind(cred); kind {
		case "key":
			key, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(cred))
			u.Keys = append(u.Keys, key)
		case "bad key":
			return nil, fmt.Errorf("%s: user %q: unreadable public key", where, name)
		default:
			if u.Password != "" || u.PasswordHash != "" {
				return nil, fmt.Errorf("%s: user %q has a second password", where, name)
			}
			if kind == "hash" {
				u.PasswordHash = cred
			} else {
				u.Password = cred
			}
		}
		users[name] = u
	}
	if len(users) == 0 {
		return nil, errors.New("SFTP_USERS must list at least one user:credential")
	}
	return users, nil
}

func credentialKind(cred string) string {
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cred)); err == nil {
		return "key"
	}
	for _, p := range []string{"$2a$", "$2b$", "$2y$"} {
		if strings.HasPrefix(cred, p) {
			return "hash"
		}
	}
	// Something meant as a key must not quietly become a password.
	for _, p := range []string{"ssh-", "ecdsa-sha2-", "sk-ssh-", "sk-ecdsa-"} {
		if strings.HasPrefix(cred, p) {
			return "bad key"
		}
	}
	return "password"
}

// plaintextUsers lists, sorted, the users whose password is stored in clear.
func plaintextUsers(users map[string]gateway.User) []string {
	var names []string
	for name, u := range users {
		if u.Password != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
