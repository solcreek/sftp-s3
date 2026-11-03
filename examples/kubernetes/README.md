# Kubernetes

A kustomization for any cluster that can provision a TCP `LoadBalancer`
Service (EKS, GKE, AKS, …).

What it sets up:

- **Deployment:** 2 replicas spread over zones, running as non-root on a
  read-only root filesystem with all capabilities dropped.
- **Graceful rollouts:** a 300 s termination grace period, so uploads in
  flight can finish during a rollout.
- **Health and metrics:** `SFTP_METRICS_ADDR=:9090` serves `/healthz` and
  `/metrics` on a container port the Service does not expose. The readiness
  probe uses `/healthz`, which turns 503 on SIGTERM while uploads finish;
  liveness stays a TCP check on the SFTP port, so a draining pod is not
  killed. Kubernetes already drops terminating pods from the Service's
  endpoints; `/healthz` matters most for a load balancer that health-checks
  pods itself, such as an AWS NLB with `ip` targets (`service.yaml` has
  commented annotations pointing its health check at `/healthz`). `prometheus.io/*` pod annotations point an
  annotation-based Prometheus at `/metrics` (with the Prometheus Operator,
  write a PodMonitor for the `http` port instead). See
  [Metrics and health](../../README.md#metrics-and-health).
- **Service:** a `LoadBalancer` on port 22 with
  `externalTrafficPolicy: Local`, so the logs show client IPs.
- **PodDisruptionBudget:** keeps one pod available.
- **ServiceAccount:** ready for workload identity.

```sh
cd examples/kubernetes
ssh-keygen -t ed25519 -N '' -f host_key        # keep it: clients pin it
cp users.example users && $EDITOR users        # one user:credential per line
$EDITOR sftp-s3.env                            # bucket, region, endpoint
kubectl apply -k .
kubectl get svc sftp-s3                        # EXTERNAL-IP, port 22
```

**S3 credentials.** Prefer workload identity: IRSA or EKS Pod Identity,
annotated on the ServiceAccount in `serviceaccount.yaml`. Otherwise create a
Secret with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` and uncomment
its `secretRef` in `deployment.yaml`. For Google Cloud Storage, use an HMAC
key and set `S3_ENDPOINT=https://storage.googleapis.com`.

**Load balancer.** Annotations are cloud-specific; `service.yaml` has the
AWS NLB ones. Restrict sources with `loadBalancerSourceRanges`. If your load
balancer hides client addresses but can send a PROXY protocol header, turn
that on and add `SFTP_PROXY_PROTOCOL=on` to `sftp-s3.env`. Every connection
must then carry the header, health checks included, unless they only open
a TCP connection. This combination is not covered by the test below.

**Memory.** Each large upload holds about 120 MiB. Keep
`SFTP_MAX_CONNECTIONS` in `sftp-s3.env` in line with the memory limit
(512Mi → 3).

The secret and config map are generated with a content hash in their names,
so editing `users` and re-applying rolls the pods.

Verified 2026-11-01 on kind (Kubernetes 1.37) with an in-cluster RustFS:

- uploads through the Service, with matching md5 in the store;
- an upload running across `kubectl rollout restart` completed.

The cloud load balancer annotations are not exercised by that test, nor is
the `/healthz` readiness probe (added later; covered by the Go tests).
