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
6. **`textDocument/symbolInfo` only answers for files that have been `didOpen`'d.** Callees whose definitions live in headers (notably `static inline`) come back from `callHierarchy/outgoingCalls` with a header URI; symbolInfo on that position returns empty USR if the header isn't open, and the symbol+edge get silently dropped. `extract.Run` exposes an `ensureOpened` callback to `extractTU` that lazily opens the callee's file on a first USR miss; the file is shared across TUs and didClose'd at end of Run. Don't bypass this on the assumption that the background index is enough — it's not, for this specific RPC.

### Address-take walker (architecture §6.5)
7. **Category precedence is the load-bearing contract.** Walker classifies each address-take via stack walk; the priority is `compared > arg_to > stored_in > array_init > assigned_to > returned_from > other`. Tests must cover the inversion case `assert(fn == square)` — that's `compared`, NOT `arg_to:assert#1`. If you ever reorder priority or change classifier behavior, re-run the system test and inspect the `Address-take precedence` block; agents will silently misclassify dispatcher candidates otherwise.
8. **The vocabulary is a published contract.** `internal/extract/categories.go` is the single source of truth. The MCP tool descriptions embed it verbatim; the `describe_address_take_categories` tool returns it. Renaming a category breaks every agent prompt that's been built around the contract; only ever *append* new categories (at the end, after `other` or as a peer).
9. **Walker sibling hints.** The walker remembers the LHS of `BinaryOperator(=)` and the callee of `CallExpr` on the *parent* frame at the moment child[0] is visited, so later children can classify themselves without back-traversing the tree. If you add new classification patterns that depend on cousins (e.g. struct-base-and-field), follow the same pattern in `visit`'s after-child[0] block. Without this, `stored_in:T.f` and `arg_to:F#i` lose their detail field.
10. **Typedefs must be collected from headers, not just from the TU's own AST.** clangd's `textDocument/ast` for a TU does NOT inline the AST of its `#include`d headers, so a TypedefDecl defined in a header is invisible to the per-TU walker — yet expression types in the TU still carry the typedef-spelled form for nested uses (e.g. `lcore_function_t *`). `extract.Run` queries `textDocument/documentLink` to discover each TU's includes, ensureOpens them, then walks the union of opened-file ASTs into a shared typedef map seeded into every per-TU walker. Don't shortcut by walking only the TU — the join-by-type workflow silently fails the moment you do, because address-takes canonicalize and indirect-call sites don't.
11. **DesignatedInit field names live in the source, not the AST.** clangd drops the field designator before serializing the AST node; `Detail` and `Arcana` carry only the type. The walker slices the TU source at the node's range start (which clang places at the `.`) to recover the identifier. The walker therefore needs the TU source bytes — `extract.Run` reads them with `os.ReadFile` and seeds them as `sourceLines`. If you stop carrying source bytes through, `stored_in:T.f` reverts to `stored_in:T.<init>` and every field-keyed registration of the same struct collapses into one opaque row.

### MCP tool contract
12. **Tool descriptions are part of the contract. Update them on every semantic change.** What the agent sees is the description string returned in `tools/list` — *not* the architecture doc, not the README, not the comments in the handler. If you change any of the following, update the relevant tool description in `internal/mcp/mcp.go` in the *same* commit:
    - **Parameter semantics** (renaming, repurposing, changing units/encoding, switching between exact-match and pattern, swapping which entity a `function_id` refers to).
    - **Response shape** (added/removed/renamed fields, new optional fields the agent needs to know to read).
    - **Defaults** (limit values, default sort order, default filtering behavior).
    - **Cross-tool relationships** (a new tool that obsoletes part of an existing one; a tool whose output is meant to feed another). The other tool's description must point at it.
    - **The category vocabulary in `internal/extract/categories.go`** — adding a category, renaming one, or reordering precedence is a contract-breaking change that must propagate to every tool description that embeds the vocabulary, and to the `describe_address_take_categories` structured form.
    Also add a fencing assertion to `TestToolDescriptionsCarryAgentGuidance` so the new phrase doesn't silently disappear in a later cleanup.

13. **Agent-facing prose must say what you'd say to a new contributor in code review.** Specifically: which other tool to call next, the *not-this-but-that* clarifications when terms are overloaded (e.g. two `function_id` parameters with opposite semantics), what an empty result means, what wildcards / case rules apply for string filters, and any "looks like" trap (e.g. `context_detail` does NOT carry the category prefix even though the agent might infer it should). When in doubt: would a careful agent get the wrong answer without this sentence? If yes, the sentence belongs in the description.

### Store
14. **Never migrate, always rebuild.** Each extraction writes a fresh DB. The daemon's `Swap` atomically points the live handle at the new file. Don't add ALTER paths.
15. **Edges with unresolved endpoints are dropped at write time**, not at extract time. Architecture §11 has an open question about external/unresolved symbols; if you add support, the dropping point is `store.WriteIndexWithFacts`.
16. **`symbols.file` is relative to `ProjectRoot`** (§5.2). Don't store absolute paths; the artifact must be portable across build and serve environments.

### Caching
17. **Per-file cache key is `(file content digest, command digest)` over raw bytes — no normalization** (§7.2). Don't add whitespace stripping or sorting. The known gap (transitive header changes invisible to the key) is accepted; don't try to close it. Manual nuke is the documented fallback. Same applies after a schema/extraction change: a cached `tuPayload` from before today's run reflects the old extraction shape (e.g. missing `decl_file`, empty signatures, missing address_takes, `<init>` instead of field names); nuke the cache root after such changes (both `whole/` and `per-file/` subdirs share the same parent — `rm -rf` clears both at once).
18. **Whole-build cache lookup must include every file referenced by the compdb**, not just the TUs. `cmd/clang-index/main.go` does this. If you change the compdb walker, keep the input-digest input set in sync. The compdb half itself is intentionally normalized via `extract.CompDBDigest` (parsed entries, sorted by absolute file path, per-entry `(Directory, Arguments)` digest) — *not* raw bytes. This is the one deliberate exception to invariant #17's "no normalization" rule: the compdb is build-system metadata whose JSON formatting (timestamps, key order, indentation) carries no meaning, so hashing raw bytes would invalidate the cache on every reconfigure. Don't revert that.

### Daemon
19. **Restart over notify** (§6.1). Don't implement `workspace/didChangeWatchedFiles`. When compdb changes, the daemon debounces (5s) and restarts clangd. The new clangd reuses on-disk shards that clangd persists automatically under `<compdb-dir>/.cache/clangd/index/` (§6.2).
20. **clangd's shard directory must survive restarts.** Path is fixed at `<compdb-dir>/.cache/clangd/index/` — clangd has no flag to relocate it; do not try to invent one (`--background-index-path` is not a real clangd flag and clangd will refuse to start). If that directory is container-ephemeral or wiped between CI runs, every restart cold-starts; the persistence policy in §6.2 then doesn't apply.

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
