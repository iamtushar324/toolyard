# Actual BKS MCP application fixture

The files `identity.py`, `leases.py`, `mcp_server.py`, `sources.py`, and
`requirements.txt` are unmodified copies from `beknown-work/beknown-services`
commit `56d39dfaa2a832b88d605ae56e91eaefdf1fc9cc`, directory
`infra/agent-test-cluster/platform`. Go checks these hashes before launch:

| File | SHA-256 |
| --- | --- |
| `identity.py` | `09605014985c1770de1abbd4514bedaab78c4d678d604853e31a9ea6144051f6` |
| `leases.py` | `c5a2916ee605269de949d497cbc62d0afeb2336f97a294cfce9b7f326d739e35` |
| `mcp_server.py` | `29e917a9c72bf34e8d013b1cd41c16de63ee2bedc249eeb5d2adcecf002f1737` |
| `sources.py` | `fa8723db37a4ae60b8705f8792bfe740fb9a138db2153faa0702874d132fa815` |
| `requirements.txt` | `264921497f93f18312e9a83f60212af10f5a7767eafd2bfc321a29ba65c484ca` |

`requirements-ci.txt` selects the exact SDK and HTTP host versions from those
requirements. The fixture imports the actual `build_app`, `IdentityMiddleware`,
and MCP SDK. It supplies only `MemoryStore`, a fictional synthetic catalog, and
a fake same-repository resolver. The fake resolver changes its result after first
use so retry tests detect a loss of immutable source identity.
One fixture mode withholds the first successful create receipt and returns an
HTTP failure. The real in-memory control mutation occurs before that failure;
a fresh connector must recover it with the same request ID and parameters.

`harness.py` is the sole adapted code. It opens an ephemeral loopback listener
only when a hosted CI test explicitly requests it. Issuer bytes enter through
stdin; stdout carries only port/readiness/SDK metadata. Access and SDK logs are
disabled. Test cleanup kills and waits for the subprocess on every exit path.

This fixture is not a deployed factory. It neither creates AWS clients nor
installs a control service, starts workers, performs source fetches, provisions
credentials, builds previews, or reaches production. Its temporary in-memory
cost window is a test input, not approval for the parent platform.
