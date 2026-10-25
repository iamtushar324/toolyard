#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq programs use jq variables ($ns, ...) in single quotes
#
# Renders the HA test overlay through scripts/k8s-test-deploy.sh --render
# with placeholder inputs (REPLICAS=2 and REPLICAS=1, an image digest, a
# ghcr.io tag and a tag on another registry), writes the manifests to OUTDIR
# for kubeconform, and checks that every input landed where it should. No
# cluster needed; resolving the ghcr.io tag reads the public registry. Used
# by .github/workflows/deploy-manifests.yml.
#
#   scripts/k8s/check-render.sh OUTDIR
#
# Needs jq and yq (mikefarah, v4); set KUSTOMIZE_BIN to render with a
# standalone kustomize instead of `kubectl kustomize`.

set -euo pipefail

out="${1:?usage: check-render.sh OUTDIR}"
mkdir -p "$out"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
deploy="$root/scripts/k8s-test-deploy.sh"

# Placeholder inputs: documentation ranges and example.com only.
export NAMESPACE=toolyard-test
export HOST=toolyard-test.example.com
export IMAGE=ghcr.io/iamtushar324/toolyard@sha256:0000000000000000000000000000000000000000000000000000000000000000
export REPLICAS=2
export TRUSTED_PROXY=192.0.2.0/24,198.51.100.0/24
export INSTANCE=toolyard-ci
export EXTRA_EGRESS_CIDRS=198.18.0.0/15,203.0.113.0/24
export NODE_SELECTOR=example.com/pool=test
export TOLERATIONS=example.com/pool=test:NoSchedule,example.com/spot:NoExecute
export STORAGE_CLASS=example-ssd
export PG_STORAGE_SIZE=20Gi
export INGRESS_CLASS=example-ingress
export INGRESS_NAMESPACE=example-ingress-system
export TLS_SECRET=toolyard-test-tls
export OWNER_EMAIL=owner@example.com
unset KUBECONFIG TOKEN_FILE

fails=0
ok() { printf 'ok    %s\n' "$1"; }
bad() {
	printf 'FAIL  %s\n' "$1"
	fails=$((fails + 1))
}

# check DOCS-JSON DESCRIPTION JQ-PROGRAM: the program must print true.
check() {
	if jq -e --arg ns "$NAMESPACE" --arg host "$HOST" --arg image "$IMAGE" \
		--arg proxy "$TRUSTED_PROXY" --arg email "$OWNER_EMAIL" "$3" "$1" >/dev/null; then
		ok "$2"
	else
		bad "$2"
	fi
}

render() { # replicas file [VAR=value...]
	local r="$1" f="$2"
	shift 2
	env REPLICAS="$r" "$@" "$deploy" --render >"$f"
	yq -o=json -I=0 '.' "$f" | jq -s '[.[] | select(. != null)]' >"$f.json"
}

common_checks() { # json replicas
	local j="$1" r="$2"
	printf -- '--- REPLICAS=%s\n' "$r"
	check "$j" "every object is in namespace $NAMESPACE" \
		'all(.[]; .metadata.namespace == $ns)'
	check "$j" "every object carries part-of=toolyard and instance=INSTANCE (used by --destroy)" \
		'all(.[]; .metadata.labels["app.kubernetes.io/part-of"] == "toolyard"
		         and .metadata.labels["app.kubernetes.io/instance"] == "toolyard-ci")'
	check "$j" "the Postgres volume claim carries the instance label" '
		.[] | select(.kind == "StatefulSet") | .spec.volumeClaimTemplates[0].metadata.labels["app.kubernetes.io/instance"] == "toolyard-ci"'
	check "$j" "the expected objects exist" '
		["Deployment/toolyard", "Service/toolyard", "Ingress/toolyard",
		 "NetworkPolicy/toolyard-gateway", "StatefulSet/toolyard-postgres",
		 "Service/toolyard-postgres", "NetworkPolicy/toolyard-postgres",
		 "Job/toolyard-db-migrate", "NetworkPolicy/toolyard-migrate"]
		- [.[] | "\(.kind)/\(.metadata.name)"]
		| length == 0'
	# $r is a validated number (1 or 2), so it is safe in the program text.
	check "$j" "the Deployment has $r replica(s)" \
		".[] | select(.kind == \"Deployment\") | .spec.replicas == $r"
	check "$j" "gateway and migration Job run IMAGE, pulled if not present" '
		[.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[]]
		| length == 2 and all(.[]; .image == $image and .imagePullPolicy == "IfNotPresent")'
	check "$j" "the migration Job mounts database-url and no master key" '
		.[] | select(.kind == "Job") | .spec.template.spec.volumes[] | select(.name == "credentials")
		| .secret.items == [{key: "database-url", path: "database-url"}]'
	check "$j" "Postgres runs the pinned postgres:16 image" '
		.[] | select(.kind == "StatefulSet") | .spec.template.spec.containers[0].image
		| test("^postgres:16\\.[0-9]+-bookworm@sha256:[0-9a-f]{64}$")'
	check "$j" "every pod has the node selector and both tolerations" '
		[.[] | select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "Job") | .spec.template.spec]
		| length == 3 and all(.[];
			.nodeSelector == {"example.com/pool": "test"}
			and .tolerations == [
				{key: "example.com/pool", operator: "Equal", value: "test", effect: "NoSchedule"},
				{key: "example.com/spot", operator: "Exists", effect: "NoExecute"}])'
	check "$j" "every pod follows the restricted Pod Security Standard and needs no host namespaces" '
		[.[] | select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "Job") | .spec.template.spec]
		| all(.[];
			.securityContext.runAsNonRoot == true
			and .securityContext.seccompProfile.type == "RuntimeDefault"
			and .automountServiceAccountToken == false
			and (.hostNetwork // false) == false and (.hostPID // false) == false
			and all(.containers[];
				.securityContext.allowPrivilegeEscalation == false
				and .securityContext.readOnlyRootFilesystem == true
				and .securityContext.capabilities.drop == ["ALL"]))'
	check "$j" "every container sets requests.cpu, requests.memory and limits.memory (for a ResourceQuota)" '
		[.[] | select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "Job") | .spec.template.spec.containers[]]
		| all(.[]; .resources.requests.cpu and .resources.requests.memory and .resources.limits.memory)'
	check "$j" "the Postgres volume uses STORAGE_CLASS and PG_STORAGE_SIZE" '
		.[] | select(.kind == "StatefulSet") | .spec.volumeClaimTemplates[0].spec
		| .storageClassName == "example-ssd" and .resources.requests.storage == "20Gi"'
	check "$j" "Postgres is created with C collation and UTF-8" '
		.[] | select(.kind == "StatefulSet") | .spec.template.spec.containers[0].env
		| any(.[]; .name == "POSTGRES_INITDB_ARGS" and .value == "--encoding=UTF8 --locale=C")'
	check "$j" "the Ingress serves HOST over TLS with INGRESS_CLASS and TLS_SECRET" '
		.[] | select(.kind == "Ingress") | .spec
		| .ingressClassName == "example-ingress" and .rules[0].host == $host
		  and .tls == [{hosts: [$host], secretName: "toolyard-test-tls"}]'
	check "$j" "the gateway NetworkPolicy lets in INGRESS_NAMESPACE" '
		.[] | select(.kind == "NetworkPolicy" and .metadata.name == "toolyard-gateway")
		| .spec.ingress[0].from[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "example-ingress-system"'
	check "$j" "the gateway reaches DNS, its Postgres, public addresses and EXTRA_EGRESS_CIDRS only" '
		.[] | select(.kind == "NetworkPolicy" and .metadata.name == "toolyard-gateway") | .spec.egress
		| length == 4
		  and .[0] == {to: [{namespaceSelector: {}}], ports: [{port: 53, protocol: "UDP"}, {port: 53, protocol: "TCP"}]}
		  and .[1].to == [{podSelector: {matchLabels: {"app.kubernetes.io/name": "postgres", "app.kubernetes.io/component": "database"}}}]
		  and .[1].ports == [{port: 5432, protocol: "TCP"}]
		  and .[2] == {to: [{ipBlock: {cidr: "0.0.0.0/0", except: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]}}]}
		  and .[3] == {to: [{ipBlock: {cidr: "198.18.0.0/15"}}, {ipBlock: {cidr: "203.0.113.0/24"}}]}'
	check "$j" "the ConfigMap holds the public URL, trusted proxy, owner email and test settings" '
		[.[] | select(.kind == "ConfigMap" and (.metadata.name | startswith("toolyard-config-")))]
		| length == 1 and (.[0].data
			| .TOOLYARD_PUBLIC_URL == ("https://" + $host)
			  and .TOOLYARD_TRUSTED_PROXY == $proxy
			  and .TOOLYARD_OWNER_EMAIL == $email
			  and .TOOLYARD_ENVIRONMENT == "test"
			  and .TOOLYARD_KEYS == "require"
			  and .TOOLYARD_MIGRATE == "off"
			  and .TOOLYARD_STATELESS_MCP == "true")'
	check "$j" "the gateway reads that ConfigMap" '
		([.[] | select(.kind == "ConfigMap" and (.metadata.name | startswith("toolyard-config-"))) | .metadata.name][0]) as $cm
		| .[] | select(.kind == "Deployment") | .spec.template.spec.containers[0].envFrom[0].configMapRef.name == $cm'
	check "$j" "the gateway Service selects no Postgres or migration pod" '
		([.[] | select(.kind == "Service" and .metadata.name == "toolyard") | .spec.selector][0]) as $sel
		| [.[] | select(.kind == "StatefulSet" or .kind == "Job") | .spec.template.metadata.labels]
		| all(.[]; . as $l | ($sel | to_entries | all(.[]; $l[.key] == .value)) | not)'
}

render 2 "$out/ha-test-2-replicas.yaml"
j="$out/ha-test-2-replicas.yaml.json"
common_checks "$j" 2
check "$j" "two replicas roll with maxUnavailable 0 and maxSurge 1" '
	.[] | select(.kind == "Deployment") | .spec.strategy
	| .type == "RollingUpdate" and .rollingUpdate.maxUnavailable == 0 and .rollingUpdate.maxSurge == 1'
check "$j" "a PodDisruptionBudget keeps one gateway" '
	[.[] | select(.kind == "PodDisruptionBudget")] | length == 1 and .[0].spec.minAvailable == 1'
check "$j" "the gateways prefer different nodes and zones" '
	.[] | select(.kind == "Deployment") | .spec.template.spec
	| (.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution | length) == 1
	  and ([.topologySpreadConstraints[].topologyKey] | sort) == ["kubernetes.io/hostname", "topology.kubernetes.io/zone"]'

render 1 "$out/ha-test-1-replica.yaml"
j="$out/ha-test-1-replica.yaml.json"
common_checks "$j" 1
check "$j" "one replica uses Recreate, so two gateways never share the database" '
	.[] | select(.kind == "Deployment") | .spec.strategy == {type: "Recreate"}'
check "$j" "one replica has no PodDisruptionBudget" \
	'[.[] | select(.kind == "PodDisruptionBudget")] | length == 0'

printf -- '--- image tags\n'
render 1 "$out/ha-test-other-registry.yaml" IMAGE=registry.example.com/team/toolyard:test
j="$out/ha-test-other-registry.yaml.json"
check "$j" "a tag on another registry is kept and pulled with imagePullPolicy Always" '
	[.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[0]]
	| length == 2 and all(.[]; .image == "registry.example.com/team/toolyard:test" and .imagePullPolicy == "Always")'
render 1 "$out/ha-test-ghcr-tag.yaml" IMAGE=ghcr.io/iamtushar324/toolyard:main
j="$out/ha-test-ghcr-tag.yaml.json"
check "$j" "a ghcr.io tag is resolved to its digest" '
	[.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[0]]
	| length == 2 and all(.[]; (.image | test("^ghcr\\.io/iamtushar324/toolyard@sha256:[0-9a-f]{64}$")) and .imagePullPolicy == "IfNotPresent")'

printf -- '--- bad inputs are refused\n'
refuse() { # description VAR=value...
	local what="$1"
	shift
	if env "$@" "$deploy" --render >/dev/null 2>&1; then
		bad "refuses $what"
	else
		ok "refuses $what"
	fi
}
refuse "an upper-case HOST" HOST=Toolyard.example.com
refuse "a HOST with a quote" 'HOST=a".example.com'
refuse "a NODE_SELECTOR value with a quote" 'NODE_SELECTOR=example.com/pool=te"st'
refuse "a toleration without an effect" TOLERATIONS=example.com/pool=test
refuse "an IMAGE without a tag or digest" IMAGE=ghcr.io/iamtushar324/toolyard
refuse "a TRUSTED_PROXY that is not a CIDR" TRUSTED_PROXY=10.0.0.1
refuse "REPLICAS=0" REPLICAS=0
refuse "a missing REPLICAS" -u REPLICAS
refuse "an EXTRA_EGRESS_CIDRS entry that is not a CIDR" EXTRA_EGRESS_CIDRS=10.0.0.0/8,example.com
refuse "an INSTANCE with a space" 'INSTANCE=toolyard ci'
refuse "an ADMIN_USERNAME with a quote" 'ADMIN_USERNAME=ad"min'
if "$deploy" --render --purge >/dev/null 2>&1; then
	bad "refuses --purge without --destroy"
else
	ok "refuses --purge without --destroy"
fi

rm -f "$out"/*.json
if [ "$fails" -gt 0 ]; then
	printf '%d check(s) failed\n' "$fails" >&2
	exit 1
fi
printf 'all checks passed\n'
