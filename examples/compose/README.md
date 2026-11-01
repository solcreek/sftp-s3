# Try it locally with Docker Compose

This runs sftp-s3 in front of [RustFS](https://github.com/rustfs/rustfs), an
S3-compatible store, all on your machine.

```sh
cd examples/compose

# 1. A host key for the server. The container runs as a non-root user, so
#    the file must be readable by others; fine for a local demo.
ssh-keygen -t ed25519 -N '' -f host_key && chmod 644 host_key

# 2. A user, here "demo", who logs in with your SSH key...
echo "demo:$(cat ~/.ssh/id_ed25519.pub)" > users
#    ...and/or with a password, stored as a bcrypt hash:
echo "demo:$(docker run --rm -i ghcr.io/solcreek/sftp-s3:0.1 -hash-password)" >> users
chmod 644 users

# 3. Start the store, create the bucket, start the server.
docker compose up -d

sftp -P 2222 demo@localhost
```

Uploads land in the `uploads` bucket under `demo/`. You can browse them in
the RustFS console at http://localhost:9001 (user `demo-access-key`,
password `demo-secret-key`).

`docker compose logs sftp` shows logins and uploads. `docker compose down -v`
removes everything, including the stored files.

The same `sftp` service works against AWS S3 or any other store: drop
`S3_ENDPOINT`, set `S3_BUCKET` and `AWS_REGION`, and provide credentials.
See the configuration reference in the [main README](../../README.md).
