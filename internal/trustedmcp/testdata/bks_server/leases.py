"""Durable, compare-and-swap preview leases. This module never scales EC2.

One bounded DynamoDB item serializes admission, renewal and cleanup claims,
including the independent watchdog. No control-host filesystem lock is trusted.
"""
from copy import deepcopy
from dataclasses import dataclass
import json
import re
import time
import uuid

CLUSTER = "bk-agent-test-pilot"
HEARTBEAT = 60
GRACE = 600
MAX_LIFETIME = 21600
REVIEW_LIFETIME = 7200
TERMINAL = {"cleaned"}


def identifier(value):
    if not isinstance(value, str) or str(uuid.UUID(value)) != value:
        raise ValueError("Expected a canonical UUID")
    return value


@dataclass(frozen=True)
class Caller:
    subject: str
    session_id: str
    human: bool = False

    def __post_init__(self):
        identifier(self.session_id)
        if not re.fullmatch(r"[A-Za-z0-9:@._/-]{1,160}", self.subject):
            raise ValueError("Invalid authenticated subject")

    @property
    def key(self):
        return self.subject + ":" + self.session_id


def initial_state():
    return {"version": 1, "revision": 0, "cluster_id": CLUSTER,
            "gate": {"enabled": False, "approved_until": 0, "pools": [],
                     "capacity_draining": False, "drain_token": None, "approval_id": None},
            "environments": {}}


def validate_state(state):
    if (not isinstance(state, dict) or state.get("version") != 1 or state.get("cluster_id") != CLUSTER
            or type(state.get("revision")) is not int or state["revision"] < 0):
        raise ValueError("Malformed durable platform state")
    gate = state.get("gate")
    if (not isinstance(gate, dict) or set(gate) != set(initial_state()["gate"])
            or type(gate["enabled"]) is not bool or type(gate["capacity_draining"]) is not bool
            or type(gate["approved_until"]) is not int or gate["approved_until"] < 0
            or not isinstance(gate["pools"], list) or len(gate["pools"]) != len(set(gate["pools"]))
            or any(pool not in {"large", "medium"} for pool in gate["pools"])
            or (gate["approval_id"] is not None and not re.fullmatch(r"[A-Za-z0-9._:-]{1,160}", gate["approval_id"]))):
        raise ValueError("Malformed cost approval gate; admission stays closed")
    if gate["capacity_draining"]:
        identifier(gate["drain_token"])
        if gate["enabled"]:
            raise ValueError("Admission cannot be enabled during capacity drain")
    elif gate["drain_token"] is not None:
        raise ValueError("Unexpected stale capacity drain token")
    if gate["enabled"] and (not gate["approval_id"] or not gate["pools"] or gate["approved_until"] == 0):
        raise ValueError("Missing concrete cost approval")
    environments = state.get("environments")
    if not isinstance(environments, dict) or len(environments) > 128:
        raise ValueError("Malformed bounded environment state")
    for env_id, env in environments.items():
        identifier(env_id)
        if not isinstance(env, dict) or env.get("environment_id") != env_id or env.get("namespace") != "bk-preview-" + env_id:
            raise ValueError("Malformed environment ownership")
        identifier(env["generation"])
        if "runtime" in env:
            from reconciler import validate_runtime
            validate_runtime(env)
        if env["state"] not in {"queued", "building", "restoring", "ready", "failed", "cleaning", "cleaned"}:
            raise ValueError("Unknown lifecycle state")
        if (type(env["created_at"]) is not int or type(env["absolute_deadline"]) is not int
                or not 0 < env["absolute_deadline"] - env["created_at"] <= MAX_LIFETIME):
            raise ValueError("Malformed lifetime bound")
        if not isinstance(env["owners"], dict) or len(env["owners"]) > 8 or not isinstance(env["active_jobs"], dict) or len(env["active_jobs"]) > 8:
            raise ValueError("Malformed owners/jobs")
        for key, owner in env["owners"].items():
            if Caller(owner["subject"], owner["session_id"]).key != key or owner["kind"] not in {"job", "review"}:
                raise ValueError("Malformed owner identity")
            if (type(owner["expires_at"]) is not int or type(owner["last_heartbeat"]) is not int
                    or owner["expires_at"] > env["absolute_deadline"]):
                raise ValueError("Malformed owner expiry")
        for key, job in env["active_jobs"].items():
            identifier(key)
            if (job["owner"] not in env["owners"] or type(job["expires_at"]) is not int
                    or type(job["last_heartbeat"]) is not int or job["expires_at"] > env["absolute_deadline"]):
                raise ValueError("Malformed active job expiry")
    return state


class Conflict(Exception):
    pass


class DynamoStore:
    """A new platform table, NOT the original attended-pilot table.

    Inject a boto3 DynamoDB Table. Provision/initialize it only after approval.
    A missing or malformed item is an error, never an implicit open gate.
    """
    key = {"cluster_id": CLUSTER, "lease_id": "platform-state-v1"}

    def __init__(self, table):
        self.table = table

    def load(self):
        item = self.table.get_item(Key=self.key, ConsistentRead=True).get("Item")
        if not item or not isinstance(item.get("payload"), str):
            raise RuntimeError("Platform state has not been initialized by an operator")
        def reject_constant(_):
            raise ValueError("Non-finite platform state")
        state = validate_state(json.loads(item["payload"], parse_constant=reject_constant))
        if (state.get("version") != 1 or state.get("cluster_id") != CLUSTER
                or state.get("revision") != int(item["revision"])):
            raise RuntimeError("Platform state identity/version mismatch")
        return state

    def cas(self, previous_revision, state):
        raw = json.dumps(state, sort_keys=True, separators=(",", ":"))
        if len(raw.encode()) > 250000:
            raise RuntimeError("Lease history bound reached; archive before admission")
        try:
            self.table.put_item(Item=dict(self.key, revision=state["revision"], payload=raw),
                                ConditionExpression="revision = :previous",
                                ExpressionAttributeValues={":previous": previous_revision})
        except Exception as error:
            if getattr(error, "response", {}).get("Error", {}).get("Code") == "ConditionalCheckFailedException":
                raise Conflict() from None
            raise RuntimeError("Durable lease write failed") from None


class MemoryStore:
    """Test backend. Never selected by a deployed service."""
    def __init__(self, state=None):
        import threading
        self.state = deepcopy(state or initial_state())
        self.lock = threading.Lock()

    def load(self):
        with self.lock:
            return deepcopy(self.state)

    def cas(self, previous_revision, state):
        with self.lock:
            if self.state["revision"] != previous_revision:
                raise Conflict()
            self.state = deepcopy(state)


def valid_owners(environment, now):
    return {key: owner for key, owner in environment["owners"].items()
            if owner["expires_at"] > now and environment["absolute_deadline"] > now}


def valid_jobs(environment, now):
    return {key: job for key, job in environment["active_jobs"].items()
            if job["expires_at"] > now and environment["absolute_deadline"] > now}


def has_demand(environment, now):
    return bool(valid_owners(environment, now) or valid_jobs(environment, now))


class LeaseManager:
    def __init__(self, store, clock=time.time):
        self.store, self.clock = store, clock

    def change(self, operation):
        for _ in range(8):
            state = validate_state(self.store.load())
            previous = state["revision"]
            result = operation(state, int(self.clock()))
            state["revision"] = previous + 1
            validate_state(state)
            try:
                self.store.cas(previous, state)
                return deepcopy(result)
            except Conflict:
                continue
        raise RuntimeError("Concurrent lease changes; retry the same idempotent request")

    @staticmethod
    def owned(state, environment_id, caller):
        identifier(environment_id)
        environment = state["environments"].get(environment_id)
        if not environment or caller.key not in environment["owners"]:
            raise PermissionError("Preview is absent or not owned by this caller")
        return environment

    def inspect(self, environment_id, caller):
        return self.public(self.owned(validate_state(self.store.load()), environment_id, caller))

    def existing_request(self, request_id, caller):
        identifier(request_id)
        env_id = str(uuid.uuid5(uuid.NAMESPACE_URL, CLUSTER + ":" + caller.key + ":" + request_id))
        state = validate_state(self.store.load())
        env = state["environments"].get(env_id)
        if env is not None:
            self.owned(state, env_id, caller)
        return deepcopy(env)

    @staticmethod
    def public(environment):
        return {key: deepcopy(environment[key]) for key in (
            "environment_id", "namespace", "state", "source", "snapshot_id", "pool",
            "created_at", "absolute_deadline", "owners", "active_jobs", "resources",
            "results", "outcome", "generation")}

    def admit(self, request_id, caller, source, snapshot_id, pool="large", lifetime=1200):
        identifier(request_id)
        if pool not in {"large", "medium"} or type(lifetime) is not int or not 300 <= lifetime <= MAX_LIFETIME:
            raise ValueError("Invalid bounded profile")
        if not re.fullmatch(r"[a-z][a-z0-9-]{0,62}", snapshot_id):
            raise ValueError("Invalid installed snapshot ID")
        if (not isinstance(source, dict) or len(json.dumps(source)) > 8192
                or not re.fullmatch(r"[0-9a-f]{40}", source.get("commit_sha", ""))
                or (source.get("archive_sha256") is not None
                    and not re.fullmatch(r"[0-9a-f]{64}", source["archive_sha256"]))):
            raise ValueError("Only an immutable resolver/build receipt is accepted")
        env_id = str(uuid.uuid5(uuid.NAMESPACE_URL, CLUSTER + ":" + caller.key + ":" + request_id))

        def operation(state, now):
            existing = state["environments"].get(env_id)
            if existing:
                # Pin the first resolution atomically. Concurrent retries can
                # observe different heads of the same moving branch/PR.
                same_source = (existing["source"].get("reference") == source.get("reference")
                               if isinstance(source.get("reference"), str) else existing["source"] == source)
                if (not same_source or existing["snapshot_id"] != snapshot_id
                        or existing["pool"] != pool or existing["requested_lifetime"] != lifetime):
                    raise ValueError("Idempotency key reused with different parameters")
                return self.public(existing)
            gate = state["gate"]
            if (not gate["enabled"] or gate["capacity_draining"] or gate["approved_until"] < now + lifetime
                    or pool not in gate["pools"] or not gate["approval_id"]):
                raise PermissionError("Preview cost window is closed or too short")
            occupied = [item for item in state["environments"].values() if item["state"] not in TERMINAL]
            if len(occupied) >= 2 or (pool == "medium" and any(item["pool"] == "medium" for item in occupied)):
                raise RuntimeError("Preview concurrency bound reached; cleaning slots remain occupied")
            if len(state["environments"]) >= 128:
                raise RuntimeError("Lease history bound reached; archive before admission")
            env = {"environment_id": env_id, "namespace": "bk-preview-" + env_id,
                   "request_id": request_id, "state": "queued", "generation": str(uuid.uuid4()),
                   "source": deepcopy(source), "snapshot_id": snapshot_id, "pool": pool,
                   "created_at": now, "absolute_deadline": now + lifetime, "requested_lifetime": lifetime,
                   "owners": {caller.key: {"subject": caller.subject, "session_id": caller.session_id,
                                          "kind": "job", "last_heartbeat": now, "expires_at": min(now + HEARTBEAT + GRACE, now + lifetime)}},
                   "active_jobs": {}, "resources": {}, "results": [], "outcome": None,
                   "approval_id": gate["approval_id"], "cleanup_token": None}
            state["environments"][env_id] = env
            return self.public(env)
        return self.change(operation)

    def heartbeat(self, environment_id, caller, job_id=None):
        if job_id is not None:
            identifier(job_id)

        def operation(state, now):
            env = self.owned(state, environment_id, caller)
            if env["state"] in {"cleaning", "cleaned"} or env["absolute_deadline"] <= now:
                raise ValueError("An expired/cleaning preview cannot be renewed")
            owner = env["owners"][caller.key]
            if owner["expires_at"] <= now:
                raise ValueError("Expired owners require a new environment")
            if owner["kind"] == "review":
                raise ValueError("Human review leases do not heartbeat or renew forever")
            owner.update(last_heartbeat=now, expires_at=min(now + HEARTBEAT + GRACE, env["absolute_deadline"]))
            if job_id:
                job = env["active_jobs"].get(job_id)
                if job and job["owner"] != caller.key:
                    raise PermissionError("Job belongs to a different owner")
                if not job and len(env["active_jobs"]) >= 8:
                    raise ValueError("Active job bound reached")
                env["active_jobs"][job_id] = {"owner": caller.key, "last_heartbeat": now,
                    "expires_at": min(now + HEARTBEAT + GRACE, env["absolute_deadline"])}
            return self.public(env)
        return self.change(operation)

    def release(self, environment_id, caller, job_id=None, outcome="released"):
        if job_id is not None:
            identifier(job_id)
        if outcome not in {"released", "success", "failure"}:
            raise ValueError("Invalid release outcome")

        def operation(state, now):
            env = self.owned(state, environment_id, caller)
            if env["state"] == "cleaned":
                return self.public(env)
            if job_id:
                job = env["active_jobs"].get(job_id)
                if job and job["owner"] != caller.key:
                    raise PermissionError("Job belongs to a different owner")
                env["active_jobs"].pop(job_id, None)
            else:
                env["owners"][caller.key]["expires_at"] = min(now, env["absolute_deadline"])
                for job in env["active_jobs"].values():
                    if job["owner"] == caller.key:
                        job["expires_at"] = min(now, env["absolute_deadline"])
            env["outcome"] = outcome
            return self.public(env)
        return self.change(operation)

    def attach_owner(self, environment_id, inviter, new_owner, review=False):
        """Trusted operator/shared-owner adapter only; not a caller-supplied identity MCP tool."""
        def operation(state, now):
            env = self.owned(state, environment_id, inviter)
            if not has_demand(env, now) or inviter.key not in valid_owners(env, now) or env["state"] in {"cleaning", "cleaned"}:
                raise ValueError("Cannot share an expired or cleaning preview")
            if review and not new_owner.human:
                raise PermissionError("A review lease requires an authenticated human")
            if len(env["owners"]) >= 8 and new_owner.key not in env["owners"]:
                raise ValueError("Owner bound reached")
            # Sharing never changes the environment's absolute deadline or approved window.
            env["owners"][new_owner.key] = {"subject": new_owner.subject, "session_id": new_owner.session_id,
                "kind": "review" if review else "job", "last_heartbeat": now,
                "expires_at": min(now + (REVIEW_LIFETIME if review else HEARTBEAT + GRACE), env["absolute_deadline"])}
            return self.public(env)
        return self.change(operation)

    def claim_cleanup(self):
        def operation(state, now):
            claimed = []
            for env in state["environments"].values():
                if env["state"] == "cleaned" or has_demand(env, now):
                    continue
                if env.get("runtime", {}).get("operation") is not None:
                    # Only the fenced coordinator may settle an in-flight writer.
                    continue
                if env["state"] != "cleaning":
                    env.update(state="cleaning", cleanup_token=str(uuid.uuid4()), cleanup_started_at=now)
                claimed.append(deepcopy(env))
            return claimed
        return self.change(operation)

    def record_runtime(self, environment_id, generation, expected_state, next_state, resources=None, results=None):
        """Trusted reconciler only. Never exposed as an MCP tool.

        Record resource creation intents BEFORE applying manifests, then attach exact
        namespace/PVC UIDs and EBS IDs. Cleanup claims prevent late runtime writes.
        """
        transitions = {"queued": {"building", "failed"}, "building": {"restoring", "failed"},
                       "restoring": {"ready", "failed"}, "ready": {"ready", "failed"}}
        if next_state not in transitions.get(expected_state, set()):
            raise ValueError("Invalid runtime transition")
        identifier(generation)
        if resources is not None and (not isinstance(resources, dict) or len(json.dumps(resources)) > 16384):
            raise ValueError("Resource receipt exceeds bound")
        if results is not None and (not isinstance(results, list) or len(json.dumps(results)) > 16384):
            raise ValueError("Result receipt exceeds bound")
        def operation(state, now):
            env = state["environments"][identifier(environment_id)]
            if env.get("runtime", {}).get("operation") is not None or env.get("runtime", {}).get("fenced", False):
                raise ValueError("Use the fenced coordinator for this runtime operation")
            if env["generation"] != generation or env["state"] != expected_state or not has_demand(env, now):
                raise ValueError("Runtime generation is expired or claimed for cleanup")
            env["state"] = next_state
            if resources is not None:
                env["resources"] = deepcopy(resources)
            if results is not None:
                env["results"] = deepcopy(results)
            return self.public(env)
        return self.change(operation)

    def confirm_cleanup(self, environment_id, token, proof):
        required = {"logs_archived", "namespace_absent", "owned_pvcs_absent", "owned_disks_deleted"}
        if not isinstance(proof, dict) or set(proof) != required or any(proof[key] is not True for key in required):
            raise ValueError("Cleanup needs positive workload, storage and durable-result evidence")

        def operation(state, now):
            env = state["environments"][identifier(environment_id)]
            if env["state"] != "cleaning" or env["cleanup_token"] != token or has_demand(env, now):
                raise Conflict("Cleanup ownership changed")
            env.update(state="cleaned", cleanup_confirmed_at=now, cleanup_proof=deepcopy(proof))
            return self.public(env)
        return self.change(operation)

    def begin_capacity_drain(self):
        """Watchdog CAS fence. Capacity calls belong to its separately reviewed adapter."""
        def operation(state, now):
            if any(env["state"] != "cleaned" for env in state["environments"].values()):
                raise ValueError("Delete owned workloads and storage before reducing capacity")
            if not state["gate"]["capacity_draining"]:
                state["gate"].update(enabled=False, capacity_draining=True, drain_token=str(uuid.uuid4()))
            return deepcopy(state["gate"])
        return self.change(operation)

    def finish_capacity_drain(self, token, actual_instances, undeleted_roots):
        identifier(token)
        if type(actual_instances) is not list or type(undeleted_roots) is not list or actual_instances or undeleted_roots:
            raise ValueError("Kubernetes node absence is not proof of EC2 termination")
        def operation(state, now):
            if not state["gate"]["capacity_draining"] or state["gate"].get("drain_token") != token:
                raise ValueError("No matching claimed capacity drain")
            state["gate"]["capacity_draining"] = False
            state["gate"]["drain_token"] = None
            # Leave admission disabled until the operator approves another window.
            return deepcopy(state["gate"])
        return self.change(operation)
