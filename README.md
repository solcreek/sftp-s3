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
- Hardened for a public port 22: delayed answers to wrong passwords, bans on
  client IPs that keep failing, a login timeout, and a connection cap.
- An [audit log](#audit-log) of logins, uploads, downloads and file
  changes, in JSON for log collectors with `SFTP_LOG_FORMAT=json`.
- Graceful shutdown: on SIGTERM it stops accepting and lets open transfers
  finish. On SIGHUP it [reloads the users](#reloading-users) without
  dropping sessions.

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
| `SFTP_HOST_KEY` | random per start | PEM private keys (e.g. from `ssh-keygen`), at most one per key type, concatenated; see [Host keys](#host-keys). Set it in production |
| `SFTP_USERS_FILE`, `SFTP_HOST_KEY_FILE` | | read the value from a file instead (Docker / Kubernetes secrets); set one form, not both |
| `PORT` | `2222` | listen port |
| `S3_PREFIX` | `""` | key prefix in front of every user's directory, e.g. `sftp/` |
| `S3_PART_SIZE_MB` | `8` | multipart part size, 5–5120. S3 allows 10,000 parts, so 8 MiB caps a file at about 80 GB |
| `SFTP_MAX_CONNECTIONS` | `32` | concurrent connections; budget about 120 MiB of memory per concurrent large upload |
| `SFTP_PROXY_PROTOCOL` | `off` | `on`: every connection must start with a PROXY protocol header (v1 or v2) from a load balancer, and logs show the client address it carries. Make the port reachable only through that load balancer |
| `SFTP_BAN_THRESHOLD` | `10` | failed logins from one client IP within `SFTP_BAN_WINDOW` that get it banned; `0` turns bans off. See [Failed-login bans](#failed-login-bans) |
| `SFTP_BAN_WINDOW` | `10m` | how far back failures are counted (Go duration: `90s`, `10m`, `1h`). This and the two below take 1s to 720h (30 days) |
| `SFTP_BAN_DURATION` | `15m` | first ban; each repeat ban doubles it |
| `SFTP_BAN_MAX_DURATION` | `24h` | longest ban, and how long a past ban counts as a repeat offence |
| `SFTP_MAX_AUTH_TRIES` | `6` | login attempts per connection, 1–100 |
| `SFTP_LOG_FORMAT` | `text` | `text` (slog's `key=value` lines) or `json` (one JSON object per line, for log collectors). See [Audit log](#audit-log) |
| `SFTP_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Audit events are at `info` |
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

### Host keys

A client pins the server's host key on first connect, and only the key of
the type it negotiated: ed25519, ecdsa or rsa, depending on the client and
its version. To replace a server without users seeing "host key changed",
serve every key it had.

`SFTP_HOST_KEY` takes several PEM private keys concatenated, at most one per
key type. OpenSSH, PKCS#8, SEC 1 (`EC PRIVATE KEY`) and PKCS#1
(`RSA PRIVATE KEY`) blocks are accepted, without a passphrase. SFTPGo keeps
`id_ed25519`, `id_ecdsa` and `id_rsa` in its config directory
(`/var/lib/sftpgo` in its Docker image):

```sh
cat id_ed25519 id_ecdsa id_rsa > host_keys      # copied from the old server
SFTP_HOST_KEY_FILE=host_keys sftp-s3 -check     # prints each key's type and fingerprint
ssh-keyscan -p 22 old-server | ssh-keygen -lf - # what clients have pinned
```

The fingerprints must match. The server logs each key with its type at
start. Two keys of one type, a block that is not a private key, or text
outside the blocks is an error. Checked 2026-11-02 against SFTPGo 2.7.6:
`ssh-keyscan` saw the same three keys from both servers.

The three keys SFTPGo generates take 2.8 KB. With a 4096-bit RSA key from
`ssh-keygen` they take about 4.3 KB, more than an SSM Parameter Store
standard parameter holds (4 KB): use the Advanced tier.

### Reloading users

On SIGHUP the server re-reads `SFTP_USERS_FILE` (either format) and replaces
its users, using the same `S3_BUCKET` and `S3_PREFIX` as at startup:

```sh
kill -HUP "$(pidof sftp-s3)"        # or: docker kill -s HUP sftp-s3
```

- New logins see the new users at once: added users can log in, removed
  users and replaced credentials no longer work.
- Sessions already logged in keep running, in the bucket and prefix they got
  at login, even when their user was removed or moved. Restart to end them.
- The new file is validated as at startup. If it is invalid or unreadable,
  the error is logged and the current users stay. Success logs
  `SIGHUP: users reloaded users=<count>`.
- `SFTP_USERS` set in the environment cannot change while the server runs,
  so a SIGHUP then only logs that there is nothing to reload.

On Kubernetes, a Secret mounted as a volume (not with `subPath`) updates the
file some time after the Secret changes, but nothing tells the server.
Something must send it SIGHUP, such as a reloader sidecar in a pod with
`shareProcessNamespace: true` (the distroless image has no `kill` for
`kubectl exec`), or restart the pods instead. The
[Kubernetes example](examples/kubernetes/README.md) names its Secret after
a content hash, so editing `users` there rolls the pods. The
[AWS stack](deploy/aws) passes the users from SSM in `SFTP_USERS` when a
task or container starts, so SIGHUP does not apply there: redeploy.

### Failed-login bans

A client IP (an IPv4 address, or an IPv6 /64) that fails to log in
`SFTP_BAN_THRESHOLD` times within `SFTP_BAN_WINDOW` is banned: its new
connections are closed at once, before the SSH handshake, and logins already
under way on its open connections are refused, even with the right password.
The first ban lasts `SFTP_BAN_DURATION`; each further ban within
`SFTP_BAN_MAX_DURATION` of the last one lasts twice as long as the one
before, up to `SFTP_BAN_MAX_DURATION`.

What counts as one failure:

- each wrong password;
- a connection that tries to log in and closes without succeeding, if it
  sent no wrong password (a client offering several unknown keys counts
  once, not once per key).

Connections that never try to log in, such as load balancer health checks
and port scans, are not counted.

Every ban is logged at warning level (`client banned`), and its end at info
level (`client unbanned`, when the next connection or failure notices it),
with the number of connections refused meanwhile. Each refused connection is
logged at debug level only. The table of clients is capped at 10,000; when
it is full, the client whose record would expire first is forgotten early.

Limitations:

- **State is per process.** Bans live in memory, so each replica (each ECS
  task, each pod) counts and bans on its own, and a restart forgets them.
  Behind a load balancer spreading connections over two replicas, a client
  gets up to about twice the threshold before both have banned it.
- **Bans need the real client IP.** Behind a TCP proxy that hides it (one
  without the PROXY protocol, or a Kubernetes Service without
  `externalTrafficPolicy: Local`), every client seems to come from the
  proxy's address, and one client's failures ban everyone. Use
  `SFTP_PROXY_PROTOCOL=on` there, or set `SFTP_BAN_THRESHOLD=0`. An AWS NLB
  and Fly.io with `proxy_proto` pass the client IP.
- **A shared address is banned as a whole.** Everyone behind one NAT (an
  office, a campus, a mobile carrier's CGNAT) shares one IPv4 address, so
  one of them mistyping a password enough times bans all of them for the
  ban's length. Raise `SFTP_BAN_THRESHOLD` if many users come from one
  address.

### Audit log

Logins and file operations are logged as audit events, so the logs can
answer "did my upload arrive?" and "who deleted this file?". They are
ordinary log lines on stderr at info level; set `SFTP_LOG_FORMAT=json` to
have a collector (CloudWatch Logs, Cloud Logging, Loki, …) parse their
fields. The [AWS](deploy/aws), [Kubernetes](examples/kubernetes) and
[Fly.io](fly.toml) setups do.

Every audit event has an `audit` attribute naming the event, and its message
is the same name: filter on `audit` to get them all, and only them. Each
operation logs exactly one event. Every event also has:

| attribute | |
|---|---|
| `session` | random id of the connection (16 hex digits); joins all events of one connection |
| `remote` | client address, `ip:port` (the PROXY protocol's when `SFTP_PROXY_PROTOCOL=on`) |
| `user` | login name, as the client sent it; in `login_failed`, cut to 64 bytes with the original length noted |

| `audit` | when | other attributes |
|---|---|---|
| `login` | a client logged in | `method` (`password` or `publickey`), `fingerprint` (SHA256 of the key, for `publickey`), `bucket`, `root` (key prefix of the user's files) |
| `login_failed` | a wrong password; a connection that tried to log in and closed without succeeding or sending a wrong password; a login refused because the client was banned meanwhile | `method` (`password`, `publickey`, or the methods tried, comma-separated), `reason` (`wrong password`, `unknown user`, `no accepted key`, `no accepted method`, `banned`) |
| `upload` | the S3 upload completed or was aborted: on close, or when the client went away | `bucket`, `key`, `bytes`, `duration_ms`, `result`, `error` |
| `download` | the file was closed, or the client went away | `bucket`, `key`, `bytes` (sent to the client; ranges read again count again, so it can exceed the object size), `duration_ms`, `result`, `error` |
| `mkdir`, `rmdir` | | `bucket`, `key` (the `dir/` marker), `result`, `error` |
| `remove`, `setstat` | | `bucket`, `key`, `result`, `error` |
| `rename` | | `bucket`, `from`, `to` (keys), `result`, `error` |
| `session_end` | a logged-in connection closed, after its open files | `duration_ms`, `bytes_in`, `bytes_out` (upload and download payload bytes of the session) |

`result` is `ok` or `failed`; `error`, present only when failed, is the
error's text (from S3, the SFTP stream or the server, never a credential).
An upload or download refused at open (a missing file, the root directory)
logs `failed` at once with `bytes` 0. A failed upload's `bytes` counts what
was received from the client before it failed; an aborted upload leaves
nothing in S3.

A client offering several keys logs one `login_failed` for the connection,
not one per key, just as [bans](#failed-login-bans) count it. Connections
refused before the SSH handshake because their client is banned, and
connections that never try to log in (health checks), are not audit
events. Listings and stats are not logged. Passwords, password hashes,
private keys and S3 credentials are not logged, with the same caveat as
sshd: a password typed into the user name field is logged as a user name.

An upload in JSON:

```json
{"time":"2026-11-02T10:00:03.1Z","level":"INFO","msg":"upload","session":"9f86d081884c7d65","remote":"203.0.113.7:51234","user":"alice","audit":"upload","bucket":"uploads","key":"sftp/alice/report.csv","bytes":1048576,"duration_ms":412,"result":"ok"}
```

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
- Skipped, with a message: users SFTPGo would not let in over SFTP
  (disabled, expired, or denied the SSH protocol in `denied_protocols`),
  users with two-factor authentication over SSH (the gateway has no second
  factor), users on a provider other than S3 or with no S3 bucket, users
  whose `key_prefix` starts with `/` or does not end in `/`, names the
  users file does not accept, a name listed twice (the first entry is
  kept), and users left with neither a bcrypt password nor a public key.
  Passwords hashed another way (argon2id, PBKDF2, ...) are dropped with a
  warning; set a new one with `sftp-s3 -hash-password`.
- Login methods denied in SFTPGo (`denied_login_methods`) stay denied: the
  password hash is not copied when `password` or `password-over-SSH` is
  denied, nor are the public keys when `publickey` is, each with a warning.
- Imported, with a warning, but without: virtual folders, quotas, IP
  filters, permissions other than full access, group settings, and
  expiration dates still in the future.
- Dropped without a warning: SFTPGo settings that do not affect who can
  log in, such as file patterns, upload size limits, bandwidth limits,
  `max_sessions`, hooks and the description. Settings inherited from a
  group are not read at all.
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
