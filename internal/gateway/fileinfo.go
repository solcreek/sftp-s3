package gateway

import (
	"io"
	"os"
	"time"
)

// listerAt is a static directory listing (or a single stat result).
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

// fileInfo is an os.FileInfo for an S3 object or a key prefix ("directory").
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
