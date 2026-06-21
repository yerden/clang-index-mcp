# clang-index-mcp

Symbol and call-graph index for C/C++ projects, extracted with [clangd](https://clangd.llvm.org/), stored in SQLite, and served to AI assistants over [MCP](https://modelcontextprotocol.io/).

Two operating modes with different freshness/cost tradeoffs:

| Mode | What it is | When to use it |
|---|---|---|
| **Static** (`clang-index build` + `serve`) | Build an `index.db` once, serve it later. Read-only, no clangd at serve time. | CI pipelines, cloud-hosted indexes (Fly.io etc.), any case where the codebase is frozen at index time. |
| **Dynamic** (`clangd-mcp-daemon`) | Long-running daemon that owns a live clangd and rebuilds the index as the tree changes. Reflects in-progress edits. | Local dev — same machine as your editor. |

Both expose the same MCP tools, so assistants don't need to know which mode is backing them.

## MCP tools

- `search_symbol(query, limit?)` — FTS5 full-text search over symbol name and signature.
- `get_symbol(id)` — fetch a symbol with its direct callers and callees. Each returned caller/callee carries an `EdgeKind` tag: `"direct"` (clangd-confirmed direct call) or `"indirect"` (synthesized function-pointer candidate — see below).
- `list_symbols_in_file(file, limit?)` — list every symbol declared *or* defined in a file. Useful for "what's the public surface of `foo.h`?" — matches either the declaration file (typically the header) or the definition file (typically the `.c`).

Symbol records carry both a definition location (`File`/`Line`) and a declaration location (`DeclFile`/`DeclLine`). For static / file-local symbols, declaration and definition coincide and `DeclFile` is empty.

### Function-pointer / indirect-call edges

`callHierarchy/outgoingCalls` only resolves direct calls — it stops dead at every function-pointer dispatcher (`fn(x)`). The extractor runs a separate AST pass over `textDocument/ast` (clangd 15+) to close the gap:

- For each indirect call site `(G, T)` — a `CallExpr` whose callee isn't a direct `DeclRefExpr` — record the enclosing function `G` and the callee type `T`.
- For each address-taken function `(F, T_F)` — a `DeclRefExpr` to a function outside any direct-call callee slot — record the function `F` and the function-pointer type.
- Synthesize `G --indirect--> F` for every pair where `T == T_F`.

The result is a sound over-approximation: if a dispatcher takes a function pointer of type `int (*)(int)`, *every* function with a matching signature whose address is taken anywhere in the project gets an indirect edge from that dispatcher. AI agents can choose to traverse only direct edges (precise but blind to dispatchers) or include indirect ones (complete but noisier). See [architecture §6.5](clang-index-architecture.md#65-function-pointer--indirect-call-edges-tier-2) for the design and what's intentionally left out (per-call-site value-flow).

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
  --background-index-path ~/.cache/clang-index-mcp/bgindex \
  --http :8080
```

The daemon watches `compile_commands.json`. When it changes, clangd is restarted (debounced 5 s) and the index is rebuilt. The live `*sql.DB` handle is then atomically swapped — in-flight MCP queries never see a half-built database.

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

`clang-index build` is disposable by default — every run cold-starts clangd and re-extracts every TU. Two opt-in caches change that:

```sh
# Whole-build cache: if (compdb + every file's content) is unchanged,
# skip clangd entirely and copy the cached index.db.
clang-index build --compdb ... --out index.db --cache ~/.cache/clang-index-mcp/wb

# Per-file cache: only TUs whose (content + compile command) changed
# get re-extracted. Useful for incremental local builds.
clang-index build --compdb ... --out index.db --per-file-cache ~/.cache/clang-index-mcp/pf
```

Both caches are keyed on raw-bytes digests — no normalization, no VCS dependency. The known limitation: per-file keys don't include transitively-included header content, so editing a shared header is invisible to the cache. Workaround: nuke the cache directory. See [architecture §7](clang-index-architecture.md#7-caching--content-digest-keyed-no-vcs-dependency).

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

## Possible directions

Sketches, not commitments — captured here so the design tradeoffs aren't re-litigated each time the topic comes up.

### Source-tree file watching

Today the daemon only watches `compile_commands.json`; source edits don't refresh the served DB until the next compdb event. A natural extension is to add a second fsnotify watcher over the project tree — every modify/create/delete on a `.c`/`.cpp`/`.h` would pulse the same debounced `Daemon.Restart()` we already use for compdb changes. Per-file extraction cache and clangd's `--background-index-path` shards both ensure unchanged TUs aren't re-extracted, so a single source edit costs one TU's worth of clangd work, not a full reindex. Trade-off: header edits invalidate broadly (the per-file cache key intentionally doesn't track transitive includes, §7.2), and editor save-storms during refactors would cause restart churn; the existing 5 s debounce (§6.1) is the lever to tune. Status quo workaround: re-run the build system to regenerate `compile_commands.json`.

### Cache invalidation on header edits

The per-file cache's documented blind spot (§7.2): editing a shared header is invisible to its `(content, command)` key, so cached TUs serve stale results. The architectural answer is manual nuke. A modest improvement would be to extend the key with a digest of *each TU's actual transitive header set*, computed once at extract time via `clang -MM` or by capturing `textDocument/documentLink` from clangd. Trade-off: this reimplements (poorly) what clangd's background-index dependency tracking already does internally; the architecture (§7.2) explicitly chose not to.

## Status

Early. The fixture-based integration tests pass against clangd 19; the architecture document calls out the policy around clangd version pinning (§6.4) and the known caveats around the per-file cache and indirect-call resolution.

Contributions and bug reports welcome. See [`clang-index-architecture.md`](clang-index-architecture.md) for the authoritative design and [`CLAUDE.md`](CLAUDE.md) for AI-agent contributor notes (hard-won invariants, what *not* to do).

## License

TBD.
