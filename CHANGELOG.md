# Changelog

All notable changes are listed here. The project follows
[Semantic Versioning](https://semver.org/); before 1.0, minor versions may
break configuration.

## Unreleased

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
