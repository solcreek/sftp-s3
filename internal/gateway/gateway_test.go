package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pkg/sftp"
)

func bytesReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

func upload(t *testing.T, c *sftp.Client, path string, data []byte) {
	t.Helper()
	f, err := c.Create(path)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	if _, err := f.ReadFrom(bytes.NewReader(data)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%s): %v", path, err)
	}
}

func download(t *testing.T, c *sftp.Client, path string) []byte {
	t.Helper()
	f, err := c.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func names(t *testing.T, c *sftp.Client, dir string) []string {
	t.Helper()
	fis, err := c.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var out []string
	for _, fi := range fis {
		n := fi.Name()
		if fi.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func TestUploadRoundTrip(t *testing.T) {
	e := newEnv(t)
	sizes := map[string]int{
		"empty":           0,
		"one-byte":        1,
		"small":           100_000,
		"multipart":       12 << 20, // 3 parts at 5 MiB
		"exact-part-size": 5 << 20,
	}
	for name, size := range sizes {
		t.Run(name, func(t *testing.T) {
			// Concurrent writes make the client send offsets out of order,
			// exercising the reorder buffer end to end.
			c := e.sftp("alice", sftp.UseConcurrentWrites(true))
			data := randBytes(t, size)
			upload(t, c, "/"+name+".bin", data)

			got, ok := e.object("alice/" + name + ".bin")
			if !ok || !bytes.Equal(got, data) {
				t.Fatalf("S3 object: ok=%v, %d bytes; want %d identical bytes", ok, len(got), size)
			}
			if back := download(t, c, "/"+name+".bin"); !bytes.Equal(back, data) {
				t.Fatalf("download: %d bytes, want %d identical bytes", len(back), size)
			}
		})
	}
}

// Compatible stores (GCS among them) reject per-part checksums, so when the
// client is configured to send checksums only when required, multipart
// uploads must not add them on their own.
func TestUploadHonorsClientChecksumMode(t *testing.T) {
	for _, mode := range []aws.RequestChecksumCalculation{
		aws.RequestChecksumCalculationWhenRequired, aws.RequestChecksumCalculationWhenSupported,
	} {
		e := newEnv(t, func(c *Config) {
			o := c.Client.Options()
			o.RequestChecksumCalculation = mode
			c.Client = s3.New(o)
		})
		if e.seen == nil {
			t.Skip("needs the in-memory store to inspect requests")
		}
		upload(t, e.sftp("alice"), "/big.bin", randBytes(t, 11<<20)) // multipart
		sent := e.seen.any("x-amz-checksum-") || e.seen.any("x-amz-sdk-checksum-") || e.seen.any("x-amz-trailer")
		if want := mode == aws.RequestChecksumCalculationWhenSupported; sent != want {
			t.Errorf("mode %v: checksum headers sent = %v, want %v", mode, sent, want)
		}
	}
}

func TestDownloadRandomAccess(t *testing.T) {
	e := newEnv(t)
	data := randBytes(t, 3*readWindow+123) // spans several read windows
	e.put("alice/big.bin", data)
	c := e.sftp("alice")
	f, err := c.Open("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, off := range []int64{0, readWindow - 10, 2*readWindow + 5, 100, int64(len(data)) - 50} {
		buf := make([]byte, 64)
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		want := data[off:min(off+64, int64(len(data)))]
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("ReadAt(%d) returned wrong bytes", off)
		}
	}
	if _, err := f.ReadAt(make([]byte, 10), int64(len(data))+10); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt past EOF = %v, want EOF", err)
	}
}

func TestOpenMissingFile(t *testing.T) {
	c := newEnv(t).sftp("alice")
	if _, err := c.Open("/nope.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open missing = %v, want ErrNotExist", err)
	}
}

func TestDirectories(t *testing.T) {
	e := newEnv(t)
	c := e.sftp("alice")

	if err := c.Mkdir("/inbox"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, ok := e.object("alice/inbox/"); !ok {
		t.Fatal("Mkdir did not create the alice/inbox/ marker")
	}
	if got := names(t, c, "/inbox"); len(got) != 0 {
		t.Fatalf("empty dir lists %v; the marker must be hidden", got)
	}

	upload(t, c, "/inbox/a.txt", []byte("a"))
	upload(t, c, "/inbox/sub/b.txt", []byte("bb")) // implicit directory, no marker
	upload(t, c, "/top.txt", []byte("top"))

	if got, want := names(t, c, "/"), []string{"inbox/", "top.txt"}; !slices.Equal(got, want) {
		t.Fatalf("ls / = %v, want %v", got, want)
	}
	if got, want := names(t, c, "/inbox"), []string{"a.txt", "sub/"}; !slices.Equal(got, want) {
		t.Fatalf("ls /inbox = %v, want %v", got, want)
	}

	for path, wantDir := range map[string]bool{"/": true, "/inbox": true, "/inbox/sub": true, "/inbox/a.txt": false} {
		fi, err := c.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if fi.IsDir() != wantDir {
			t.Fatalf("Stat(%s).IsDir() = %v, want %v", path, fi.IsDir(), wantDir)
		}
	}
	fi, err := c.Lstat("/inbox/sub/b.txt")
	if err != nil || fi.Size() != 2 || fi.ModTime().IsZero() {
		t.Fatalf("Lstat file = %+v, %v; want size 2 with a mod time", fi, err)
	}
	if _, err := c.Stat("/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat missing = %v, want ErrNotExist", err)
	}

	if err := c.RemoveDirectory("/inbox"); err == nil {
		t.Fatal("rmdir of a non-empty directory succeeded")
	}
	if err := c.Mkdir("/empty"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/empty"); err != nil {
		t.Fatalf("rmdir empty: %v", err)
	}
	if _, ok := e.object("alice/empty/"); ok {
		t.Fatal("rmdir left the directory marker behind")
	}
}

func TestListPaginates(t *testing.T) {
	e := newEnv(t)
	if testing.Short() {
		t.Skip("creates 1005 objects")
	}
	const n = 1005 // ListObjectsV2 returns at most 1000 keys per page
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			key := fmt.Sprintf("%salice/many/f%04d", e.base, i)
			if _, err := e.s3.PutObject(context.Background(), &s3.PutObjectInput{
				Bucket: &e.bucket, Key: &key, Body: bytes.NewReader(nil)}); err != nil {
				t.Error(err) // not Fatal: this is not the test goroutine
			}
		}()
	}
	wg.Wait()
	if got := len(names(t, e.sftp("alice"), "/many")); got != n {
		t.Fatalf("listed %d entries, want %d", got, n)
	}
}

func TestRenameAndRemove(t *testing.T) {
	e := newEnv(t)
	c := e.sftp("alice")
	upload(t, c, "/report.csv.filepart", []byte("data"))

	if err := c.Rename("/report.csv.filepart", "/report.csv"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, ok := e.object("alice/report.csv.filepart"); ok {
		t.Fatal("rename left the source object")
	}
	if got, ok := e.object("alice/report.csv"); !ok || string(got) != "data" {
		t.Fatalf("renamed object = %q, %v", got, ok)
	}
	if err := c.PosixRename("/report.csv", "/final.csv"); err != nil {
		t.Fatalf("PosixRename: %v", err)
	}
	if err := c.Rename("/missing", "/x"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Rename missing = %v, want ErrNotExist", err)
	}

	if err := c.Remove("/final.csv"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := e.object("alice/final.csv"); ok {
		t.Fatal("Remove left the object")
	}
}

func TestSetstatIsAcceptedAndIgnored(t *testing.T) {
	e := newEnv(t)
	c := e.sftp("alice")
	upload(t, c, "/f.txt", []byte("x"))
	if err := c.Chmod("/f.txt", 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := c.Chtimes("/f.txt", time.Now(), time.Now()); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func TestUnsupportedOperations(t *testing.T) {
	c := newEnv(t).sftp("alice")
	if err := c.Symlink("/a", "/b"); err == nil {
		t.Fatal("Symlink succeeded; want unsupported")
	}
	if _, err := c.ReadLink("/a"); err == nil {
		t.Fatal("ReadLink succeeded; want unsupported")
	}
}

func TestRandomAccessWriteFailsTheUpload(t *testing.T) {
	e := newEnv(t)
	c := e.sftp("alice")
	f, err := c.Create("/resume.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("hello world"), 0); err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("HELLO"), 0) // rewrite: the server must refuse
	if err := f.Close(); err == nil {
		t.Fatal("Close succeeded after a random-access rewrite")
	}
	if _, ok := e.object("alice/resume.bin"); ok {
		t.Fatal("a failed upload left an object behind")
	}
}

func TestUsersAreIsolated(t *testing.T) {
	e := newEnv(t)
	upload(t, e.sftp("alice"), "/secret.txt", []byte("alice only"))
	bob := e.sftp("bob")

	if got := names(t, bob, "/"); len(got) != 0 {
		t.Fatalf("bob's root lists %v, want empty", got)
	}
	for _, p := range []string{"../alice/secret.txt", "/../alice/secret.txt", "/../../alice/secret.txt"} {
		if _, err := bob.Open(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bob Open(%q) = %v, want ErrNotExist", p, err)
		}
	}
	// A traversal attempt on write lands inside bob's own root.
	upload(t, bob, "../alice/secret.txt", []byte("bob"))
	if got, _ := e.object("alice/secret.txt"); string(got) != "alice only" {
		t.Fatalf("alice's object was overwritten: %q", got)
	}
	if _, ok := e.object("bob/alice/secret.txt"); !ok {
		t.Fatal("bob's write did not land under bob/")
	}
}

func TestPrefix(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Prefix += "tenants/" })
	upload(t, e.sftp("alice"), "/x.txt", []byte("x"))
	if _, ok := e.object("tenants/alice/x.txt"); !ok {
		t.Fatal("object not written under the configured prefix")
	}
}
