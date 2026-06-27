# clang-index-mcp

Symbol and call-graph index for C/C++ projects, extracted with [clangd](https://clangd.llvm.org/), stored in SQLite, and served to AI assistants over [MCP](https://modelcontextprotocol.io/).

Two operating modes with different freshness/cost tradeoffs:

| Mode | What it is | When to use it |
|---|---|---|
| **Static** (`clang-index build` + `serve`) | Build an `index.db` once, serve it later. Read-only, no clangd at serve time. | CI pipelines, cloud-hosted indexes (Fly.io etc.), any case where the codebase is frozen at index time. |
| **Dynamic** (`clangd-mcp-daemon`) | Long-running daemon that owns a live clangd and rebuilds the index as the tree changes. Reflects in-progress edits. | Local dev — same machine as your editor. |

Both expose the same MCP tools, so assistants don't need to know which mode is backing them.

## MCP tools

Symbol & call-graph:
- `search_symbol(query, limit?)` — FTS5 full-text search over symbol name and signature.
- `get_symbol(id)` — fetch a symbol with its direct callers and callees.
- `list_symbols_in_file(file, limit?)` — list every symbol declared *or* defined in a file. Useful for "what's the public surface of `foo.h`?" — matches either the declaration file (typically the header) or the definition file (typically the `.c`).

Function-pointer dispatch (architecture §6.5):
- `get_indirect_call_sites(function_id?, type?, limit?)` — CallExprs whose callee isn't a directly-named function. Each row carries `callee_type` and `callee_expr` (e.g. `fn`, `ops[i]`, `<base>.cb`).
- `find_address_takes(type?, category?, context_detail_pattern?, limit?)` — sites where a function's address is taken (registered as a callback, stored in a struct/array, compared, returned, etc.). Each row carries a precedence-resolved `category` tag.
- `get_address_take_sites(function_id, limit?)` — every recorded address-take for a specific function.
- `describe_address_take_categories()` — returns the category vocabulary structured for programmatic use (same prose is embedded in the other tools' descriptions).

Symbol records carry both a definition location (`File`/`Line`) and a declaration location (`DeclFile`/`DeclLine`). For static / file-local symbols, declaration and definition coincide and `DeclFile` is empty.

Both transports — **stdio** and **Streamable HTTP** (2025-03 spec, single endpoint) — are supported by every binary.

## Install

Requires Go 1.22+ and a [pinned clangd](https://github.com/clangd/clangd/releases) on `PATH`.

```sh
go install github.com/yerden/clang-index-mcp/cmd/clang-index@latest
go install github.com/yerden/clang-index-mcp/cmd/clangd-mcp-daemon@latest
```

## Quick start — static mode

```sh
# 1. Build the index. compile_commands.json must already exist for your project.
clang-index build \
  --compdb /path/to/compile_commands.json \
  --out index.db \
  --project-root /path/to/project

# 2. Serve over stdio (for direct MCP clients like Claude Desktop)
clang-index serve --db index.db

# Or over Streamable HTTP at http://localhost:8080/mcp
clang-index serve --db index.db --http :8080
```

## Quick start — dynamic mode

Run on your dev machine, natively (not in a container — toolchain/header parity matters; see [architecture §5.3](clang-index-architecture.md#53-why-the-dynamic-daemon-runs-natively-not-in-docker)):

```sh
clangd-mcp-daemon \
  --compdb /path/to/compile_commands.json \
  --project-root /path/to/project \
  --http :8080
```

The daemon watches `compile_commands.json`. When it changes, clangd is restarted (debounced 5 s) and the index is rebuilt. The live `*sql.DB` handle is then atomically swapped — in-flight MCP queries never see a half-built database.

> **Disable clang-tidy in clangd.** The indexer calls `textDocument/documentLink` on every TU (to resolve typedefs from headers). clangd processes that request through its AST worker, which also runs any enabled clang-tidy checkers. A crashing checker kills clangd mid-extraction; the daemon will retry, but the crash recurs on the same file. Add this to `~/.clangd` (or the project's `.clangd`) to suppress all clang-tidy checks for clangd — it does not affect your editor or CI runs:
>
> ```yaml
> Diagnostics:
>   ClangTidy:
>     Remove: ["*"]
> ```

## Configuring an MCP client

For Claude Desktop, add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "clang-index": {
      "command": "clang-index",
      "args": ["serve", "--db", "/abs/path/to/index.db"]
    }
  }
}
```

For HTTP clients, point them at `http://host:8080/mcp` — Streamable HTTP returns an `Mcp-Session-Id` header on `initialize` which subsequent requests must echo.

## Caching

`clang-index build` is disposable by default — every run cold-starts clangd and re-extracts every TU. One opt-in cache flag enables both layers:

```sh
clang-index build --compdb ... --out index.db --cache ~/.cache/clang-index-mcp
```

Under that root, two subdirs hold the two layers:

- `whole/` — if `(compdb + every file's content)` is unchanged, skip clangd entirely and copy the cached `index.db`.
- `per-file/` — on a whole-build miss, only TUs whose `(content + compile command)` changed get re-extracted; the rest are replayed from JSON.

`clangd-mcp-daemon` accepts the same `-cache` flag and uses the `per-file/` subdir (it doesn't produce a frozen artifact, so whole-build doesn't apply). Pointing both binaries at the same root lets them share per-file work.

Both layers key on raw-bytes digests — no normalization, no VCS dependency. The known limitation: per-file keys don't include transitively-included header content, so editing a shared header is invisible to the cache. Workaround: `rm -rf` the cache root. See [architecture §7](clang-index-architecture.md#7-caching--content-digest-keyed-no-vcs-dependency).

## Speeding up clangd's background index

Both `clang-index build` and `clangd-mcp-daemon` accept tuning flags. By default clangd sizes its worker pool to roughly *half* your logical cores and runs the indexer at the OS's lowest priority (Linux: nice 19 + idle I/O) so it doesn't fight with a foreground IDE. On a dedicated build host neither default helps:

```sh
clang-index build --compdb ... \
  --clangd-jobs $(nproc) --clangd-boost \
  --extract-jobs $(nproc)
```

- `--clangd-jobs N` — forwarded as `-j=N`; sets clangd's worker count. `0` (default) keeps clangd's heuristic.
- `--clangd-boost` — sets `--background-index-priority=normal` so the indexer competes equally with foreground work. Usually the bigger win.
- `--extract-jobs N` — max concurrent per-TU extraction workers (default `NumCPU`). We dispatch parallel LSP requests to clangd; without this, clangd's worker threads sit idle waiting on our next sequential call. Usually the biggest win on the extraction phase of large projects.

clangd's background indexer is on by default and persists shards under `<compdb-dir>/.cache/clangd/index/` automatically (the path is fixed by clangd — there is no flag to relocate it). On subsequent runs clangd reuses those shards keyed on per-file `(content, command)`, so unchanged TUs are skipped at the indexer level (architecture §6.2). For CI: cache that directory across runs to get warm starts.

Note: cranking workers means more concurrent disk I/O against the shard directory and higher clangd RSS (multiple in-flight ASTs); on slow storage or tight-memory hosts that becomes the bottleneck before CPU does.

## Project layout

```
cmd/
  clang-index/          build + serve subcommands
  clangd-mcp-daemon/    the live daemon
internal/
  lsp/                  JSON-RPC framing for clangd
  clangdproc/           clangd lifecycle + debounced restart
  extract/              compdb walker, drives clangd, produces symbols + edges
  store/                SQLite schema + read/write/swap
  cache/                content-digest cache (whole-build + per-file)
  mcp/                  tool registration via github.com/mark3labs/mcp-go
testdata/
  fixture/              tiny C project for integration tests
```

## Walking a function-pointer dispatcher

When `get_symbol` shows a dispatcher whose body contains `fn(x)` and no direct callers/callees explain where the dispatch goes:

1. `get_indirect_call_sites(function_id=dispatcher_id)` — read off `callee_type` (e.g. `int (*)(int)`) and `callee_expr` (e.g. `fn`, `<base>.cb`, `ops[i]`).
2. `find_address_takes(type=callee_type, category="arg_to", context_detail_pattern="dispatcher_name#%")` — enumerate the functions registered as that dispatcher's callback. For struct-stored callbacks use `category="stored_in"` with `context_detail_pattern="struct_type.field"`; for table dispatch, `category="array_init"`.
3. Apply project-specific filters (naming patterns, header membership) the indexer can't infer.

For the reverse direction (you have a callback, want to find its dispatcher):

1. `get_address_take_sites(function_id=callback_id)` — locate the `stored_in:<struct>.<field>` row.
2. `get_indirect_call_sites(type=fn_ptr_type, callee_expr_pattern="%.<field>")` — narrow to dispatchers reading exactly that field, dropping noise from same-typed but unrelated callbacks elsewhere in the codebase.

The `category` field is precedence-resolved; treat it as authoritative. `compared` rows are NEGATIVE signals (the pointer is being tested, not invoked) — exclude them. Types are canonicalized at extract time (typedef-spelled forms like `lcore_function_t *` are substituted to canonical `int (*)(void *)`), so always match against the canonical. See `describe_address_take_categories` for the full vocabulary and precedence rule.

## Possible directions

Sketches, not commitments — captured here so the design tradeoffs aren't re-litigated each time the topic comes up.

### Source-tree file watching

Today the daemon only watches `compile_commands.json`; source edits don't refresh the served DB until the next compdb event. A natural extension is to add a second fsnotify watcher over the project tree — every modify/create/delete on a `.c`/`.cpp`/`.h` would pulse the same debounced `Daemon.Restart()` we already use for compdb changes. Per-file extraction cache and clangd's persisted shards (at `<compdb-dir>/.cache/clangd/index/`) both ensure unchanged TUs aren't re-extracted, so a single source edit costs one TU's worth of clangd work, not a full reindex. Trade-off: header edits invalidate broadly (the per-file cache key intentionally doesn't track transitive includes, §7.2), and editor save-storms during refactors would cause restart churn; the existing 5 s debounce (§6.1) is the lever to tune. Status quo workaround: re-run the build system to regenerate `compile_commands.json`.

### Function-pointer-aware call edges

`call_edges` today only carries what clangd's `callHierarchy/outgoingCalls` returns — direct calls plus a fragile by-accident edge when a function pointer is passed as a literal at the call site (e.g. `dispatch(square, x)` produces `tu1_indirect → square`). The moment the pointer is wrapped in a variable, a struct field, or a dispatch table, the edge disappears and AI traversal hits a dead end at any dispatcher.

A reasonable closing of this gap, in tiers:

| tier | what it adds | cost |
|---|---|---|
| current | direct calls + literal-at-call-site as one edge kind | none |
| **address-taken × indirect-call sites**, type-narrowed | for every function whose address is taken anywhere (discoverable via `textDocument/references`) and every call site through a function-pointer-typed argument, synthesize an edge tagged `edge_kind = "indirect"`. Sound over-approximation, type-narrowed to cut noise. | one extra LSP query per function, one new schema column, MCP tools gain an "include indirect" flag |
| true value-flow / points-to | precise edges per call site | substantial — needs a real analyzer (clang static analyzer, libclang AST walk), out of scope for an LSP-driven indexer |

Tier 2 is the sweet spot. It would let an AI agent traverse `entrypoint → dispatch → square` even when `square` is registered into a dispatch table elsewhere, at the cost of false-positive edges between dispatchers and any same-typed address-taken function. The schema migration is a single `edge_kind TEXT` column on `call_edges` plus an index; `get_symbol` / `search_symbol` would learn an `include_indirect` option so the default stays conservative.

### Cache invalidation on header edits

The per-file cache's documented blind spot (§7.2): editing a shared header is invisible to its `(content, command)` key, so cached TUs serve stale results. The architectural answer is manual nuke. A modest improvement would be to extend the key with a digest of *each TU's actual transitive header set*, computed once at extract time via `clang -MM` or by capturing `textDocument/documentLink` from clangd. Trade-off: this reimplements (poorly) what clangd's background-index dependency tracking already does internally; the architecture (§7.2) explicitly chose not to.

## Status

Early. The fixture-based integration tests pass against clangd 19; the architecture document calls out the policy around clangd version pinning (§6.4) and the known caveats around the per-file cache and indirect-call resolution.

Contributions and bug reports welcome. See [`clang-index-architecture.md`](clang-index-architecture.md) for the authoritative design and [`CLAUDE.md`](CLAUDE.md) for AI-agent contributor notes (hard-won invariants, what *not* to do).

## License

TBD.
