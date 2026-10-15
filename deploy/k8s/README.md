# toolyard on Kubernetes (reference)

`base/` is a reference Kustomize base. It is not deployed anywhere: we run
toolyard on Docker Swarm today. CI renders it and validates it against the
Kubernetes schemas with kubeconform
(`.github/workflows/deploy-manifests.yml`), so it stays a working starting
point for a cluster later.

What it sets up:

- One `Deployment` replica with `strategy: Recreate` (never two gateways on
  one database) and `terminationGracePeriodSeconds: 75` for the drain.
- The `restricted` Pod Security Standard: non-root uid/gid 10001,
  `readOnlyRootFilesystem`, no privilege escalation, all capabilities
  dropped, `RuntimeDefault` seccomp, no service account token.
- `emptyDir` volumes for the data directory (disk, 1 GiB) and `/tmp`
  (memory, 64 MiB).
- The `toolyard-credentials` Secret at `/etc/toolyard/credentials`
  (`TOOLYARD_CREDENTIALS_DIR`), and an optional `toolyard-db-ca` ConfigMap
  for the database CA bundle.
- Settings from `base/config.env` as `TOOLYARD_<FLAG>` variables.
- Startup and liveness probes on `/v1/livez`, readiness on `/v1/readyz`
  (port 8787).
- The ops listener on `127.0.0.1:9090` and the pod IP (both private, so no
  token), and a `Service` for ports 8787 and 9090.

It needs Postgres: the data directory is an `emptyDir`, so SQLite would lose
its data on every restart. There is no Ingress; add one for your controller
and set `TOOLYARD_TRUSTED_PROXY` to its pod CIDR.

## Use it

1. Create the database (C collation; see `docs/self-hosting.md`,
   "Creating the Postgres database").
2. Create the Secret. Generate fresh keys with the image and keep a backup
   of them, apart from the database:

   ```bash
   mkdir -m 0700 creds
   docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/creds:/keys" \
     ghcr.io/iamtushar324/toolyard:main keys generate -all -dir /keys
   printf '%s\n' 'postgres://toolyard:PASSWORD@db.example.com:5432/toolyard?sslmode=verify-full&sslrootcert=/etc/toolyard/db-ca/ca.pem' \
     > creds/database-url
   kubectl create secret generic toolyard-credentials --from-file=creds/
   kubectl create configmap toolyard-db-ca --from-file=ca.pem=./rds-ca-bundle.pem
   ```

3. Make an overlay that sets the namespace, pins the image by digest and
   overrides `config.env` values (public URL, trusted proxy, owner email).
4. Migrate the schema, then deploy. With `TOOLYARD_MIGRATE=off` the pod
   refuses to start (exit 78) on a schema that is behind its binary. Run
   the migration as a Job with the same image digest, Secret and settings:

   ```yaml
   apiVersion: batch/v1
   kind: Job
   metadata:
     name: toolyard-db-migrate
   spec:
     backoffLimit: 0
     template:
       spec:
         restartPolicy: Never
         automountServiceAccountToken: false
         securityContext:
           runAsNonRoot: true
           runAsUser: 10001
           runAsGroup: 10001
           fsGroup: 10001
           seccompProfile: { type: RuntimeDefault }
         containers:
           - name: migrate
             image: ghcr.io/iamtushar324/toolyard@sha256:<digest>
             args: ["db", "migrate"]
             env:
               - { name: TOOLYARD_CREDENTIALS_DIR, value: /etc/toolyard/credentials }
             securityContext:
               allowPrivilegeEscalation: false
               readOnlyRootFilesystem: true
               capabilities: { drop: ["ALL"] }
             volumeMounts:
               - { name: data, mountPath: /var/lib/toolyard/data }
               - { name: credentials, mountPath: /etc/toolyard/credentials, readOnly: true }
               - { name: db-ca, mountPath: /etc/toolyard/db-ca, readOnly: true }
         volumes:
           - { name: data, emptyDir: {} }
           - { name: credentials, secret: { secretName: toolyard-credentials, defaultMode: 0440 } }
           - { name: db-ca, configMap: { name: toolyard-db-ca, optional: true } }
   ```

   Or set `TOOLYARD_MIGRATE=auto` to let the pod migrate when it starts.

5. `kubectl apply -k <overlay>`. When the pod is ready, read how code-mode
   script workers are confined:

   ```bash
   kubectl port-forward deploy/toolyard 8787:8787 &
   curl -s http://127.0.0.1:8787/v1/health | jq -r .codemode_sandbox
   ```

   It should read `landlock-abi-<n>+seccomp`; then set
   `TOOLYARD_CODEMODE_SANDBOX=required`.

## Notes

- **Memory and the OOM killer.** On cgroup v2, Kubernetes 1.28 and later set
  `memory.oom.group=1` on the container, so a cgroup OOM kills the whole
  gateway, not only the script worker that used the memory. Keep the limit
  at 1536Mi or more, so each worker's `RLIMIT_DATA` (256 MiB, four workers)
  and `GOMEMLIMIT` act first. The kubelet setting `singleProcessOOMKill`
  (1.32 and later) restores the old behaviour. We did not check this on a
  real cluster.
- **Ops listener.** `$(POD_IP):9090` works for IPv4 pod IPs in private,
  link-local or `100.64.0.0/10` ranges. For any other address, set
  `TOOLYARD_OPS_TOKEN_FILE` to a token file from the Secret; every request,
  probes on that port included, then needs `Authorization: Bearer <token>`.
- **Downtime.** `Recreate` means each rollout has a short outage: the drain
  plus startup (about 20 s).
