package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/crypto/ssh"
)

var pemBegin = []byte("-----BEGIN ")

// hostKeys parses one or more PEM private keys, concatenated, one per key
// type (ed25519, ecdsa, rsa): the files an existing server keeps, joined
// with cat, so a single secret holds them all. It generates an ephemeral
// ed25519 key when pemData is empty. Production must pin its keys: an
// ephemeral one changes on every deploy and clients refuse to connect
// ("HOST IDENTIFICATION HAS CHANGED").
//
// Errors name a key by its position and PEM type, never by its contents.
func hostKeys(pemData string) ([]ssh.Signer, error) {
	if pemData == "" {
		slog.Warn("SFTP_HOST_KEY not set; generating an ephemeral ed25519 host key")
		// Neither call can fail: crypto/rand reads never return an error
		// (Go 1.24+), and ssh supports every ed25519 key.
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(priv)
		return []ssh.Signer{s}, nil
	}
	var keys []ssh.Signer
	byType := map[string]int{} // key type -> position, from 1
	rest := []byte(pemData)
	for {
		i := bytes.Index(rest, pemBegin)
		if i < 0 {
			break
		}
		n := len(keys) + 1
		if len(bytes.TrimSpace(rest[:i])) > 0 {
			return nil, fmt.Errorf("SFTP_HOST_KEY: unexpected text before key %d", n)
		}
		blk, after := pem.Decode(rest[i:])
		// pem.Decode skips a damaged block and returns the next one; count
		// the BEGIN lines it consumed so that cannot go unnoticed.
		if blk == nil || bytes.Count(rest[i:len(rest)-len(after)], pemBegin) != 1 {
			return nil, fmt.Errorf("SFTP_HOST_KEY: key %d is not a valid PEM block (truncated or damaged?)", n)
		}
		rest = after
		if !strings.HasSuffix(blk.Type, "PRIVATE KEY") {
			return nil, fmt.Errorf("SFTP_HOST_KEY: key %d is a %q block, not a private key", n, blk.Type)
		}
		s, err := ssh.ParsePrivateKey(pem.EncodeToMemory(blk))
		if err != nil {
			return nil, fmt.Errorf("SFTP_HOST_KEY: key %d (%s): %w", n, blk.Type, err)
		}
		typ := s.PublicKey().Type()
		if prev, ok := byType[typ]; ok {
			// ssh would silently offer only the last one.
			return nil, fmt.Errorf("SFTP_HOST_KEY: keys %d and %d are both %s; give one key per type", prev, n, typ)
		}
		byType[typ] = n
		keys = append(keys, s)
	}
	if len(keys) == 0 {
		return nil, errors.New("SFTP_HOST_KEY: no PEM private key found")
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("SFTP_HOST_KEY: unexpected text after key %d", len(keys))
	}
	return keys, nil
}
