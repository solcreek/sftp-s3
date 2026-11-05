output "sftp_address" {
  description = "Where clients connect, on `sftp_port`."
  value       = var.compute == "ecs" ? aws_lb.this[0].dns_name : aws_eip.ec2[0].public_ip
}

output "sftp_port" {
  description = "The public SFTP port."
  value       = var.port
}

output "public_ips" {
  description = "Fixed addresses to give clients that allowlist by IP."
  value       = var.compute == "ecs" ? aws_eip.nlb[*].public_ip : aws_eip.ec2[*].public_ip
}

output "host_key_fingerprint" {
  description = "SHA256 fingerprint clients see on first connect (generated host key only)."
  value       = var.generate_host_key ? tls_private_key.host[0].public_key_fingerprint_sha256 : null
}

output "known_hosts" {
  description = "Lines for ~/.ssh/known_hosts, so clients can pin the host key (generated host key only)."
  value = var.generate_host_key ? join("\n", [
    for addr in concat([var.compute == "ecs" ? aws_lb.this[0].dns_name : aws_eip.ec2[0].public_ip], var.compute == "ecs" ? aws_eip.nlb[*].public_ip : []) :
    # known_hosts names a non-default port as [host]:port.
    "${var.port == 22 ? addr : "[${addr}]:${var.port}"} ${trimspace(tls_private_key.host[0].public_key_openssh)}"
  ]) : null
}

output "parameters" {
  description = "SSM parameters the gateway reads at start."
  value       = { users = local.users_param.name, host_key = local.host_key_param.name }
}

output "bucket_policy_statement" {
  description = "With create_bucket = false: merge this into the existing bucket's policy by hand."
  value       = var.create_bucket ? null : data.aws_iam_policy_document.bucket.json
}

output "extra_bucket_policy_statements" {
  description = "Per bucket in extra_bucket_names: merge this into that bucket's policy by hand."
  value       = { for name, doc in data.aws_iam_policy_document.extra_bucket : name => doc.json }
}

output "gateway_role_arn" {
  description = "The role the gateway runs as, for bucket policies kept elsewhere."
  value       = aws_iam_role.gateway.arn
}
