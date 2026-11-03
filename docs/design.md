# Design

## How an upload works

The SSH layer is `golang.org/x/crypto/ssh`; the SFTP layer is the request
server from `github.com/pkg/sftp`. Every user gets a filesystem rooted at
`<S3_PREFIX><user>/` in the bucket. An upload goes straight into an S3
multipart upload, using the `aws-sdk-go-v2` upload manager, and never
touches local disk.

- **Out-of-order writes.** `pkg/sftp` runs 8 workers, so `WriteAt` offsets
  arrive out of order. The writer parks chunks in a bounded reorder buffer
  (64 MiB) and feeds the upload in order. Writes behind the stream position
  (true random access, e.g. resuming with `reput`) are rejected: S3 objects
  are immutable.
- **The upload result is the CLOSE status.** The multipart completion error
  is returned on `SSH_FXP_CLOSE`, so a failed upload is never reported as
  success.
- **Abandoned uploads.** A disconnect aborts the multipart upload. A process
  that is killed cannot abort it, so buckets should have an
  `AbortIncompleteMultipartUpload` lifecycle rule as a backstop.
- **Memory.** A large upload holds about 120 MiB: the parts in flight (4 ×
  `S3_PART_SIZE_MB`) plus the reorder buffer. `SFTP_MAX_CONNECTIONS` should
  be sized from that.
- **Downloads** use ranged GETs in 4 MiB windows.
- **Directories** are prefixes. `mkdir` writes a zero-byte `dir/` marker so
  that empty directories show up; listing treats both common prefixes and
  markers as directories.
- **Rename** is CopyObject plus DeleteObject, so it is limited to 5 GB.
  WinSCP and FileZilla upload to `name.filepart` and then rename, which
  doubles the write for big files. Turning off temporary-file upload on the
  client avoids that.

## S3-compatible stores

Setting `S3_ENDPOINT` changes three defaults (see the README), each the
result of a real failure:

- **Path-style addressing:** many stores and local deployments have no
  wildcard DNS or TLS for bucket subdomains.
- **Checksums only when required:** since early 2025 the AWS SDKs add CRC32
  checksums, with aws-chunked trailers on streaming uploads, to every
  request. Several compatible stores reject them. The SDK's multipart
  uploader has a checksum setting of its own that ignores the client's; the
  gateway copies the client's setting into it.
- **`Accept-Encoding` left out of the signature:** the Go SDK signs it, and
  Google Cloud Storage rewrites it before verifying the signature, so every
  request failed with `SignatureDoesNotMatch`.

## Port 22 on the internet

- A wrong password is answered after 1 s.
- The SSH handshake and login must finish within 30 s.
- Connections are capped (`SFTP_MAX_CONNECTIONS`). Excess ones are closed
  at once; the warning is logged at most every 10 s.
- Connections that close before the handshake (load balancer health checks)
  are logged at debug level.
- With `SFTP_PROXY_PROTOCOL=on`, the PROXY header is read within the same
  login timeout, before any SSH byte, from a buffer that then feeds the SSH
  layer. Without the option a client cannot spoof its address: SSH skips
  text lines before its version line, so a stray v1 header is ignored, and
  a v2 header breaks the handshake.
- Accepted connections use Go's default TCP keepalive (15 s), which also
  keeps idle sessions alive through load balancers with idle timeouts
  (verified against an AWS NLB with a 60 s timeout).
- Every host key in `SFTP_HOST_KEY` is offered, and the client picks the
  algorithm. An rsa key is offered as `rsa-sha2-256`, `rsa-sha2-512` and,
  for old clients, `ssh-rsa` (SHA-1), as `golang.org/x/crypto/ssh` does by
  default.

## Why Go (decision record)

A Rust prototype (`russh` 0.65 + `russh-sftp` 3.0 + `aws-sdk-s3`) was built
alongside and removed after this comparison; it lives in git history
(commit 58d207a).

Measured 2026-10-31 on an Intel i9-10910, against MinIO on localhost, with
the OpenSSH `sftp` client. Each run uploaded a 1 GiB random file with `put`
and checked its md5 in S3, 3 runs per implementation.

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
