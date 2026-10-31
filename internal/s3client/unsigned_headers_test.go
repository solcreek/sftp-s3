package s3client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// GCS rewrites Accept-Encoding before checking signatures, so for custom
// endpoints the header must still be sent but not be signed.
func TestAcceptEncodingIsSentButNotSigned(t *testing.T) {
	for _, tc := range []struct {
		name       string
		custom     bool
		wantSigned bool
	}{
		{"custom endpoint", true, false},
		{"AWS", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t, "us-east-1")
			var auth, enc string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth, enc = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Length", "3")
				w.Write([]byte("abc"))
			}))
			defer srv.Close()

			opts := Options{Endpoint: srv.URL}
			c, _, err := New(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.custom { // AWS settings, but aimed at the test server
				c, _, _ = New(context.Background(), Options{})
				o := c.Options()
				o.BaseEndpoint, o.UsePathStyle = aws.String(srv.URL), true
				c = s3.New(o)
			}
			if _, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("b"), Key: aws.String("k")}); err != nil {
				t.Fatal(err)
			}
			if enc == "" {
				t.Fatal("Accept-Encoding was not sent")
			}
			_, signed, _ := strings.Cut(auth, "SignedHeaders=")
			signed, _, _ = strings.Cut(signed, ",")
			if got := strings.Contains(signed, "accept-encoding"); got != tc.wantSigned {
				t.Fatalf("accept-encoding signed = %v, want %v (SignedHeaders=%s)", got, tc.wantSigned, signed)
			}
		})
	}
}
