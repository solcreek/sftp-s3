# Security policy

## Reporting a vulnerability

Please do not open a public issue. Report it privately through GitHub:
**Security → Report a vulnerability** on this repository. Include what an
attacker can do, the version or commit, and steps to reproduce.

You should get a first answer within a week. Once a fix is released, the
advisory is published, with credit to you if you want it.

## Supported versions

Until 1.0, only the latest release gets security fixes.

## Scope

In scope: authentication bypass, escaping a user's prefix, reading or
writing another user's objects, denial of service that the built-in limits
should prevent, and leaking credentials or secrets.

Out of scope: weaknesses of password authentication itself (use long random
passwords and restrict source IPs), and misconfigured buckets or IAM.
