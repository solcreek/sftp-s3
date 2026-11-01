# sftp-s3

An SFTP server whose filesystem is an S3 bucket. Users connect with any SFTP
client (OpenSSH `sftp`, WinSCP, FileZilla, …); uploads stream straight into
S3 as multipart uploads and never touch local disk.

- One static binary or a distroless container. Configuration is environment
  variables only.
- Works with AWS S3 and S3-compatible stores: Google Cloud Storage,
  Cloudflare R2 and MinIO all pass the integration suite.
- Each user is confined to `s3://<bucket>/<prefix><user>/`.
- Hardened for a public port 22: delayed answers to wrong passwords, a login
  timeout, and a connection cap.
- Graceful shutdown: on SIGTERM it stops accepting and lets open transfers
  finish.

## Quick start

```sh
git clone https://github.com/solcreek/sftp-s3 && cd sftp-s3
make build                                  # or: docker build -t sftp-s3 .

ssh-keygen -t ed25519 -N '' -f host_ed25519 # keep it: clients pin the host key
export S3_BUCKET=my-bucket AWS_REGION=us-east-1   # plus AWS credentials
export SFTP_USERS='alice:a-long-password'
export SFTP_HOST_KEY="$(cat host_ed25519)"
./sftp-s3                                   # listens on :2222

sftp -P 2222 alice@localhost                # put / get / ls / mkdir / rm / rename
```

## Configuration

| variable | default | |
|---|---|---|
| `S3_BUCKET` | required | bucket to serve |
| `SFTP_USERS` | required | `user:password,user2:password2`; names may not contain `/` or `.` |
| `SFTP_HOST_KEY` | random per start | PEM private key (OpenSSH format, e.g. from `ssh-keygen`); set it in production |
| `PORT` | `2222` | listen port |
| `S3_PREFIX` | `""` | key prefix in front of every user's directory, e.g. `sftp/` |
| `S3_PART_SIZE_MB` | `8` | multipart part size, 5–5120. S3 allows 10,000 parts, so 8 MiB caps a file at about 80 GB |
| `SFTP_MAX_CONNECTIONS` | `32` | concurrent connections; budget about 120 MiB of memory per concurrent large upload |
| `S3_ENDPOINT` | AWS | URL of an S3-compatible store; see below |
| `S3_REGION` | `AWS_REGION` | region to sign for |
| `S3_PATH_STYLE` | see below | `true` or `false` |
| `S3_CHECKSUM` | see below | `when_supported` or `when_required` |

Credentials come from the AWS SDK's default chain: `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, `AWS_PROFILE`, or an IAM role.

### S3-compatible stores

Setting `S3_ENDPOINT` switches to defaults that compatible stores accept:

| setting | AWS (no endpoint) | custom endpoint |
|---|---|---|
| `S3_REGION` | from `AWS_REGION` / profile (required) | `us-east-1` if unset |
| `S3_PATH_STYLE` | `false` | `true` |
| `S3_CHECKSUM` | `when_supported` (SDK default) | `when_required` |
| `Accept-Encoding` in the signature | signed | not signed (GCS rewrites it) |

| store | `S3_ENDPOINT` | notes | integration suite |
|---|---|---|---|
| AWS S3 | (unset) | | ✅ 2026-10-31 |
| Google Cloud Storage | `https://storage.googleapis.com` | HMAC key of a service account with `roles/storage.objectAdmin` | ✅ 2026-10-31 |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `S3_REGION=auto`; access key and secret of an R2 API token | ✅ 2026-10-31, both checksum modes |
| MinIO | `http://host:9000` | | ✅ 2026-10-31, both checksum modes |

Another store can be checked with `make conformance` (see
[CONTRIBUTING.md](CONTRIBUTING.md)).

## Supported operations

put, get (with random-access reads), ls, stat, mkdir, rmdir (empty
directories only), rm, rename (up to 5 GB), and chmod/chown/touch (accepted
and ignored).

Not supported: random-access writes such as resuming an upload (S3 objects
are immutable), symlinks, and `statvfs`.

## Deploying

- **AWS:** OpenTofu for ECS Fargate behind an NLB, or a single EC2 instance,
  in [deploy/aws](deploy/aws/README.md).
- **Fly.io:** [fly.toml](fly.toml).
- **Other platforms**, and the things every deployment needs:
  [docs/hosting.md](docs/hosting.md).

## Documentation

- [docs/design.md](docs/design.md): how uploads work, compatibility
  details, hardening, and why Go.
- [docs/hosting.md](docs/hosting.md): where it can run, and alternatives.
- [docs/deploy-aws.md](docs/deploy-aws.md): AWS measurements (throughput,
  rolling deploys, load balancer behaviour).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
