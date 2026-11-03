# Where it can run

SFTP is raw TCP, usually on port 22, so the platform must expose a TCP port,
not only HTTP.

| platform | works | notes |
|---|---|---|
| A VM (EC2, GCE, any VPS) | yes | simplest; disable the host's `sshd` or move it off port 22 |
| AWS ECS Fargate + NLB, or EC2 | yes | [deploy/aws](../deploy/aws/README.md); measured in [deploy-aws.md](deploy-aws.md) |
| Kubernetes (EKS, GKE, AKS, …) | yes | a `LoadBalancer` Service in TCP / passthrough mode: [examples/kubernetes](../examples/kubernetes/README.md) |
| Fly.io | yes | TCP services; needs a dedicated IPv4 for port 22: [fly.toml](../fly.toml), with the PROXY protocol on so logs show client IPs. Two machines keep uploads alive across deploys; a few new connections are refused meanwhile |
| Google Cloud Run | no | ingress is HTTP/1, HTTP/2, gRPC and WebSocket only |
| AWS App Runner, Render | no | HTTP only |

"SSH for Cloud Run" (IAP) is not a way around that. It opens a debug shell
into one instance for someone with Google IAM rights and gcloud, using
short-lived certificates, and it lands on the instance's in-memory
filesystem.

## Things every deployment needs

- **Pinned host keys** (`SFTP_HOST_KEY`, one per key type): clients remember
  them, and a new one on every start looks like an attack to them. When
  replacing a server, carry over every key it had
  ([Host keys](../README.md#host-keys)).
- **A graceful stop window.** On SIGTERM the gateway stops accepting
  connections and waits for open sessions. Give it as long as your platform
  allows (Fargate: `stopTimeout` up to 120 s; Kubernetes:
  `terminationGracePeriodSeconds`; Fly.io: `kill_signal = "SIGTERM"` and
  `kill_timeout`, whose defaults of SIGINT and 5 s would cut uploads).
- **Memory for `SFTP_MAX_CONNECTIONS`**: about 120 MiB per concurrent large
  upload.
- **Optionally, health and metrics** on a private port:
  `SFTP_METRICS_ADDR=:9090` serves `/healthz` (503 from SIGTERM on, while
  sessions drain) and Prometheus `/metrics`. It has no authentication, so
  never route it through the public SFTP load balancer or open it in a
  public security group. Point HTTP health checks that probe the instance
  directly at `/healthz`; a plain TCP check on the SFTP port keeps working,
  but cannot tell a draining instance from a serving one
  ([Metrics and health](../README.md#metrics-and-health)).
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
