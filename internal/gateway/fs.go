package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"

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
	root       string // "<prefix><user>/"
	partSize   int64
	maxReorder int64
}

// key maps an SFTP path ("/a/b.txt") to an S3 key ("<root>a/b.txt").
// path.Clean on a rooted path drops any "..", so keys never leave root.
func (u *userFS) key(p string) string {
	return u.root + strings.TrimPrefix(path.Clean("/"+p), "/")
}

// dirKey returns the S3 prefix that represents directory p.
func (u *userFS) dirKey(p string) string {
	k := u.key(p)
	if !strings.HasSuffix(k, "/") {
		k += "/"
	}
	return k
}

// ---------- uploads ----------

func (u *userFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	key := u.key(r.Filepath)
	up := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = u.partSize
		m.Concurrency = 4
	})
	slog.Info("upload start", "key", key)
	return newStreamWriter(u.maxReorder, func(body io.Reader) error {
		_, err := up.Upload(context.Background(), &s3.PutObjectInput{Bucket: &u.bucket, Key: &key, Body: body})
		return err
	}), nil
}

// ---------- downloads ----------

func (u *userFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	key := u.key(r.Filepath)
	if _, err := u.client.HeadObject(r.Context(), &s3.HeadObjectInput{Bucket: &u.bucket, Key: &key}); err != nil {
		return nil, mapErr(err)
	}
	return &s3Reader{client: u.client, bucket: u.bucket, key: key}, nil
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
