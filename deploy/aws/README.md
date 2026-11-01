# deploy/aws

OpenTofu for running the gateway on AWS, in either of the two shapes measured
in [docs/deploy-aws.md](../../docs/deploy-aws.md):

- `compute = "ecs"` (default): 2 Fargate tasks (ARM64) in two AZs behind an
  NLB with one Elastic IP per AZ. Highly available; rolling deploys keep
  in-flight uploads for about 4–6 minutes.
- `compute = "ec2"`: one t4g.small with an Elastic IP and the container on
  port 22. Cheapest; a deploy restarts it.

Both shapes get the same base:

- **Network:** their own VPC with an S3 gateway endpoint and no NAT gateway.
  A bucket policy makes the gateway's role use that endpoint.
- **Credentials:** IAM roles only, no access keys.
- **Secrets:** stored in SSM Parameter Store and kept out of the state.
- **Images:** an ECR repository with immutable tags.
- **CI:** optionally, a role GitHub Actions assumes through OIDC to deploy.

Verified 2026-11-01 with `compute = "ecs"` in ap-northeast-1 (OpenTofu
1.13, AWS provider 6.71):

- apply from scratch, then an upload with its md5 checked in S3;
- client IPs in the logs;
- the 1 s delay after a wrong password, the 30 s login timeout, and the
  connection cap.

## First deploy

```sh
cd deploy/aws
cp terraform.tfvars.example terraform.tfvars    # edit it
export AWS_PROFILE=...                           # an admin-capable profile

# 1. The repository has to exist before an image can be pushed.
tofu init
tofu apply -target=aws_ecr_repository.this

# 2. Build and push an image (no Docker needed).
aws ecr get-login-password | ko login "$(tofu output -raw ecr_repository | cut -d/ -f1)" -u AWS --password-stdin
KO_DOCKER_REPO=$(tofu output -raw ecr_repository) \
KO_DEFAULTBASEIMAGE=gcr.io/distroless/static-debian12:nonroot \
  ko build --bare --platform=linux/arm64 --tags="$(git rev-parse --short HEAD)" ../..
#    and put that tag in terraform.tfvars as image_tag

# 3. Everything else.
tofu apply

# 4. Secrets, written straight to SSM so they never enter the state.
ssh-keygen -t ed25519 -N '' -f host_ed25519      # keep it: clients pin it
aws ssm put-parameter --name /sftp-s3/host-key --type SecureString --overwrite --value "$(cat host_ed25519)"
aws ssm put-parameter --name /sftp-s3/users    --type SecureString --overwrite --value 'alice:long-random-password'

# 5. Start with the real secrets.
aws ecs update-service --cluster sftp-s3 --service sftp-s3 --force-new-deployment   # ecs
aws ssm send-command --instance-ids "$(tofu output -json deploy_variables | jq -r .EC2_INSTANCE_ID)" \
  --document-name AWS-RunShellScript --parameters 'commands=["systemctl restart sftp-s3"]'   # ec2

tofu output sftp_address
```

Until step 4 the gateway refuses to start: the `users` placeholder is not a
valid `SFTP_USERS`.

## Deploying new versions

`.github/workflows/deploy.yml` (manual trigger, `main` only) builds the image
with ko and rolls it out.

- **ECS:** it registers a copy of the newest task definition revision with
  the new image, updates the service, and fails unless that revision ends up
  running.
- **EC2:** it writes the image to `/sftp-s3/image` and restarts the unit
  through SSM.

Set these as repository variables (`gh variable set NAME --body VALUE`):

- every key of `tofu output deploy_variables`;
- `AWS_DEPLOY_ROLE_ARN` from `tofu output github_deploy_role_arn`.

`tofu apply` never rolls an image back. The ECS service ignores task
definition changes; for EC2 the image lives in SSM, and the stack ignores
that parameter's value. If apply changes the task definition (for example
an environment variable), run the deploy workflow to make it live.

## Using an existing bucket

Set `create_bucket = false`. The stack then leaves the bucket's policy and
lifecycle untouched, because those resources replace the whole
configuration. Instead:

- merge `tofu output bucket_policy_statement` into the bucket policy;
- add an `AbortIncompleteMultipartUpload` lifecycle rule. A task killed
  mid-upload leaves parts behind (seen in the 2026-10-31 test).

## Knobs

| variable | default | notes |
|---|---|---|
| `max_connections` | from memory | about 120 MiB per concurrent large upload: 3 per 512 MiB task, 14 on a t4g.small |
| `cross_zone_load_balancing` | true | off, connections from one client pile onto one AZ's task (measured) |
| `task_cpu` / `task_memory` | 256 / 512 | 0.25 vCPU uploaded at 113 MiB/s in-region |
| `s3_prefix` | `""` | every user lives under `<prefix><user>/` |

State is local. For a team, add an `s3` backend with `use_lockfile = true`.
