"""Cloud-CI-only fixture around the unmodified reviewed BKS application.

No factory, AWS client, runtime credentials, builder or worker is installed.
Only synthetic issuer bytes arrive on standard input. Output is readiness metadata.
"""
import asyncio
import importlib.metadata
import json
import logging
import socket
import sys
import time
from types import SimpleNamespace

import uvicorn
from leases import LeaseManager, MemoryStore, initial_state
from mcp_server import PreviewService, build_app
from sources import REPOSITORY, Source


def main():
    logging.disable(logging.CRITICAL)
    configuration = json.loads(sys.stdin.readline())
    key = bytes(configuration["key"])
    if len(key) < 32:
        raise ValueError("Synthetic issuer missing")
    source_delay = float(configuration.get("source_delay_seconds", 0))
    if not 0 <= source_delay <= 4:
        raise ValueError("Unbounded synthetic resolver delay")
    now = int(time.time())
    state = initial_state()
    state["gate"].update(enabled=True, approved_until=now + 3600,
                         pools=["large"], approval_id="cloud-contract-only")
    catalog = SimpleNamespace(digest="b" * 64, description="Fictional fixture",
                              fixture_version="synthetic-v1")
    resolutions = 0

    def resolve(reference):
        nonlocal resolutions
        if reference == "branch:private-error":
            raise ValueError("synthetic private diagnostic must not escape")
        resolutions += 1
        if resolutions == 1:
            time.sleep(source_delay)
        # A moving resolver makes an idempotency regression observable.
        digest = "a" if resolutions == 1 else "c"
        return Source(REPOSITORY, reference, digest * 40, "feature/synthetic", 42, True)

    service = PreviewService(LeaseManager(MemoryStore(state)),
                             {"synthetic-v1": catalog}, resolve)
    application = build_app(service, key)
    delay = float(configuration.get("initialize_delay_seconds", 0))
    if not 0 <= delay <= 1:
        raise ValueError("Unbounded fixture delay")

    lose_create_response = configuration.get("lose_first_create_response", False) is True
    lost_create_response = False
    reflect_snapshots = configuration.get("reflect_snapshot_fragments", False) is True

    async def delayed_app(scope, receive, send):
        nonlocal lost_create_response
        # This transport seam lets CI prove that approval expiry bounds the
        # actual initialize request, not only a fake connector's preflight.
        if scope["type"] == "http" and scope["method"] == "POST":
            first = await receive()
            try:
                request = json.loads(first.get("body", b"{}"))
                initialize = request["method"] == "initialize"
                create = (request["method"] == "tools/call"
                          and request.get("params", {}).get("name") == "preview_create")
                snapshots = (request["method"] == "tools/call"
                             and request.get("params", {}).get("name") == "preview_snapshots")
            except (ValueError, KeyError, TypeError):
                initialize, create, snapshots = False, False, False
            if initialize:
                await asyncio.sleep(delay)
            supplied = False

            async def replay():
                nonlocal supplied
                if not supplied:
                    supplied = True
                    return first
                return await receive()

            if create and lose_create_response and not lost_create_response:
                lost_create_response = True

                async def lose_response(message):
                    # The real operation commits; only its transport receipt is
                    # withheld. Retry must retain the original idempotency key.
                    pass

                await application(scope, replay, lose_response)
                await send({"type": "http.response.start", "status": 500,
                            "headers": [(b"content-type", b"application/json")]})
                return await send({"type": "http.response.body",
                                   "body": b'{"error":"synthetic lost control receipt"}'})
            if snapshots and reflect_snapshots:
                # The real SDK handles the operation first. This fixture-only
                # ASGI response fault proves split credentials cannot reach the
                # deferred result cache or normal gateway audit path.
                captured = []

                async def capture(message):
                    captured.append(message)

                await application(scope, replay, capture)
                auth = [value for name, value in scope["headers"]
                        if name.lower() == b"authorization"]
                if len(auth) != 1 or not auth[0].startswith(b"Bearer "):
                    raise ValueError("Synthetic reflection input missing")
                token = auth[0][7:].decode("ascii")
                body = json.loads(b"".join(message.get("body", b"") for message in captured))
                midpoint = len(token) // 2
                body["result"]["content"] = [{"type": "text", "text": token[:midpoint]},
                                             {"type": "text", "text": token[midpoint:]}]
                body["result"]["structuredContent"] = {"chunks": [token[:7], token[7:]]}
                encoded = json.dumps(body).encode()
                start = next(message for message in captured if message["type"] == "http.response.start")
                start["headers"] = [(name, value) for name, value in start.get("headers", [])
                                    if name.lower() != b"content-length"]
                await send(start)
                return await send({"type": "http.response.body", "body": encoded})
            return await application(scope, replay, send)
        return await application(scope, receive, send)

    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.bind(("127.0.0.1", 0))
    listener.listen(16)

    class ReadyServer(uvicorn.Server):
        async def startup(self, sockets=None):
            await super().startup(sockets=sockets)
            if self.started:
                print(json.dumps({"ready": True, "port": listener.getsockname()[1],
                                  "sdk": importlib.metadata.version("mcp")}), flush=True)

    config = uvicorn.Config(delayed_app, host="127.0.0.1", log_config=None,
                            access_log=False, log_level="critical", lifespan="on",
                            limit_concurrency=16, timeout_keep_alive=1)
    ReadyServer(config).run(sockets=[listener])


if __name__ == "__main__":
    main()
