# fs.read — read contract

The behavioral contract `fs.read` implements. It is pinned by the
characterization net in `../../read_test.go` and the golden assertions over the
fixtures in `tree/`. This is our owned specification, restated in our own terms;
it is the parity floor that gates the read/write/edit deny-list swap. (How the
contract was originally established is recorded in the chain history, not here.)

The fixture tree under `tree/` is shared with the future grep/glob nets (it
carries the `needle` token across `big.txt`, `readme.md`, and `sub/*.go`).

---

## Numbered-line format

Strip a leading UTF-8 BOM; split the content on `"\n"`; strip a trailing `"\r"`
per line; select the range `[offset-1, offset-1+limit)`; number each selected
part 1-based from `offset` as `"<n>\t<content>"` with an **UNPADDED** line
number; join with `"\n"` and **no trailing newline**. The final fragment after
the last `"\n"` is always counted, so a file ending in `"\n"` yields a trailing
empty numbered line.

This is deliberately **not** `cat -n`: line numbers are unpadded (not 6-wide
right-justified), and a file ending in `\n` produces a trailing empty line.

| input file (bytes)         | fs.read Content                     | note                                   |
|----------------------------|-------------------------------------|----------------------------------------|
| `alpha\nbeta\ngamma\n`     | `1\talpha\n2\tbeta\n3\tgamma\n4\t`  | trailing `\n` ⇒ empty line 4; no final `\n` |
| `one\ntwo` (no final `\n`) | `1\tone\n2\ttwo`                    | no trailing empty line                 |
| BOM + `a\r\nb\r\n`         | `1\ta\n2\tb\n3\t`                   | BOM stripped, CRLF→LF                   |

## Params

- `file_path` (required).
- `offset` — 1-based start line, default 1; values `< 1` normalize to 1;
  line numbering starts at `offset`.
- `limit` — maximum lines; omit (or `<= 0`) to read the whole file.

## Caps & warnings

- **Byte cap:** a whole-file read (`limit` omitted) of a file larger than 256 KB
  fails with `file content (<size>) exceeds maximum allowed size (256KB). Use
  offset and limit parameters ...`. A ranged read (`limit` set) bypasses the cap.
- **Empty output:** offset past EOF, or an empty file, returns no content; the
  result carries a `warning` system-reminder verbatim
  (`...shorter than the provided offset (<offset>). The file has <n> lines.` or
  `...contents are empty.`) rather than erroring.

## Deliberately out of contract

- A model-coupled token cap — this surface is model-agnostic, so the byte cap is
  the only size guard.
- Per-line length — capped at 60000 chars as a context-flood guard.
