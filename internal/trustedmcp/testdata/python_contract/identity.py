"""Verify short-lived, signed Toolyard caller assertions; never accept tool identities.

Toolyard must mint these from its authenticated context, not from tool arguments.
Static shared upstream headers cannot satisfy this protocol. Integration remains
disabled until that separate adapter and its access policy are reviewed.
"""
import base64
from contextvars import ContextVar
import hashlib
import hmac
import json
import time
from leases import Caller

CURRENT_CALLER = ContextVar("bk_preview_caller", default=None)
AUDIENCE = "bk-agent-test-pilot-preview-v1"


def b64(value):
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def verify_assertion(token, key, now=None):
    now = int(time.time()) if now is None else now
    if not isinstance(key, bytes) or len(key) < 32 or not isinstance(token, str) or len(token) > 4096:
        raise PermissionError("Invalid caller assertion")
    try:
        header_raw, payload_raw, signature = token.split(".")
        expected = b64(hmac.new(key, (header_raw + "." + payload_raw).encode(), hashlib.sha256).digest())
        if not hmac.compare_digest(expected, signature):
            raise ValueError()
        def decode(value):
            raw = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
            if b64(raw) != value:
                raise ValueError()
            return json.loads(raw)
        header, claims = decode(header_raw), decode(payload_raw)
        if header != {"alg": "HS256", "typ": "JWT", "kid": "toolyard-preview-v1"}:
            raise ValueError()
        if set(claims) != {"iss", "aud", "sub", "sid", "iat", "exp", "human"}:
            raise ValueError()
        if (claims["iss"] != "toolyard" or claims["aud"] != AUDIENCE or type(claims["human"]) is not bool
                or type(claims["iat"]) is not int or type(claims["exp"]) is not int
                or not 0 < claims["exp"] - claims["iat"] <= 60
                or claims["iat"] > now + 5 or claims["exp"] <= now):
            raise ValueError()
        return Caller(claims["sub"], claims["sid"], claims["human"])
    except (ValueError, KeyError, TypeError, UnicodeError):
        raise PermissionError("Invalid caller assertion") from None


def current_caller():
    caller = CURRENT_CALLER.get()
    if not isinstance(caller, Caller):
        raise PermissionError("Authenticated caller context required")
    return caller


class IdentityMiddleware:
    """ASGI middleware. No HTTP access logs or credential-bearing errors."""
    def __init__(self, app, key, clock=time.time):
        self.app, self.key, self.clock = app, key, clock

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        auth = [value for name, value in scope.get("headers", []) if name.lower() == b"authorization"]
        try:
            if len(auth) != 1 or not auth[0].startswith(b"Bearer "):
                raise PermissionError()
            caller = verify_assertion(auth[0][7:].decode("ascii"), self.key, int(self.clock()))
        except (PermissionError, UnicodeError):
            await send({"type": "http.response.start", "status": 401,
                        "headers": [(b"content-type", b"application/json"), (b"cache-control", b"no-store")]})
            await send({"type": "http.response.body", "body": b'{"error":"authenticated caller assertion required"}'})
            return
        context_token = CURRENT_CALLER.set(caller)
        try:
            await self.app(scope, receive, send)
        finally:
            CURRENT_CALLER.reset(context_token)
