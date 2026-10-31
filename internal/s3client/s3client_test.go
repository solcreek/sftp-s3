package s3client

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// isolate keeps the SDK from reading the developer's ~/.aws or real env.
func isolate(t *testing.T, region string) {
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_REGION", region)
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "")
}

func TestDefaults(t *testing.T) {
	tests := []struct {
		name      string
		envRegion string
		opts      Options
		want      Settings
	}{
		{
			name:      "AWS keeps SDK defaults",
			envRegion: "ap-northeast-1",
			want:      Settings{Region: "ap-northeast-1", PathStyle: false, Checksum: "when_supported"},
		},
		{
			name:      "custom endpoint gets compatible defaults",
			envRegion: "",
			opts:      Options{Endpoint: "https://storage.googleapis.com"},
			want:      Settings{Endpoint: "https://storage.googleapis.com", Region: "us-east-1", PathStyle: true, Checksum: "when_required"},
		},
		{
			name:      "R2 region and explicit overrides win",
			envRegion: "us-west-2",
			opts:      Options{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", PathStyle: "false", Checksum: "when_supported"},
			want:      Settings{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", PathStyle: false, Checksum: "when_supported"},
		},
		{
			name:      "AWS with checksums relaxed",
			envRegion: "us-east-2",
			opts:      Options{Checksum: "when_required", PathStyle: "true"},
			want:      Settings{Region: "us-east-2", PathStyle: true, Checksum: "when_required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t, tt.envRegion)
			c, got, err := New(context.Background(), tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("settings = %+v, want %+v", got, tt.want)
			}
			o := c.Options()
			if o.Region != tt.want.Region || o.UsePathStyle != tt.want.PathStyle {
				t.Fatalf("client region %q path-style %v, want %q %v", o.Region, o.UsePathStyle, tt.want.Region, tt.want.PathStyle)
			}
			wantReq := aws.RequestChecksumCalculationWhenSupported
			wantResp := aws.ResponseChecksumValidationWhenSupported
			if tt.want.Checksum == "when_required" {
				wantReq, wantResp = aws.RequestChecksumCalculationWhenRequired, aws.ResponseChecksumValidationWhenRequired
			}
			if o.RequestChecksumCalculation != wantReq || o.ResponseChecksumValidation != wantResp {
				t.Fatalf("checksum modes %v/%v, want %v/%v", o.RequestChecksumCalculation, o.ResponseChecksumValidation, wantReq, wantResp)
			}
			if gotEP := aws.ToString(o.BaseEndpoint); gotEP != tt.want.Endpoint {
				t.Fatalf("endpoint %q, want %q", gotEP, tt.want.Endpoint)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	tests := map[string]struct {
		region string
		opts   Options
		want   string
	}{
		"AWS without region": {"", Options{}, "no AWS region"},
		"bad path style":     {"us-east-1", Options{PathStyle: "yes please"}, "S3_PATH_STYLE"},
		"bad checksum mode":  {"us-east-1", Options{Checksum: "never"}, "S3_CHECKSUM"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			isolate(t, tt.region)
			if _, _, err := New(context.Background(), tt.opts); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
