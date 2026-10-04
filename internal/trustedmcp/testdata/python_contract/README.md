# Pinned assertion contract

These files are exact, read-only test fixtures from `beknown-work/beknown-services`
commit `56d39dfaa2a832b88d605ae56e91eaefdf1fc9cc`.
Their source directory is `infra/agent-test-cluster/platform`.

SHA-256:

- `identity.py`: `09605014985c1770de1abbd4514bedaab78c4d678d604853e31a9ea6144051f6`
- `leases.py`: `c5a2916ee605269de949d497cbc62d0afeb2336f97a294cfce9b7f326d739e35`

CI imports the actual verifier and its `Caller` contract. Tests pass synthetic
assertions through standard input. No fixtures configure, install, activate,
or contact the control service, Kubernetes, DynamoDB, or AWS.
