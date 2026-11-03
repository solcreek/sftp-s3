package gateway

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T, key crypto.Signer) ssh.Signer {
	t.Helper()
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// dialHostKeyAlg logs in as alice with a client that accepts only the host
// key algorithm alg and only the pinned key.
func (e *env) dialHostKeyAlg(alg string, pinned ssh.PublicKey) error {
	c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User:              "alice",
		Auth:              []ssh.AuthMethod{ssh.Password("alice-pw")},
		HostKeyAlgorithms: []string{alg},
		HostKeyCallback:   ssh.FixedHostKey(pinned),
	})
	if err == nil {
		c.Close()
	}
	return err
}

// A client stores the host key of whichever type it negotiated, so a server
// replacing another must offer every type the old one did.
func TestHostKeyPerType(t *testing.T) {
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ed, ec, rs := newSigner(t, edKey), newSigner(t, ecKey), newSigner(t, rsaKey)
	e := newEnv(t, func(c *Config) { c.HostKeys = []ssh.Signer{ed, ec, rs} })

	for alg, key := range map[string]ssh.Signer{
		ssh.KeyAlgoED25519:   ed,
		ssh.KeyAlgoECDSA256:  ec,
		ssh.KeyAlgoRSASHA256: rs,
		ssh.KeyAlgoRSASHA512: rs,
	} {
		if err := e.dialHostKeyAlg(alg, key.PublicKey()); err != nil {
			t.Errorf("client pinned to the %s host key: %v", alg, err)
		}
	}
	// The pin bites: a client that negotiates ed25519 but holds the ecdsa
	// key is refused.
	if err := e.dialHostKeyAlg(ssh.KeyAlgoED25519, ec.PublicKey()); err == nil {
		t.Error("client pinned to the ecdsa key accepted the ed25519 one")
	}
}

// The restriction to one algorithm bites: a server without an ecdsa key
// cannot satisfy a client that accepts only ecdsa.
func TestHostKeyTypeMissing(t *testing.T) {
	e := newEnv(t)
	if err := e.dialHostKeyAlg(ssh.KeyAlgoECDSA256, e.hostPK); err == nil {
		t.Fatal("ecdsa-only client connected to an ed25519-only server")
	}
}
