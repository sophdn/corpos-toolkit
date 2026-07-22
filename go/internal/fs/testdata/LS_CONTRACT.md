# fs.ls — directory listing contract (self-defined)

`fs.ls` is **self-defined**, not parity-matched: the harness has no LS tool
(directory listing there is done via Bash/Glob), so there is no source oracle to
characterize. Its contract is designed from first principles and pinned by the
net in `../ls_test.go`.

## Purpose

List the immediate entries of a directory as structured rows, so a model can
orient in a directory without shelling out and without the token cost of a
recursive glob.

## Params

- `path` — directory to list; defaults to the working directory.
- `all` — include entries whose name begins with `.` (default false).

## Behavior

- Lists the **immediate** children of `path` (non-recursive), sorted by name
  (case-sensitive byte order).
- Each entry reports `name`, `type` (`dir` | `file` | `symlink`), and `size`
  (bytes; `0` for directories).
- Dotfiles are omitted unless `all` is set.

## Result (`LSResult`)

`path` (the listed directory), `entries` (the rows), `count` (number of rows).

## Errors

- `path` missing/empty ⇒ defaults to the working directory (not an error).
- `path` does not exist ⇒ error.
- `path` is a file, not a directory ⇒ error.
