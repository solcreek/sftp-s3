# Where it can run

SFTP is raw TCP, usually on port 22, so the platform must expose a TCP port,
not only HTTP.

| platform | works | notes |
|---|---|---|
| A VM (EC2, GCE, any VPS) | yes | simplest; disable the host's `sshd` or move it off port 22 |
| AWS ECS Fargate + NLB, or EC2 | yes | [deploy/aws](../deploy/aws/README.md); measured in [deploy-aws.md](deploy-aws.md) |
| Kubernetes (EKS, GKE, AKS, …) | yes | a `LoadBalancer` Service in TCP / passthrough mode: [examples/kubernetes](../examples/kubernetes/README.md) |
| Fly.io | should | TCP services; needs a dedicated IPv4 for port 22. [fly.toml](../fly.toml) is not verified yet |
| Google Cloud Run | no | ingress is HTTP/1, HTTP/2, gRPC and WebSocket only |
| AWS App Runner, Render | no | HTTP only |

"SSH for Cloud Run" (IAP) is not a way around that. It opens a debug shell
into one instance for someone with Google IAM rights and gcloud, using
short-lived certificates, and it lands on the instance's in-memory
filesystem.

## Things every deployment needs

- **A pinned host key** (`SFTP_HOST_KEY`): clients remember it, and a new
  one on every start looks like an attack to them.
- **A graceful stop window.** On SIGTERM the gateway stops accepting
  connections and waits for open sessions. Give it as long as your platform
  allows (Fargate: `stopTimeout` up to 120 s; Kubernetes:
  `terminationGracePeriodSeconds`).
- **Memory for `SFTP_MAX_CONNECTIONS`**: about 120 MiB per concurrent large
  upload.
- **An `AbortIncompleteMultipartUpload` lifecycle rule** on the bucket.
- **Keep the gateway in the bucket's region.** Uploads leave the provider's
  network otherwise, and egress is billed per GB. On AWS, add an S3 gateway
  VPC endpoint so that traffic to S3 does not pass a NAT gateway.

## Alternatives

- **AWS Transfer Family:** managed SFTP to S3. Custom identity providers
  need a Lambda; it is billed per endpoint-hour plus per GB.
- **[SFTPGo](https://github.com/drakkan/sftpgo):** a mature server (AGPL)
  with many backends and a web admin.
- **`rclone serve sftp`:** a quick way to put an SFTP front on any rclone
  remote.
