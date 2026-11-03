// Package s3client builds an S3 client that works with AWS S3 and with
// S3-compatible stores (GCS XML API, Cloudflare R2, MinIO, ...).
//
// The SDK's defaults target AWS. For any custom endpoint we switch to the
// settings compatible stores accept most widely:
//
//   - Path-style addressing (bucket in the path, not the hostname), since
//     many stores and local deployments have no wildcard DNS/TLS.
//   - Accept-Encoding left out of the request signature (GCS rewrites it).
//   - Checksums only when an operation requires them. Since early 2025 the
//     AWS SDKs add CRC32 checksums (with aws-chunked trailers on streaming
//     uploads) to every request by default; several compatible stores reject
//     or mis-handle those.
//
// Every default can be overridden.
package s3client

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Options selects the store and how to talk to it. Zero values mean "pick
// the right default for this endpoint".
type Options struct {
	Endpoint  string // empty means AWS S3
	Region    string // overrides the SDK chain (AWS_REGION, profile)
	PathStyle string // "", "true" or "false"
	Checksum  string // "", "when_supported" or "when_required"
}

// Settings are the effective choices, for logging at startup.
type Settings struct {
	Endpoint  string
	Region    string
	PathStyle bool
	Checksum  string
}

// New returns a client and the settings it resolved to.
func New(ctx context.Context, o Options) (*s3.Client, Settings, error) {
	custom := o.Endpoint != ""
	s := Settings{Endpoint: o.Endpoint, PathStyle: custom, Checksum: "when_supported"}
	if custom {
		s.Checksum = "when_required"
	}

	if o.PathStyle != "" {
		v, err := strconv.ParseBool(o.PathStyle)
		if err != nil {
			return nil, s, fmt.Errorf("S3_PATH_STYLE: %q is not true or false", o.PathStyle)
		}
		s.PathStyle = v
	}
	var reqSum aws.RequestChecksumCalculation
	var respSum aws.ResponseChecksumValidation
	if o.Checksum != "" {
		s.Checksum = o.Checksum
	}
	switch s.Checksum {
	case "when_supported":
		reqSum, respSum = aws.RequestChecksumCalculationWhenSupported, aws.ResponseChecksumValidationWhenSupported
	case "when_required":
		reqSum, respSum = aws.RequestChecksumCalculationWhenRequired, aws.ResponseChecksumValidationWhenRequired
	default:
		return nil, s, fmt.Errorf("S3_CHECKSUM: %q is not when_supported or when_required", s.Checksum)
	}

	var loadOpts []func(*config.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(o.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, s, err
	}
	s.Region = awsCfg.Region
	if s.Region == "" {
		if !custom {
			return nil, s, errors.New("no AWS region: set S3_REGION or AWS_REGION")
		}
		// Compatible stores ignore the region but SigV4 still signs one.
		s.Region = "us-east-1"
	}

	client := s3.NewFromConfig(awsCfg, func(so *s3.Options) {
		so.Region = s.Region
		so.UsePathStyle = s.PathStyle
		so.RequestChecksumCalculation = reqSum
		so.ResponseChecksumValidation = respSum
		// Ranged GETs carry no full-object checksum, and the SDK would print
		// a DEBUG line for each to stderr through its own logger, outside
		// the server's log format.
		so.DisableLogOutputChecksumValidationSkipped = true
		if custom {
			so.BaseEndpoint = aws.String(o.Endpoint)
			so.APIOptions = append(so.APIOptions, unsignedHeaders("Accept-Encoding"))
		}
	})
	return client, s, nil
}
