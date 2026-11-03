package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"

	"github.com/solcreek/sftp-s3/internal/gateway"
)

// loadUsers parses the users configuration: the JSON format when it starts
// with "{", otherwise the line format of parseUsers. JSON users are placed
// in their buckets here, defaulting to bucket and <prefix><name>/; line
// users are left to the gateway's defaults.
func loadUsers(s, bucket, prefix string) (map[string]gateway.User, error) {
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		return parseUsersJSON([]byte(s), bucket, prefix)
	}
	return parseUsers(s)
}

// usersFile is the JSON users format:
//
//	{"users": [{"username": "alice", "password_hash": "$2a$...",
//	            "public_keys": ["ssh-ed25519 AAAA..."],
//	            "bucket": "alice-uploads", "prefix": ""}]}
//
// bucket defaults to S3_BUCKET. An absent prefix means <S3_PREFIX><name>/,
// as for line-format users; "" is the bucket root; otherwise it ends in "/".
type usersFile struct {
	Users []jsonUser `json:"users"`
}

type jsonUser struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"password_hash"`
	PublicKeys   []string `json:"public_keys"`
	Bucket       string   `json:"bucket"`
	Prefix       *string  `json:"prefix"`
	// Password is only recognized to refuse it with a useful message.
	Password string `json:"password"`
}

func parseUsersJSON(data []byte, bucket, prefix string) (map[string]gateway.User, error) {
	var f usersFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	if dec.More() {
		return nil, errors.New("users file: unexpected data after the JSON object")
	}
	if len(f.Users) == 0 {
		return nil, errors.New("users file: \"users\" is empty")
	}
	users := map[string]gateway.User{}
	for i, ju := range f.Users {
		name := ju.Username
		where := fmt.Sprintf("users file: user %d (%q)", i+1, name)
		_, dup := users[name]
		switch {
		case name == "":
			return nil, fmt.Errorf("users file: user %d has no username", i+1)
		case name == "." || name == ".." || strings.ContainsFunc(name, func(r rune) bool {
			return r == '/' || unicode.IsSpace(r) || unicode.IsControl(r)
		}):
			return nil, fmt.Errorf("%s: a username may not be '.' or '..', or contain '/' or spaces", where)
		case dup:
			return nil, fmt.Errorf("%s: listed twice", where)
		case ju.Password != "":
			return nil, fmt.Errorf("%s: plaintext \"password\" is not accepted here; use \"password_hash\" (sftp-s3 -hash-password)", where)
		case ju.PasswordHash != "" && credentialKind(ju.PasswordHash) != "hash":
			return nil, fmt.Errorf("%s: password_hash is not a bcrypt hash ($2a$, $2b$ or $2y$)", where)
		}
		u := gateway.User{PasswordHash: ju.PasswordHash, Bucket: ju.Bucket}
		for j, k := range ju.PublicKeys {
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
			if err != nil {
				return nil, fmt.Errorf("%s: public key %d is unreadable", where, j+1)
			}
			u.Keys = append(u.Keys, key)
		}
		if u.Bucket == "" {
			if u.Bucket = bucket; bucket == "" {
				return nil, fmt.Errorf("%s: no bucket, and S3_BUCKET is not set", where)
			}
		}
		if u.Root = prefix + name + "/"; ju.Prefix != nil {
			u.Root = *ju.Prefix
		}
		users[name] = u
	}
	return users, nil
}

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
