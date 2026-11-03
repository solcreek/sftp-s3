#!/bin/bash
# Rendered by ec2.tf (templatefile). Installs docker and a systemd unit that
# runs ${image}, with users and the host key read from SSM at every start.
# Configuration hash (a change replaces the instance): ${config_hash}
set -euxo pipefail

# Port 22 belongs to SFTP; administration goes through SSM Session Manager.
systemctl disable --now sshd.socket sshd.service || true

dnf install -y docker
systemctl enable --now docker

cat > /usr/local/bin/sftp-s3-run <<'EOF'
#!/bin/bash
set -euo pipefail
export AWS_REGION=${region}
p() { aws ssm get-parameter --with-decryption --name "${param_prefix}/$1" --query Parameter.Value --output text; }
image='${image}'
case "$image" in # private ECR images need a login; public images do not
  *.dkr.ecr.*.amazonaws.com/*) aws ecr get-login-password | docker login -u AWS --password-stdin "$${image%%/*}" ;;
esac
docker pull "$image"
docker rm -f sftp-s3 2>/dev/null || true
exec docker run --rm --name sftp-s3 -p 22:2222 \
  -e PORT=2222 -e AWS_REGION=${region} \
  -e S3_BUCKET=${bucket} -e S3_PREFIX=${s3_prefix} -e SFTP_MAX_CONNECTIONS=${max_connections} \
  -e SFTP_LOG_FORMAT=json \
  -e SFTP_USERS="$(p users)" -e SFTP_HOST_KEY="$(p host-key)" \
  "$image"
EOF
chmod 755 /usr/local/bin/sftp-s3-run

cat > /etc/systemd/system/sftp-s3.service <<'EOF'
[Unit]
Description=sftp-s3 gateway
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/sftp-s3-run
# SIGTERM stops new logins; open sessions get up to 120 s.
ExecStop=/usr/bin/docker stop -t 120 sftp-s3
TimeoutStopSec=150
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now sftp-s3
