package gateway

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

// User holds one login's credentials: a password (plaintext or a bcrypt
// hash, at most one of them), public keys, or both.
//
// Bucket and Root say where the user's files live: s3://Bucket/Root, where
// Root is "" for the bucket root or a key prefix ending in "/". Without a
// Bucket the user lives at s3://Config.Bucket/Config.Prefix<name>/.
type User struct {
	Password     string // plaintext; meant for quick starts
	PasswordHash string // bcrypt ($2a$, $2b$ or $2y$)
	Keys         []ssh.PublicKey
	Bucket       string
	Root         string
}

func (u User) validate() error {
	switch {
	case u.Password != "" && u.PasswordHash != "":
		return errors.New("both a password and a password hash")
	case u.Password == "" && u.PasswordHash == "" && len(u.Keys) == 0:
		return errors.New("no password, hash or key")
	}
	if u.PasswordHash != "" {
		if _, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil {
			return fmt.Errorf("password hash: %w", err)
		}
	}
	switch {
	case u.Bucket == "" && u.Root != "":
		return errors.New("a root without a bucket")
	case strings.Contains(u.Bucket, "/"):
		return fmt.Errorf("bucket %q contains '/'", u.Bucket)
	}
	return validRoot(u.Root)
}

// validRoot accepts "" (the bucket root) or a clean key prefix ending in
// "/", such as "a/b/". Anything else could make two users' trees overlap in
// ways their configuration does not show, or produce keys like "a//b".
func validRoot(root string) error {
	if root == "" {
		return nil
	}
	if !strings.HasSuffix(root, "/") || strings.HasPrefix(root, "/") ||
		path.Clean("/"+root) != "/"+strings.TrimSuffix(root, "/") {
		return fmt.Errorf("prefix %q must be empty or a clean path ending in '/', like \"a/b/\"", root)
	}
	return nil
}

// dummyHash is compared against when the user is unknown or has no
// password, so that a wrong user name costs as much time as a wrong bcrypt
// password and names cannot be probed by timing. (Plaintext passwords are
// for quick starts and skip it.)
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

// authenticator.users returns the current set; see Server.SetUsers.
type authenticator struct {
	users func() map[string]User
	delay time.Duration // after a wrong password
	// locate gives a user's bucket and root. The callbacks record them in
	// the Permissions they grant, so a session lives where the user set it
	// authenticated against says, even if SetUsers runs during the login.
	locate func(name string, u User) (bucket, root string)
}

func (a authenticator) grant(name string, u User, ext map[string]string) *ssh.Permissions {
	if ext == nil {
		ext = map[string]string{}
	}
	ext[extBucket], ext[extRoot] = a.locate(name, u)
	return &ssh.Permissions{Extensions: ext}
}

// Permissions.Extensions keys set by the authenticator.
const (
	extBucket = "sftp-s3-bucket"
	extRoot   = "sftp-s3-root"
	extKeyFP  = "pubkey-fp"
)

func (a authenticator) password(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	u, ok := a.users()[c.User()]
	var good bool
	switch {
	case ok && u.PasswordHash != "":
		good = bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), pass) == nil
	case ok && u.Password != "":
		good = subtle.ConstantTimeCompare([]byte(u.Password), pass) == 1
	default:
		bcrypt.CompareHashAndPassword(dummyHash, pass)
	}
	if good {
		return a.grant(c.User(), u, nil), nil
	}
	time.Sleep(a.delay)
	reason := errWrongPassword
	if !ok {
		reason = errUnknownUser
	}
	return nil, fmt.Errorf("password rejected for %q: %w", c.User(), reason)
}

// publicKey is not delayed on failure: clients offer every key they hold,
// one after another, and keys cannot be guessed.
func (a authenticator) publicKey(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if u, ok := a.users()[c.User()]; ok {
		offered := key.Marshal()
		for _, k := range u.Keys {
			if bytes.Equal(k.Marshal(), offered) {
				return a.grant(c.User(), u, map[string]string{extKeyFP: ssh.FingerprintSHA256(key)}), nil
			}
		}
	}
	return nil, fmt.Errorf("key rejected for %q", c.User())
}
