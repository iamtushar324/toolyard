# toolyard on Kubernetes

Two layouts live here:

| Directory | What it is |
| --- | --- |
| `base/` | A reference Kustomize base: one replica, `strategy: Recreate`, an external Postgres. A starting point for your own overlay. |
| `overlays/ha-test/` | A test deployment for high availability: two replicas on an in-cluster Postgres 16, with a PodDisruptionBudget, an Ingress and NetworkPolicies. `scripts/k8s-test-deploy.sh` deploys and smoke tests it. Not for production data. |

CI renders both and validates them against the Kubernetes schemas with
kubeconform (`.github/workflows/deploy-manifests.yml`). We run production
on Docker Swarm.

## The single-replica base

What it sets up:

- One `Deployment` replica with `strategy: Recreate` (never two gateways on
  one database without v0.1 high availability) and
  `terminationGracePeriodSeconds: 75` for the drain.
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

### Use it

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
   the migration as a Job with the same image digest, Secret and settings;
   `overlays/ha-test/migrate/job.yaml` is a complete example. Or set
   `TOOLYARD_MIGRATE=auto` to let the pod migrate when it starts.
5. `kubectl apply -k <overlay>`. When the pod is ready, read how code-mode
   script workers are confined:

   ```bash
   kubectl port-forward deploy/toolyard 8787:8787 &
   curl -s http://127.0.0.1:8787/v1/health | jq -r .codemode_sandbox
   ```

   It should read `landlock-abi-<n>+seccomp`; then set
   `TOOLYARD_CODEMODE_SANDBOX=required`.

## The HA test overlay

`overlays/ha-test/` has four parts, which the deploy script applies in
this order:

| Part | Objects |
| --- | --- |
| `postgres/` | Postgres 16 (the official multi-arch image, pinned by digest) as a one-replica StatefulSet on a 10 GiB volume, created with `--encoding=UTF8 --locale=C`; a headless Service `toolyard-postgres`; a NetworkPolicy that lets in only the gateway and the migration Job. Runs as the postgres user under the restricted Pod Security Standard. |
| `migrate/` | The `toolyard db migrate` Job, with the gateway's image and only the `database-url` key of its Secret (no master keys), and its NetworkPolicy (Postgres and DNS only). |
| `app/` | The base with two replicas, `RollingUpdate` with `maxUnavailable: 0` and `maxSurge: 1`, `minReadySeconds: 10`, preferred spreading over nodes and zones, a PodDisruptionBudget (`minAvailable: 1`), a NetworkPolicy and test settings (`app/config.env`). The NetworkPolicy lets in the ingress controller's namespace and this namespace, and lets the gateway reach DNS, its Postgres and public addresses only (no private, carrier-grade NAT or link-local range, so neither other workloads nor the cloud metadata service). |
| `ingress/` | The Ingress. It comes last, once the first admin account exists: until then `/v1/auth/setup` is open to any caller. |

`overlays/ha-test/kustomization.yaml` renders `postgres/`, `app/` and
`ingress/` together; the Job is left out because a Job's pod template
cannot change. Every committed value is a placeholder
(`toolyard-test.example.com`, documentation address ranges). The script
writes the cluster's own values into a temporary overlay, so nothing
cluster-specific is committed.

`REPLICAS` has no default. Use `REPLICAS=1` for an image without v0.1 high
availability (every image until that work merges): the script then switches
to `strategy: Recreate` and leaves out the PodDisruptionBudget, so two
gateways never share the database. Use `REPLICAS=2` only with an image that
has it.

The first rollout of a v0.1 image over a deployment that still runs an older
image must also use `REPLICAS=1` (`Recreate`): `RollingUpdate` would start
the new pod next to the old one, and a gateway older than v0.1 must never
share the database. The same holds for a rollback to an image older than
v0.1. Once every pod runs v0.1 or later, deploy again with `REPLICAS=2`.
See "Several replicas on one database" in `docs/self-hosting.md`.

### What a cluster admin creates first

The deployer needs rights in one namespace only. A cluster admin creates:

1. The namespace, with any labels your cluster's admission rules need.
   Every pod here meets the `restricted` Pod Security Standard.
2. A ResourceQuota. The deployment requests about 0.5 CPU and 1.1 GiB of
   memory (limits about 6 GiB, during a rollout), one 10 GiB volume, and no
   LoadBalancer or NodePort Service; every container sets the requests and
   limits a quota asks for.
3. A ServiceAccount bound to the `admin` ClusterRole in that namespace only,
   and a token for it.
4. A kubeconfig file for that ServiceAccount, mode 0600.

For example (replace the names and sizes as needed):

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: toolyard-test
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: ResourceQuota
metadata: { name: toolyard-test, namespace: toolyard-test }
spec:
  hard:
    requests.cpu: "4"
    requests.memory: 8Gi
    limits.memory: 12Gi
    persistentvolumeclaims: "3"
    requests.storage: 50Gi
    services.loadbalancers: "0"
    services.nodeports: "0"
---
apiVersion: v1
kind: ServiceAccount
metadata: { name: toolyard-deployer, namespace: toolyard-test }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: toolyard-deployer-admin, namespace: toolyard-test }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: admin }
subjects:
  - { kind: ServiceAccount, name: toolyard-deployer, namespace: toolyard-test }
---
apiVersion: v1
kind: Secret
metadata:
  name: toolyard-deployer-token
  namespace: toolyard-test
  annotations: { kubernetes.io/service-account.name: toolyard-deployer }
type: kubernetes.io/service-account-token
EOF
```

Then build the kubeconfig from the admin's cluster entry, without putting
the token on a command line:

```bash
umask 077
server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
ca="$(kubectl config view --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
token="$(kubectl -n toolyard-test get secret toolyard-deployer-token -o jsonpath='{.data.token}' | base64 -d)"
cat > ~/.kube/toolyard-test.yaml <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: toolyard-test
    cluster: { server: "${server}", certificate-authority-data: "${ca}" }
users:
  - name: toolyard-deployer
    user: { token: "${token}" }
contexts:
  - name: toolyard-test
    context: { cluster: toolyard-test, user: toolyard-deployer, namespace: toolyard-test }
current-context: toolyard-test
EOF
unset token
KUBECONFIG=~/.kube/toolyard-test.yaml kubectl auth can-i create deployments -n toolyard-test  # yes
KUBECONFIG=~/.kube/toolyard-test.yaml kubectl auth can-i get secrets -n kube-system           # no
```

### Deploy

```bash
KUBECONFIG=~/.kube/toolyard-test.yaml \
NAMESPACE=toolyard-test \
HOST=toolyard-test.example.com \
IMAGE=ghcr.io/iamtushar324/toolyard@sha256:<digest> \
REPLICAS=1 \
TRUSTED_PROXY='<pod CIDR of the ingress controller>' \
INGRESS_CLASS=traefik \
scripts/k8s-test-deploy.sh
```

| Input | Meaning |
| --- | --- |
| `KUBECONFIG` | The deployer's kubeconfig (required). |
| `NAMESPACE` | The namespace (required; it must exist). |
| `HOST` | The public host name; the Ingress serves it over HTTPS and `TOOLYARD_PUBLIC_URL` becomes `https://<HOST>` (required). |
| `IMAGE` | The image, by digest (best) or tag (required). Every pull request from this repository publishes `pr-<number>-<7-character head commit>`; the publish run's summary prints its digest. A tag can move (a re-run pushes it again) while nodes keep the old image, so a `ghcr.io` tag is resolved to its digest first, and a tag on another registry is pulled with `imagePullPolicy: Always`. |
| `REPLICAS` | Gateway replicas (required): 1 for an image without v0.1 high availability, 2 for one with it. |
| `TRUSTED_PROXY` | CIDR(s) of the ingress controller pods, for `X-Forwarded-*` (required). Use the pod CIDR of the nodes the controller runs on, nothing wider. |
| `INSTANCE` | The `app.kubernetes.io/instance` label on everything the script creates; `--destroy` selects by it (default `toolyard-ha-test`). |
| `NODE_SELECTOR` | `key=value[,key=value]` for every pod. |
| `TOLERATIONS` | `key=value:Effect` or `key:Effect`, comma separated, for every pod. |
| `STORAGE_CLASS`, `PG_STORAGE_SIZE` | The Postgres volume's class (default: the cluster default) and size (default `10Gi`). Both are fixed once the volume exists. |
| `INGRESS_CLASS` | `ingressClassName` (default: the cluster's default class). |
| `INGRESS_NAMESPACE` | The ingress controller's namespace, let in by the NetworkPolicy (default `kube-system`, where K3s runs Traefik). |
| `TLS_SECRET` | A TLS Secret for the Ingress; without it the controller serves its default certificate (for example a wildcard certificate). |
| `EXTRA_EGRESS_CIDRS` | Comma-separated CIDRs the gateway may also reach, for upstream MCP servers on private addresses (for example another namespace's pod range). |
| `OWNER_EMAIL` | `TOOLYARD_OWNER_EMAIL` (links the first Clerk sign-in with that address to the admin account; it does not create one. On an install without an owner yet, only that address can become the owner: docs/self-hosting.md, The owner and admins). |
| `ADMIN_USERNAME` | Username of the first admin account (default `admin`). |
| `TOKEN_FILE` | A file with a toolyard agent token, for the authenticated smoke checks. |
| `SMOKE_INSECURE` | `1` skips certificate checks in the smoke test. |
| `TIMEOUT` | Seconds to wait for each step (default 600). |

The script is idempotent. Each run:

1. checks the deployer's rights (`kubectl auth can-i`) in the namespace;
2. creates the Secrets `toolyard-credentials` (four fresh keys and
   `database-url`) and `toolyard-postgres` (the database password) if
   neither exists, and reuses them if both do. It refuses to create them
   when the Postgres volume `data-toolyard-postgres-0` already exists, since
   new Secrets would not match its data. The values go from files in a
   private temporary directory to `kubectl create secret --from-file`; they
   never reach a command line or the output;
3. applies Postgres and waits until it is ready;
4. deletes the previous migration Job, runs `toolyard db migrate` and
   prints the end of its log (it stops here if the migration fails, before
   the gateway changes);
5. applies the gateway and waits for the rollout;
6. creates the first admin account if none exists: through a port-forward
   to one gateway pod it posts `/v1/auth/setup` with a generated password,
   which it stores first in the Secret `toolyard-admin` (keys `username`
   and `password`), and checks that the set-up route is closed;
7. applies the Ingress, so the gateway is public only once it has an
   admin;
8. runs the smoke test and prints a summary.

While it waits for Postgres, the migration and the gateway, it also reads
the namespace's events: when the cluster refuses to create a pod (an
admission policy that wants a node selector or toleration, a full
ResourceQuota) it stops at once with the refusal, and on any failure it
prints the recent events.

Sign in as the admin with the credentials from the Secret:

```bash
kubectl -n toolyard-test get secret toolyard-admin -o jsonpath='{.data.username}' | base64 -d; echo
kubectl -n toolyard-test get secret toolyard-admin -o jsonpath='{.data.password}' | base64 -d; echo
```

Back up `toolyard-credentials`, `toolyard-postgres` and `toolyard-admin`
apart from the database; without the first two its data cannot be read.

### Smoke test

`scripts/k8s-test-deploy.sh --smoke` runs it alone (it needs `KUBECONFIG`,
`NAMESPACE` and `HOST`). It checks:

- `GET /v1/health` through the Ingress (`ok`, the version, the code-mode
  sandbox) and `GET /v1/readyz` through the Ingress;
- `GET /v1/readyz` answers 200 on every gateway pod, through a
  `kubectl port-forward` to each (no exec and no extra pod, so admission
  rules for pods do not apply), the number of pods matches the Deployment's
  `spec.replicas`, and all run the same version;
- MCP `server/discover` (protocol 2026-07-28) on `/mcp`, sent with
  `Origin: https://<HOST>`. Without `TOKEN_FILE` it expects 401: a public URL
  makes `/mcp` require a token, and without the Origin header the Origin
  check would answer 403 first. With `TOKEN_FILE`: 200, `2026-07-28` among
  the supported versions, and no `Mcp-Session-Id`;
- with `TOKEN_FILE`, a code-mode run over `POST /v1/code/run`.

For the token checks, sign in, enroll an agent (**Agents → Generate code**,
then `toolyard exchange`), and pass the file that holds its token.

### Destroy

```bash
KUBECONFIG=... NAMESPACE=toolyard-test scripts/k8s-test-deploy.sh --destroy
```

This deletes the workloads, Services, Ingress, NetworkPolicies,
PodDisruptionBudget, ConfigMaps and the Job: everything labelled
`app.kubernetes.io/part-of=toolyard` and
`app.kubernetes.io/instance=<INSTANCE>`, so nothing else in a shared
namespace. It keeps the Postgres volume (`data-toolyard-postgres-0`) and
the Secrets `toolyard-credentials`, `toolyard-postgres` and
`toolyard-admin`, because the data needs them; a later deploy picks them up
again. To delete those too, for good:

```bash
CONFIRM_PURGE=toolyard-test KUBECONFIG=... NAMESPACE=toolyard-test \
  scripts/k8s-test-deploy.sh --destroy --purge
```

`--render` prints the manifests for the given inputs without a cluster
(`KUBECONFIG` is not needed); CI uses it with placeholder inputs
(`scripts/k8s/check-render.sh`).

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
- **Downtime.** With the base's `Recreate`, each rollout has a short
  outage: the drain plus startup (about 20 s). The HA overlay starts the new
  pod first and drains an old one only when the new one is ready.
- **NetworkPolicies** need a network plugin that enforces them (K3s does,
  with its embedded controller; so do Calico and Cilium). The gateway
  reaches DNS, its Postgres and public IPv4 addresses; add private upstream
  ranges with `EXTRA_EGRESS_CIDRS`. IPv4 only.
- **The test Postgres** has no backups, no replication and no TLS inside the
  cluster, and its `toolyard` role is a superuser (the image creates it).
  Production uses a managed Postgres and a dedicated role
  (`docs/self-hosting.md`).
