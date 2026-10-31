# Deploying on AWS (Tokyo experiment, 2026-10-31)

## Current state (verified 2026-10-31)

Both shapes ran in `ap-northeast-1` against a bucket in the same region and
passed the functional check (mkdir, put, rename, ls, get, rm, md5 of the
object in S3). Bucket access was **only** possible through the S3 gateway VPC
endpoint (bucket policy denies everything else), so uploads crossed no NAT
and no paid path.

| | EC2 | ECS Fargate + NLB |
|---|---|---|
| shape | t4g.small (Graviton), AL2023, docker, container on :22 | 2 tasks × 0.25 vCPU / 0.5 GB ARM64, one per AZ, NLB TCP 22 → 2222 |
| 1 GiB upload, client inside the region | **153 MiB/s** (client on the same box) | **113 MiB/s** |
| memory while uploading 1 GiB | 94 MiB (container) | ≈ 118 MiB (≈ 23% of 512 MB) |
| admin access | SSM Session Manager; host `sshd` disabled | `ecs exec` (distroless: no shell) |
| S3 credentials | instance profile | task role |
| list price, approx. | ≈ $17 / month | ≈ $45–50 / month |

From the office/home link the gateway ran at the same speed as a direct
`aws s3 cp` to Tokyo (≈ 2.3 MB/s both): the client's uplink is the limit, not
the gateway. Prices are list prices from memory, not re-checked.

## Findings

- **NLB idle timeout is not a problem.** With the listener's
  `tcp.idle_timeout.seconds` lowered to 60, an SFTP session idle for 90 s
  still worked. Go's `net.Listen` enables TCP keepalive on accepted
  connections (15 s idle, 15 s interval, `net/dial.go`), and the NLB counts
  those probes as activity. No SSH-level keepalive is needed.
- **Rolling deploys keep in-flight uploads for ≈ 4–6 minutes.** On
  `--force-new-deployment` ECS starts the new task, deregisters the old one
  (120 s NLB deregistration delay, connections keep flowing), then sends
  SIGTERM; the server waits for open sessions until `stopTimeout` (120 s,
  the Fargate maximum) and is then killed.
  - 64, 256 and 512 MiB uploads started before the deploy all completed
    (51 s, 178 s, 274 s).
  - A 256 MiB upload throttled to 500 KB/s was cut 398 s after the deploy
    began. The client got an error (exit 255), and **no object appeared** in
    S3.
  - The kill left **one incomplete multipart upload**. The bucket needs an
    `AbortIncompleteMultipartUpload` lifecycle rule.
- **Health checks flooded the log.** The NLB's TCP health checks open and
  close a connection every few seconds per target. They are now logged at
  debug level (commit f15bebf).
- **Port 22 on EC2** is free once the host `sshd` is disabled; SSM Session
  Manager replaces it for administration.

## Recipe (what the experiment ran)

Images are built without Docker using [ko](https://ko.build):

```sh
aws ecr get-login-password | ko login <acct>.dkr.ecr.ap-northeast-1.amazonaws.com -u AWS --password-stdin
KO_DOCKER_REPO=<acct>.dkr.ecr.ap-northeast-1.amazonaws.com/sftp-s3 \
KO_DEFAULTBASEIMAGE=gcr.io/distroless/static-debian12:nonroot \
  ko build --bare --platform=linux/arm64,linux/amd64 --tags=<tag> .
```

Shared pieces:

- **Network:** a VPC with two public subnets in two AZs, an internet gateway,
  and an **S3 gateway endpoint** on the route table. No NAT gateway.
- **Bucket policy:** deny `s3:*` unless `aws:SourceVpce` is that endpoint,
  excepting the operator.
- **Secrets:** `SFTP_USERS` and `SFTP_HOST_KEY` as SSM SecureString
  parameters.
- **Security group:** port 22 from the client CIDRs only; port 2222 from the
  VPC for NLB health checks.

EC2 user data (instance role: `AmazonSSMManagedInstanceCore`,
`AmazonEC2ContainerRegistryReadOnly`, bucket access, read on the
parameters; IMDSv2 with hop limit 2 so the container reaches IMDS):

```sh
systemctl disable --now sshd.socket sshd.service
dnf install -y docker && systemctl enable --now docker
aws ecr get-login-password | docker login -u AWS --password-stdin <registry>
p() { aws ssm get-parameter --with-decryption --name "$1" --query Parameter.Value --output text; }
docker run -d --name sftp-s3 --restart=always -p 22:2222 \
  -e S3_BUCKET=<bucket> -e AWS_REGION=ap-northeast-1 \
  -e SFTP_USERS="$(p /sftp-s3/users)" -e SFTP_HOST_KEY="$(p /sftp-s3/host-key)" <image>
```

ECS:

- **Task definition:** Fargate, ARM64, 256 CPU / 512 MiB, `stopTimeout`
  120. `SFTP_USERS` and `SFTP_HOST_KEY` come from the SSM parameters through
  `secrets`; the execution role needs `ssm:GetParameters` on them.
- **Service:** `minimumHealthyPercent=100,maximumPercent=200`, circuit
  breaker with rollback, public IPs so tasks reach ECR without a NAT.
- **Network:** NLB TCP listener on 22, an IP target group on 2222 with TCP
  health checks, and a deregistration delay of 120.

## Before production

- [ ] Add a lifecycle rule `AbortIncompleteMultipartUpload` (e.g. 1 day).
- [ ] Attach an Elastic IP per AZ to the NLB if customers allowlist our address.
- [ ] Rate-limit failed logins and cap concurrent sessions in the server.
      Size Fargate memory for about 120 MiB per concurrent large upload.
- [ ] Turn the recipe into Terraform, and set up GitHub Actions with OIDC to
      deploy (ko build → new task definition → `update-service`).
