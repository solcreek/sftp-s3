package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const readWindow = 4 << 20

// s3Reader serves ReadAt from a 4 MiB window fetched with ranged GETs, so the
// 32 KiB reads an SFTP client issues don't each become an S3 request.
type s3Reader struct {
	client *s3.Client
	bucket string
	key    string
	mu     sync.Mutex
	winOff int64
	win    []byte
}

func (r *s3Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off < r.winOff || off+int64(len(p)) > r.winOff+int64(len(r.win)) {
		rng := fmt.Sprintf("bytes=%d-%d", off, off+readWindow-1)
		out, err := r.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: &r.bucket, Key: &r.key, Range: &rng})
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
