package gateway

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestMapErr(t *testing.T) {
	other := errors.New("throttled")
	tests := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"typed NoSuchKey", fmt.Errorf("op: %w", &types.NoSuchKey{}), os.ErrNotExist},
		{"typed NotFound", fmt.Errorf("op: %w", &types.NotFound{}), os.ErrNotExist},
		{"generic NoSuchKey (CopyObject)", fmt.Errorf("op: %w", &smithy.GenericAPIError{Code: "NoSuchKey"}), os.ErrNotExist},
		{"generic NotFound", &smithy.GenericAPIError{Code: "NotFound"}, os.ErrNotExist},
		{"other API error", &smithy.GenericAPIError{Code: "AccessDenied"}, nil},
		{"non-API error", other, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.in)
			if tt.want == nil && tt.in != nil {
				if got != tt.in {
					t.Fatalf("mapErr(%v) = %v, want it unchanged", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("mapErr(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestKeyStaysUnderRoot(t *testing.T) {
	u := &userFS{root: "p/alice/"}
	tests := map[string]string{
		"":                 "p/alice/",
		"/":                "p/alice/",
		"a.txt":            "p/alice/a.txt",
		"/a/b.txt":         "p/alice/a/b.txt",
		"/a/../b.txt":      "p/alice/b.txt",
		"../bob/x":         "p/alice/bob/x",
		"/../../../etc/pw": "p/alice/etc/pw",
		"./a//b/":          "p/alice/a/b",
	}
	for in, want := range tests {
		if got := u.key(in); got != want {
			t.Errorf("key(%q) = %q, want %q", in, got, want)
		}
	}
	if got := u.dirKey("/a"); got != "p/alice/a/" {
		t.Errorf("dirKey(/a) = %q", got)
	}
	if got := u.dirKey("/"); got != "p/alice/" {
		t.Errorf("dirKey(/) = %q", got)
	}
}
