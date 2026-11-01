package gateway

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

// User holds one login's credentials: a password (plaintext or a bcrypt
// hash, at most one of them), public keys, or both.
type User struct {
	Password     string // plaintext; meant for quick starts
	PasswordHash string // bcrypt ($2a$, $2b$ or $2y$)
	Keys         []ssh.PublicKey
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
	return nil
}

// dummyHash is compared against when the user is unknown or has no
// password, so that a wrong user name costs as much time as a wrong bcrypt
// password and names cannot be probed by timing. (Plaintext passwords are
// for quick starts and skip it.)
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

type authenticator struct {
	users map[string]User
	delay time.Duration // after a wrong password
}

func (a authenticator) password(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	u, ok := a.users[c.User()]
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
		return &ssh.Permissions{}, nil
	}
	time.Sleep(a.delay)
	return nil, fmt.Errorf("password rejected for %q", c.User())
}

// publicKey is not delayed on failure: clients offer every key they hold,
// one after another, and keys cannot be guessed.
func (a authenticator) publicKey(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if u, ok := a.users[c.User()]; ok {
		offered := key.Marshal()
		for _, k := range u.Keys {
			if bytes.Equal(k.Marshal(), offered) {
				return &ssh.Permissions{Extensions: map[string]string{"pubkey-fp": ssh.FingerprintSHA256(key)}}, nil
			}
		}
	}
	return nil, fmt.Errorf("key rejected for %q", c.User())
}
