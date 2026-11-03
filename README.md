# sftp-s3

An SFTP server whose filesystem is an S3 bucket. Users connect with any SFTP
client (OpenSSH `sftp`, WinSCP, FileZilla, …); uploads stream straight into
S3 as multipart uploads and never touch local disk.

- One static binary or a distroless container. Configuration is environment
  variables only.
- Works with AWS S3 and S3-compatible stores: Google Cloud Storage,
  Cloudflare R2, MinIO, RustFS, SeaweedFS and Versity Gateway all pass the
  integration suite.
- Each user is confined to `s3://<bucket>/<prefix><user>/`.
- Hardened for a public port 22: delayed answers to wrong passwords, a login
  timeout, and a connection cap.
- Graceful shutdown: on SIGTERM it stops accepting and lets open transfers
  finish.

## Quick start

The fastest way to try it is [Docker Compose with a local S3 store](examples/compose/README.md).
To run it against your own bucket:

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
| `S3_BUCKET` | required | bucket to serve; optional when every user in a [JSON users file](#users-in-their-own-buckets) names a bucket |
| `SFTP_USERS` | required | users and their credentials, see [Users](#users) |
| `SFTP_HOST_KEY` | random per start | PEM private key (OpenSSH format, e.g. from `ssh-keygen`); set it in production |
| `SFTP_USERS_FILE`, `SFTP_HOST_KEY_FILE` | | read the value from a file instead (Docker / Kubernetes secrets); set one form, not both |
| `PORT` | `2222` | listen port |
| `S3_PREFIX` | `""` | key prefix in front of every user's directory, e.g. `sftp/` |
| `S3_PART_SIZE_MB` | `8` | multipart part size, 5–5120. S3 allows 10,000 parts, so 8 MiB caps a file at about 80 GB |
| `SFTP_MAX_CONNECTIONS` | `32` | concurrent connections; budget about 120 MiB of memory per concurrent large upload |
| `SFTP_PROXY_PROTOCOL` | `off` | `on`: every connection must start with a PROXY protocol header (v1 or v2) from a load balancer, and logs show the client address it carries. Make the port reachable only through that load balancer |
| `S3_ENDPOINT` | AWS | URL of an S3-compatible store; see below |
| `S3_REGION` | `AWS_REGION` | region to sign for |
| `S3_PATH_STYLE` | see below | `true` or `false` |
| `S3_CHECKSUM` | see below | `when_supported` or `when_required` |

### Users

`SFTP_USERS` lists `user:credential` entries, separated by commas or, when the
value has several lines, by newlines. A credential is a public key in
`authorized_keys` format, a bcrypt hash, or a plaintext password. A user may
have several keys and at most one password; lines starting with `#` are
ignored.

```
# one entry per line
alice:ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... alice@laptop
alice:$2a$10$Kx2C3e2n0yIXJm1Ii5v9VewC0Q7m0Jx2c8p0b1mR5o2t5oQ8m3L2a
bob:ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ...
```

Make a hash with `sftp-s3 -hash-password` (it prompts, or reads a line from
stdin). `SFTP_USERS='alice:a-long-password'` is fine for a first try; the server logs
a warning for every plaintext password. User names may not contain `/`, `.`
or spaces: they become S3 key prefixes.

Each user lives at `s3://$S3_BUCKET/$S3_PREFIX<user>/` and cannot see
anything outside it.

### Users in their own buckets

When `SFTP_USERS` (usually through `SFTP_USERS_FILE`) starts with `{`, it is
a JSON file that can place each user anywhere:

```json
{"users": [
  {"username": "alice", "password_hash": "$2a$10$...", "bucket": "alice-uploads", "prefix": ""},
  {"username": "bob", "public_keys": ["ssh-ed25519 AAAA... bob@laptop"], "prefix": "partners/bob/"},
  {"username": "carol", "password_hash": "$2a$10$..."}
]}
```

- `bucket` defaults to `S3_BUCKET`.
- An absent `prefix` keeps the usual `<S3_PREFIX><user>/`. `""` is the
  bucket root. Any other value ends in `/`.
- `public_keys` lists authorized_keys lines; `password_hash` is a bcrypt
  hash. Plaintext passwords are not accepted in this format.
- Unknown fields are errors, so a typo cannot silently change where a user
  lives.

All buckets are reached with the same credentials and region. Users stay
confined to their bucket and prefix; their root directory itself cannot be
removed or renamed.

### S3 credentials

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
| RustFS 1.0.4 | `http://host:9000` | | ✅ 2026-11-01, both checksum modes |
| SeaweedFS | `http://host:8333` | `weed server -s3` | ✅ 2026-11-01, both checksum modes |
| Versity Gateway | `http://host:7070` | `versitygw posix` | ✅ 2026-11-01, both checksum modes |

Another store can be checked with `make conformance` (see
[CONTRIBUTING.md](CONTRIBUTING.md)).

### Command line

Configuration is environment-only; the flags are tools:

| flag | |
|---|---|
| `-check` | load the configuration as the server would, reach the bucket, print a summary, exit non-zero on any problem |
| `-hash-password` | read a password (prompt, or a line on stdin) and print its bcrypt hash |
| `-version` | print the version |
| `import-sftpgo <dump.json>` | convert an SFTPGo data dump to a [JSON users file](#users-in-their-own-buckets); see [Moving from SFTPGo](#moving-from-sftpgo) |

## Moving from SFTPGo

`sftp-s3 import-sftpgo` turns the users of an SFTPGo data dump into a
[JSON users file](#users-in-their-own-buckets). Users keep their passwords
(SFTPGo stores bcrypt hashes, which are copied as they are), their public
keys, and their bucket and key prefix.

```sh
# from the REST API (an admin token), or run `sftpgo dumpdata` on the host
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "https://sftpgo.example.com/api/v2/dumpdata?output-data=1" > dump.json
sftp-s3 import-sftpgo dump.json > users.json   # warnings go to stderr
SFTP_USERS_FILE=users.json S3_REGION=... sftp-s3 -check
```

- Only the JSON dump is read; SFTPGo's SQLite (or other) database is not.
- An empty `key_prefix` (the bucket root in SFTPGo) is written as
  `"prefix": ""`, so the user stays at the bucket root.
- Skipped, with a message: disabled users, users whose expiration date
  has passed, users denied the SSH protocol
  (`denied_protocols`), users with two-factor authentication over SSH
  (the gateway has no second factor), users on a provider other than S3, and users left with neither a bcrypt password nor a public key.
  Passwords hashed another way (argon2id, PBKDF2, ...) are dropped with a
  warning; set a new one with `sftp-s3 -hash-password`.
- Login methods denied in SFTPGo (`denied_login_methods`) stay denied: the
  password hash is not copied when `password` or `password-over-SSH` is
  denied, nor are the public keys when `publickey` is, each with a warning.
- Imported, with a warning, but without: virtual folders, quotas, IP
  filters, permissions other than full access, group settings, and
  expiration dates still in the future.
- All users share one region, endpoint and set of credentials (`S3_REGION`,
  `S3_ENDPOINT`, the AWS default chain). Users whose region or endpoint
  differs from the others are named in a warning. The S3 keys stored in
  the dump are never read or copied.

The dump holds password hashes: keep it and `users.json` private, and
delete the dump when done.

## Supported operations

put, get (with random-access reads), ls, stat, mkdir, rmdir (empty
directories only), rm, rename (up to 5 GB), and chmod/chown/touch (accepted
and ignored).

Not supported: random-access writes such as resuming an upload (S3 objects
are immutable), symlinks, and `statvfs`.

## Deploying

- **Docker Compose**, locally with an S3-compatible store:
  [examples/compose](examples/compose/README.md).
- **Kubernetes** (EKS, GKE, AKS, …): [examples/kubernetes](examples/kubernetes/README.md).
- **AWS:** an OpenTofu / Terraform module for ECS Fargate behind an NLB, or a
  single EC2 instance: [deploy/aws](deploy/aws/README.md), with a runnable
  [example](examples/aws).
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
