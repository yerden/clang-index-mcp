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
- `get_symbol(id)` — fetch a symbol with its direct callers and callees.

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

## Status

Early. The fixture-based integration tests pass against clangd 19; the architecture document calls out the policy around clangd version pinning (§6.4) and the known caveats around the per-file cache and indirect-call resolution.

Contributions and bug reports welcome. See [`clang-index-architecture.md`](clang-index-architecture.md) for the authoritative design and [`CLAUDE.md`](CLAUDE.md) for AI-agent contributor notes (hard-won invariants, what *not* to do).

## License

TBD.
