# Changelog

All notable changes are listed here. The project follows
[Semantic Versioning](https://semver.org/); before 1.0, minor versions may
break configuration.

## Unreleased

First public release candidate.

- SFTP server backed by S3: streaming multipart uploads, ranged downloads,
  directories, rename, and per-user prefixes.
- S3-compatible stores: AWS S3, Google Cloud Storage, Cloudflare R2, MinIO.
- Hardening: a delay after wrong passwords, a login timeout, and a
  connection cap (`SFTP_MAX_CONNECTIONS`).
- Graceful shutdown on SIGTERM.
- OpenTofu stack for AWS (ECS Fargate + NLB, or EC2).
