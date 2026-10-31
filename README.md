# sftp-s3

An SFTP server whose filesystem is an S3 bucket: users upload with any SFTP
client and the bytes stream straight into S3 (multipart), never touching local
disk. Written in Go: `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` (request
server) + `aws-sdk-go-v2` upload manager.

`scripts/e2e.sh` is a functional test + rough benchmark against any S3 endpoint.

## Where it can run

Cloud Run cannot host this: its ingress is HTTP/1, HTTP/2, gRPC and WebSocket only, with no
raw TCP on port 22. "SSH for Cloud Run" (IAP, Preview) is a root debug shell
into one instance, gated on Google IAM + gcloud, with 5-minute certificates. It is not a
customer-facing endpoint, and it lands on the instance's in-memory FS, not S3.
App Runner and Render have the same HTTP-only limit.

| option | ops | notes |
|---|---|---|
| AWS Transfer Family | none | managed SFTP→S3, custom IdP via Lambda; billed per endpoint-hour + per GB (check current pricing) |
| SFTPGo / `rclone serve sftp` | run a container | mature OSS with an S3 backend and web admin; the baseline to beat before writing your own |
| this POC on Fly.io | `fly deploy` | raw TCP services, needs a dedicated IPv4 for :22 (`fly.toml`) |
| this POC on ECS Fargate + NLB / GKE Autopilot / EKS | moderate | TCP load balancer in front of a container |

## Running

```sh
export S3_BUCKET=my-bucket AWS_REGION=... SFTP_USERS='alice:secret,bob:other'
export SFTP_HOST_KEY="$(cat host_ed25519)"   # pin it; otherwise a new key per start
# optional: S3_PREFIX=sftp/, PORT=2222, S3_PART_SIZE_MB=8 (10,000 parts: 80 GB max file)
# for a compatible store: S3_ENDPOINT, S3_REGION, S3_PATH_STYLE, S3_CHECKSUM (below)
./sftp-s3   # go build -o sftp-s3 .
```

Each user is chrooted to `s3://$S3_BUCKET/$S3_PREFIX<user>/`; `..` cannot escape.
Supported: put, get, ls, stat, mkdir (zero-byte `dir/` marker), rmdir, rm, rename
(copy+delete, ≤ 5 GB), setstat (accepted, ignored).

## S3 and S3-compatible stores

Credentials always come from the AWS SDK chain (`AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or an IAM role). Setting
`S3_ENDPOINT` switches to defaults that compatible stores accept:

| setting | AWS (no endpoint) | custom endpoint |
|---|---|---|
| `S3_REGION` | from `AWS_REGION` / profile (required) | `us-east-1` if unset |
| `S3_PATH_STYLE` | `false` | `true` |
| `S3_CHECKSUM` | `when_supported` (SDK default) | `when_required` |
| `Accept-Encoding` in signature | signed | not signed (GCS rewrites it) |

| store | `S3_ENDPOINT` | notes | conformance |
|---|---|---|---|
| AWS S3 | (unset) | | ✅ 2026-10-31, ap-northeast-1 |
| Google Cloud Storage | `https://storage.googleapis.com` | HMAC key of a service account with `roles/storage.objectAdmin` | ✅ 2026-10-31, asia-northeast1 |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `S3_REGION=auto`; R2 API token's access key / secret | ⏳ pending credentials |
| MinIO | `http://host:9000` | | ✅ 2026-10-31, both checksum modes |

"Conformance" means the whole integration suite passed against that store
(`make conformance`, see Testing). Two incompatibilities it caught, both
fixed: the SDK signs `Accept-Encoding`, which GCS rewrites, and the SDK's
multipart uploader adds per-part CRC32 checksums regardless of the client's
checksum setting.

## Testing

```sh
make test     # vet + race-enabled unit and integration tests
make cover    # same, plus coverage.html; fails below 90%
make e2e BIG=/path/to/1g.bin   # OpenSSH sftp against MinIO/S3 (see scripts/e2e.sh)
```

The integration tests need nothing installed: each one starts a real gateway
on a loopback port backed by an in-memory S3 (gofakes3) and drives it with
the `pkg/sftp` client. Pointed at a real bucket they become the conformance
suite for that store; each test writes under `sftp-s3-conformance/` and
cleans up after itself:

```sh
make conformance CONFORMANCE_BUCKET=my-bucket AWS_PROFILE=dev                  # AWS
AWS_ACCESS_KEY_ID=GOOG1... AWS_SECRET_ACCESS_KEY=... \
  make conformance CONFORMANCE_BUCKET=my-bucket CONFORMANCE_ENDPOINT=https://storage.googleapis.com
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... CONFORMANCE_REGION=auto \
  make conformance CONFORMANCE_BUCKET=my-bucket CONFORMANCE_ENDPOINT=https://<acct>.r2.cloudflarestorage.com
```
 `make e2e` covers what they cannot: the OpenSSH
client, a real S3 implementation, and throughput / memory.

## Why Go (decision record)

A Rust POC (`russh` 0.65 + `russh-sftp` 3.0 + `aws-sdk-s3`) was built alongside
and removed after this comparison; it lives in git history (commit 58d207a).

### Measured (2026-10-31, s-imac i9-10910, MinIO on localhost, OpenSSH sftp)

1 GiB random file, `put` then md5 verified in S3. 3 runs each.

| | Go | Rust |
|---|---|---|
| upload throughput | 167–203 MB/s | 193–218 MB/s |
| peak RSS during upload | 81–99 MB | 197–251 MB |
| idle RSS | 8.7 MB | 12–14 MB |
| stripped binary | 12.5 MB | 20.4 MB |
| source lines | 547 | 664 |
| dependency count | 31 modules | ~420 crates |
| clean release build | ~19 s | ~2.5 min (thin LTO) |

Throughput was bound by local MinIO, so both were fast enough. Go won on
memory (the Rust RSS gap was never explained), dependency count, and build
time.

## Design notes / traps

- **Out-of-order writes.** `pkg/sftp` runs 8 workers, so `WriteAt` offsets arrive
  out of order; the Go writer parks chunks in a bounded (64 MiB) reorder buffer.
  Writes behind the stream position (true random access, e.g. resume) are
  rejected: S3 objects are immutable.
- **Upload result = CLOSE status.** The multipart completion error is returned on
  `SSH_FXP_CLOSE`, so a failed upload is never reported as success.
- **Abandoned uploads.** Disconnects abort the multipart upload; also add an
  `AbortIncompleteMultipartUpload` lifecycle rule as a backstop.
- **WinSCP/FileZilla** upload to `name.filepart` then rename: rename is
  copy+delete, which doubles the write for big files. Consider disabling
  temp-file upload client-side, or use multipart copy for > 5 GB.
- **Host key** must be pinned across deploys (`SFTP_HOST_KEY`).
- **Not done (POC):** public-key auth, users from a DB/IdP, rate limits,
  metrics, resume (`reput`), > 5 GB rename, `statvfs`.
