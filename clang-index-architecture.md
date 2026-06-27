# Clang Index + MCP Architecture — v1.0

**Status:** Finalized. Captures the architecture as discussed and agreed.
Implementation can proceed against this document. The one remaining open
item (§11) is an implementation detail, not a blocking architectural
question, and can be resolved during fixture-building without revisiting
anything above it.

## 1. Goal

Extract symbol and call-graph information from a C/C++ codebase (general
case: a project with dependencies on other packages, not tied to any
specific one) via clangd over LSP, store it in SQLite, and serve it to an
AI assistant over MCP — in two distinct operating modes with different
freshness/cost tradeoffs. The system is agnostic of version control; there
is no assumption that a "commit" or "clean tree" concept exists.

## 2. Two modes

### Static (frozen, content-keyed)
A SQLite database built once for a given input state (compdb + source
content), treated as an immutable artifact. No clangd process exists at
serve time. Read-only.

### Dynamic (live daemon)
A long-running process managing a real clangd instance bound to a specific
working directory and compile_commands.json, rebuilding the index whenever
compile_commands.json itself changes (debounced — see §6.1). Source-file
edits are not watched directly; the expected workflow is that the build
system regenerates compile_commands.json as part of its own cycle and the
daemon picks that up. clangd's background indexer still reacts to
individual file edits internally, but the SQLite snapshot served over MCP
is only refreshed on a compdb event.

These modes are deliberately not unified into one process. The static
mode's value is reproducibility and cheapness; the dynamic mode's value is
tracking a live tree without manual rebuild steps between compdb
regenerations. Forcing them into one long-running process would blur both
guarantees.

## 3. Binaries

Two binaries, not three — `serve` is a subcommand of `clang-index`, not a
separate program, since it shares everything but the entrypoint with
`build`.

| Binary | Subcommands | Lifecycle | Runs where |
|---|---|---|---|
| `clang-index` | `build` — produces `index.db` from a compdb+source snapshot | one-shot, exits | Docker / CI (recommended, not mandatory — see §9) |
| | `serve` — serves a frozen `index.db` over MCP | long-running, read-only | Docker / cloud (e.g. Fly.io) |
| `clangd-mcp-daemon` | — | long-running, watches compdb, owns a live clangd | **native on dev host** (see §5.3) |

## 4. Package layout (standard Go layout)

```
cmd/
  clang-index/
    main.go          # dispatches to build/serve subcommands
  clangd-mcp-daemon/
    main.go
internal/
  lsp/                # generic LSP client: framing, initialize, request/response correlation
  clangdproc/         # spawn/stop clangd, wait for index-settle; daemon adds restart-on-watch on top
  extract/            # walks compdb files via an lsp.Client -> []Symbol, []Edge (pure, no lifecycle knowledge)
  store/
    schema.sql        # embedded via //go:embed
    queries.sql        # embedded via //go:embed
    store.go           # WriteIndex, swap-on-rebuild, query helpers
  cache/              # content-digest cache, used at two granularities — see §7
  mcp/                # tool registration (search_symbol, get_symbol), transport setup
testdata/
  fixture/            # small fixture C project for integration/system tests
```

`store`'s SQL lives in plain `.sql` files, embedded into the binary with
`//go:embed`, rather than as Go string literals — keeps schema/query text
diffable and syntax-highlightable independent of the Go source around it.

Dependency summary:
- `clang-index build` and the daemon share the full pipeline (`lsp`,
  `clangdproc`, `extract`, `store`, `cache`).
- `clang-index serve` depends only on `store`'s read path — it never
  touches clangd.
- `cache` sits in front of `build` (whole-build dedup) and inside `extract`
  (per-file dedup) — see §7. The daemon doesn't use the whole-build layer,
  since its purpose is reflecting state that was never snapshotted anywhere.

## 5. compile_commands.json and path handling

The JSON Compilation Database spec requires `directory` to be an absolute
path, and build systems frequently bake absolute paths into `-I`/`-D` flags
and sometimes `file` itself. Two different problems follow, with two
different fixes:

### 5.1 Build-time resolution (CI / `clang-index build`)
Don't rewrite compdb paths to be relative — fragile, and fights the spec's
own anchor mechanism. Instead, always build inside a container with the
project mounted at the same fixed canonical path (e.g. `/workspace`)
regardless of host location, and regenerate compile_commands.json fresh
inside that container as part of the build step. There is never a
relocation problem because nothing actually moves from clangd's point of
view.

### 5.2 Artifact portability (the stored DB)
The DB itself should not store build-time absolute paths. The `file`
column in `symbols` should be stored relative to a configurable
`ProjectRoot`, so the frozen artifact can be built in one environment and
served later from a different path, as long as `serve` is given the
current project root for resolving relative paths if needed.

### 5.3 Why the dynamic daemon runs natively, not in Docker
**Reason: toolchain/header parity with the host build**, not filesystem
events. A Linux container bind-mounting the project sees host file changes
in real time via the same kernel inotify subsystem as native — that's not
the deciding factor. The deciding factor is that clangd must resolve the
exact same system/kernel headers and library paths that the host's real
build used to generate compile_commands.json. Running clangd in a
container with a different header layout than the host risks parse
failures or silently degraded extraction, independent of any path-mapping
issue. Native execution guarantees parity for free. (Secondary factors:
compdb's absolute `directory` field is trivially correct natively, with no
mount-path bookkeeping; Docker filesystem-event delivery is only as
reliable as it is on Linux because containers share the host kernel
directly — that doesn't hold on Docker Desktop / macOS / Windows.)

## 6. clangd lifecycle policy

### 6.1 Restart over notify
When compile_commands.json changes, kill and respawn clangd rather than
implementing `workspace/didChangeWatchedFiles` correctly. The notification
path requires the daemon to act as a full LSP client and clangd's exact
behavior afterward isn't reliably documented and is version-coupled. Since
the daemon always knows exactly when it triggered a compdb regeneration, a
clean restart removes the ambiguity at the cost of reindex time, which is
acceptable for a non-interactive backend. Restart is debounced (e.g. 5s) to
coalesce rapid successive writes to the file.

### 6.2 Persisted background index
clangd's `--background-index` flag is on by default. clangd writes shards
to `<compdb-dir>/.cache/clangd/index/` automatically — the path is
derived from the project root and there is no clangd flag to relocate it
(do not invent one: `--background-index-path` is not a real clangd flag
and the process exits on unknown args). On restart, clangd reuses those
shards keyed by per-file content+command digest, so only changed or new
files get re-indexed — most restarts become warm starts. A global
compile-flag change still invalidates broadly, since the digest includes
the command.

This applies equally to both restart triggers the daemon has: a
compdb-change-driven restart (§6.1) and a full daemon-process restart
(redeploy, crash recovery). Neither is special-cased — both just spawn a
fresh clangd against the same shard directory. The one precondition for
either to actually be warm: that directory must survive the restart
(in CI: cache `<compdb-dir>/.cache/clangd/index/` between runs; in
production: don't host it on container-ephemeral storage). If it's ever
wiped, both restart paths degrade to a cold start regardless of which
one triggered it.

Same mechanism applies to `clang-index build` — clangd persists shards
identically there. A first build is cold; subsequent builds against the
same project directory warm-start automatically.

### 6.3 Index growth and cleanup
No confirmed automatic garbage collection for shards belonging to
deleted/renamed files. No eviction policy is being designed — per
decision, manual nuke-and-rebuild is the accepted fallback whenever
staleness or disk growth is suspected. Applies uniformly to: clangd's own
background-index cache, the whole-build cache, and the per-file extraction
cache (§7).

### 6.4 Version pinning
clangd's behavior — especially §6.1/6.2 — is version-coupled and not
always documented precisely. Pin a specific release:

- **Docker (CI builds):** download a pinned release binary directly from
  clangd's GitHub releases in the Dockerfile, keyed off a build arg /
  `versions.env` entry, rather than relying on a distro package.
- **Native (daemon):** same pinned version, installed by a setup script;
  assert it at startup (`clangd --version` compared against the expected
  constant) and fail loudly on mismatch.
- **On deliberate version bumps:** rerun the integration test tier and
  re-verify §6.1/6.2 specifically, since they're the most likely to shift
  silently across releases.

#### 6.4.1 Disable clang-tidy in clangd
`extract.Run` calls `textDocument/documentLink` for every opened TU to
discover `#include` targets for typedef collection (§6.5.1). clangd
processes `DocumentLinks` via its AST worker, which also runs any
enabled clang-tidy matchers. A crashing checker kills the clangd
process mid-extraction; the daemon auto-restarts (up to 3 times), but
the same file will crash clangd every time if the checker is still
active.

clangd's indexer is used here purely for symbol/AST queries — not for
diagnostics. Disable clang-tidy in your `~/.clangd` (user-wide) or in
the project's `.clangd`:

```yaml
Diagnostics:
  ClangTidy:
    Remove: ["*"]
```

This suppresses all clang-tidy checks globally for clangd without
affecting your editor's or CI's own clang-tidy runs.

#### 6.4.2 Throughput knobs (build hosts vs. shared dev machines)
clangd's defaults are tuned for an interactive IDE sharing a machine
with the developer: the background-index worker pool sizes to
`llvm::heavyweight_hardware_concurrency()` (≈ half the logical cores),
and the indexer threads run at the OS's lowest scheduling/I/O priority
("background" — Linux: nice 19 + idle I/O). Both make sense when a
human is typing in the same process and the indexer must not steal
cycles. On a dedicated build host neither does.

Two flags are exposed on both `clang-index build` and
`clangd-mcp-daemon`, and threaded into `clangdproc.Options`:

- `--clangd-jobs N` → `-j=N`; overrides the worker-pool heuristic.
- `--clangd-boost` → `--background-index-priority=normal`; lifts the
  indexer to foreground scheduling priority.

The boost flag is typically the larger win — the throttled priority
caps wall-clock throughput regardless of how many workers you allocate.
On a dedicated CI box, both together (`--clangd-jobs=$(nproc)
--clangd-boost`) is the default to reach for. Note that more
concurrency increases disk I/O against the shard directory
(`<compdb-dir>/.cache/clangd/index/`); on slow storage that becomes
the binding constraint before CPU does.

### 6.5 Function-pointer dispatch: facts, not synthesized edges
clangd's `callHierarchy/outgoingCalls` only resolves direct calls — it
stops dead at every function-pointer dispatcher (`fn(x)` inside a
function that takes `op_t fn` as a parameter). A previous attempt
(Tier 2, reverted at `1ec7c64`) tried to close the gap with sound
over-approximation: for every indirect call site of type T, synthesize
edges to every address-taken function of matching T. That was
syntactically correct but practically too noisy — typedef-shape
sharing alone over-connects dozens of unrelated callbacks in any
real codebase.

The current approach surfaces **raw facts** instead and lets the MCP
consumer (typically an AI agent) decide how to bridge the gap. The
agent has contextual knowledge — naming conventions, header
membership, registry conventions — that a static synthesis rule
cannot embed.

Two tables hold the facts:

- `address_takes(function_id, taken_at_file, taken_at_line,
  fn_ptr_type, category, context_detail)` — one row per use of a
  function's address.
- `indirect_call_sites(caller_id, file, line, callee_type,
  callee_expr)` — one row per CallExpr whose callee isn't a direct
  function reference.

Extraction runs over clangd's `textDocument/ast` (clangd 15+
extension; degrades to "Tier 2 disabled, both tables empty" on older
builds). The walker:

1. Detects each `DeclRefExpr → Function` and classifies it by the
   precedence rule below. Direct callees (child[0] of an enclosing
   `CallExpr` after peeling cast/paren wrappers) are SKIPPED — they
   are not address-takes.
2. Detects each `CallExpr` whose callee isn't a direct
   `DeclRefExpr → Function` and records it as an indirect call site
   with the callee expression's static type (canonical form after
   typedef expansion) and a short textual representation
   (`fn` / `ops[i]` / `<base>.cb` / `<expr>`).

**Category precedence (the load-bearing contract).** When multiple
patterns apply to one address-take, the highest-precedence one wins.
The agent receives the already-resolved value and must NOT re-derive:

| rank | category | example | note |
|---|---|---|---|
| 1 | `compared` | `if (fn == square)`, `assert(fn != null_op)` | Negative signal — not invoking, just testing. Always exclude when looking for dispatchers. |
| 2 | `arg_to:F#i` | `register_handler(square)` → `arg_to:register_handler#0` | Strongest dispatcher signal. |
| 3 | `stored_in:T.f` | `ops.cb = square` → `stored_in:struct_ops.cb` | Registry pattern. |
| 4 | `array_init:N[i?]` | `static op_t ops[] = {square}` → `array_init:ops[0]` | Dispatch table pattern. |
| 5 | `assigned_to:v` | `op_t fn = square` → `assigned_to:fn` | Weaker; local flow. |
| 6 | `returned_from:F` | `return square;` inside `pick_op` → `returned_from:pick_op` | Factory pattern. |
| 7 | `other` | `(void*)square`, hash keys, debug uses | Not a dispatcher signal. |

The canonical, agent-facing prose form of this table lives in
`internal/extract/categories.go` as
`AddressTakeCategoryVocabulary`; the `describe_address_take_categories`
MCP tool returns it (and a structured form) verbatim. The vocabulary
is a public contract — adding categories is safe, renaming or
reordering is not.

**Implementation notes worth carrying.** Walker state is a stack of
frames; each frame remembers its child index in its parent so the
classifier can read `inCalleeSubtree` without separate AST passes.
After visiting child[0] of a `BinaryOperator` or `CallExpr`, the
parent frame is populated with a `siblingHint` / `calleeName` so the
later children's classification can read context that's already
beneath us in the tree. This turns the walker into a single-pass
visitor with O(depth) extra state.

Failure modes are non-fatal in the same way the symbol/edge pipeline
is: per-TU output (including `address_takes` and
`indirect_call_sites`) is cached under the same per-file key, so
warm rebuilds reuse them when the TU didn't change. Per architecture
§6.3 the cache must be nuked after extraction-shape changes (e.g.
adding a new category).

#### 6.5.1 Three gotchas that broke the documented workflow in practice

Three correctness gaps surfaced when this index was used to trace
real DPDK-style dispatch. The fixes each cross a layer boundary
that's easy to revert by accident.

**Typedef canonicalization across the join.** clangd's
`textDocument/ast` returns the AST of the requested TU *without*
inlining `#include`d headers, so TypedefDecls defined in headers are
invisible to a per-TU walker. Address-takes (rooted at a DeclRef to a
Function whose type is the bare function signature) end up canonical
on their own; indirect-call sites (rooted at an ImplicitCast to a
field's static type) carry the typedef-spelled form when the typedef
is nested inside a pointer (e.g. `lcore_function_t *`). The two
columns disagree and the documented "find dispatcher by callee_type /
enumerate candidates by type" workflow silently breaks.

Fix: `extract.Run` uses `textDocument/documentLink` to discover the
`#include` targets of every opened TU, opens each one, and walks the
union of opened-file ASTs to build a shared typedef table. The
per-TU walker is seeded with that table, so both the address-take
and indirect-call-site paths canonicalize against the same source of
truth. The typedef substitution is whole-word; trailing `*`s next to
a function-type body are reshaped into `(*)` so `int (void *) *` →
`int (*)(void *)`.

**Designated-initializer field names.** clangd's AST DesignatedInit
node carries neither the field name in `Detail` nor in `Arcana` —
the field designator is dropped before serialization. Without
recovery the walker falls back to `<struct>.<init>`, which collapses
every field-keyed registration of the same struct into one opaque
row and makes the address-take → indirect-call join by field name
impossible.

Fix: when the classifier walks up the stack and hits a
`DesignatedInit` ancestor, it slices the TU source at the node's
range start (which clang places at the `.`) and reads the
identifier. The enclosing aggregate type comes from the next-up
VarDecl. This is the one place the walker depends on raw source
text — worth carrying.

**Querying indirect_call_sites by which field is dispatched.**
`callee_expr` like `<base>.cb` is descriptive but only useful if
queryable. `get_indirect_call_sites` accepts a `callee_expr_pattern`
SQL LIKE filter; for member-access dispatch sites the
agent-friendly form is `%.<field>` (any base, specific field). The
canonical reverse-traversal recipe is now:

  1. `get_address_take_sites(callback_id)` — find the registration
     site, including the `stored_in:<struct>.<field>` row.
  2. `get_indirect_call_sites(type=canonical_fn_ptr_type,
     callee_expr_pattern="%.<field>")` — narrow to dispatchers
     reading that field, dropping noise from same-typed but
     unrelated callbacks.

Without the field filter, a permissive type-only query returns
every same-typed dispatch site in the project, which is how the
agent that filed the second gap report constructed a false dispatch
chain through an unrelated symbol.

## 7. Caching — content-digest keyed, no VCS dependency

Two granularities of the same idea, both keyed purely on content/command
digests rather than any source-control identity:

### 7.1 Whole-build cache (`cache` + `clang-index build`)
Before running the full clangd pipeline, check whether a digest (raw bytes,
no normalization — same convention as §7.2) of the current input state
(compdb content + referenced file contents) already
has a corresponding `index.db`. On hit, skip clangd/extraction entirely and
reuse it — useful for repeated builds against an unchanged input snapshot.
On miss, run the pipeline and store the result under that digest.

### 7.2 Per-file extraction cache (`cache` used inside `extract`)
Within a single build that does run, cache extraction results per file
keyed on `(file content digest, compile command digest)` — both digests
computed over raw bytes, no normalization. Skip re-querying
clangd for any file whose key is already cached; always assemble and write
a complete fresh DB from cached+fresh results combined — the DB write
itself stays a full rebuild regardless of which files were skipped, so the
dangling-edge/stale-row problems that incremental *DB* mutation would cause
never come up.

**Caveat (the one that matters):** this key captures a file's own content
and flags, not the content of headers it transitively includes. Editing a
shared header won't be detected by this key for the TUs that include it,
so those TUs can be served from stale cached results. Properly closing that
gap means tracking each TU's actual header dependency set and folding a
digest of it into the key — real complexity, effectively reimplementing
what clangd's own background-index dependency tracking already does
internally. Not adopting that; the accepted tradeoff is: per-file-only key,
and rely on the manual nuke (§6.3) for correctness after header-level
changes or whenever staleness is suspected.

Eviction for both layers: none designed. Manual nuke-and-rebuild only.

## 8. SQLite store

### 8.1 Schema (initial, `internal/store/schema.sql`, embedded via go:embed)
```sql
CREATE TABLE symbols (
  id        INTEGER PRIMARY KEY,
  usr       TEXT UNIQUE,
  name      TEXT,
  kind      TEXT,
  file      TEXT,   -- relative to ProjectRoot, see §5.2
  line      INTEGER,
  signature TEXT
);

CREATE VIRTUAL TABLE symbols_fts USING fts5(
  name, signature, content='symbols', content_rowid='id',
  tokenize='unicode61 separators _'
);

CREATE TABLE call_edges (
  caller_id INTEGER REFERENCES symbols(id),
  callee_id INTEGER REFERENCES symbols(id)
);
CREATE INDEX idx_caller ON call_edges(caller_id);
CREATE INDEX idx_callee ON call_edges(callee_id);
```

### 8.2 Rebuild, not migrate
Each extraction run produces a fresh database rather than mutating an
existing one. Consistent across `clang-index build` and the daemon's
swap-on-rebuild cycle.

### 8.3 Swap-on-rebuild (daemon only)
Extraction writes to a new file, then the daemon atomically swaps its live
`*sql.DB` handle to point at it and closes the old one. MCP reads never
observe a partially-rebuilt database.

## 9. MCP serving

Both `clang-index serve` and the daemon expose the same tool surface,
backed by `store`'s read path:

- `search_symbol(query)` → FTS5 match over `symbols`
- `get_symbol(id)` → definition + direct callers + direct callees

Transports: stdio and SSE run concurrently against the same tool registry
and the same `Store`, so local CLI access and remote/mobile access (SSE,
e.g. over Fly.io) are available from one process without duplicating tool
logic per transport.

## 10. Testing strategy

Three tiers. Docker is recommended for CI reproducibility (pinned clangd
version, isolated fixture) but is no longer mandatory for every tier —
non-destructive tests may run against clangd installed locally on the host.

| Tier | Covers | Needs clangd? | Where it may run |
|---|---|---|---|
| Unit | `lsp` framing, `store`, MCP tool handlers | No | anywhere, every `go test` |
| Integration | `extract`, `clangdproc` against a small fixture project | Yes (pinned version recommended) | Docker (CI) or locally on host, as long as the test only touches `testdata/fixture/` and never the real project |
| System | full binary lifecycle: `clang-index build` golden-file diff, daemon watch→restart, `clang-index serve` end-to-end query | Yes | Docker (build/serve); natively on host for the daemon, matching its actual deployment environment (§5.3) — resolves the earlier tension between "Docker-only tests" and "daemon runs natively," since Docker is no longer an absolute requirement |

Fixture project (`testdata/fixture/`) — case list still being expanded,
see §11.

## 11. Open questions

1. **Fixture project case list** — confirmed so far: cross-TU USR dedup via
   a shared header; a function-pointer dispatch to assert the known
   callHierarchy gap stays absent; fan-in (multiple TUs calling the same
   function); and recursive/cyclic call chains (a function that calls
   itself, and/or a longer cycle A→B→A) to verify `call_edges` represents
   loops correctly rather than something that assumes a DAG. Still
   undecided: a multi-hop chain several frames deep distinct from a cycle,
   and an unresolved/external symbol (declared via an included third-party
   header but not defined in the fixture).
