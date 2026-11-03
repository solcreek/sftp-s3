//go:build unix

package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"golang.org/x/crypto/ssh"
)

// TestRunReloadsUsersOnSIGHUP delivers real SIGHUPs to the process while
// run serves, and checks logins and an open session around each one.
func TestRunReloadsUsersOnSIGHUP(t *testing.T) {
	isolateAWS(t)
	// Until run registers its own handler, a stray SIGHUP would kill the
	// test binary; this one keeps it alive either way.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGHUP)
	defer signal.Stop(guard)

	backend := s3mem.New()
	if err := backend.CreateBucket("uploads"); err != nil {
		t.Fatal(err)
	}
	store := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(store.Close)
	keyPEM, hostPub := testKeyPEM(t)
	usersFile := filepath.Join(t.TempDir(), "users")
	writeFile(t, usersFile, "alice:alice-pw\n")
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, envMap(map[string]string{
			"PORT": "0", "S3_BUCKET": "uploads", "S3_ENDPOINT": store.URL,
			"SFTP_USERS_FILE": usersFile, "SFTP_HOST_KEY": keyPEM,
		}))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run = %v", err)
		}
	}()

	addrRE := regexp.MustCompile(`msg=listening .*addr=\S*:(\d+)`)
	var port string
	waitFor(t, func() bool {
		m := addrRE.FindStringSubmatch(logs.String())
		if m != nil {
			port = m[1]
		}
		return m != nil
	})
	dial := func(user, pass string) (*ssh.Client, error) {
		return ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", port), &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
			HostKeyCallback: ssh.FixedHostKey(hostPub), Timeout: 5 * time.Second,
		})
	}
	canLogIn := func(user string) bool {
		c, err := dial(user, user+"-pw")
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	hup := func(wantLog string, times int) {
		t.Helper()
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return strings.Count(logs.String(), wantLog) == times })
	}

	alice, err := dial("alice", "alice-pw")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	defer alice.Close()
	if canLogIn("carol") {
		t.Fatal("carol logged in before being added")
	}

	writeFile(t, usersFile, "carol:carol-pw\n")
	hup(`users reloaded" users=1`, 1)
	if !canLogIn("carol") {
		t.Error("added user cannot log in after SIGHUP")
	}
	if canLogIn("alice") {
		t.Error("removed user can still log in after SIGHUP")
	}
	if s, err := alice.NewSession(); err != nil {
		t.Errorf("alice's open connection broke on reload: %v", err)
	} else {
		s.Close()
	}

	writeFile(t, usersFile, "not a users file\n")
	hup("users not reloaded", 1)
	if !canLogIn("carol") {
		t.Error("an invalid users file replaced the users")
	}
	alice.Close()
}
