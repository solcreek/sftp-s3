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

- A wrong password is answered after 1 s. A connection gets at most
  `SFTP_MAX_AUTH_TRIES` (6) login attempts.
- A client IP (IPv4 address or IPv6 /64) with `SFTP_BAN_THRESHOLD` (10)
  failed logins within `SFTP_BAN_WINDOW` (10 min) is banned for
  `SFTP_BAN_DURATION` (15 min), doubling on each repeat ban up to
  `SFTP_BAN_MAX_DURATION` (24 h). The ban is checked right after the PROXY
  header, if any, and before the SSH handshake, so a banned connection is
  closed before any key exchange or bcrypt work; a ban also refuses logins
  already under way. Failures are wrong passwords, plus one for a connection
  that tried a method and ended without logging in and without a wrong
  password (so a client offering many keys counts once). Connections that
  never try (health checks, port scans) are not counted: NLB health checks
  come from fixed addresses, and banning them would take the service down.
  The table is swept of expired clients and capped at 10,000 entries. Bans
  and unbans are logged; refused connections only at debug level.
  State is per process: replicas ban independently. Without real client
  addresses (a TCP proxy without the PROXY protocol), every client shares
  the proxy's address and a ban would lock everyone out; turn bans off
  there (`SFTP_BAN_THRESHOLD=0`).
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

## Audit log

The events and their attributes are listed in the
[README](../README.md#audit-log). How they are produced:

- **Plain slog records.** An audit event is an info-level record whose
  message is the event name and which carries `audit=<event>`; there is no
  separate audit sink. `SFTP_LOG_FORMAT` picks slog's text or JSON handler
  for the whole process, so audit events and operational lines go to the
  same place and a collector filters on `audit`.
- **One logger per connection.** Once the PROXY header (if any) is read and
  the ban check passed, the connection gets a random 8-byte session id and
  a `slog.Logger` carrying `session` and `remote`, plus `user` after login.
  The authentication callbacks log through it, and the user's filesystem
  holds it with the session's byte counters, so the SFTP handlers need no
  extra arguments.
- **One `AuthLogCallback`.** ssh calls it after every authentication
  attempt. `watchAuth` copies the server's `ssh.ServerConfig` for each
  connection and sets one callback that both counts failures for bans and
  logs `login_failed`. With bans off, the ban calls are no-ops on a nil
  limiter, so behaviour does not depend on the setting. Wrong passwords are
  logged one by one; rejected keys are not (clients offer every key they
  hold), and a connection that tried and gave up without a wrong password
  logs one summary event when its handshake ends, as it counts one ban
  failure.
- **Logged at the end, not the start.** An upload's event comes from the
  stream writer's close, after the multipart upload completed or was
  aborted, so `result` is what S3 did; a download's from the reader's
  close. When a client disconnects with files open, `pkg/sftp` reports a
  transfer error and closes them, which logs them as failed. `session_end`
  waits for every channel of the connection, so it comes after the
  connection's transfer events.

## Metrics and health

The metrics are listed in the
[README](../README.md#metrics-and-health). How they are produced:

- **No new dependency.** `prometheus/client_golang` (its `prometheus` and
  `promhttp` packages) would compile 7 new modules into the binary
  (`client_golang`, `client_model`, `common`, `beorn7/perks`, `xxhash`,
  `munnerz/goautoneg`, `google.golang.org/protobuf`) and put 32 modules in
  `go.mod`'s graph (measured 2026-11-03 with `go list`), for a handful of
  counters. The gateway writes the text exposition
  format (0.0.4) itself, counters and gauges only, with no histograms, in
  about 150 lines. A test parses the output (HELP and TYPE lines, sample
  lines, label escaping) so the format cannot drift unnoticed.
- **Counted where the audit events are logged.** `audit()` is the one
  function every audit event goes through; it hands the event and its
  attributes to the counters before logging. Logins, failed logins, file
  operations and bytes therefore cannot disagree with the audit log, and a
  test checks that they agree after a scripted session. Only connection
  accounting (accepted, rejected by the limit or a ban), which has no audit
  event, is counted in `Serve` and `handleConn`, and bans in the limiter.
- **Fixed label sets.** Label values come from fixed lists (`method`,
  `reason`, `op`, `result`); a value outside them counts as `other`. Nothing
  the client chooses (user names, keys, addresses, error texts) becomes a
  label, so a client cannot grow the number of series, and no secret can
  reach a metric. Every series is written from the start, at zero.
- **Atomics, no locks on the hot path.** Counters are `atomic.Int64` in
  fixed arrays indexed by label. The banned-clients gauge and the open
  connections are read at scrape time (the limiter's lock, the length of
  the connection-slot channel), not maintained on every connection.
- **A separate, optional listener.** HTTP never shares the SFTP port, so a
  scan of port 22 sees only SSH, and an HTTP bug cannot reach the SSH
  listener. Off by default: a deployment that does not ask for it has no
  HTTP surface at all. It has read, write and idle timeouts and a header
  size cap, and serves only `GET`/`HEAD` on two paths.
- **Health is "accepting connections".** `Server.Serving` turns false when
  `Serve`'s listener closes, before it waits for open sessions, so
  `/healthz` reports 503 for the whole drain while the HTTP listener keeps
  running until the process exits. It does not probe S3: per-probe cost and
  latency, and an S3 outage would fail every replica's check at once and
  empty the load balancer, which helps no one.

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
