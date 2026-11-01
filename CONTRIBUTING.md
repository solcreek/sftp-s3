# Contributing

Issues and pull requests are welcome. For anything larger than a fix,
please open an issue first so we can agree on the approach.

## Development

You need Go (the version in `go.mod`). Nothing else is needed for the
default tests.

```sh
make test     # go vet + race-enabled unit and integration tests
make cover    # the same, plus coverage.html; fails below 90% (CI runs this)
```

The integration tests start a real gateway on a loopback port, backed by an
in-memory S3 (gofakes3), and drive it with the `pkg/sftp` client.

### Against a real store (conformance)

Pointed at a real bucket, the same suite checks a store's compatibility.
Each test writes under `sftp-s3-conformance/<run>/` and cleans up after
itself.

```sh
make conformance CONFORMANCE_BUCKET=my-bucket AWS_PROFILE=dev           # AWS
AWS_ACCESS_KEY_ID=GOOG1... AWS_SECRET_ACCESS_KEY=... \
  make conformance CONFORMANCE_BUCKET=my-bucket CONFORMANCE_ENDPOINT=https://storage.googleapis.com
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... CONFORMANCE_REGION=auto \
  make conformance CONFORMANCE_BUCKET=my-bucket CONFORMANCE_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
```

`CONFORMANCE_PATH_STYLE` and `CONFORMANCE_CHECKSUM` override the endpoint
defaults, like `S3_PATH_STYLE` and `S3_CHECKSUM` do.

### End to end

`make e2e BIG=/path/to/large-file` runs the OpenSSH `sftp` client against
the built binary and MinIO, and reports throughput and memory. It needs
`sshpass` and a MinIO server; see `scripts/e2e.sh`.

## Pull requests

- Keep each commit to one topic, with a message that says why.
- Add a test that fails without your change.
- Run `gofmt` and `make cover` before pushing.
- For changes to `deploy/aws`, run `tofu fmt` and `tofu validate`, and say
  in the pull request whether you applied the change.
