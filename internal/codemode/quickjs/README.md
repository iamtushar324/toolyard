# QuickJS-NG for code mode

`qjs-wasi.wasm` is the JavaScript engine code mode runs scripts with
(`executeToolCode` with `language: "javascript"`, `POST /v1/code/run` with
`text/javascript`). It is the official WASI build of the `qjs` command from
[QuickJS-NG](https://github.com/quickjs-ng/quickjs), embedded unchanged:

| | |
|---|---|
| Release | v0.17.0 (2026-09-18) |
| Source | https://github.com/quickjs-ng/quickjs/releases/download/v0.17.0/qjs-wasi.wasm |
| sha256 | `42a732a676ec2d93488c19411e0fad283bf72658fdad746f089914b523c783b1` (as GitHub publishes it for the asset) |
| Licence | MIT, see `LICENSE` in this folder |

`internal/codemode/js.go` runs it with [wazero](https://github.com/tetratelabs/wazero)
(pure Go, Apache-2.0) in its interpreter, inside the code-mode worker
process and its sandbox. The module gets no files, no environment and no
network; its stdin and stdout are the worker's bridge to the gateway.

## Updating

1. Download `qjs-wasi.wasm` from the new release's assets and compare its
   sha256 with the digest GitHub shows for the asset.
2. Replace the file here and its `LICENSE` from the same tag.
3. Update `QuickJSVersion` and `quickJSWasmSHA256` in `internal/codemode/js.go`
   and the table above. `TestEmbeddedQuickJS` fails until they agree.
4. Let CI run the code-mode tests, the sandbox tests included.
