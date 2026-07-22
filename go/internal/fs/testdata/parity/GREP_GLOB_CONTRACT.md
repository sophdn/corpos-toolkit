# fs.grep / fs.glob — search contract

The behavioral contract `fs.grep` and `fs.glob` implement, pinned by the
characterization nets in `../../grep_test.go` and `../../glob_test.go` over the
shared fixture tree in `tree/` (which carries the `needle`/`NEEDLE` tokens
across `big.txt`, `readme.md`, and `sub/widget.go`). These are our owned
specifications restated in our own terms; they are the parity floor that gates
the harness search-tool deny-list swap.

Both ride **one engine — ripgrep** — the same engine the harness search tool
wraps. Riding a single engine for both content search and file globbing is an
owned simplification (the harness uses a separate file-glob engine); the
observable contracts below are what we pin, not the engine internals.

---

## fs.grep — content search (ripgrep wrapper)

### Params

- `pattern` (required) — the regular expression. A pattern beginning with `-` is
  passed as an explicit pattern (so it is not parsed as a flag).
- `path` — file or directory to search; defaults to the working directory. The
  search root; results are reported **relative to it**.
- `glob` — glob filter on filenames (e.g. `*.go`, `*.{ts,tsx}`); whitespace- and
  comma-separated, brace groups preserved.
- `type` — ripgrep file type (e.g. `go`, `py`, `rust`).
- `output_mode` — `content` | `files_with_matches` | `count`. Default
  `files_with_matches`.
- `-B` / `-A` / `-C` (and `context`) — context lines before / after / both;
  honored only in `content` mode. `-C`/`context` takes precedence over `-B`/`-A`.
- `-n` — show line numbers; `content` mode only; default **true**.
- `-i` — case-insensitive; default false.
- `head_limit` — cap output to the first N entries (lines / files / counts).
  Default **250**; `0` means unlimited.
- `offset` — skip the first N entries before applying `head_limit`. Default 0.
- `multiline` — `.` matches newlines and patterns may span lines. Default false.

### Behavior (ported)

- Always hidden-aware (`--hidden`); version-control metadata dirs
  (`.git`, `.svn`, `.hg`, `.bzr`, `.jj`, `.sl`) are excluded; lines are clamped
  to 500 columns to keep minified/base64 content from flooding output.
- `multiline` ⇒ multiline + dotall; `-i` ⇒ case-insensitive; `files_with_matches`
  ⇒ list paths; `count` ⇒ per-file counts; `content` + `-n` ⇒ line numbers.
- `head_limit`/`offset` page the result set: `appliedLimit` is reported **only
  when truncation actually occurred** (so the caller knows to page);
  `appliedOffset` is reported only when a non-zero offset was applied.
- Paths are reported relative to the search root.
- In `content` mode the filename is always emitted (`path:line:text` or, with
  line numbers off, `path:text`), so a single-file search stays addressable —
  ripgrep otherwise omits the filename when given one file. (Owned addressability
  choice; documented, not faithful to bare ripgrep single-file output.)
- `files_with_matches` is sorted by modification time, newest first, with the
  path as a tiebreaker.

### Result (`GrepResult`)

`mode`, `num_files`, `filenames` (files_with_matches), `content` (content/count
text), `num_lines` (content), `num_matches` (count total), and the optional
`applied_limit` / `applied_offset` paging markers.

### Model-agnostic / harness carve-outs (dropped)

- A specific harness's plugin-cache exclusions and its permission-context ignore
  patterns (those are harness state, not search behavior).
- A test-only deterministic-sort hook keyed on an env var (the owned net controls
  ordering through fixtures instead).

---

## fs.glob — filename pattern matching (ripgrep `--files -g`)

### Params

- `pattern` (required) — the glob (ripgrep glob dialect: `**` spans directories,
  `*`/`?` within a segment, `{a,b}` groups). e.g. `**/*.go`, `src/**/*.ts`.
- `path` — directory to search in; defaults to the working directory.

### Behavior

- Enumerates files under `path` matching `pattern`, hidden-aware, excluding the
  same version-control metadata dirs as grep, honoring `.gitignore`.
- Results are sorted by modification time, **newest first** (path tiebreak), and
  capped at **100** files; `truncated` is true when the cap elided matches.
- Paths are reported relative to the search root.

> Glob rides ripgrep's glob dialect rather than a shell/`fast-glob` dialect. This
> is the deliberate owned contract (one engine for grep + glob); `**/*.ext` is
> the recursive form, and a bare `*.ext` matches at any depth (ripgrep glob
> semantics), which differs from a shell `*.ext`. Documented, not faithful to a
> shell glob.

### Result (`GlobResult`)

`filenames`, `num_files`, `truncated`, `duration_ms`.
