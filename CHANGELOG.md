# Changelog

All notable changes are listed here. The project follows
[Semantic Versioning](https://semver.org/); before 1.0, minor versions may
break configuration.

## Unreleased

- Audit log: logins, failed logins, uploads (logged when the S3 upload
  completes or is aborted), downloads, mkdir, rmdir, remove, rename,
  setstat and session ends are each logged as one info-level event with
  `audit=<event>`, a per-connection `session` id, `remote` and `user`, and
  a `result` (`ok` or `failed`, with the error). See the README for every
  event's attributes. Secrets are never logged.
- `SFTP_LOG_FORMAT=json` logs one JSON object per line (default `text`);
  `SFTP_LOG_LEVEL` sets the level (`debug`, `info`, `warn`, `error`;
  default `info`). Invalid values stop the server at startup. The AWS,
  Kubernetes and Fly.io setups log JSON.
  - **Log changes:** the `login` line has `method` (`password` or
    `publickey`) and `fingerprint` instead of `auth`; `upload done` is
    replaced by the `upload` event (`duration_ms` instead of `secs` and
    `MBps`); `upload start` is logged at debug level; `login refused:
    client was banned during the login` is now a `login_failed` event with
    `reason=banned`. `remote` is always a string, also in JSON.
- Clients that fail to log in too often are banned before the SSH
  handshake. A client is an IPv4 address or an IPv6 /64; by default 10
  failures in 10 minutes ban it for 15 minutes, doubling on repeat bans up
  to 24 hours. Health checks and connections that never try to log in are
  not counted. Settings: `SFTP_BAN_THRESHOLD` (`0` turns bans off),
  `SFTP_BAN_WINDOW`, `SFTP_BAN_DURATION`, `SFTP_BAN_MAX_DURATION`. Bans are
  per process, need real client IPs, and ban everyone behind a shared NAT
  address (see README). **On by default.**
- `SFTP_MAX_AUTH_TRIES` sets the login attempts per connection (default 6).
- SIGHUP reloads the users from `SFTP_USERS_FILE` without dropping
  sessions; an invalid file is logged and the current users stay. Open
  sessions keep the bucket and prefix they authenticated with, even when
  their user is removed: restart to end them. SIGHUP no longer stops the
  process (before, Go's default ended it), also when only `SFTP_USERS` is
  set.
- Several host keys: `SFTP_HOST_KEY` / `SFTP_HOST_KEY_FILE` may hold one
  PEM private key per type (ed25519, ecdsa, rsa) concatenated, so clients
  that pinned any of an old server's keys keep connecting. `-check` and the
  startup log show every fingerprint.
  - **Breaking:** text outside the PEM blocks (a comment line, an appended
    `.pub` line) is now an error at startup; before, it was ignored. Two
    keys of the same type are refused.
  - **Log change:** the `listening` line no longer has a `fingerprint`
    attribute; each key is logged on its own `host key` line.
  - `gateway.Config.HostKey` is now `HostKeys`.
- `sftp-s3 import-sftpgo <dump.json>` converts the users of an SFTPGo JSON
  data dump to a JSON users file: bcrypt hashes, public keys, bucket and key
  prefix, with a warning for every setting it cannot carry over. Login
  methods denied in SFTPGo stay denied; users SFTPGo would not let in over
  SFTP (disabled, expired, SSH denied, two-factor over SSH) are skipped. It
  never reads the S3 secrets in the dump.
- A JSON users format (`SFTP_USERS` / `SFTP_USERS_FILE` starting with `{`)
  that gives each user their own bucket and prefix, including a bucket
  root. `S3_BUCKET` is optional when every user names a bucket, and
  `-check` reaches every bucket.
- The user's root directory now refuses mkdir, remove, rename and writes
  with "permission denied". Before, `mkdir /` and `rmdir /` created or
  removed a marker object for the user's prefix.

- `-version` reports the module version for binaries built with
  `go install github.com/solcreek/sftp-s3@<version>` (it printed `dev`).

## 0.1.0 — 2026-11-02

First public release.

- SFTP server backed by S3: streaming multipart uploads, ranged downloads,
  directories, rename, and per-user prefixes.
- S3-compatible stores: AWS S3, Google Cloud Storage, Cloudflare R2, MinIO,
  RustFS, SeaweedFS and versitygw.
- Users: public keys (authorized_keys format), bcrypt hashes or plaintext
  passwords in `SFTP_USERS`; several keys per user.
- `SFTP_USERS_FILE` and `SFTP_HOST_KEY_FILE` for Docker and Kubernetes
  secrets.
- `-check` validates the configuration and reaches the bucket;
  `-hash-password` makes bcrypt hashes; `-version`.
- Hardening: a delay after wrong passwords, a login timeout, and a
  connection cap (`SFTP_MAX_CONNECTIONS`).
- `SFTP_PROXY_PROTOCOL=on` reads PROXY protocol v1/v2 headers, so logs
  show client addresses behind Fly.io and other TCP proxies.
- Graceful shutdown on SIGTERM.
- Deployment: an OpenTofu / Terraform module for AWS (ECS Fargate + NLB, or
  EC2), a Kubernetes kustomization, and a Docker Compose example.
- Releases: binaries for Linux, macOS and Windows, and a multi-arch image at
  `ghcr.io/solcreek/sftp-s3`, all signed with cosign.
