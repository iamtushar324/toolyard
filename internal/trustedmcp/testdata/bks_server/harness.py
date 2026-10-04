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
            except (ValueError, KeyError, TypeError):
                initialize, create = False, False
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
