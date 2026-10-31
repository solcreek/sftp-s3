package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/pkg/sftp"
)

type s3fs struct {
	client     *s3.Client
	bucket     string
	prefix     string // global key prefix, e.g. "sftp/"
	partSize   int64
	maxReorder int64 // bytes of out-of-order writes we are willing to buffer per file
}

func (f *s3fs) forUser(user string) *userFS {
	return &userFS{s3fs: f, root: f.prefix + user + "/"}
}

// userFS implements the pkg/sftp request-server handlers for one user.
type userFS struct {
	*s3fs
	root string
}

// key maps an SFTP path ("/a/b.txt") to an S3 key ("<prefix><user>/a/b.txt").
func (u *userFS) key(p string) string {
	return u.root + strings.TrimPrefix(path.Clean("/"+p), "/")
}

// dirKey returns the S3 prefix that represents directory p ("" + root for "/").
func (u *userFS) dirKey(p string) string {
	k := u.key(p)
	if !strings.HasSuffix(k, "/") {
		k += "/"
	}
	return k
}

// ---------- uploads ----------

func (u *userFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	pr, pw := io.Pipe()
	w := &s3Writer{pw: pw, pending: map[int64][]byte{}, max: u.maxReorder, done: make(chan error, 1)}
	key := u.key(r.Filepath)
	up := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = u.partSize
		m.Concurrency = 4
	})
	go func() {
		_, err := up.Upload(context.Background(), &s3.PutObjectInput{
			Bucket: &u.bucket, Key: &key, Body: pr,
		})
		pr.CloseWithError(err) // unblock writers if the upload died
		w.done <- err
	}()
	slog.Info("upload start", "key", key)
	return w, nil
}

// s3Writer turns WriteAt (which pkg/sftp calls from 8 concurrent workers, so
// offsets arrive out of order) into a sequential stream for the S3 uploader.
// Out-of-order chunks are parked until the gap fills, bounded by max bytes.
type s3Writer struct {
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

func (w *s3Writer) WriteAt(p []byte, off int64) (int, error) {
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
		// Rewrite of bytes already streamed: S3 cannot do this.
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

func (w *s3Writer) emit(p []byte) error {
	if _, err := w.pw.Write(p); err != nil {
		w.failed = err
		return err
	}
	w.off += int64(len(p))
	return nil
}

// Close finishes the multipart upload. Its error is reported to the client as
// the result of SSH_FXP_CLOSE, so a failed upload is never silently "ok".
func (w *s3Writer) Close() error {
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
		w.pw.CloseWithError(err) // aborts the multipart upload
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
func (w *s3Writer) TransferError(err error) {
	w.mu.Lock()
	if w.failed == nil {
		w.failed = fmt.Errorf("transfer aborted: %w", err)
	}
	w.mu.Unlock()
	w.Close()
}

// ---------- downloads ----------

func (u *userFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	key := u.key(r.Filepath)
	if _, err := u.client.HeadObject(r.Context(), &s3.HeadObjectInput{Bucket: &u.bucket, Key: &key}); err != nil {
		return nil, mapErr(err)
	}
	return &s3Reader{u: u, key: key}, nil
}

// s3Reader serves ReadAt from a 4 MiB window fetched with ranged GETs.
type s3Reader struct {
	u      *userFS
	key    string
	mu     sync.Mutex
	winOff int64
	win    []byte
}

const readWindow = 4 << 20

func (r *s3Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off < r.winOff || off+int64(len(p)) > r.winOff+int64(len(r.win)) {
		rng := fmt.Sprintf("bytes=%d-%d", off, off+readWindow-1)
		out, err := r.u.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: &r.u.bucket, Key: &r.key, Range: &rng})
		if err != nil {
			var re interface{ HTTPStatusCode() int }
			if errors.As(err, &re) && re.HTTPStatusCode() == 416 {
				return 0, io.EOF
			}
			return 0, err
		}
		r.win, err = io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil {
			return 0, err
		}
		r.winOff = off
	}
	n := copy(p, r.win[off-r.winOff:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// ---------- commands ----------

func (u *userFS) Filecmd(r *sftp.Request) error {
	ctx := r.Context()
	switch r.Method {
	case "Setstat":
		return nil // chmod/chown/utimes have no S3 equivalent; accept and ignore
	case "Mkdir":
		k := u.dirKey(r.Filepath) // zero-byte "dir/" marker so empty dirs show up
		_, err := u.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &u.bucket, Key: &k, Body: strings.NewReader("")})
		return mapErr(err)
	case "Remove":
		k := u.key(r.Filepath)
		_, err := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &u.bucket, Key: &k})
		return mapErr(err)
	case "Rmdir":
		k := u.dirKey(r.Filepath)
		out, err := u.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &u.bucket, Prefix: &k, MaxKeys: aws.Int32(2)})
		if err != nil {
			return mapErr(err)
		}
		for _, o := range out.Contents {
			if *o.Key != k {
				return fmt.Errorf("directory not empty")
			}
		}
		_, err = u.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &u.bucket, Key: &k})
		return mapErr(err)
	case "Rename", "PosixRename":
		// WinSCP/FileZilla upload to "x.filepart" then rename; S3 needs copy+delete.
		// CopyObject handles objects up to 5 GB, enough for a POC.
		src, dst := u.key(r.Filepath), u.key(r.Target)
		cp := u.bucket + "/" + src
		if _, err := u.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &u.bucket, Key: &dst, CopySource: &cp}); err != nil {
			return mapErr(err)
		}
		_, err := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &u.bucket, Key: &src})
		return mapErr(err)
	}
	return sftp.ErrSSHFxOpUnsupported
}

// ---------- listing / stat ----------

func (u *userFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	ctx := r.Context()
	switch r.Method {
	case "List":
		return u.list(ctx, r.Filepath)
	case "Stat", "Lstat":
		fi, err := u.stat(ctx, r.Filepath)
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (u *userFS) stat(ctx context.Context, p string) (os.FileInfo, error) {
	name := path.Base(path.Clean("/" + p))
	if path.Clean("/"+p) == "/" {
		return fileInfo{name: "/", dir: true}, nil
	}
	k := u.key(p)
	if h, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &u.bucket, Key: &k}); err == nil {
		return fileInfo{name: name, size: aws.ToInt64(h.ContentLength), mtime: aws.ToTime(h.LastModified)}, nil
	}
	dk := u.dirKey(p) // not an object: is it a "directory" (any key under it)?
	out, err := u.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &u.bucket, Prefix: &dk, MaxKeys: aws.Int32(1)})
	if err != nil {
		return nil, mapErr(err)
	}
	if len(out.Contents) > 0 || len(out.CommonPrefixes) > 0 {
		return fileInfo{name: name, dir: true}, nil
	}
	return nil, os.ErrNotExist
}

func (u *userFS) list(ctx context.Context, p string) (sftp.ListerAt, error) {
	dk := u.dirKey(p)
	if path.Clean("/"+p) == "/" {
		dk = u.root
	}
	var items listerAt
	pg := s3.NewListObjectsV2Paginator(u.client, &s3.ListObjectsV2Input{
		Bucket: &u.bucket, Prefix: &dk, Delimiter: aws.String("/")})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, cp := range out.CommonPrefixes {
			items = append(items, fileInfo{name: path.Base(strings.TrimSuffix(*cp.Prefix, "/")), dir: true})
		}
		for _, o := range out.Contents {
			if *o.Key == dk {
				continue // the directory marker itself
			}
			items = append(items, fileInfo{name: path.Base(*o.Key), size: aws.ToInt64(o.Size), mtime: aws.ToTime(o.LastModified)})
		}
	}
	return items, nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return os.ErrNotExist
	}
	return err
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

type fileInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.size }
func (f fileInfo) ModTime() time.Time { return f.mtime }
func (f fileInfo) IsDir() bool        { return f.dir }
func (f fileInfo) Sys() any           { return nil }
func (f fileInfo) Mode() os.FileMode {
	if f.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
