package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func TestSetUsersAddsAndRemovesUsers(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.SetUsers(map[string]User{"alice": {Password: "alice-pw"}, "carol": {Password: "carol-pw"}}); err != nil {
		t.Fatal(err)
	}
	c, err := e.sshDial("carol", "carol-pw")
	if err != nil {
		t.Fatalf("added user cannot log in: %v", err)
	}
	c.Close()
	if c, err := e.sshDial("bob", "bob-pw"); err == nil {
		c.Close()
		t.Fatal("removed user can still log in with a password")
	}
	c, err = e.sshDial("alice", "alice-pw")
	if err != nil {
		t.Fatalf("kept user cannot log in: %v", err)
	}
	c.Close()
}

func TestSetUsersRemovesKeyLogins(t *testing.T) {
	key := newKey(t)
	e := newEnv(t, func(c *Config) { c.Users["carol"] = User{Keys: []ssh.PublicKey{key.PublicKey()}} })
	c, err := e.sshDialKey("carol", key)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := e.srv.SetUsers(map[string]User{"alice": {Password: "alice-pw"}}); err != nil {
		t.Fatal(err)
	}
	if c, err := e.sshDialKey("carol", key); err == nil {
		c.Close()
		t.Fatal("removed user can still log in with a key")
	}
}

func TestSetUsersRejectsInvalidUsersAndKeepsTheOldOnes(t *testing.T) {
	e := newEnv(t)
	for want, users := range map[string]map[string]User{
		"at least one user":        nil,
		"no password, hash or key": {"alice": {Password: "alice-pw"}, "carol": {}},
		"password hash":            {"alice": {PasswordHash: "$2a$not-a-hash"}},
		"prefix":                   {"alice": {Password: "p", Bucket: "b", Root: "x"}},
	} {
		if err := e.srv.SetUsers(users); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("SetUsers: err = %v, want it to mention %q", err, want)
		}
	}
	for _, u := range []string{"alice", "bob"} {
		c, err := e.sshDial(u, u+"-pw")
		if err != nil {
			t.Fatalf("%s cannot log in after a rejected SetUsers: %v", u, err)
		}
		c.Close()
	}
}

func TestSetUsersCopiesTheMap(t *testing.T) {
	e := newEnv(t)
	users := map[string]User{"alice": {Password: "alice-pw"}}
	if err := e.srv.SetUsers(users); err != nil {
		t.Fatal(err)
	}
	users["mallory"] = User{Password: "mallory-pw"} // never validated
	if c, err := e.sshDial("mallory", "mallory-pw"); err == nil {
		c.Close()
		t.Fatal("a change to the caller's map after SetUsers took effect")
	}
}

func TestSessionKeepsItsLocationAcrossSetUsers(t *testing.T) {
	e := newEnv(t)
	old := e.sftp("alice")
	write := func(name string) {
		t.Helper()
		f, err := old.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("before.txt")

	// alice moves to another prefix; her open session stays where it was.
	moved := User{Password: "alice-pw", Bucket: e.bucket, Root: e.base + "moved/"}
	if err := e.srv.SetUsers(map[string]User{"alice": moved}); err != nil {
		t.Fatal(err)
	}
	write("after.txt")
	for _, key := range []string{"alice/before.txt", "alice/after.txt"} {
		if _, ok := e.object(key); !ok {
			t.Errorf("%s missing: the open session lost its root", key)
		}
	}

	// A new login lands in the new place.
	f, err := e.sftp("alice").Create("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, ok := e.object("moved/new.txt"); !ok {
		t.Error("a new login did not get the new root")
	}

	// Removing alice does not end her open session either.
	if err := e.srv.SetUsers(map[string]User{"bob": {Password: "bob-pw"}}); err != nil {
		t.Fatal(err)
	}
	write("removed.txt")
	if _, ok := e.object("alice/removed.txt"); !ok {
		t.Error("the open session stopped working after its user was removed")
	}
}

func TestConcurrentLoginsDuringSetUsers(t *testing.T) {
	e := newEnv(t)
	sets := []map[string]User{
		{"alice": {Password: "alice-pw"}, "bob": {Password: "bob-pw"}},
		{"alice": {Password: "alice-pw"}, "carol": {Password: "carol-pw"}},
	}
	stop := make(chan struct{})
	var reloads sync.WaitGroup
	reloads.Add(1)
	go func() {
		defer reloads.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := e.srv.SetUsers(sets[i%2]); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var logins sync.WaitGroup
	for range 8 {
		logins.Add(1)
		go func() {
			defer logins.Done()
			for range 3 {
				c, err := e.sshDial("alice", "alice-pw")
				if err != nil {
					t.Errorf("alice is in every set but could not log in: %v", err)
					return
				}
				s, err := c.NewSession()
				if err != nil {
					t.Errorf("session: %v", err)
				} else {
					s.Close()
				}
				c.Close()
			}
		}()
	}
	logins.Wait()
	close(stop)
	reloads.Wait()
}

// A session lives where the user set it authenticated against says, even
// when SetUsers moves or removes the user during the login.
func TestLocationIsFixedAtAuthentication(t *testing.T) {
	for name, next := range map[string]map[string]User{
		"moved":   {"alice": {Password: "alice-pw", Bucket: testBucket, Root: "second/"}},
		"removed": {"bob": {Password: "bob-pw"}},
	} {
		t.Run(name, func(t *testing.T) {
			client, bucket, _ := memoryStore(t)
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			signer, _ := ssh.NewSignerFromKey(priv)
			srv, err := New(Config{Client: client, Bucket: bucket, HostKeys: []ssh.Signer{signer},
				Users: map[string]User{"alice": {Password: "alice-pw", Bucket: bucket, Root: "first/"}}})
			if err != nil {
				t.Fatal(err)
			}
			// Before Serve, so the server goroutines see the wrapped callback.
			check := srv.sshCfg.PasswordCallback
			srv.sshCfg.PasswordCallback = func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
				p, err := check(c, pass)
				if err == nil {
					if err := srv.SetUsers(next); err != nil {
						t.Error(err)
					}
				}
				return p, err
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			served := make(chan error, 1)
			go func() { served <- srv.Serve(ln) }()
			defer func() { ln.Close(); <-served }()

			conn, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
				User: "alice", Auth: []ssh.AuthMethod{ssh.Password("alice-pw")},
				HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			c, err := sftp.NewClient(conn)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			upload(t, c, "/x.txt", []byte("x"))
			for key, want := range map[string]bool{"first/x.txt": true, "second/x.txt": false} {
				_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
				if (err == nil) != want {
					t.Errorf("object %s exists: %v, want %v", key, err == nil, want)
				}
			}
		})
	}
}
