package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// session logs in as alice and returns an SFTP client and its connection.
func (e *env) session() (*sftp.Client, *ssh.Client) {
	e.t.Helper()
	conn, err := e.sshDial("alice", "alice-pw")
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, conn
}

// Each operation logs exactly one event, with the listed attributes, and
// all of a session's events share its session id.
func TestAuditFileOperations(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	c, conn := e.session()
	data := []byte("hello audit") // 11 bytes
	upload(t, c, "/a.txt", data)
	if got := download(t, c, "/a.txt"); !bytes.Equal(got, data) {
		t.Fatalf("downloaded %q", got)
	}
	for _, op := range []error{
		c.Mkdir("/d"),
		c.Rename("/a.txt", "/d/b.txt"),
		c.Chmod("/d/b.txt", 0o600),
		c.Remove("/d/b.txt"),
		c.RemoveDirectory("/d"),
	} {
		if op != nil {
			t.Fatal(op)
		}
	}
	c.Close()
	conn.Close()

	evs := stopAndCollect(t, e, logs)
	if got := fmt.Sprint(eventNames(evs)); got != "[login upload download mkdir rename setstat remove rmdir session_end]" {
		t.Fatalf("events %s", got)
	}
	root, b := e.base+"alice/", e.bucket
	for i, want := range []map[string]any{
		1: {"bucket": b, "key": root + "a.txt", "bytes": 11.0, "duration_ms": nil, "result": "ok"},
		2: {"bucket": b, "key": root + "a.txt", "bytes": 11.0, "duration_ms": nil, "result": "ok"},
		3: {"bucket": b, "key": root + "d/", "result": "ok"},
		4: {"bucket": b, "from": root + "a.txt", "to": root + "d/b.txt", "result": "ok"},
		5: {"bucket": b, "key": root + "d/b.txt", "result": "ok"},
		6: {"bucket": b, "key": root + "d/b.txt", "result": "ok"},
		7: {"bucket": b, "key": root + "d/", "result": "ok"},
		8: {"duration_ms": nil, "bytes_in": 11.0, "bytes_out": 11.0},
	} {
		if i == 0 {
			continue // login: see TestAuditPasswordLoginAndSessionEnd
		}
		want["user"] = "alice"
		check(t, evs[i], want)
		if evs[i]["session"] != evs[0]["session"] {
			t.Errorf("%v event: session %v, login's %v", evs[i]["audit"], evs[i]["session"], evs[0]["session"])
		}
	}

	// "upload start" stays, at debug level, outside the audit events.
	var starts []any
	for _, r := range records(t, logs) {
		if r["msg"] == "upload start" {
			starts = append(starts, r["level"], r["key"])
		}
	}
	if fmt.Sprint(starts) != fmt.Sprint([]any{"DEBUG", root + "a.txt"}) {
		t.Errorf("upload start records: %v", starts)
	}
}

// failed asserts that ev is a failed op event whose error contains msg.
func failed(t *testing.T, ev event, op, msg string) {
	t.Helper()
	if ev["audit"] != op || ev["result"] != "failed" || !strings.Contains(fmt.Sprint(ev["error"]), msg) {
		t.Errorf("event %v, want a failed %s with an error containing %q", ev, op, msg)
	}
}

func TestAuditFailedOperations(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	e.put("alice/full/x", []byte("x"))
	c, conn := e.session()

	f, err := c.Create("/resume.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("HELLO"), 0) // rewrite: refused
	if f.Close() == nil {
		t.Fatal("random-access upload succeeded")
	}
	expectErr := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("operation succeeded; want a failure")
		}
	}
	_, err = c.Create("/")
	expectErr(err)
	_, err = c.Open("/")
	expectErr(err)
	_, err = c.Open("/missing")
	expectErr(err)
	expectErr(c.Mkdir("/"))
	expectErr(c.RemoveDirectory("/full"))
	expectErr(c.Rename("/missing", "/x"))
	expectErr(c.Symlink("/a", "/b")) // unsupported: changes nothing, not logged
	c.Close()
	conn.Close()

	evs := stopAndCollect(t, e, logs)
	if got := fmt.Sprint(eventNames(evs)); got != "[login upload upload download download mkdir rmdir rename session_end]" {
		t.Fatalf("events %s", got)
	}
	root := e.base + "alice/"
	failed(t, evs[1], evUpload, "random-access write")
	check(t, evs[1], map[string]any{"bucket": e.bucket, "key": root + "resume.bin", "bytes": 5.0, "duration_ms": nil, "result": "failed", "error": nil})
	failed(t, evs[2], evUpload, "permission denied")
	check(t, evs[2], map[string]any{"bucket": e.bucket, "key": root, "bytes": 0.0, "duration_ms": nil, "result": "failed", "error": nil})
	failed(t, evs[3], evDownload, "permission denied")
	failed(t, evs[4], evDownload, "file does not exist")
	check(t, evs[4], map[string]any{"bucket": e.bucket, "key": root + "missing", "bytes": 0.0, "duration_ms": nil, "result": "failed", "error": nil})
	failed(t, evs[5], evMkdir, "permission denied")
	failed(t, evs[6], evRmdir, "directory not empty")
	check(t, evs[6], map[string]any{"bucket": e.bucket, "key": root + "full/", "result": "failed", "error": nil})
	failed(t, evs[7], evRename, "file does not exist")
	check(t, evs[7], map[string]any{"bucket": e.bucket, "from": root + "missing", "to": root + "x", "result": "failed", "error": nil})
	check(t, evs[8], map[string]any{"duration_ms": nil, "bytes_in": 5.0, "bytes_out": 0.0})
}

// A client that goes away mid-transfer: the upload and the download log as
// failed when the server notices, before session_end.
func TestAuditTransfersAbortedByDisconnect(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t)
	e.put("alice/big", bytes.Repeat([]byte("x"), 100))
	c, conn := e.session()
	up, err := c.Create("/partial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	down, err := c.Open("/big")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(down, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	conn.Close() // with both files open

	evs := stopAndCollect(t, e, logs)
	if got := fmt.Sprint(eventNames(evs)); got != "[login upload download session_end]" && got != "[login download upload session_end]" {
		t.Fatalf("events %s", got)
	}
	for _, ev := range evs[1:3] {
		failed(t, ev, ev["audit"].(string), "transfer aborted")
	}
	if _, ok := e.object("alice/partial"); ok {
		t.Error("an aborted upload left an object behind")
	}
	if evs[3]["bytes_in"] != 5.0 {
		t.Errorf("bytes_in = %v, want 5", evs[3]["bytes_in"])
	}
}

// An upload that S3 refuses logs the store's error as failed.
func TestAuditUploadRefusedByTheStore(t *testing.T) {
	logs := captureJSON(t)
	e := newEnv(t, func(c *Config) {
		c.Users["carol"] = User{Password: "carol-pw", Bucket: "no-such-bucket-sftp-s3-test"}
	})
	conn, err := e.sshDial("carol", "carol-pw")
	if err != nil {
		t.Fatal(err)
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Create("/x")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("x"))
	if f.Close() == nil {
		t.Fatal("upload to a missing bucket succeeded")
	}
	c.Close()
	conn.Close()
	ev := only(t, stopAndCollect(t, e, logs), evUpload)
	failed(t, ev, evUpload, "NoSuchBucket")
	noSecrets(t, logs, "carol-pw")
}

// s3Reader on its own: a store error mid-download, Close twice.
func TestS3ReaderReportsOnce(t *testing.T) {
	client, bucket, _ := memoryStore(t)
	ctx := context.Background()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String("k"), Body: strings.NewReader("abcdef")}); err != nil {
		t.Fatal(err)
	}
	var calls int
	var sent int64
	var got error
	r := &s3Reader{client: client, bucket: bucket, key: "k", finish: func(n int64, err error) { calls++; sent, got = n, err }}
	p := make([]byte, 4)
	if n, err := r.ReadAt(p, 0); n != 4 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if n, err := r.ReadAt(p, 4); n != 2 || err != io.EOF {
		t.Fatalf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: aws.String("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(p, 100); err == nil || err == io.EOF { // outside the window: fetched, and gone
		t.Fatalf("ReadAt of a deleted object: %v", err)
	}
	r.TransferError(errors.New("later")) // the first error wins
	r.Close()
	r.Close()
	if calls != 1 || sent != 6 || got == nil || strings.Contains(got.Error(), "later") {
		t.Fatalf("finish called %d times with %d, %v; want once with 6 and the read error", calls, sent, got)
	}
}

// TransferError alone (the client went away) makes the download failed.
func TestS3ReaderTransferError(t *testing.T) {
	var got error
	r := &s3Reader{finish: func(_ int64, err error) { got = err }}
	r.TransferError(os.ErrClosed)
	r.Close()
	if !errors.Is(got, os.ErrClosed) || !strings.HasPrefix(got.Error(), "transfer aborted") {
		t.Fatalf("finish got %v", got)
	}
}
