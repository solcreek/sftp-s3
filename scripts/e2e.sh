#!/usr/bin/env bash
# End-to-end test + rough benchmark of the server binary against a local S3 (MinIO).
# usage: scripts/e2e.sh <server-binary> <bigfile>
# needs: sshpass, OpenSSH sftp, aws CLI, MinIO on $S3_ENDPOINT with bucket $S3_BUCKET
set -euo pipefail
BIN=$1 BIG=$2
PORT=${PORT:-2222}
export S3_BUCKET=${S3_BUCKET:-sftp-s3-e2e} S3_ENDPOINT=${S3_ENDPOINT:-http://127.0.0.1:9000}
export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-minioadmin} AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-minioadmin}
export AWS_REGION=${AWS_REGION:-us-east-1} SFTP_USERS="alice:secret,bob:other" PORT
TMP=$(mktemp -d); trap 'kill $SRV 2>/dev/null; rm -rf $TMP' EXIT
s3() { aws --endpoint-url "$S3_ENDPOINT" s3 "$@"; }
s3 rm --recursive --quiet "s3://$S3_BUCKET/alice/" || true

"$BIN" 2>"$TMP/server.log" & SRV=$!
for _ in $(seq 50); do nc -z 127.0.0.1 "$PORT" 2>/dev/null && break; sleep 0.1; done
idle_rss=$(ps -o rss= -p $SRV)

SFTP=(sshpass -p secret sftp -P "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=no -o PubkeyAuthentication=no -o LogLevel=ERROR -b -)
# Peak RSS sampler
( peak=0; while kill -0 $SRV 2>/dev/null; do r=$(ps -o rss= -p $SRV || echo 0); ((r>peak)) && peak=$r && echo $peak >"$TMP/peak"; sleep 0.05; done ) &

echo "hello" >"$TMP/small.txt"
t0=$(python3 -c "import time;print(time.time())")
"${SFTP[@]}" alice@127.0.0.1 >"$TMP/out.txt" <<EOF
mkdir inbox
put $TMP/small.txt inbox/small.txt
put $BIG inbox/big.bin
rename inbox/small.txt inbox/renamed.txt
ls -l inbox
get inbox/renamed.txt $TMP/back.txt
rm inbox/renamed.txt
ls inbox
EOF
t1=$(python3 -c 'import time;print(time.time())')

cat "$TMP/out.txt"
diff -q "$TMP/small.txt" "$TMP/back.txt" && echo "PASS small file roundtrip (get)"
s3 cp --quiet "s3://$S3_BUCKET/alice/inbox/big.bin" "$TMP/big.back"
[ "$(md5 -q "$BIG")" = "$(md5 -q "$TMP/big.back")" ] && echo "PASS big file md5 matches in S3"
# isolation: bob must not see alice's files
# isolation: bob logs in fine but cannot see or reach alice's files, even via ../
BOB=("${SFTP[@]}"); BOB[2]=other
"${BOB[@]}" bob@127.0.0.1 <<<"ls /" >"$TMP/bob.txt" 2>&1 && ! grep -q inbox "$TMP/bob.txt" && echo "PASS bob login, root is empty" || { echo "FAIL bob root"; cat "$TMP/bob.txt"; }
"${BOB[@]}" bob@127.0.0.1 <<<"get ../alice/inbox/big.bin $TMP/stolen" >/dev/null 2>&1 && echo "FAIL bob read alice via ../" || echo "PASS bob cannot escape via ../"
sshpass -p wrong sftp -P "$PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=no -o PubkeyAuthentication=no -o LogLevel=ERROR -b - alice@127.0.0.1 <<<"ls" >/dev/null 2>&1 && echo "FAIL bad password accepted" || echo "PASS bad password rejected"

grep "audit=upload " "$TMP/server.log" | tail -1 || true
echo "binary_bytes=$(stat -f %z "$BIN") idle_rss_kb=$idle_rss peak_rss_kb=$(cat "$TMP/peak") session_secs=$(python3 -c "print(round($t1-$t0,1))")"
