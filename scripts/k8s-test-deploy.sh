#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq programs use jq variables ($ops, ...) in single quotes
#
# Deploy the toolyard HA test overlay (deploy/k8s/overlays/ha-test) to a
# Kubernetes namespace, migrate the schema, wait for the rollout, create the
# first admin account, open the Ingress and smoke test it. See
# deploy/k8s/README.md.
#
#   scripts/k8s-test-deploy.sh             deploy (idempotent) and smoke test
#   scripts/k8s-test-deploy.sh --smoke     smoke test the running deployment
#   scripts/k8s-test-deploy.sh --render    print the manifests; no cluster
#   scripts/k8s-test-deploy.sh --destroy   remove it, keeping the database
#                                          volume and the Secrets
#   scripts/k8s-test-deploy.sh --destroy --purge
#                                          also delete the volume and the
#                                          Secrets (needs CONFIRM_PURGE)
#
# Inputs come from the environment; run with --help for the list. Cluster
# values (host, node selector, storage class, ...) are inputs and never
# committed: the script writes them into a temporary overlay.
#
# Secrets never appear in argv or output: generated keys and passwords go
# to files in a private temporary directory, kubectl reads them with
# --from-file and curl with --data-binary @file. The directory is removed
# on exit.

set -euo pipefail

usage() {
	cat <<'EOF'
usage: scripts/k8s-test-deploy.sh [--render | --smoke | --destroy [--purge]]

Inputs (environment variables):
  KUBECONFIG          kubeconfig of a deployer with namespaced admin rights
                      (required, except for --render)
  NAMESPACE           target namespace; it must exist (required)
  HOST                public host name, served by the Ingress over HTTPS
                      (required for deploy, --smoke and --render)
  IMAGE               toolyard image by digest (best) or tag, for example
                      ghcr.io/iamtushar324/toolyard@sha256:<digest> or
                      ghcr.io/iamtushar324/toolyard:pr-12-abc1234. A ghcr.io
                      tag is resolved to its digest first; a tag on another
                      registry is pulled with imagePullPolicy Always.
                      (required for deploy and --render)
  REPLICAS            gateway replicas, 1 or more (required for deploy and
                      --render). 1: an image without v0.1 high availability;
                      Recreate strategy, no PodDisruptionBudget. 2 or more:
                      only an image with v0.1 high availability (two
                      gateways on one database).
  TRUSTED_PROXY       CIDR(s) of the ingress controller pods, comma
                      separated (required for deploy and --render)
  INSTANCE            value of the app.kubernetes.io/instance label on every
                      object this script creates; --destroy selects by it
                      (default toolyard-ha-test)
  NODE_SELECTOR       key=value[,key=value] for every pod (optional)
  TOLERATIONS         key=value:Effect or key:Effect, comma separated, for
                      every pod (optional)
  STORAGE_CLASS       storage class of the Postgres volume (default: the
                      cluster's default class); fixed once the volume exists
  PG_STORAGE_SIZE     size of the Postgres volume (default 10Gi); fixed once
                      the volume exists
  INGRESS_CLASS       ingressClassName (default: the cluster's default class)
  INGRESS_NAMESPACE   namespace of the ingress controller pods, allowed in by
                      the NetworkPolicy (default kube-system, as on K3s)
  TLS_SECRET          TLS Secret for the Ingress (default: the controller's
                      default certificate)
  EXTRA_EGRESS_CIDRS  comma-separated CIDRs the gateway may also reach, for
                      upstream MCP servers on private addresses (optional;
                      by default it reaches DNS, its Postgres and public
                      addresses only)
  OWNER_EMAIL         TOOLYARD_OWNER_EMAIL (optional)
  ADMIN_USERNAME      username of the first admin account (default admin);
                      its generated password goes to the toolyard-admin
                      Secret
  TOKEN_FILE          file holding a toolyard agent token; enables the
                      authenticated smoke checks (MCP server/discover with a
                      token, a code-mode run). Optional.
  SMOKE_INSECURE      1 = do not verify the Ingress certificate in the smoke
                      test (optional)
  TIMEOUT             seconds to wait for each step (default 600)
  KUSTOMIZE_BIN       a standalone kustomize to render with instead of
                      `kubectl kustomize` (optional)
  CONFIRM_PURGE       must equal NAMESPACE for --destroy --purge
EOF
}

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
K8S_DIR="$REPO_ROOT/deploy/k8s"
DEFAULT_IMAGE_NAME="ghcr.io/iamtushar324/toolyard"
COMPONENTS=(postgres migrate app ingress)

MODE=deploy
PURGE=0
while [ $# -gt 0 ]; do
	case "$1" in
	--render) MODE=render ;;
	--smoke) MODE=smoke ;;
	--destroy) MODE=destroy ;;
	--purge) PURGE=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
	shift
done
if [ "$PURGE" = 1 ] && [ "$MODE" != destroy ]; then
	die "--purge goes with --destroy"
fi

NAMESPACE="${NAMESPACE:-}"
HOST="${HOST:-}"
IMAGE="${IMAGE:-}"
REPLICAS="${REPLICAS:-}"
TRUSTED_PROXY="${TRUSTED_PROXY:-}"
INSTANCE="${INSTANCE:-toolyard-ha-test}"
NODE_SELECTOR="${NODE_SELECTOR:-}"
TOLERATIONS="${TOLERATIONS:-}"
STORAGE_CLASS="${STORAGE_CLASS:-}"
PG_STORAGE_SIZE="${PG_STORAGE_SIZE:-10Gi}"
INGRESS_CLASS="${INGRESS_CLASS:-}"
INGRESS_NAMESPACE="${INGRESS_NAMESPACE:-kube-system}"
TLS_SECRET="${TLS_SECRET:-}"
EXTRA_EGRESS_CIDRS="${EXTRA_EGRESS_CIDRS:-}"
OWNER_EMAIL="${OWNER_EMAIL:-}"
ADMIN_USERNAME="${ADMIN_USERNAME:-admin}"
TOKEN_FILE="${TOKEN_FILE:-}"
SMOKE_INSECURE="${SMOKE_INSECURE:-}"
TIMEOUT="${TIMEOUT:-600}"
KUSTOMIZE_BIN="${KUSTOMIZE_BIN:-}"
CONFIRM_PURGE="${CONFIRM_PURGE:-}"

# --- Input validation -------------------------------------------------------
# Every value ends up in generated YAML or in a URL, so each one is checked
# against a strict pattern first (and then written as a JSON string by jq).

RE_DNS_LABEL='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
RE_DNS_SUBDOMAIN='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$'
RE_HOST='^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
RE_LABEL_KEY='^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$'
RE_LABEL_VALUE='^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$'
RE_CIDR='^[0-9A-Fa-f:.]+/[0-9]{1,3}$'
RE_SIZE='^[1-9][0-9]{0,5}(Mi|Gi|Ti)$'
RE_EMAIL='^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$'
RE_USERNAME='^[A-Za-z0-9._-]{2,64}$'
RE_IMAGE_REPO='^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)+$'
RE_IMAGE_TAG='^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'
RE_DIGEST='^sha256:[0-9a-f]{64}$'

need_tool() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

require() { # name value
	[ -n "$2" ] || die "$1 is required (run with --help)"
}

IMAGE_NAME=""
IMAGE_TAG=""
IMAGE_DIGEST=""
parse_image() {
	local ref="$1" repo last
	repo="$ref"
	case "$ref" in
	*@*)
		repo="${ref%%@*}"
		IMAGE_DIGEST="${ref#*@}"
		[[ "$IMAGE_DIGEST" =~ $RE_DIGEST ]] || die "IMAGE: the digest must be sha256:<64 hex digits>"
		;;
	esac
	last="${repo##*/}"
	if [[ "$last" == *:* ]]; then
		IMAGE_TAG="${last##*:}"
		repo="${repo%:*}"
	fi
	[[ "$repo" =~ $RE_IMAGE_REPO ]] || die "IMAGE: '$repo' is not a registry/repository name"
	if [ -n "$IMAGE_TAG" ] && ! [[ "$IMAGE_TAG" =~ $RE_IMAGE_TAG ]]; then
		die "IMAGE: '$IMAGE_TAG' is not a valid tag"
	fi
	[ -n "$IMAGE_DIGEST" ] || [ -n "$IMAGE_TAG" ] || die "IMAGE needs a tag or a digest, for example $DEFAULT_IMAGE_NAME@sha256:<digest>"
	IMAGE_NAME="$repo"
}

# Comma-separated CIDRs in $2, checked, as a JSON array (name for errors in $1).
cidr_list_json() {
	local cidr json='[]' rest="$2,"
	while [ -n "$rest" ]; do
		cidr="${rest%%,*}"
		rest="${rest#*,}"
		[ -n "$cidr" ] || continue
		[[ "$cidr" =~ $RE_CIDR ]] || die "$1: '$cidr' is not a CIDR"
		json="$(jq -c --arg c "$cidr" '. + [$c]' <<<"$json")"
	done
	printf '%s\n' "$json"
}

NODE_SELECTOR_JSON='{}'
TOLERATIONS_JSON='[]'
EXTRA_EGRESS_JSON='[]'

parse_node_selector() {
	local item key value json='{}' rest="$NODE_SELECTOR,"
	while [ -n "$rest" ]; do
		item="${rest%%,*}"
		rest="${rest#*,}"
		[ -n "$item" ] || continue
		[[ "$item" == *=* ]] || die "NODE_SELECTOR: '$item' is not key=value"
		key="${item%%=*}"
		value="${item#*=}"
		[[ "$key" =~ $RE_LABEL_KEY ]] || die "NODE_SELECTOR: '$key' is not a label key"
		[[ "$value" =~ $RE_LABEL_VALUE ]] || die "NODE_SELECTOR: '$value' is not a label value"
		json="$(jq -c --arg k "$key" --arg v "$value" '. + {($k): $v}' <<<"$json")"
	done
	NODE_SELECTOR_JSON="$json"
}

parse_tolerations() {
	local item kv effect key value json='[]' rest="$TOLERATIONS,"
	while [ -n "$rest" ]; do
		item="${rest%%,*}"
		rest="${rest#*,}"
		[ -n "$item" ] || continue
		[[ "$item" == *:* ]] || die "TOLERATIONS: '$item' needs an effect (key=value:Effect or key:Effect)"
		kv="${item%:*}"
		effect="${item##*:}"
		case "$effect" in
		NoSchedule | PreferNoSchedule | NoExecute) ;;
		*) die "TOLERATIONS: the effect '$effect' must be NoSchedule, PreferNoSchedule or NoExecute" ;;
		esac
		if [[ "$kv" == *=* ]]; then
			key="${kv%%=*}"
			value="${kv#*=}"
			[[ "$value" =~ $RE_LABEL_VALUE ]] || die "TOLERATIONS: '$value' is not a valid value"
		else
			key="$kv"
			value=""
		fi
		[[ "$key" =~ $RE_LABEL_KEY ]] || die "TOLERATIONS: '$key' is not a valid key"
		if [[ "$kv" == *=* ]]; then
			json="$(jq -c --arg k "$key" --arg v "$value" --arg e "$effect" \
				'. + [{key: $k, operator: "Equal", value: $v, effect: $e}]' <<<"$json")"
		else
			json="$(jq -c --arg k "$key" --arg e "$effect" \
				'. + [{key: $k, operator: "Exists", effect: $e}]' <<<"$json")"
		fi
	done
	TOLERATIONS_JSON="$json"
}

validate_inputs() {
	need_tool jq
	require NAMESPACE "$NAMESPACE"
	[[ "$NAMESPACE" =~ $RE_DNS_LABEL ]] || die "NAMESPACE: '$NAMESPACE' is not a valid namespace name"
	if [ -z "$INSTANCE" ] || ! [[ "$INSTANCE" =~ $RE_LABEL_VALUE ]]; then
		die "INSTANCE: '$INSTANCE' is not a valid label value"
	fi
	[[ "$TIMEOUT" =~ ^[1-9][0-9]{0,4}$ ]] || die "TIMEOUT must be a number of seconds"
	if [ "$MODE" = deploy ] || [ "$MODE" = smoke ] || [ "$MODE" = render ]; then
		require HOST "$HOST"
		[[ "$HOST" =~ $RE_HOST ]] || die "HOST: '$HOST' is not a lower-case host name"
	fi
	if [ "$MODE" = deploy ] || [ "$MODE" = render ]; then
		require REPLICAS "$REPLICAS"
		[[ "$REPLICAS" =~ ^[1-9][0-9]?$ ]] || die "REPLICAS must be a number from 1 to 99"
		require IMAGE "$IMAGE"
		parse_image "$IMAGE"
		require TRUSTED_PROXY "$TRUSTED_PROXY"
		cidr_list_json TRUSTED_PROXY "$TRUSTED_PROXY" >/dev/null
		EXTRA_EGRESS_JSON="$(cidr_list_json EXTRA_EGRESS_CIDRS "$EXTRA_EGRESS_CIDRS")"
		[ -z "$NODE_SELECTOR" ] || parse_node_selector
		[ -z "$TOLERATIONS" ] || parse_tolerations
		if [ -n "$STORAGE_CLASS" ] && ! [[ "$STORAGE_CLASS" =~ $RE_DNS_SUBDOMAIN ]]; then
			die "STORAGE_CLASS: '$STORAGE_CLASS' is not a valid name"
		fi
		[[ "$PG_STORAGE_SIZE" =~ $RE_SIZE ]] || die "PG_STORAGE_SIZE: '$PG_STORAGE_SIZE' is not a size such as 10Gi"
		if [ -n "$INGRESS_CLASS" ] && ! [[ "$INGRESS_CLASS" =~ $RE_DNS_SUBDOMAIN ]]; then
			die "INGRESS_CLASS: '$INGRESS_CLASS' is not a valid name"
		fi
		[[ "$INGRESS_NAMESPACE" =~ $RE_DNS_LABEL ]] || die "INGRESS_NAMESPACE: '$INGRESS_NAMESPACE' is not a valid namespace name"
		if [ -n "$TLS_SECRET" ] && ! [[ "$TLS_SECRET" =~ $RE_DNS_SUBDOMAIN ]]; then
			die "TLS_SECRET: '$TLS_SECRET' is not a valid Secret name"
		fi
		if [ -n "$OWNER_EMAIL" ] && ! [[ "$OWNER_EMAIL" =~ $RE_EMAIL ]]; then
			die "OWNER_EMAIL: '$OWNER_EMAIL' is not an email address"
		fi
		[[ "$ADMIN_USERNAME" =~ $RE_USERNAME ]] || die "ADMIN_USERNAME: '$ADMIN_USERNAME' must be 2-64 characters of A-Z a-z 0-9 . _ -"
		if [ -n "$KUSTOMIZE_BIN" ]; then
			need_tool "$KUSTOMIZE_BIN"
		else
			need_tool kubectl
		fi
	fi
	if [ "$MODE" != render ]; then
		need_tool kubectl
		[ -n "${KUBECONFIG:-}" ] || die "KUBECONFIG is required: point it at the deployer's kubeconfig"
		[ -r "${KUBECONFIG%%:*}" ] || die "KUBECONFIG: ${KUBECONFIG%%:*} is not readable"
	fi
	if [ "$MODE" = deploy ] || [ "$MODE" = smoke ]; then
		need_tool curl
		if [ -n "$TOKEN_FILE" ] && ! [ -r "$TOKEN_FILE" ]; then
			die "TOKEN_FILE: $TOKEN_FILE is not readable"
		fi
	fi
	if [ "$MODE" = deploy ]; then
		need_tool openssl
		need_tool base64
	fi
}

# --- Temporary directory and cleanup ----------------------------------------

umask 077
WORK=""
PF_PIDS=()
cleanup() {
	local pid
	for pid in ${PF_PIDS[@]+"${PF_PIDS[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	if [ -n "$WORK" ] && [ -d "$WORK" ]; then
		rm -rf "$WORK"
	fi
}
trap cleanup EXIT
make_workdir() {
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/toolyard-k8s.XXXXXX")"
	chmod 0700 "$WORK"
}

k() { kubectl --namespace "$NAMESPACE" "$@"; }

# --- Image -------------------------------------------------------------------
# A tag can move (a re-run of the publish workflow pushes the same tag
# again) while nodes keep the old image (imagePullPolicy IfNotPresent). So a
# ghcr.io tag is resolved to its digest here, and any other tag is pulled
# with imagePullPolicy Always.

IMAGE_PULL_ALWAYS=0
resolve_image() {
	local registry path tok digest
	[ -z "$IMAGE_DIGEST" ] || return 0
	registry="${IMAGE_NAME%%/*}"
	path="${IMAGE_NAME#*/}"
	if [ "$registry" != ghcr.io ]; then
		warn "IMAGE is a tag on $registry, which this script does not resolve: the pods pull it with imagePullPolicy Always"
		IMAGE_PULL_ALWAYS=1
		return 0
	fi
	need_tool curl
	# An anonymous pull token (the package is public); it goes to curl in a
	# header file, never on its command line.
	tok="$(curl -fsS --max-time 15 "https://ghcr.io/token?scope=repository:${path}:pull" | jq -r '.token // empty')" || tok=""
	[ -n "$tok" ] || die "cannot get a pull token from ghcr.io for $path: pass IMAGE by digest"
	printf 'Authorization: Bearer %s\n' "$tok" >"$WORK/ghcr-auth"
	tok=""
	digest="$(curl -fsSI --max-time 15 -H "@$WORK/ghcr-auth" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"https://ghcr.io/v2/${path}/manifests/${IMAGE_TAG}" |
		tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }' | tail -n 1)" || digest=""
	rm -f "$WORK/ghcr-auth"
	[[ "$digest" =~ $RE_DIGEST ]] || die "cannot resolve $IMAGE_NAME:$IMAGE_TAG to a digest on ghcr.io: pass IMAGE by digest"
	log "IMAGE $IMAGE_NAME:$IMAGE_TAG is $digest: deploying that digest"
	IMAGE_DIGEST="$digest"
}

# --- Overlays ----------------------------------------------------------------
# A copy of deploy/k8s in the temporary directory, plus one overlay per
# component under overlays/run/<component>. Each has two layers: patched/
# (patches, image, settings) and the outer one, which only sets the
# namespace and the instance label, so those apply after every generator
# merge and patch has found its target.

RUN_DIR=""

kustomization_json() { # resources-json extra-json
	jq -n --argjson res "$1" --argjson extra "$2" \
		'{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization", resources: $res} + $extra'
}

# JSON6902 operations that add the node selector and tolerations under $1,
# and imagePullPolicy Always when $2 is "pull".
pod_ops() {
	local pull=""
	if [ "${2:-}" = pull ] && [ "$IMAGE_PULL_ALWAYS" = 1 ]; then
		pull=yes
	fi
	jq -cn --argjson ns "$NODE_SELECTOR_JSON" --argjson tol "$TOLERATIONS_JSON" --arg p "$1" --arg pull "$pull" '
		[ (if ($ns | length) > 0 then {op: "add", path: ($p + "/nodeSelector"), value: $ns} else empty end),
		  (if ($tol | length) > 0 then {op: "add", path: ($p + "/tolerations"), value: $tol} else empty end),
		  (if $pull == "yes" then {op: "replace", path: ($p + "/containers/0/imagePullPolicy"), value: "Always"} else empty end) ]'
}

# A patches entry for kind $1, name $2 and the operations in $3, or nothing
# when there are no operations.
patch_entry() {
	jq -cn --arg kind "$1" --arg name "$2" --argjson ops "$3" '
		if ($ops | length) > 0 then [{target: {kind: $kind, name: $name}, patch: ($ops | tojson)}] else [] end'
}

write_overlays() {
	local images pg_ops job_ops deploy_ops ingress_ops netpol_ops patches literals pdb_delete c
	cp -R "$K8S_DIR" "$WORK/k8s"
	RUN_DIR="$WORK/k8s/overlays/run"
	for c in "${COMPONENTS[@]}"; do
		mkdir -p "$RUN_DIR/$c/patched"
		jq -n --arg ns "$NAMESPACE" --arg inst "$INSTANCE" '
			{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
			 namespace: $ns, resources: ["patched"],
			 labels: [{pairs: {"app.kubernetes.io/instance": $inst}, includeSelectors: false, includeTemplates: true}]}' \
			>"$RUN_DIR/$c/kustomization.yaml"
	done

	images="$(jq -cn --arg from "$DEFAULT_IMAGE_NAME" --arg n "$IMAGE_NAME" --arg t "$IMAGE_TAG" --arg d "$IMAGE_DIGEST" '
		[{name: $from, newName: $n} + (if $d != "" then {digest: $d} else {newTag: $t} end)]')"

	# Postgres: scheduling, storage class and size, the instance label on
	# the volume.
	pg_ops="$(pod_ops /spec/template/spec)"
	pg_ops="$(jq -cn --argjson ops "$pg_ops" --arg sc "$STORAGE_CLASS" --arg size "$PG_STORAGE_SIZE" --arg inst "$INSTANCE" '
		$ops
		+ [{op: "replace", path: "/spec/volumeClaimTemplates/0/spec/resources/requests/storage", value: $size},
		   {op: "add", path: "/spec/volumeClaimTemplates/0/metadata/labels/app.kubernetes.io~1instance", value: $inst}]
		+ (if $sc != "" then [{op: "add", path: "/spec/volumeClaimTemplates/0/spec/storageClassName", value: $sc}] else [] end)')"
	patches="$(patch_entry StatefulSet toolyard-postgres "$pg_ops")"
	kustomization_json '["../../../ha-test/postgres"]' "$(jq -cn --argjson p "$patches" '{patches: $p}')" \
		>"$RUN_DIR/postgres/patched/kustomization.yaml"

	# Migration Job: image and scheduling.
	job_ops="$(pod_ops /spec/template/spec pull)"
	patches="$(patch_entry Job toolyard-db-migrate "$job_ops")"
	kustomization_json '["../../../ha-test/migrate"]' \
		"$(jq -cn --argjson p "$patches" --argjson i "$images" '{patches: $p, images: $i}')" \
		>"$RUN_DIR/migrate/patched/kustomization.yaml"

	# Gateway: replicas (and Recreate for one), scheduling, NetworkPolicy
	# and settings.
	deploy_ops="$(pod_ops /spec/template/spec pull)"
	deploy_ops="$(jq -cn --argjson ops "$deploy_ops" --argjson r "$REPLICAS" '
		[{op: "replace", path: "/spec/replicas", value: $r}]
		+ (if $r == 1 then [{op: "replace", path: "/spec/strategy", value: {type: "Recreate"}}] else [] end)
		+ $ops')"
	netpol_ops="$(jq -cn --arg ns "$INGRESS_NAMESPACE" --argjson extra "$EXTRA_EGRESS_JSON" '
		[{op: "replace", path: "/spec/ingress/0/from/0/namespaceSelector/matchLabels/kubernetes.io~1metadata.name", value: $ns}]
		+ (if ($extra | length) > 0
		   then [{op: "add", path: "/spec/egress/-", value: {to: [$extra[] | {ipBlock: {cidr: .}}]}}]
		   else [] end)')"
	patches="$(jq -cn \
		--argjson d "$(patch_entry Deployment toolyard "$deploy_ops")" \
		--argjson n "$(patch_entry NetworkPolicy toolyard-gateway "$netpol_ops")" \
		'$d + $n')"
	if [ "$REPLICAS" = 1 ]; then
		# One gateway: a PodDisruptionBudget of minAvailable 1 would block
		# every node drain.
		pdb_delete='{"apiVersion":"policy/v1","kind":"PodDisruptionBudget","metadata":{"name":"toolyard"},"$patch":"delete"}'
		patches="$(jq -cn --argjson p "$patches" --arg del "$pdb_delete" '$p + [{patch: $del}]')"
	fi
	literals="$(jq -cn --arg host "$HOST" --arg tp "$TRUSTED_PROXY" --arg owner "$OWNER_EMAIL" '
		["TOOLYARD_PUBLIC_URL=https://" + $host, "TOOLYARD_TRUSTED_PROXY=" + $tp]
		+ (if $owner != "" then ["TOOLYARD_OWNER_EMAIL=" + $owner] else [] end)')"
	kustomization_json '["../../../ha-test/app"]' "$(jq -cn --argjson p "$patches" --argjson i "$images" --argjson l "$literals" '
		{patches: $p, images: $i,
		 configMapGenerator: [{name: "toolyard-config", behavior: "merge", literals: $l}]}')" \
		>"$RUN_DIR/app/patched/kustomization.yaml"

	# Ingress: host, class and TLS Secret.
	ingress_ops="$(jq -cn --arg host "$HOST" --arg class "$INGRESS_CLASS" --arg tls "$TLS_SECRET" '
		[{op: "replace", path: "/spec/rules/0/host", value: $host},
		 {op: "replace", path: "/spec/tls/0/hosts/0", value: $host}]
		+ (if $class != "" then [{op: "add", path: "/spec/ingressClassName", value: $class}] else [] end)
		+ (if $tls != "" then [{op: "add", path: "/spec/tls/0/secretName", value: $tls}] else [] end)')"
	patches="$(patch_entry Ingress toolyard "$ingress_ops")"
	kustomization_json '["../../../ha-test/ingress"]' "$(jq -cn --argjson p "$patches" '{patches: $p}')" \
		>"$RUN_DIR/ingress/patched/kustomization.yaml"
}

kbuild() {
	if [ -n "$KUSTOMIZE_BIN" ]; then
		"$KUSTOMIZE_BIN" build "$1"
	else
		kubectl kustomize "$1"
	fi
}

render_all() {
	local c
	resolve_image
	write_overlays
	for c in "${COMPONENTS[@]}"; do
		kbuild "$RUN_DIR/$c" >"$WORK/$c.yaml" || die "rendering $c failed"
	done
}

# --- Cluster access ------------------------------------------------------------

check_access() { # verb:resource ...
	local item verb res sub missing=()
	k get serviceaccount default -o name >/dev/null 2>&1 ||
		die "cannot read namespace $NAMESPACE with this KUBECONFIG: it does not exist, or the deployer has no rights in it"
	for item in "$@"; do
		verb="${item%%:*}"
		res="${item#*:}"
		sub=""
		if [[ "$res" == */* ]]; then
			sub="--subresource=${res#*/}"
			res="${res%%/*}"
		fi
		if ! k auth can-i --quiet "$verb" "$res" ${sub:+"$sub"} 2>/dev/null; then
			missing+=("$verb $res${sub:+ (${sub#--subresource=})}")
		fi
	done
	if [ "${#missing[@]}" -gt 0 ]; then
		printf 'error: the deployer may not do this in %s:\n' "$NAMESPACE" >&2
		printf '  - %s\n' "${missing[@]}" >&2
		die "grant it the admin ClusterRole in $NAMESPACE (see deploy/k8s/README.md)"
	fi
}

WORKLOAD_RESOURCES=(
	deployments.apps statefulsets.apps jobs.batch services configmaps
	ingresses.networking.k8s.io networkpolicies.networking.k8s.io poddisruptionbudgets.policy
)

access_for_deploy() {
	local r checks=()
	for r in "${WORKLOAD_RESOURCES[@]}"; do
		checks+=("get:$r" "create:$r" "patch:$r" "delete:$r")
	done
	checks+=("get:secrets" "create:secrets" "patch:secrets" "delete:secrets"
		"get:persistentvolumeclaims" "get:pods" "list:pods" "get:pods/log"
		"create:pods/portforward" "list:events")
	check_access "${checks[@]}"
}

# --- Waiting -------------------------------------------------------------------
# A pod the cluster refuses (an admission policy that wants a node selector,
# a ResourceQuota) never appears: the controller only records a FailedCreate
# event. So each wait also reads events, stops at the first FailedCreate on
# its own objects, and prints the namespace's recent events on any failure.

# FailedCreate events newer than epoch $1 on objects whose "Kind/name"
# matches the regex $2.
failed_creates() {
	k get events -o json 2>/dev/null | jq -r --argjson since "$1" --arg re "$2" '
		.items[]
		| select(.reason == "FailedCreate")
		| select("\(.involvedObject.kind)/\(.involvedObject.name)" | test($re))
		| select(((.lastTimestamp // .eventTime // .metadata.creationTimestamp // "1970-01-01T00:00:00Z")
		          | sub("\\.[0-9]+"; "") | fromdateiso8601) >= $since)
		| "\(.involvedObject.kind)/\(.involvedObject.name): \(.message)"' | sort -u || true
}

show_events() {
	log "Recent events in $NAMESPACE:"
	k get events --sort-by=.lastTimestamp 2>/dev/null | grep -E '^LAST|toolyard' | tail -n 30 >&2 || true
}

refused() { # message-lines what
	printf '%s\n' "$1" | sed 's/^/    /' >&2
	show_events
	die "the cluster refused to create pods for $2 (see above): check NODE_SELECTOR, TOLERATIONS and the ResourceQuota"
}

# Waits for the rollout of $1/$2; $3 is the "Kind/name" regex of the objects
# that create its pods (the StatefulSet, or the Deployment's ReplicaSets).
wait_rollout() {
	local since deadline msg
	since=$(($(date +%s) - 30))
	deadline=$((SECONDS + TIMEOUT))
	while :; do
		if k rollout status "$1/$2" --timeout=10s >/dev/null 2>&1; then
			log "$1/$2 is ready"
			return 0
		fi
		msg="$(failed_creates "$since" "$3")"
		[ -z "$msg" ] || refused "$msg" "$1/$2"
		if [ "$SECONDS" -ge "$deadline" ]; then
			show_events
			die "$1/$2 was not ready within ${TIMEOUT}s"
		fi
		sleep 2
	done
}

wait_for_migration() {
	local since deadline state msg
	since=$(($(date +%s) - 30))
	deadline=$((SECONDS + TIMEOUT))
	while :; do
		state="$(k get job toolyard-db-migrate -o json | jq -r '
			(.status.conditions // []) as $c
			| if ($c | any(.type == "Complete" and .status == "True")) then "complete"
			  elif ($c | any(.type == "Failed" and .status == "True")) then "failed"
			  else "running" end')"
		case "$state" in
		complete) return 0 ;;
		failed) break ;;
		esac
		msg="$(failed_creates "$since" '^Job/toolyard-db-migrate$')"
		[ -z "$msg" ] || refused "$msg" "the migration Job"
		if [ "$SECONDS" -ge "$deadline" ]; then
			warn "the migration did not finish within ${TIMEOUT}s"
			break
		fi
		sleep 3
	done
	k logs job/toolyard-db-migrate --tail=80 >&2 || true
	show_events
	die "the schema migration failed; the gateway was not changed"
}

# --- Secrets ---------------------------------------------------------------------

SECRETS_CREATED=0
label_secret() {
	k label secret "$@" app.kubernetes.io/part-of=toolyard "app.kubernetes.io/instance=$INSTANCE" --overwrite >/dev/null
}

ensure_secrets() {
	local creds pg pvc missing key d names pw
	creds="$(k get secret toolyard-credentials --ignore-not-found -o name)" || die "cannot read Secrets in $NAMESPACE"
	pg="$(k get secret toolyard-postgres --ignore-not-found -o name)" || die "cannot read Secrets in $NAMESPACE"
	if [ -n "$creds" ] && [ -n "$pg" ]; then
		# Key names only; jq prints no value.
		names=" $(k get secret toolyard-credentials -o json | jq -r '.data // {} | keys | join(" ")') "
		missing=""
		for key in identity.key oauth.key secrets.key session.key database-url; do
			case "$names" in
			*" $key "*) ;;
			*) missing="$missing $key" ;;
			esac
		done
		[ -z "$missing" ] || die "the toolyard-credentials Secret lacks:$missing"
		log "Secrets toolyard-credentials and toolyard-postgres exist: reusing them"
		return
	fi
	if [ -n "$creds" ] || [ -n "$pg" ]; then
		die "only one of the Secrets toolyard-credentials and toolyard-postgres exists. They belong together (database-url holds the Postgres password): restore the missing one from your backup, or run --destroy --purge to start again"
	fi
	# New Secrets over an old volume would not match its data: Postgres
	# keeps the old password and the database keeps the old keys' canaries.
	pvc="$(k get persistentvolumeclaim data-toolyard-postgres-0 --ignore-not-found -o name)" ||
		die "cannot read PersistentVolumeClaims in $NAMESPACE"
	if [ -n "$pvc" ]; then
		die "the Postgres volume data-toolyard-postgres-0 exists, but the Secrets toolyard-credentials and toolyard-postgres do not. New Secrets would not match its data (password and keys): restore both Secrets from your backup, or delete the volume for good with --destroy --purge"
	fi

	log "Creating the Secrets toolyard-credentials and toolyard-postgres (fresh keys and password)"
	d="$WORK/secrets"
	mkdir -m 0700 "$d" "$d/creds" "$d/pg"
	# Each key is the base64 of 32 random bytes, the form toolyard reads
	# (`toolyard keys generate` writes the same; docs/self-hosting.md).
	for key in identity oauth secrets session; do
		openssl rand -base64 32 >"$d/creds/$key.key"
	done
	# Hex, so the password needs no percent-encoding in the URL.
	openssl rand -hex 32 >"$d/pg/password"
	pw="$(<"$d/pg/password")"
	# printf is a shell builtin: the password never reaches a command line.
	printf 'postgres://toolyard:%s@toolyard-postgres:5432/toolyard?sslmode=disable\n' "$pw" >"$d/creds/database-url"
	pw=""
	k create secret generic toolyard-postgres --from-file=password="$d/pg/password" >/dev/null
	if ! k create secret generic toolyard-credentials --from-file="$d/creds" >/dev/null; then
		k delete secret toolyard-postgres --ignore-not-found >/dev/null || true
		die "creating toolyard-credentials failed; removed toolyard-postgres again"
	fi
	label_secret toolyard-credentials toolyard-postgres
	rm -rf "$d"
	SECRETS_CREATED=1
}

# --- Port-forward ------------------------------------------------------------------
# One pod at a time, to a random local port: no exec and no extra pod, so
# nothing here meets the admission rules that pods must follow.

PF_PID=""
PF_PORT=""
pf_start() { # pod
	local pflog="$WORK/pf-$1.log"
	PF_PORT=""
	# kubectl itself, not the k function: $! must be kubectl's own PID, or
	# killing it would leave the port-forward running.
	kubectl --namespace "$NAMESPACE" port-forward "pod/$1" ":8787" </dev/null >"$pflog" 2>&1 &
	PF_PID=$!
	PF_PIDS+=("$PF_PID")
	for _ in $(seq 1 75); do
		PF_PORT="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> 8787$/\1/p' "$pflog" | head -n 1)"
		[ -z "$PF_PORT" ] || return 0
		kill -0 "$PF_PID" 2>/dev/null || break
		sleep 0.2
	done
	sed 's/^/    /' "$pflog" >&2
	pf_stop
	return 1
}

pf_stop() {
	if [ -n "$PF_PID" ]; then
		kill "$PF_PID" 2>/dev/null || true
		wait "$PF_PID" 2>/dev/null || true
		PF_PID=""
	fi
}

# "name node" for each running gateway pod that is not being deleted.
gateway_pods() {
	k get pods -l app.kubernetes.io/name=toolyard,app.kubernetes.io/component=gateway -o json | jq -r '
		.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running")
		| "\(.metadata.name) \(.spec.nodeName)"'
}

# --- First admin -------------------------------------------------------------------
# Until the first account exists, POST /v1/auth/setup is open to any caller.
# So the Ingress waits: the admin account is created here, through a
# port-forward, with a generated password stored in the toolyard-admin
# Secret, and the Ingress is applied only once the set-up route is closed.

ADMIN_CREATED=0
setup_closed() { # base-url; true when an account exists
	local code
	code="$(curl -sS --max-time 10 -o "$WORK/me.json" -w '%{http_code}' "$1/v1/auth/me" 2>/dev/null || true)"
	if [ "$code" = 200 ] && jq -e '.setup_required == true' "$WORK/me.json" >/dev/null 2>&1; then
		return 1
	fi
	[ "$code" = 401 ] || die "GET /v1/auth/me answered HTTP ${code:-none}; the Ingress was not applied"
}

bootstrap_admin() {
	local pod base d code created=0 have
	pod="$(gateway_pods | head -n 1 | cut -d' ' -f1)"
	[ -n "$pod" ] || die "no running gateway pod to create the admin account on; the Ingress was not applied"
	pf_start "$pod" || die "cannot port-forward to $pod; the Ingress was not applied"
	base="http://127.0.0.1:$PF_PORT"
	if setup_closed "$base"; then
		pf_stop
		log "An account exists already: no admin set-up needed"
		return 0
	fi

	log "Creating the first admin account through a port-forward to $pod"
	d="$WORK/admin"
	mkdir -m 0700 "$d"
	have="$(k get secret toolyard-admin --ignore-not-found -o name)" || die "cannot read Secrets in $NAMESPACE"
	if [ -n "$have" ]; then
		# An earlier run stored the credentials but did not finish the
		# set-up: use them. The values go from kubectl to files only.
		k get secret toolyard-admin -o jsonpath='{.data.username}' | base64 -d >"$d/username"
		k get secret toolyard-admin -o jsonpath='{.data.password}' | base64 -d >"$d/password"
	else
		printf '%s' "$ADMIN_USERNAME" >"$d/username"
		openssl rand -hex 24 | tr -d '\n' >"$d/password"
		# Stored before the set-up call, so the password cannot be lost.
		k create secret generic toolyard-admin \
			--from-file=username="$d/username" --from-file=password="$d/password" >/dev/null
		label_secret toolyard-admin
		created=1
	fi
	jq -n --rawfile u "$d/username" --rawfile p "$d/password" '{username: $u, password: $p}' >"$d/setup.json"
	code="$(curl -sS --max-time 30 -o "$WORK/setup.out" -w '%{http_code}' -X POST "$base/v1/auth/setup" \
		-H "Origin: https://$HOST" \
		-H 'Content-Type: application/json' \
		--data-binary "@$d/setup.json" 2>/dev/null || true)"
	rm -rf "$d"
	case "$code" in
	200)
		ADMIN_CREATED=1
		log "Created the admin account; its username and password are in the Secret toolyard-admin"
		;;
	404 | 409)
		warn "an account was created by someone else meanwhile (HTTP $code)"
		if [ "$created" = 1 ]; then
			k delete secret toolyard-admin --ignore-not-found >/dev/null || true
		fi
		;;
	*)
		pf_stop
		die "POST /v1/auth/setup answered HTTP ${code:-none} ($(jq -r '.error // empty' "$WORK/setup.out" 2>/dev/null || true)); the Ingress was not applied"
		;;
	esac
	# Check that the route is closed before the Ingress opens the gateway.
	setup_closed "$base" || {
		pf_stop
		die "the set-up route is still open after the set-up; the Ingress was not applied"
	}
	pf_stop
}

# --- Steps -------------------------------------------------------------------------

deploy() {
	access_for_deploy
	render_all
	ensure_secrets

	log "Applying Postgres"
	k apply -f "$WORK/postgres.yaml"
	log "Waiting for Postgres (a new volume and node can take a few minutes)"
	wait_rollout statefulset toolyard-postgres '^StatefulSet/toolyard-postgres$'

	log "Running the schema migration (toolyard db migrate)"
	k delete job toolyard-db-migrate --ignore-not-found --cascade=foreground --wait=true >/dev/null
	k apply -f "$WORK/migrate.yaml"
	wait_for_migration
	log "Migration log (last 20 lines):"
	k logs job/toolyard-db-migrate --tail=20 >&2 || true

	log "Applying the gateway ($REPLICAS replica(s))"
	k apply -f "$WORK/app.yaml"
	if [ "$REPLICAS" = 1 ]; then
		k delete poddisruptionbudget toolyard --ignore-not-found >/dev/null
	fi
	# A ReplicaSet name is the Deployment's name plus a hash without
	# vowels, so the pattern never matches toolyard-postgres.
	wait_rollout deployment toolyard '^ReplicaSet/toolyard-[bcdfghjklmnpqrstvwxz2456789]+$'

	bootstrap_admin

	log "Applying the Ingress for https://$HOST"
	k apply -f "$WORK/ingress.yaml"
}

# --- Smoke test ----------------------------------------------------------------------

RESULTS=()
FAILED=0
pass() { RESULTS+=("ok    $*"); }
fail() {
	RESULTS+=("FAIL  $*")
	FAILED=1
}
skip() { RESULTS+=("skip  $*"); }

CURL_OPTS=(-sS --max-time 15)
AUTH=()
POD_VERSION=""
WANT_REPLICAS=""

# Checks /v1/readyz on one pod and reads its version (into POD_VERSION).
check_pod() {
	local code status
	POD_VERSION="unknown"
	pf_start "$1" || return 1
	code="$(curl -sS --max-time 5 -o "$WORK/readyz-$1.json" -w '%{http_code}' "http://127.0.0.1:$PF_PORT/v1/readyz" 2>/dev/null || true)"
	status="$(jq -r '.status // empty' "$WORK/readyz-$1.json" 2>/dev/null || true)"
	POD_VERSION="$(curl -sS --max-time 5 "http://127.0.0.1:$PF_PORT/v1/health" 2>/dev/null | jq -r '.version // empty' 2>/dev/null || true)"
	POD_VERSION="${POD_VERSION:-unknown}"
	pf_stop
	[ "$code" = 200 ] && [ "$status" = ready ]
}

smoke_health() { # base
	local code="" deadline=$((SECONDS + 120)) version sandbox
	# A new Ingress takes the controller a few seconds to pick up: retry.
	while :; do
		code="$(curl "${CURL_OPTS[@]}" -o "$WORK/health.json" -w '%{http_code}' "$1/v1/health" 2>/dev/null || true)"
		if [ "$code" = 200 ] && jq -e '.ok == true' "$WORK/health.json" >/dev/null 2>&1; then
			version="$(jq -r '.version // "?"' "$WORK/health.json")"
			sandbox="$(jq -r '.codemode_sandbox // "?"' "$WORK/health.json")"
			pass "GET /v1/health through the Ingress: ok, version $version, code-mode sandbox \"$sandbox\""
			return
		fi
		[ "$SECONDS" -lt "$deadline" ] || break
		sleep 3
	done
	fail "GET /v1/health through the Ingress: HTTP ${code:-none} (check DNS for $HOST, the certificate and the Ingress)"
}

smoke_pods() {
	local pods pod node got=0 first="" mixed=0
	pods="$(gateway_pods)"
	while IFS=' ' read -r pod node; do
		[ -n "$pod" ] || continue
		got=$((got + 1))
		if check_pod "$pod"; then
			pass "GET /v1/readyz on pod $pod (node $node): 200, version $POD_VERSION"
		else
			fail "GET /v1/readyz on pod $pod (node $node): not ready"
		fi
		if [ -z "$first" ]; then
			first="$POD_VERSION"
		elif [ "$POD_VERSION" != "$first" ]; then
			mixed=1
		fi
	done <<<"$pods"
	if [ "$got" = "$WANT_REPLICAS" ]; then
		pass "$got running gateway pod(s), as the Deployment asks"
	else
		fail "$got running gateway pod(s), the Deployment asks for $WANT_REPLICAS"
	fi
	[ "$mixed" = 0 ] || fail "the gateway pods report different versions"
}

smoke_discover() { # base
	local code session
	# Protocol 2026-07-28: no initialize and no session; the version, client
	# info and capabilities travel in _meta and in the Mcp-* headers.
	cat >"$WORK/discover.json" <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"k8s-test-deploy","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}
EOF
	: >"$WORK/discover.out"
	: >"$WORK/discover.headers"
	# With a public URL, a POST without a bearer token must come from that
	# origin, or the Origin check answers 403 before the token check can
	# answer 401.
	code="$(curl "${CURL_OPTS[@]}" -o "$WORK/discover.out" -D "$WORK/discover.headers" -w '%{http_code}' \
		-X POST "$1/mcp" \
		-H "Origin: $1" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H 'Mcp-Protocol-Version: 2026-07-28' \
		-H 'Mcp-Method: server/discover' \
		${AUTH[@]+"${AUTH[@]}"} \
		--data-binary "@$WORK/discover.json" 2>/dev/null || true)"
	# The answer is JSON, or one server-sent event that carries it.
	if grep -q '^data:' "$WORK/discover.out"; then
		sed -n 's/^data: *//p' "$WORK/discover.out" | head -n 1 >"$WORK/discover.result"
	else
		cp "$WORK/discover.out" "$WORK/discover.result"
	fi
	session="$(grep -i '^mcp-session-id:' "$WORK/discover.headers" || true)"
	if [ -n "$TOKEN_FILE" ]; then
		if [ "$code" != 200 ]; then
			fail "MCP server/discover with the token: HTTP ${code:-none}, want 200"
		elif [ -n "$session" ]; then
			fail "MCP server/discover with the token: the answer carries an Mcp-Session-Id"
		elif ! jq -e '.result.supportedVersions | index("2026-07-28")' "$WORK/discover.result" >/dev/null 2>&1; then
			fail "MCP server/discover with the token: 200, but supportedVersions lacks 2026-07-28"
		else
			pass "MCP server/discover with the token: 200, offers 2026-07-28, no session"
		fi
		return
	fi
	case "$code" in
	401) pass "MCP server/discover without a token: 401 (a public URL makes /mcp require one; set TOKEN_FILE for the 200 check)" ;;
	*) fail "MCP server/discover without a token: HTTP ${code:-none}, want 401" ;;
	esac
}

smoke_code_run() { # base
	local code run_status
	if [ -z "$TOKEN_FILE" ]; then
		skip "code-mode run: set TOKEN_FILE to an agent token file"
		return
	fi
	jq -n '{code: "print(\"toolyard k8s smoke test\")\n",
		reason: "Smoke test of the Kubernetes HA test deployment after a rollout",
		wait_seconds: 30}' >"$WORK/run.json"
	: >"$WORK/run.out"
	code="$(curl "${CURL_OPTS[@]}" --max-time 45 -o "$WORK/run.out" -w '%{http_code}' \
		-X POST "$1/v1/code/run" \
		-H 'Content-Type: application/json' \
		"${AUTH[@]}" \
		--data-binary "@$WORK/run.json" 2>/dev/null || true)"
	run_status="$(jq -r '.status // empty' "$WORK/run.out" 2>/dev/null || true)"
	if [ "$code" = 200 ] && [ "$run_status" = succeeded ]; then
		pass "code-mode run over POST /v1/code/run: succeeded"
	else
		fail "code-mode run over POST /v1/code/run: HTTP ${code:-none}, status ${run_status:-none}"
	fi
}

smoke() {
	local base="https://$HOST" code tok
	[ "$SMOKE_INSECURE" != 1 ] || CURL_OPTS+=(-k)
	WANT_REPLICAS="$(k get deployment toolyard -o jsonpath='{.spec.replicas}')" ||
		die "cannot read the toolyard Deployment in $NAMESPACE"
	if [ -n "$TOKEN_FILE" ]; then
		tok="$(tr -d ' \r\n\t' <"$TOKEN_FILE")"
		[ -n "$tok" ] || die "TOKEN_FILE $TOKEN_FILE is empty"
		# curl reads the header from a file, never from its command line.
		printf 'Authorization: Bearer %s\n' "$tok" >"$WORK/auth-header"
		tok=""
		AUTH=(-H "@$WORK/auth-header")
	fi
	log "Smoke test against $base"

	smoke_health "$base"
	code="$(curl "${CURL_OPTS[@]}" -o /dev/null -w '%{http_code}' "$base/v1/readyz" 2>/dev/null || true)"
	if [ "$code" = 200 ]; then
		pass "GET /v1/readyz through the Ingress: 200"
	else
		fail "GET /v1/readyz through the Ingress: HTTP ${code:-none}"
	fi
	smoke_pods
	smoke_discover "$base"
	smoke_code_run "$base"
}

summary() {
	local image
	image="$(k get pods -l app.kubernetes.io/name=toolyard,app.kubernetes.io/component=gateway -o json |
		jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .status.containerStatuses[]?.imageID] | unique | join(" ")')"
	printf '\n== toolyard test deployment: %s ==\n' "$NAMESPACE"
	printf 'URL:       https://%s\n' "$HOST"
	printf 'Image:     %s\n' "${image:-unknown}"
	printf 'Replicas:  %s\n\n' "$WANT_REPLICAS"
	printf '%s\n' "${RESULTS[@]}"
	printf '\n'
	k get pods -l "app.kubernetes.io/instance=$INSTANCE" -o wide >&2 || true
	if [ "$ADMIN_CREATED" = 1 ]; then
		printf '\nThe admin account was created. Its username and password are in the\n'
		printf 'Secret toolyard-admin (keys username and password) in %s.\n' "$NAMESPACE"
	fi
	if [ "$SECRETS_CREATED" = 1 ]; then
		printf '\nNew keys were created. Back up the Secrets toolyard-credentials,\n'
		printf 'toolyard-postgres and toolyard-admin apart from the database; without\n'
		printf 'the first two the data cannot be read.\n'
	fi
}

# --- Destroy -------------------------------------------------------------------------

destroy() {
	local r checks=() kinds="" selector="app.kubernetes.io/part-of=toolyard,app.kubernetes.io/instance=$INSTANCE"
	for r in "${WORKLOAD_RESOURCES[@]}"; do
		checks+=("delete:$r")
		kinds="${kinds:+$kinds,}$r"
	done
	if [ "$PURGE" = 1 ]; then
		[ "$CONFIRM_PURGE" = "$NAMESPACE" ] ||
			die "--purge deletes the database volume and the keys for good: set CONFIRM_PURGE=$NAMESPACE to confirm"
		checks+=("delete:secrets" "delete:persistentvolumeclaims")
	fi
	check_access "${checks[@]}"
	log "Deleting the objects labelled $selector in $NAMESPACE (workloads, Services, Ingress, NetworkPolicies, ConfigMaps)"
	# Foreground: return only once the pods are gone too.
	k delete "$kinds" -l "$selector" --ignore-not-found --cascade=foreground --wait=true
	if [ "$PURGE" = 1 ]; then
		log "Purging the Postgres volume and the Secrets"
		k delete persistentvolumeclaim -l "$selector" --ignore-not-found --wait=true
		k delete persistentvolumeclaim data-toolyard-postgres-0 --ignore-not-found --wait=true
		k delete secret toolyard-credentials toolyard-postgres toolyard-admin --ignore-not-found
	else
		log "Kept: the Postgres volume (data-toolyard-postgres-0) and the Secrets toolyard-credentials,"
		log "toolyard-postgres and toolyard-admin, which the data needs. --destroy --purge deletes them too."
	fi
}

# --- Main ------------------------------------------------------------------------------

validate_inputs
make_workdir
case "$MODE" in
render)
	render_all
	for c in "${COMPONENTS[@]}"; do
		[ "$c" = "${COMPONENTS[0]}" ] || printf -- '---\n'
		cat "$WORK/$c.yaml"
	done
	;;
deploy)
	if [ "$REPLICAS" -gt 1 ]; then
		log "REPLICAS=$REPLICAS: the image must include v0.1 high availability (two gateways on one database)"
	fi
	deploy
	smoke
	summary
	;;
smoke)
	check_access "get:pods" "list:pods" "create:pods/portforward" "get:deployments.apps"
	smoke
	summary
	;;
destroy)
	destroy
	;;
esac
[ "$FAILED" = 0 ] || die "the smoke test failed"
