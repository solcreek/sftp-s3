package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// streamWriter turns WriteAt calls into a sequential stream consumed by an
// upload function running in its own goroutine.
//
// pkg/sftp dispatches writes to a pool of workers, so offsets arrive out of
// order. Chunks ahead of the stream are parked until the gap fills, bounded by
// max bytes; a write behind the stream (true random access) is an error since
// S3 objects cannot be rewritten in place.
type streamWriter struct {
	mu       sync.Mutex
	pw       *io.PipeWriter
	off      int64 // next offset the stream expects
	pending  map[int64][]byte
	buffered int64
	max      int64
	failed   error
	done     chan error
	closed   bool
	start    time.Time
}

func newStreamWriter(max int64, upload func(io.Reader) error) *streamWriter {
	pr, pw := io.Pipe()
	w := &streamWriter{pw: pw, pending: map[int64][]byte{}, max: max, done: make(chan error, 1)}
	go func() {
		err := upload(pr)
		pr.CloseWithError(err) // unblock writers if the upload died
		w.done <- err
	}()
	return w
}

func (w *streamWriter) WriteAt(p []byte, off int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.start.IsZero() {
		w.start = time.Now()
	}
	if w.failed != nil {
		return 0, w.failed
	}
	switch {
	case off < w.off:
		w.failed = fmt.Errorf("random-access write at %d (stream at %d) is not supported", off, w.off)
		return 0, w.failed
	case off > w.off:
		if w.buffered+int64(len(p)) > w.max {
			w.failed = fmt.Errorf("out-of-order window exceeded %d bytes", w.max)
			return 0, w.failed
		}
		w.pending[off] = append([]byte(nil), p...) // p is reused by pkg/sftp
		w.buffered += int64(len(p))
		return len(p), nil
	}
	if err := w.emit(p); err != nil {
		return 0, err
	}
	for { // drain any parked chunks that are now contiguous
		next, ok := w.pending[w.off]
		if !ok {
			break
		}
		delete(w.pending, w.off)
		w.buffered -= int64(len(next))
		if err := w.emit(next); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *streamWriter) emit(p []byte) error {
	if _, err := w.pw.Write(p); err != nil {
		w.failed = err
		return err
	}
	w.off += int64(len(p))
	return nil
}

// Close ends the stream and waits for the upload. pkg/sftp reports its error
// as the result of SSH_FXP_CLOSE, so a failed upload is never silently "ok".
func (w *streamWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	err := w.failed
	if err == nil && len(w.pending) > 0 {
		err = fmt.Errorf("file closed with a hole at offset %d", w.off)
	}
	if err != nil {
		w.pw.CloseWithError(err) // the uploader sees the error and aborts
	} else {
		w.pw.Close()
	}
	w.mu.Unlock()
	upErr := <-w.done
	if err == nil {
		err = upErr
	}
	secs := time.Since(w.start).Seconds()
	slog.Info("upload done", "bytes", w.off, "secs", fmt.Sprintf("%.2f", secs),
		"MBps", fmt.Sprintf("%.1f", float64(w.off)/1e6/max(secs, 1e-9)), "err", err)
	return err
}

// TransferError is called by pkg/sftp when the client disconnects mid-upload.
func (w *streamWriter) TransferError(err error) {
	w.mu.Lock()
	if w.failed == nil {
		w.failed = fmt.Errorf("transfer aborted: %w", err)
	}
	w.mu.Unlock()
	w.Close()
}
