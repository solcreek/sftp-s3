package s3client

import (
	"context"
	"net/http"

	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// unsignedHeaders keeps headers out of the SigV4 signature while still
// sending them.
//
// The Go SDK signs Accept-Encoding, and Google Cloud Storage's front end
// rewrites that header before verifying the signature, so every request to
// GCS's XML API fails with SignatureDoesNotMatch. Other SDKs (the AWS CLI,
// for one) do not sign it, which is why only Go clients hit this.
func unsignedHeaders(names ...string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		if err := stack.Finalize.Insert(stashHeaders{names}, "Signing", middleware.Before); err != nil {
			return err
		}
		return stack.Finalize.Insert(restoreHeaders{}, "Signing", middleware.After)
	}
}

type stashedKey struct{}

type stashHeaders struct{ names []string }

func (stashHeaders) ID() string { return "sftp-s3/StashUnsignedHeaders" }

func (m stashHeaders) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	if req, ok := in.Request.(*smithyhttp.Request); ok {
		saved := http.Header{}
		for _, n := range m.names {
			if v := req.Header.Values(n); len(v) > 0 {
				saved[http.CanonicalHeaderKey(n)] = v
				req.Header.Del(n)
			}
		}
		ctx = middleware.WithStackValue(ctx, stashedKey{}, saved)
	}
	return next.HandleFinalize(ctx, in)
}

type restoreHeaders struct{}

func (restoreHeaders) ID() string { return "sftp-s3/RestoreUnsignedHeaders" }

func (restoreHeaders) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	if req, ok := in.Request.(*smithyhttp.Request); ok {
		if saved, ok := middleware.GetStackValue(ctx, stashedKey{}).(http.Header); ok {
			for k, v := range saved {
				req.Header[k] = v
			}
		}
	}
	return next.HandleFinalize(ctx, in)
}
