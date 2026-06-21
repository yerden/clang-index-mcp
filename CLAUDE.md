# CLAUDE.md

Project-level guidance for AI agents working in this repo. The authoritative architecture lives in `clang-index-architecture.md` — this file summarizes the layout and captures hard-won implementation details that aren't obvious from the design doc alone.

## What this is

A C/C++ symbol + call-graph indexer driven by `clangd` over LSP, persisted to SQLite, and served to an AI assistant over MCP. Two operating modes (architecture §2):

- **Static** — `clang-index build` produces a frozen, content-keyed `index.db`; `clang-index serve` reads it. No clangd at serve time.
- **Dynamic** — `clangd-mcp-daemon` owns a live clangd, rebuilds the index as the tree changes, and serves MCP. Runs *natively* on the host for toolchain/header parity (§5.3).

These modes are intentionally not unified; don't merge them.

## Layout

```
cmd/
  clang-index/        build + serve subcommands (one binary, dispatched in main.go)
  clangd-mcp-daemon/  the dynamic daemon
internal/
  lsp/                JSON-RPC framing, request/response correlation, auto-reply to server→client requests
  clangdproc/         spawn clangd; Daemon wraps it with debounced restart
  extract/            compdb walker → []Symbol, []Edge; takes an lsp.Client (no lifecycle ownership)
  store/              SQLite; schema.sql + queries.sql embedded via //go:embed; atomic Swap
  cache/              content-digest cache, used at whole-build and per-file granularity
  mcp/                tool registration on top of github.com/mark3labs/mcp-go; stdio + Streamable HTTP transports
testdata/
  fixture/            small C project used by integration/system tests
```

Dependency map:
- `clang-index build` and the daemon share `lsp`, `clangdproc`, `extract`, `store`, `cache`.
- `clang-index serve` only depends on `store`'s read path.
- `cache` sits in front of `build` (whole-build) and inside `extract` (per-file). The daemon doesn't use the whole-build layer.

## Hard-won invariants — break these and things silently degrade

### LSP / clangd
1. **Auto-reply to server→client requests.** clangd gates `$/progress` on the client successfully replying to `window/workDoneProgress/create`. `internal/lsp` auto-replies `{result: null}` to any inbound request. Don't remove that — if it goes away, `WaitIndexed` hangs forever.
2. **Advertise hierarchical DocumentSymbol.** Without `textDocument.documentSymbol.hierarchicalDocumentSymbolSupport: true` in `initialize`, clangd falls back to legacy `SymbolInformation[]`, where the location range covers the entire declaration body. `textDocument/symbolInfo` queried at that range's start returns empty — extraction silently loses every non-static function.
3. **Background indexing only starts after a `didOpen`.** Not after `initialize`, not after `workspace/symbol`. `extract.Run` opens every TU first, then calls `WaitForIndex`, then queries symbols + call hierarchy. Don't reorder.
4. **USRs come from a clangd extension.** Stock LSP doesn't expose USRs. We use `textDocument/symbolInfo` (clangd-specific). If you swap to a different language server it won't have this.
5. **Cross-TU edges require the background index.** Within-TU edges (self-recursion, intra-file cycles) work without it. If you see those edges in tests but cross-TU is empty, the index hasn't finished — check the `WaitForIndex` callback wiring.

### Store
6. **Never migrate, always rebuild.** Each extraction writes a fresh DB. The daemon's `Swap` atomically points the live handle at the new file. Don't add ALTER paths.
7. **Edges with unresolved endpoints are dropped at write time**, not at extract time. Architecture §11 has an open question about external/unresolved symbols; if you add support, the dropping point is `store.WriteIndex`.
8. **`symbols.file` is relative to `ProjectRoot`** (§5.2). Don't store absolute paths; the artifact must be portable across build and serve environments.

### Caching
9. **Per-file cache key is `(file content digest, command digest)` over raw bytes — no normalization** (§7.2). Don't add whitespace stripping or sorting. The known gap (transitive header changes invisible to the key) is accepted; don't try to close it. Manual nuke is the documented fallback. Same applies after a schema/extraction change: a cached `tuPayload` from before today's run reflects the old extraction shape (e.g. missing `decl_file`, empty signatures); nuke the per-file cache directory after such changes.
10. **Whole-build cache lookup must include every file referenced by the compdb**, not just the TUs. `cmd/clang-index/main.go` does this. If you change the compdb walker, keep the input-digest input set in sync.

### Daemon
11. **Restart over notify** (§6.1). Don't implement `workspace/didChangeWatchedFiles`. When compdb changes, the daemon debounces (5s) and restarts clangd. The new clangd reuses on-disk shards via `--background-index-path` (§6.2).
12. **`--background-index-path` must be on persistent storage.** If it's container-ephemeral, every restart cold-starts; the persistence policy in §6.2 then doesn't apply.

## Testing

```
go test ./...                    # unit + integration; system test skips if clangd missing
go test -tags clangd_debug ./... # enable build-tagged debug probes (none currently)
```

- Unit: `store`, `lsp` framing, `mcp` handlers, `cache`, `extract` decode helpers — no clangd needed.
- System: `internal/extract/system_test.go` runs the full pipeline against `testdata/fixture/`; `cmd/clang-index/e2e_test.go` builds the binary and exercises `build` + `serve`.
- Both system tests `t.Skip` when `clangd` isn't on PATH.

When asserting against clangd behavior, prefer assertions that describe **what we want from the artifact** (e.g. "two callers of `hot_callee`") over assertions about clangd internals — clangd's call-hierarchy behavior shifts subtly across versions (see architecture §6.4).

### Specifically about callHierarchy and function pointers
The architecture (§11.1) predicted clangd's outgoingCalls would NOT surface indirect callees. In practice (clangd 19) it does add an edge `tu1_indirect → square` because `square` is referenced literally at the `dispatch(square, x)` call site. The genuine gap is **inside** `dispatch`: clangd does not know what `fn` resolves to, so there is no `dispatch → square` edge. The system test asserts the latter.

## What NOT to do

- Don't add features the architecture doesn't call for: no incremental DB mutation, no header-tracking for cache keys, no eviction logic, no VCS integration.
- Don't pull in `gopls`/`golang.org/x/tools/lsp` — the LSP client is intentionally minimal and bespoke for clangd's specific extensions.
- For MCP, do use `github.com/mark3labs/mcp-go`. Don't hand-roll JSON-RPC framing or HTTP transport here — those bugs (Ctrl+C parking on `bufio.Scanner.Scan`, etc.) all already exist solved in that library.
- HTTP transport is Streamable HTTP only (2025-03 MCP spec; single endpoint, default `/mcp`). Don't add back the legacy HTTP+SSE two-endpoint transport — modern clients use Streamable HTTP.
- Don't change the schema without re-checking embedded queries — they're SQL strings in `internal/store/queries.sql`, not Go-typed.
- Don't add `--no-verify`, `--force`, or bypass any check to make tests pass. If a test fails on clangd-version drift, document the version (§6.4) and update fixtures.
- Don't add comments explaining *what* code does. Only add a comment when *why* is non-obvious — a hidden constraint, a clangd quirk, a non-obvious LSP requirement. The auto-reply to server→client requests and the hierarchical-DocumentSymbol capability are good examples; both deserve their existing comments.

## Build / version pinning

clangd's exact version matters for §6.1/6.2 (architecture §6.4). Currently no pinning is in place — when CI/Docker arrives, follow the architecture's pinning policy: download a pinned release in the Dockerfile, install the same version natively for the daemon, assert via `clangd --version` at startup.
