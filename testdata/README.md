# Test data

`sftpgo-dump.json` is the output of `GET /api/v2/dumpdata?output-data=1`
from SFTPGo 2.7.6 (`drakkan/sftpgo` image, 2026-11-02), with these users
created through its REST API:

| user | password | status | storage | notes |
|---|---|---|---|---|
| alice | `alice-pass` | enabled | S3 `cust-alpha`, bucket root | |
| bob | `bob-pass` | enabled | S3 `cust-beta`, prefix `incoming/` | plus an ed25519 public key |
| carol | `carol-pass` | disabled | S3 `cust-gamma` | |
| dave | `dave-pass` | enabled | local filesystem | |
| erin | `erin-pass` | enabled | S3 `cust-delta`, prefix `erin/` | quota, IP allow list, limited permissions |

All credentials are fake: the S3 keys are placeholders, encrypted by that
container's throwaway key; the admin password is `fixture-admin`.
