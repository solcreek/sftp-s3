package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"golang.org/x/crypto/ssh"
)

// hostKeyPair is a private key as an existing server stores it, and its
// public half.
type hostKeyPair struct {
	pem string
	pub ssh.PublicKey
}

func newKeys(t *testing.T) (ed, ec, ec384, rs crypto.Signer) {
	t.Helper()
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return edKey, ecKey, ec384Key, rsaKey
}

// openssh encodes key as ssh-keygen writes it today.
func openssh(t *testing.T, key crypto.Signer) hostKeyPair {
	t.Helper()
	blk, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return pair(t, key, blk)
}

// legacy encodes key in the older formats other servers write: PKCS#1
// "RSA PRIVATE KEY", SEC 1 "EC PRIVATE KEY", PKCS#8 "PRIVATE KEY".
func legacy(t *testing.T, key crypto.Signer) hostKeyPair {
	t.Helper()
	var blk *pem.Block
	switch k := key.(type) {
	case *rsa.PrivateKey:
		blk = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		blk = &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	default:
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		blk = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	return pair(t, key, blk)
}

func pair(t *testing.T, key crypto.Signer, blk *pem.Block) hostKeyPair {
	t.Helper()
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return hostKeyPair{pem: string(pem.EncodeToMemory(blk)), pub: pub}
}

func TestHostKeys(t *testing.T) {
	ed, ec, ec384, rs := newKeys(t)
	for name, keys := range map[string][]hostKeyPair{
		"one key":           {openssh(t, ed)},
		"openssh format":    {openssh(t, ed), openssh(t, ec), openssh(t, rs)},
		"legacy formats":    {legacy(t, rs), legacy(t, ec), legacy(t, ed)},
		"two ecdsa curves":  {openssh(t, ec), openssh(t, ec384)},
		"mixed and spacing": {openssh(t, ed), legacy(t, rs)},
	} {
		var all strings.Builder
		for _, k := range keys {
			// cat of files that may or may not end in a newline, with
			// blank lines and CRLF line ends as editors leave them.
			all.WriteString("\r\n" + strings.TrimRight(k.pem, "\n") + "\n\n")
		}
		got, err := hostKeys(all.String())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got) != len(keys) {
			t.Errorf("%s: %d keys, want %d", name, len(got), len(keys))
			continue
		}
		for i, k := range keys {
			if !bytes.Equal(got[i].PublicKey().Marshal(), k.pub.Marshal()) {
				t.Errorf("%s: key %d is not the one supplied", name, i+1)
			}
		}
	}
}

func TestHostKeysEphemeral(t *testing.T) {
	a, err := hostKeys("")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hostKeys("")
	if len(a) != 1 || a[0].PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("ephemeral keys %v, want one ed25519 key", a)
	}
	if bytes.Equal(a[0].PublicKey().Marshal(), b[0].PublicKey().Marshal()) {
		t.Fatal("two ephemeral keys are identical")
	}
}

func TestHostKeysErrors(t *testing.T) {
	ed, ec, _, rs := newKeys(t)
	edPEM, ecPEM, rsaPEM := openssh(t, ed).pem, openssh(t, ec).pem, openssh(t, rs).pem
	damaged := strings.Replace(ecPEM, "\n", "\n!!!", 2) // body no longer base64
	_, edOther, _ := ed25519.GenerateKey(rand.Reader)
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(edOther, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not really")}))
	junk := string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("not a key")}))

	for _, tc := range []struct{ name, in, want string }{
		{"garbage", "not a key", "no PEM private key found"},
		{"whitespace", " \n\t\n", "no PEM private key found"},
		{"text before", "comment\n" + edPEM, "unexpected text before key 1"},
		{"text between", edPEM + "comment\n" + ecPEM, "unexpected text before key 2"},
		{"text after", edPEM + ecPEM + "comment", "unexpected text after key 2"},
		{"truncated", edPEM + rsaPEM[:len(rsaPEM)/2], "key 2 is not a valid PEM block"},
		{"damaged then valid", edPEM + damaged + rsaPEM, "key 2 is not a valid PEM block"},
		{"not a private key", edPEM + cert, `key 2 is a "CERTIFICATE" block, not a private key`},
		{"not a key inside", junk, "key 1 (OPENSSH PRIVATE KEY): ssh:"},
		{"passphrase", string(pem.EncodeToMemory(encrypted)), "key 1 (OPENSSH PRIVATE KEY): ssh: this private key is passphrase protected"},
		{"two of a type", edPEM + rsaPEM + legacy(t, edOther).pem, "keys 1 and 3 are both ssh-ed25519"},
		{"two rsa", rsaPEM + legacy(t, rs).pem, "keys 1 and 2 are both ssh-rsa"},
	} {
		_, err := hostKeys(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "SFTP_HOST_KEY: ") {
			t.Errorf("%s: err = %v, want SFTP_HOST_KEY: ...%s", tc.name, err, tc.want)
			continue
		}
		// No line of key material may reach the message (and so the log).
		for _, line := range strings.Split(tc.in, "\n") {
			if len(line) >= 8 && !strings.HasPrefix(line, "-----") && strings.Contains(err.Error(), line) {
				t.Errorf("%s: error %q echoes the input line %q", tc.name, err, line)
			}
		}
	}
}

func TestLoadConfigHostKeys(t *testing.T) {
	isolateAWS(t)
	ed, ec, _, rs := newKeys(t)
	want := []hostKeyPair{openssh(t, ed), legacy(t, ec), legacy(t, rs)}
	cfg, err := loadConfig(context.Background(), envMap(map[string]string{
		"S3_BUCKET":     "b",
		"SFTP_USERS":    "a:b",
		"SFTP_HOST_KEY": want[0].pem + want[1].pem + want[2].pem,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostKeys) != len(want) {
		t.Fatalf("%d host keys, want %d", len(cfg.HostKeys), len(want))
	}
	for i, k := range want {
		if !bytes.Equal(cfg.HostKeys[i].PublicKey().Marshal(), k.pub.Marshal()) {
			t.Errorf("host key %d is not the one supplied", i+1)
		}
	}
}

// Operators compare these with what clients have pinned, so every key is
// shown with its type, in -check output and in the startup log.
func TestHostKeyFingerprints(t *testing.T) {
	isolateAWS(t)
	backend := s3mem.New()
	if err := backend.CreateBucket("uploads"); err != nil {
		t.Fatal(err)
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	ed, ec, _, rs := newKeys(t)
	keys := []hostKeyPair{openssh(t, ed), openssh(t, ec), legacy(t, rs)}
	env := map[string]string{
		"S3_BUCKET":     "uploads",
		"S3_ENDPOINT":   store.URL,
		"SFTP_USERS":    "alice:pw",
		"SFTP_HOST_KEY": keys[0].pem + keys[1].pem + keys[2].pem,
		"PORT":          "0",
	}

	var out strings.Builder
	if err := runCheck(context.Background(), envMap(env), &out); err != nil {
		t.Fatalf("check failed: %v\n%s", err, out.String())
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, envMap(env)); err != nil {
		t.Fatal(err)
	}

	for _, k := range keys {
		line := "host key: " + k.pub.Type() + " " + ssh.FingerprintSHA256(k.pub) + "\n"
		if !strings.Contains(out.String(), line) {
			t.Errorf("-check output lacks %q:\n%s", line, out.String())
		}
		attrs := `msg="host key" type=` + k.pub.Type() + " fingerprint=" + ssh.FingerprintSHA256(k.pub)
		if !strings.Contains(logs.String(), attrs) {
			t.Errorf("startup log lacks %q:\n%s", attrs, logs.String())
		}
	}
}
