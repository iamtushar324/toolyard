"""Closed Kubernetes preview MCP upstream. Import creates no AWS clients/services."""
from dataclasses import asdict
from typing import Any
from identity import IdentityMiddleware, current_caller
from sources import resolve_source


class PreviewService:
    def __init__(self, leases, catalogs, resolver=resolve_source):
        self.leases, self.catalogs, self.resolver = leases, dict(catalogs), resolver

    def create(self, source_ref, request_id, snapshot_id, pool="large", lifetime_seconds=1200):
        caller = current_caller()
        existing = self.leases.existing_request(request_id, caller)
        if existing:
            if (existing["source"]["reference"] != source_ref or existing["snapshot_id"] != snapshot_id
                    or existing["pool"] != pool or existing["requested_lifetime"] != lifetime_seconds):
                raise ValueError("Idempotency key reused with different parameters")
            return self.leases.public(existing)
        if snapshot_id not in self.catalogs:
            raise ValueError("Snapshot is not in the installed reviewed synthetic catalog")
        # Resolution runs in trusted code. Agent args cannot supply image, manifest,
        # archive, production config, GitHub credentials or an alternative repository.
        source = asdict(self.resolver(source_ref))
        source["archive_sha256"] = None
        source["build_recipe"] = "bks-arm64-v1"
        return self.leases.admit(request_id, caller, source, snapshot_id, pool, lifetime_seconds)

    def inspect(self, environment_id):
        return self.leases.inspect(environment_id, current_caller())

    def heartbeat(self, environment_id, job_id=None):
        return self.leases.heartbeat(environment_id, current_caller(), job_id)

    def release(self, environment_id, outcome="released", job_id=None):
        return self.leases.release(environment_id, current_caller(), job_id, outcome)

    def snapshots(self):
        current_caller()
        return [{"snapshot_id": key, "digest": value.digest, "description": value.description,
                 "fixture_version": value.fixture_version, "data_class": "synthetic"}
                for key, value in sorted(self.catalogs.items())]

    def results(self, environment_id):
        # Durable object receipts only. Raw application logs can contain secrets and
        # need the reviewed artifact redaction/download route, not arbitrary log exec.
        env = self.inspect(environment_id)
        return {"environment_id": environment_id, "state": env["state"], "results": env["results"]}


def build_server(service):
    from mcp.server import MCPServer
    from mcp.server.mcpserver.exceptions import ToolError
    from mcp_types import CallToolResult, TextContent, ToolAnnotations
    allowed_arguments = {
        "preview_create": {"source_ref", "request_id", "snapshot_id", "pool", "lifetime_seconds"},
        "preview_inspect": {"environment_id"}, "preview_heartbeat": {"environment_id", "job_id"},
        "preview_release": {"environment_id", "outcome", "job_id"}, "preview_snapshots": set(),
        "preview_results": {"environment_id"},
    }

    async def strict_arguments(context, call_next):
        if context.method == "tools/call":
            params = context.params
            arguments = params.get("arguments", {}) if isinstance(params, dict) else None
            fields = allowed_arguments.get(params.get("name")) if isinstance(params, dict) else None
            if fields is None or not isinstance(arguments, dict) or set(arguments) - fields:
                return CallToolResult(isError=True, content=[TextContent(type="text", text="Unexpected preview request fields")])
        return await call_next(context)

    server = MCPServer("BKS Kubernetes Previews", version="0.1.0", log_level="WARNING", middleware=[strict_arguments])
    read = ToolAnnotations(readOnlyHint=True, destructiveHint=False, idempotentHint=True, openWorldHint=False)
    create = ToolAnnotations(readOnlyHint=False, destructiveHint=False, idempotentHint=True, openWorldHint=False)
    release = ToolAnnotations(readOnlyHint=False, destructiveHint=True, idempotentHint=True, openWorldHint=False)

    def safe_call(operation, *arguments):
        try:
            return operation(*arguments)
        except (ValueError, PermissionError, RuntimeError) as error:
            # These service errors are deliberately credential-free. Expected
            # ToolError avoids the SDK's unexpected-exception traceback logger.
            raise ToolError(str(error)) from None
        except Exception:
            raise ToolError("Preview operation failed; inspect its durable operator receipt") from None

    @server.tool(annotations=create, structured_output=True)
    def preview_create(source_ref: str, request_id: str, snapshot_id: str, pool: str = "large", lifetime_seconds: int = 1200) -> dict[str, Any]:
        """Queue an isolated same-repository branch/PR/draft preview within an approved cost window. Does not imply readiness."""
        return safe_call(service.create, source_ref, request_id, snapshot_id, pool, lifetime_seconds)

    @server.tool(annotations=read, structured_output=True)
    def preview_inspect(environment_id: str) -> dict[str, Any]:
        """Inspect only the authenticated owner's environment and immutable source."""
        return safe_call(service.inspect, environment_id)

    @server.tool(annotations=create, structured_output=True)
    def preview_heartbeat(environment_id: str, job_id: str | None = None) -> dict[str, Any]:
        """Report a real active owner/job heartbeat; cannot extend the absolute deadline."""
        return safe_call(service.heartbeat, environment_id, job_id)

    @server.tool(annotations=release, structured_output=True)
    def preview_release(environment_id: str, outcome: str = "released", job_id: str | None = None) -> dict[str, Any]:
        """Release this caller's job/ownership; other valid owners keep the preview alive."""
        return safe_call(service.release, environment_id, outcome, job_id)

    @server.tool(annotations=read, structured_output=True)
    def preview_snapshots() -> list[dict[str, Any]]:
        """List operator-reviewed synthetic snapshots; arbitrary S3 objects are never accepted."""
        return safe_call(service.snapshots)

    @server.tool(annotations=read, structured_output=True)
    def preview_results(environment_id: str) -> dict[str, Any]:
        """Return owned durable test-result receipts outside Spot disks, without raw secret-bearing logs."""
        return safe_call(service.results, environment_id)

    return server


def build_app(service, identity_key, allowed_hosts=("127.0.0.1:*", "localhost:*")):
    from mcp.server.transport_security import TransportSecuritySettings
    server = build_server(service)
    app = server.streamable_http_app(stateless_http=True, json_response=True,
                                     max_request_body_size=16384, max_sessions=16,
                                     transport_security=TransportSecuritySettings(
                                         enable_dns_rebinding_protection=True,
                                         allowed_hosts=list(allowed_hosts), allowed_origins=[]))
    return IdentityMiddleware(app, identity_key)
