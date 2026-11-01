package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func authorizedKey(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if comment != "" {
		line += " " + comment
	}
	return line, k
}

const testHash = "$2a$04$J0BzbF3JxL5tOm3m2T6Qzu9H0iJ6q3Bx2jv8vB0b8l3oZ6sOe2hW2"

func TestParseUsersInline(t *testing.T) {
	got, err := parseUsers(" alice:s3cret , bob:pa:ss , carol:" + testHash + ",")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["alice"].Password != "s3cret" || got["bob"].Password != "pa:ss" ||
		got["carol"].PasswordHash != testHash || got["carol"].Password != "" {
		t.Fatalf("parseUsers = %+v", got)
	}
	if names := plaintextUsers(got); !slices.Equal(names, []string{"alice", "bob"}) {
		t.Fatalf("plaintextUsers = %v", names)
	}
}

func TestParseUsersFile(t *testing.T) {
	k1, pub1 := authorizedKey(t, "dave@laptop, the work one") // a comma in the comment
	k2, pub2 := authorizedKey(t, "")
	file := strings.Join([]string{
		"# users for the gateway",
		"",
		"dave: " + k1,
		"dave:" + k2,
		"dave:" + testHash,
		"erin: no-pty " + k2, // authorized_keys options are accepted (and ignored)
		"  frank:plain pass with spaces  ",
	}, "\n")
	got, err := parseUsers(file)
	if err != nil {
		t.Fatal(err)
	}
	dave := got["dave"]
	if len(dave.Keys) != 2 || dave.PasswordHash != testHash ||
		string(dave.Keys[0].Marshal()) != string(pub1.Marshal()) || string(dave.Keys[1].Marshal()) != string(pub2.Marshal()) {
		t.Fatalf("dave = %+v", dave)
	}
	if len(got["erin"].Keys) != 1 || got["frank"].Password != "plain pass with spaces" || len(got) != 3 {
		t.Fatalf("parseUsers = %+v", got)
	}
}

func TestParseUsersErrors(t *testing.T) {
	key, _ := authorizedKey(t, "")
	for in, want := range map[string]string{
		"":                                "at least one",
		" , ":                             "at least one",
		"# only a comment\n\n":            "at least one",
		"alice":                           "entry 1 is not user:credential",
		"alice:":                          "not user:credential",
		":pw":                             "not user:credential",
		"alice:pa,ss":                     "entry 2 is not user:credential", // comma inside an inline password
		"../evil:pw":                      "may not contain",
		"a.b:pw":                          "may not contain",
		"a b:pw":                          "may not contain",
		"alice:one,alice:2":               `user "alice" has a second password`,
		"alice:one\nalice:" + testHash:    "second password",
		"alice:ssh-ed25519 AAAAnotbase64": "unreadable public key",
		"alice:" + key[:len(key)-4]:       "unreadable public key",
	} {
		_, err := parseUsers(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseUsers(%q) = %v, want error containing %q", in, err, want)
		}
		if err != nil && strings.Contains(in, "one") && strings.Contains(err.Error(), "one") {
			t.Errorf("error %q leaks the password", err)
		}
	}
}
