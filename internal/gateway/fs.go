package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/pkg/sftp"
)

// userFS implements the pkg/sftp request-server handlers for one user,
// confined to keys under root.
type userFS struct {
	client     *s3.Client
	bucket     string
	root       string // "" (the bucket root) or a prefix ending in "/"
	partSize   int64
	maxReorder int64
	sess       *session // logs the operations as audit events
}

// key maps an SFTP path ("/a/b.txt") to an S3 key ("<root>a/b.txt").
// path.Clean on a rooted path drops any "..", so keys never leave root.
func (u *userFS) key(p string) string {
	return u.root + strings.TrimPrefix(path.Clean("/"+p), "/")
}

// dirKey returns the S3 prefix that represents directory p.
func (u *userFS) dirKey(p string) string {
	k := u.key(p)
	if k != "" && !strings.HasSuffix(k, "/") {
		k += "/"
	}
	return k
}

// isRoot reports whether p names the user's root directory, which cannot be
// written, created, removed or renamed: at the bucket root it has no key.
func isRoot(p string) bool { return path.Clean("/"+p) == "/" }

// ---------- uploads ----------

// Filewrite starts an upload. Its upload event is logged once S3 has
// completed or aborted the multipart upload: when the handle is closed, or
// the client goes away. An upload refused at once is logged then.
func (u *userFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	key := u.key(r.Filepath)
	start := time.Now()
	if isRoot(r.Filepath) {
		u.sess.transfer(evUpload, u.bucket, key, 0, start, sftp.ErrSSHFxPermissionDenied)
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	up := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = u.partSize
		m.Concurrency = 4
		// The uploader has its own checksum setting that defaults to "always"
		// regardless of the client's; S3-compatible stores such as GCS reject
		// the resulting per-part CRC32 checksums.
		m.RequestChecksumCalculation = u.client.Options().RequestChecksumCalculation
	})
	u.sess.log.Debug("upload start", "bucket", u.bucket, "key", key)
	w := newStreamWriter(u.maxReorder, func(body io.Reader) error {
		_, err := up.Upload(context.Background(), &s3.PutObjectInput{Bucket: &u.bucket, Key: &key, Body: body})
		return err
	})
	w.finish = func(n int64, err error) {
		u.sess.bytesIn.Add(n)
		u.sess.transfer(evUpload, u.bucket, key, n, start, err)
	}
	return w, nil
}

// ---------- downloads ----------

// Fileread opens a download. Its download event is logged when the handle
// is closed, or at once when the file cannot be opened.
func (u *userFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	key := u.key(r.Filepath)
	start := time.Now()
	if isRoot(r.Filepath) {
		u.sess.transfer(evDownload, u.bucket, key, 0, start, sftp.ErrSSHFxPermissionDenied)
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	if _, err := u.client.HeadObject(r.Context(), &s3.HeadObjectInput{Bucket: &u.bucket, Key: &key}); err != nil {
		err = mapErr(err)
		u.sess.transfer(evDownload, u.bucket, key, 0, start, err)
		return nil, err
	}
	return &s3Reader{client: u.client, bucket: u.bucket, key: key, finish: func(n int64, err error) {
		u.sess.bytesOut.Add(n)
		u.sess.transfer(evDownload, u.bucket, key, n, start, err)
	}}, nil
}

// ---------- commands ----------

// Filecmd runs a command and logs it as one audit event.
func (u *userFS) Filecmd(r *sftp.Request) error {
	err := u.filecmd(r)
	u.logCmd(r, err)
	return err
}

// logCmd logs the audit event of a command that ended with err. Commands
// the gateway does not support (links) change nothing and are not logged.
func (u *userFS) logCmd(r *sftp.Request, err error) {
	var event string
	var attrs []any
	switch r.Method {
	case "Mkdir":
		event, attrs = evMkdir, []any{"key", u.dirKey(r.Filepath)}
	case "Rmdir":
		event, attrs = evRmdir, []any{"key", u.dirKey(r.Filepath)}
	case "Remove":
		event, attrs = evRemove, []any{"key", u.key(r.Filepath)}
	case "Setstat":
		event, attrs = evSetstat, []any{"key", u.key(r.Filepath)}
	case "Rename", "PosixRename":
		event, attrs = evRename, []any{"from", u.key(r.Filepath), "to", u.key(r.Target)}
	default:
		return
	}
	attrs = append([]any{"bucket", u.bucket}, attrs...)
	audit(u.sess.metrics, u.sess.log, event, append(attrs, result(err)...)...)
}

func (u *userFS) filecmd(r *sftp.Request) error {
	ctx := r.Context()
	switch r.Method {
	case "Setstat":
		return nil // chmod/chown/utimes have no S3 equivalent; accept and ignore
	}
	if isRoot(r.Filepath) || (r.Method == "Rename" || r.Method == "PosixRename") && isRoot(r.Target) {
		return sftp.ErrSSHFxPermissionDenied
	}
	switch r.Method {
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
		// CopyObject handles objects up to 5 GB.
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
	clean := path.Clean("/" + p)
	if clean == "/" {
		return fileInfo{name: "/", dir: true}, nil
	}
	name := path.Base(clean)
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
	dirs := map[string]bool{}
	addDir := func(key string) {
		name := path.Base(strings.TrimSuffix(key, "/"))
		if !dirs[name] {
			dirs[name] = true
			items = append(items, fileInfo{name: name, dir: true})
		}
	}
	pg := s3.NewListObjectsV2Paginator(u.client, &s3.ListObjectsV2Input{
		Bucket: &u.bucket, Prefix: &dk, Delimiter: aws.String("/")})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, cp := range out.CommonPrefixes {
			addDir(*cp.Prefix)
		}
		for _, o := range out.Contents {
			switch {
			case *o.Key == dk:
				// the listed directory's own marker
			case strings.HasSuffix(*o.Key, "/"):
				// A child's "dir/" marker. S3 and MinIO report it as a common
				// prefix, but some S3-compatible stores return it as an object.
				addDir(*o.Key)
			default:
				items = append(items, fileInfo{name: path.Base(*o.Key), size: aws.ToInt64(o.Size), mtime: aws.ToTime(o.LastModified)})
			}
		}
	}
	return items, nil
}

// mapErr turns S3 "not found" errors into os.ErrNotExist, which pkg/sftp
// reports to clients as SSH_FX_NO_SUCH_FILE.
//
// Only some operations model NoSuchKey as a typed error; CopyObject (rename)
// returns it as a generic API error, so the error code is checked too.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return os.ErrNotExist
	}
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
		return os.ErrNotExist
	}
	return err
}
