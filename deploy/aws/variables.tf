variable "name" {
  description = "Name prefix for every resource, and the value of the `project` tag."
  type        = string
  default     = "sftp-s3"
}

variable "compute" {
  description = "\"ecs\" (Fargate tasks behind an NLB, highly available) or \"ec2\" (one instance, cheapest)."
  type        = string
  default     = "ecs"
  validation {
    condition     = contains(["ecs", "ec2"], var.compute)
    error_message = "compute must be \"ecs\" or \"ec2\"."
  }
}

variable "allowed_cidrs" {
  description = "Client CIDRs allowed to reach port 22. Use [\"0.0.0.0/0\"] only on purpose."
  type        = list(string)
}

# ---------- bucket ----------

variable "bucket_name" {
  description = "Bucket the gateway writes to."
  type        = string
}

variable "create_bucket" {
  description = <<-EOT
    true: create and fully manage the bucket (policy, lifecycle, encryption).
    false: use an existing bucket and leave its policy and lifecycle alone,
    because those resources replace the whole configuration; merge the
    `bucket_policy_statement` output and add an AbortIncompleteMultipartUpload
    rule by hand.
  EOT
  type        = bool
  default     = true
}

variable "s3_prefix" {
  description = "Key prefix in front of every user's directory (S3_PREFIX), e.g. \"sftp/\"."
  type        = string
  default     = ""
}

variable "abort_incomplete_multipart_days" {
  description = "Delete parts of uploads that were never completed (killed tasks leave some) after this many days."
  type        = number
  default     = 1
}

variable "force_destroy_bucket" {
  description = "Allow `tofu destroy` to delete a non-empty bucket. For test stacks only."
  type        = bool
  default     = false
}

# ---------- image and runtime ----------

variable "image" {
  description = "Container image to run. Changing it rolls the gateway (ECS) or replaces the instance (EC2)."
  type        = string
  default     = "ghcr.io/solcreek/sftp-s3:0.1"
}

variable "users" {
  description = <<-EOT
    SFTP_USERS entries, one per element: "name:<authorized_keys line>",
    "name:<bcrypt hash>" (sftp-s3 -hash-password) or "name:<password>".
    Stored in the state. null: manage /<name>/users yourself (see README).
  EOT
  type        = list(string)
  default     = null
}

variable "generate_host_key" {
  description = "Generate the SSH host key here (it is then in the state). false: write /<name>/host-key yourself."
  type        = bool
  default     = true
}

variable "max_connections" {
  description = "SFTP_MAX_CONNECTIONS. null derives it from memory: about 120 MiB per concurrent large upload."
  type        = number
  default     = null
}

variable "log_retention_days" {
  description = "CloudWatch log retention (ECS)."
  type        = number
  default     = 30
}

# ---------- ecs ----------

variable "task_cpu" {
  description = "Fargate CPU units; 256 (0.25 vCPU) uploaded at 113 MiB/s in the 2026-10-31 test."
  type        = number
  default     = 256
}

variable "task_memory" {
  description = "Fargate memory in MiB."
  type        = number
  default     = 512
}

variable "desired_count" {
  description = "Fargate tasks; they are spread over the two subnets (AZs)."
  type        = number
  default     = 2
}

variable "cross_zone_load_balancing" {
  description = <<-EOT
    Spread connections over tasks in both AZs. Off, each NLB address only
    reaches its own AZ's task, and a client that resolved (or pinned) one
    address fills one task while the other idles; measured 2026-11-01.
    Costs inter-AZ transfer (about $0.01/GB each way) for the half of the
    traffic that crosses.
  EOT
  type        = bool
  default     = true
}

# ---------- ec2 ----------

variable "instance_type" {
  description = "Graviton (arm64) instance type; the AMI is the arm64 Amazon Linux 2023."
  type        = string
  default     = "t4g.small"
}

# ---------- network ----------

variable "vpc_cidr" {
  description = "CIDR of the stack's VPC; two /24 public subnets are carved from it."
  type        = string
  default     = "10.77.0.0/16"
}
