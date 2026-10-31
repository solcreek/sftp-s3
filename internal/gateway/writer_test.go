package gateway

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	mrand "math/rand/v2"
	"strings"
	"sync"
	"testing"
)

// captureUpload returns an upload func that reads the whole stream, plus a
// function that returns what it read and the read error.
func captureUpload() (func(io.Reader) error, func() ([]byte, error)) {
	var (
		buf     bytes.Buffer
		readErr error
		done    = make(chan struct{})
	)
	up := func(r io.Reader) error {
		defer close(done)
		_, readErr = io.Copy(&buf, r)
		return readErr
	}
	return up, func() ([]byte, error) { <-done; return buf.Bytes(), readErr }
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// chunks splits data into size-byte pieces keyed by offset.
func chunks(data []byte, size int) (offs []int64, parts [][]byte) {
	for off := 0; off < len(data); off += size {
		end := min(off+size, len(data))
		offs = append(offs, int64(off))
		parts = append(parts, data[off:end])
	}
	return
}

func TestStreamWriterInOrder(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(1<<20, up)
	data := randBytes(t, 100_000)
	offs, parts := chunks(data, 32*1024)
	for i := range parts {
		if n, err := w.WriteAt(parts[i], offs[i]); err != nil || n != len(parts[i]) {
			t.Fatalf("WriteAt(%d) = %d, %v", offs[i], n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if b, _ := got(); !bytes.Equal(b, data) {
		t.Fatalf("uploaded %d bytes, want %d identical bytes", len(b), len(data))
	}
}

func TestStreamWriterReordersShuffledWrites(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(1<<20, up)
	data := randBytes(t, 200_000)
	offs, parts := chunks(data, 4096)
	order := mrand.Perm(len(parts))
	for _, i := range order {
		if _, err := w.WriteAt(parts[i], offs[i]); err != nil {
			t.Fatalf("WriteAt(%d): %v", offs[i], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if b, _ := got(); !bytes.Equal(b, data) {
		t.Fatal("reordered stream differs from input")
	}
}

func TestStreamWriterConcurrentWriters(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(8<<20, up)
	data := randBytes(t, 1<<20)
	offs, parts := chunks(data, 8192)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // like pkg/sftp's worker pool
	for i := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if _, err := w.WriteAt(parts[i], offs[i]); err != nil {
				t.Errorf("WriteAt(%d): %v", offs[i], err)
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if b, _ := got(); !bytes.Equal(b, data) {
		t.Fatal("concurrent stream differs from input")
	}
}

func TestStreamWriterCopiesParkedChunks(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(1<<20, up)
	buf := []byte("BBBB")
	if _, err := w.WriteAt(buf, 4); err != nil {
		t.Fatal(err)
	}
	copy(buf, "XXXX") // pkg/sftp reuses packet buffers after WriteAt returns
	if _, err := w.WriteAt([]byte("AAAA"), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := got(); string(b) != "AAAABBBB" {
		t.Fatalf("got %q, want AAAABBBB", b)
	}
}

func TestStreamWriterErrors(t *testing.T) {
	tests := []struct {
		name    string
		writes  func(w *streamWriter) error // returns the first WriteAt error
		wantErr string                      // substring expected from Close
	}{
		{
			name: "write behind the stream",
			writes: func(w *streamWriter) error {
				if _, err := w.WriteAt([]byte("abcd"), 0); err != nil {
					return err
				}
				_, err := w.WriteAt([]byte("x"), 1)
				return err
			},
			wantErr: "random-access write",
		},
		{
			name: "reorder window exceeded",
			writes: func(w *streamWriter) error {
				_, err := w.WriteAt(make([]byte, 17), 100)
				return err
			},
			wantErr: "out-of-order window exceeded",
		},
		{
			name: "hole left at close",
			writes: func(w *streamWriter) error {
				_, err := w.WriteAt([]byte("tail"), 10)
				return err
			},
			wantErr: "hole at offset 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up, got := captureUpload()
			w := newStreamWriter(16, up)
			werr := tt.writes(w)
			cerr := w.Close()
			if cerr == nil || !strings.Contains(cerr.Error(), tt.wantErr) {
				t.Fatalf("Close() = %v, want error containing %q", cerr, tt.wantErr)
			}
			if werr != nil && !strings.Contains(werr.Error(), tt.wantErr) {
				t.Fatalf("WriteAt error = %v, want %q", werr, tt.wantErr)
			}
			// The uploader must see the failure, so it aborts instead of
			// completing a truncated object.
			if _, rerr := got(); rerr == nil {
				t.Fatal("upload stream ended cleanly; want an error so the upload aborts")
			}
			// Writes after a failure keep failing.
			if _, err := w.WriteAt([]byte("z"), 1<<30); err == nil && werr != nil {
				t.Fatal("WriteAt after failure succeeded")
			}
		})
	}
}

func TestStreamWriterUploadFailure(t *testing.T) {
	boom := errors.New("s3 is down")
	w := newStreamWriter(1<<20, func(r io.Reader) error {
		io.CopyN(io.Discard, r, 3)
		return boom
	})
	// Writes either succeed into the pipe or fail once the upload is gone.
	for off := int64(0); off < 1<<16; off += 1024 {
		if _, err := w.WriteAt(make([]byte, 1024), off); err != nil {
			break
		}
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("Close() = %v, want %v", err, boom)
	}
}

func TestStreamWriterTransferErrorAborts(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(1<<20, up)
	if _, err := w.WriteAt([]byte("partial"), 0); err != nil {
		t.Fatal(err)
	}
	w.TransferError(errors.New("client went away"))
	if _, rerr := got(); rerr == nil || !strings.Contains(rerr.Error(), "client went away") {
		t.Fatalf("upload read error = %v, want transfer aborted", rerr)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil (already closed)", err)
	}
}

func TestStreamWriterEmptyFile(t *testing.T) {
	up, got := captureUpload()
	w := newStreamWriter(1<<20, up)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := got(); err != nil || len(b) != 0 {
		t.Fatalf("got %d bytes, err %v; want empty upload", len(b), err)
	}
}
