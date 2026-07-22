package fs

// action_doc.go is the descriptor-registry seam for the fs surface's action
// docs (the per-surface instantiation of the contract established on work,
// docs/ACTION_DOC_CONTRACT.md). It is the single source of the fs surface's
// action docs: each param's TYPE is DERIVED from the handler's typed param
// struct (ParamStruct), and only the irreducible semantics (purpose, param
// name-list/order/required/description, errors, notes, returns) are authored in
// a co-located Go descriptor.
//
// The generated corpus (corpus/fs/*.toml) + admin.action_describe(fs, X) derive
// from this registry via FsActionSpecs(); byte-parity is pinned by the
// characterization net (internal/actiondocs/surface_contract_net_test.go). The
// surface-wide _general.toml chunk stays hand-authored cross-cutting prose (the
// corpus generator exempts every <surface>/_general.toml).

import (
	"reflect"

	"toolkit/internal/actionspec"
)

var readDoc = actionspec.ActionDoc{
	Purpose: "Read a file (optionally a line range) and return its contents as numbered lines. The owned, substrate-native replacement for the harness Read tool; the default output is byte-for-byte faithful to it (predictability is the parity floor that gates the deny-list swap).",
	Params: []actionspec.DocParam{
		{Name: "file_path", Required: true, Description: "Absolute or working-directory-relative path to the file to read."},
		{Name: "path", Required: false, Description: "Alias of file_path (accepted when file_path is absent).", AliasOf: "file_path"},
		{Name: "offset", Required: false, Description: "1-based line number to start reading from (default 1). Values < 1 are normalized to 1. Use with limit to page through a large file."},
		{Name: "limit", Required: false, Description: "Maximum number of lines to return. Omit (or pass <= 0) to read the WHOLE file, which is then subject to the 256 KB byte cap. A ranged read (limit set) bypasses the byte cap."},
		{Name: "outline", Required: false, Description: "OPT-IN mode: return a go/ast structural summary (top-level signatures + first doc line, struct/interface bodies collapsed) instead of the full file — measurably smaller, for orientation. Go source only. Attaches the `outline` view."},
		{Name: "symbol", Required: false, Description: "OPT-IN mode: resolve a single named declaration via go/ast and return just its source span (numbered like a normal read). Accepts a top-level name or `Type.Method`. Go source only. Attaches the `symbol` view."},
		{Name: "provenance", Required: false, Description: "OPT-IN mode: attach intent-annotated mutation history for the read range — git blame over the range (commit subjects are the intent) plus matching substrate events (CommitLanded subjects, owned artifact write/edit rationales). Fail-soft for untracked files. Attaches the `provenance` view."},
		{Name: "oriented", Required: false, Description: "OPT-IN mode: attach the file's package intended-use block (from doc.go) and related knowledge_pointers. Attaches the `oriented` view."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "file_path missing or empty", Message: "fs.read requires file_path"},
		{Condition: "the path does not exist", Message: "fs.read: file does not exist: <path>"},
		{Condition: "the path is a directory", Message: "fs.read: <path> is a directory, not a file"},
		{Condition: "the process lacks read permission", Message: "fs.read: permission denied: <path>"},
		{Condition: "a whole-file read (no limit) of a file larger than 256 KB", Message: "fs.read: file content (<size>) exceeds maximum allowed size (256KB). Use offset and limit parameters ..."},
		{Condition: "outline/symbol mode on a non-Go file", Message: "fs.read: outline mode requires a Go source file (.go): <path>"},
		{Condition: "symbol mode with a name that resolves to no declaration", Message: "fs.read: symbol <name> not found in <path>"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "ReadResult",
		Description: "content (the numbered-line text; empty when no lines are selected), file_path, start_line (the 1-based offset), line_count (lines returned), total_lines (lines in the file), and warning (a system-reminder, set verbatim when content is empty: empty file / offset past EOF). An OPT-IN mode additionally attaches exactly one of outline / symbol / provenance / oriented; all are omitted on a default read.",
	},
	Notes: "CONTRACT (see go/internal/fs/testdata/parity/OBSERVED_HARNESS_CONTRACT.md): the content is split on newline (leading UTF-8 BOM stripped, trailing \\r stripped), each part numbered 1-based as `<n>\\t<content>` with an UNPADDED line number, joined with newline and NO trailing newline; a file ending in a newline gets a trailing empty numbered line. Whole-file reads over 256 KB throw (byte cap); a ranged read bypasses it. A model-coupled token cap is intentionally NOT part of the contract — this surface is model-agnostic and the byte cap is the only size guard. Streams line by line. SUBSTRATE UPGRADES (opt-in, default stays byte-parity): outline (go/ast signatures), symbol (go/ast declaration resolution), provenance (git blame + substrate events), oriented (doc.go intended-use + knowledge_pointers). Each is an explicit opt-in param; a mode read is a partial/derived view and does NOT record full read-state, so it never satisfies the fs.write/fs.edit precondition. No mode changes the default byte output.",
}

var writeDoc = actionspec.ActionDoc{
	Purpose: "Write content to a file, replacing the whole file (creating it and any missing parent directories). The owned replacement for the harness Write tool.",
	Params: []actionspec.DocParam{
		{Name: "file_path", Required: true, Description: "Absolute or working-directory-relative path to write."},
		{Name: "path", Required: false, Description: "Alias of file_path (accepted when file_path is absent).", AliasOf: "file_path"},
		{Name: "content", Required: true, Description: "The full new file content, written verbatim (UTF-8). May be the empty string (writes an empty file)."},
		{Name: "record", Required: false, Description: "OPT-IN provenance mode: emit an ArtifactWritten event for this write so fs.read provenance mode can fold it into the file's mutation history. Default off (no event)."},
		{Name: "intent", Required: false, Description: "Free-text intent recorded as the ArtifactWritten event's rationale (only meaningful with record=true)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "file_path missing or empty", Message: "fs.write requires file_path"},
		{Condition: "the path is a directory", Message: "fs.write: <path> is a directory, not a file"},
		{Condition: "overwriting an existing file that was not fully read first", Message: "fs.write: File has not been read yet. Read it first before writing to it."},
		{Condition: "the existing file changed on disk since it was read", Message: "fs.write: File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."},
		{Condition: "the process lacks permission", Message: "fs.write: permission denied: <path>"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "WriteResult",
		Description: "file_path, created (true when the file did not exist before), bytes_written, line_count, and (record mode only) event — the {event_id, type} receipt of the emitted ArtifactWritten event; omitted on a default write.",
	},
	Notes: "FAMILY PRECONDITION: overwriting an EXISTING file requires a prior full fs.read of it (and no change since) — the read/write/edit family's owned read-state, so the family is self-consistent without any harness read-state. A NEW file needs no prior read. After a successful write the new state is recorded as read, so an immediate follow-up write/edit does not require a re-read. Content is written verbatim UTF-8 with parent directories created. SUBSTRATE UPGRADE (opt-in, default stays a plain write): record mode emits an ArtifactWritten event (intent → event rationale) keyed by the absolute path — the write half of the write->read provenance loop. Emission is fail-open: an event-log error never fails a write that already committed.",
}

var editDoc = actionspec.ActionDoc{
	Purpose: "Replace an exact string in a file. The owned replacement for the harness Edit tool.",
	Params: []actionspec.DocParam{
		{Name: "file_path", Required: true, Description: "Absolute or working-directory-relative path to edit."},
		{Name: "path", Required: false, Description: "Alias of file_path (accepted when file_path is absent).", AliasOf: "file_path"},
		{Name: "old_string", Required: true, Description: "The exact text to replace. Empty string creates a new file (nonexistent path) or fills an empty file from new_string."},
		{Name: "new_string", Required: true, Description: "The replacement text. Must differ from old_string."},
		{Name: "replace_all", Required: false, Description: "Replace every occurrence of old_string (default false → replace the single occurrence; >1 match without this errors)."},
		{Name: "record", Required: false, Description: "OPT-IN provenance mode: emit an ArtifactEdited event for this edit so fs.read provenance mode can fold it into the file's mutation history. Default off (no event)."},
		{Name: "intent", Required: false, Description: "Free-text intent recorded as the ArtifactEdited event's rationale (only meaningful with record=true)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "file_path missing or empty", Message: "fs.edit requires file_path"},
		{Condition: "old_string equals new_string", Message: "fs.edit: No changes to make: old_string and new_string are exactly the same."},
		{Condition: "old_string not present in the file", Message: "fs.edit: String to replace not found in file.\\nString: <old_string>"},
		{Condition: "old_string occurs more than once and replace_all is false", Message: "fs.edit: Found <n> matches of the string to replace, but replace_all is false. ...\\nString: <old_string>"},
		{Condition: "editing an existing file that was not fully read first (or changed since read)", Message: "fs.edit: File has not been read yet. Read it first before writing to it. (or: ... modified since read ...)"},
		{Condition: "non-empty old_string on a nonexistent file", Message: "fs.edit: file does not exist: <path>"},
		{Condition: "empty old_string on a non-empty existing file", Message: "fs.edit: Cannot create new file - file already exists."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "EditResult",
		Description: "file_path, created (true when an empty old_string created a new file), replacements (occurrences replaced), and (record mode only) event — the {event_id, type} receipt of the emitted ArtifactEdited event; omitted on a default edit.",
	},
	Notes: "CONTRACT: the file is read and CRLF normalized to LF before matching; the result is written with LF endings. old_string must occur exactly once unless replace_all is set. FAMILY PRECONDITION: editing an EXISTING file requires a prior full fs.read (and no change since) via the owned read-state — the same coupling as fs.write, so read/write/edit are self-consistent and can be denied together. SUBSTRATE UPGRADE (opt-in, default stays a plain exact-string replace): record mode emits an ArtifactEdited event (intent → event rationale) keyed by the absolute path — the edit half of the write->read provenance loop. Emission is fail-open: an event-log error never fails an edit that already committed.",
}

var moveDoc = actionspec.ActionDoc{
	Purpose: "Rename or relocate a file or directory in-process (pure Go os.Rename, with a copy-then-remove fallback across filesystems). The owned, substrate-native move primitive — no shell, so it works in the distroless container. Mutating.",
	Params: []actionspec.DocParam{
		{Name: "source", Required: true, Description: "Path to move FROM. Must exist (file or directory)."},
		{Name: "src", Required: false, Description: "Alias of source (accepted when source is absent).", AliasOf: "source"},
		{Name: "from", Required: false, Description: "Alias of source (accepted when source and src are absent).", AliasOf: "source"},
		{Name: "dest", Required: true, Description: "Path to move TO. When dest is an existing directory the entry is moved INTO it (final path = dest/basename(source), like mv); otherwise dest is the literal target path and its missing parent directories are created. The final destination must NOT already exist — fs.move refuses to clobber."},
		{Name: "destination", Required: false, Description: "Alias of dest (accepted when dest is absent).", AliasOf: "dest"},
		{Name: "to", Required: false, Description: "Alias of dest (accepted when dest and destination are absent).", AliasOf: "dest"},
		{Name: "record", Required: false, Description: "OPT-IN provenance mode: emit an ArtifactMoved event for this move so fs.read provenance mode can fold it into the file's mutation history. Default off (no event)."},
		{Name: "intent", Required: false, Description: "Free-text intent recorded as the ArtifactMoved event's rationale (only meaningful with record=true)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "source missing or empty", Message: "fs.move requires source"},
		{Condition: "dest missing or empty", Message: "fs.move requires dest"},
		{Condition: "the source path does not exist", Message: "fs.move: source does not exist: <source>"},
		{Condition: "the final destination already exists", Message: "fs.move: destination already exists: <dest> (remove it first if a replace is intended)"},
		{Condition: "the process lacks permission on source or destination", Message: "fs.move: permission denied: <path>"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "MoveResult",
		Description: "source, dest (the FINAL destination path with any dir-into resolved), is_dir (the moved entry was a directory), cross_device (true when source and dest were on different filesystems so a copy-then-remove ran instead of a plain rename), and (record mode only) event — the {event_id, type} receipt of the emitted ArtifactMoved event; omitted on a default move.",
	},
	EnvelopeRequirements: []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required. fs.move is a mutating relocation; the rationale records WHY a path was moved.",
		AppliesToActorKinds: []string{"agent"},
	}},
	Notes: "CONTRACT: in-process via os.Rename (no shell). A cross-device move (EXDEV) falls back to a recursive copy-then-remove so the move still works when source and dest live on different filesystems (cross_device reports which path ran). fs.move REFUSES to overwrite an existing destination — move is mutating, not destructive; remove the target first (fs.remove) for a deliberate replace. Unlike fs.write/fs.edit, fs.move is NOT coupled to the read-state registry (it relocates bytes without inspecting content), so no prior fs.read is required. SUBSTRATE UPGRADE (opt-in, default stays a plain rename): record mode emits an ArtifactMoved event (intent → event rationale) keyed by the destination absolute path. Emission is fail-open: an event-log error never fails a move that already committed.",
}

var removeDoc = actionspec.ActionDoc{
	Purpose: "Delete a file, or a directory only when an explicit recursive flag is set (pure Go os.Remove / os.RemoveAll). The owned, substrate-native delete primitive — no shell, so it works in the distroless container. Destructive.",
	Params: []actionspec.DocParam{
		{Name: "file_path", Required: true, Description: "Absolute or working-directory-relative path to remove (file or directory)."},
		{Name: "path", Required: false, Description: "Alias of file_path (accepted when file_path is absent).", AliasOf: "file_path"},
		{Name: "recursive", Required: false, Description: "Required to delete a NON-EMPTY directory (os.RemoveAll). Without it, a non-empty directory is refused and nothing is deleted; a regular file or an empty directory removes without it. Default false."},
		{Name: "record", Required: false, Description: "OPT-IN provenance mode: emit an ArtifactRemoved event for this deletion so fs.read provenance mode can record it in the file's history. Default off (no event)."},
		{Name: "intent", Required: false, Description: "Free-text intent recorded as the ArtifactRemoved event's rationale (only meaningful with record=true)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "file_path missing or empty", Message: "fs.remove requires file_path"},
		{Condition: "the path does not exist", Message: "fs.remove: path does not exist: <path>"},
		{Condition: "a non-empty directory without recursive=true", Message: "fs.remove: <path> is a non-empty directory; pass recursive=true to delete it and its contents"},
		{Condition: "the target is a protected filesystem root (/, /home, /etc, …)", Message: "fs.remove: refusing to remove a protected filesystem root: <path>"},
		{Condition: "the process lacks permission", Message: "fs.remove: permission denied: <path>"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "RemoveResult",
		Description: "file_path, was_dir (the removed entry was a directory), and (record mode only) event — the {event_id, type} receipt of the emitted ArtifactRemoved event; omitted on a default remove.",
	},
	EnvelopeRequirements: []actionspec.ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required. fs.remove is destructive; the rationale records WHY a path was deleted.",
		AppliesToActorKinds: []string{"agent"},
	}},
	Notes: "CONTRACT: in-process via os.Remove (file / empty dir) or os.RemoveAll (non-empty dir, recursive=true only). A NON-EMPTY directory without the explicit recursive flag is refused and nothing is deleted — the guard against a single call wiping a tree. A small closed set of obviously dangerous absolute targets (filesystem roots like /, /home, /etc) is refused outright as a backstop; real confinement is the process's filesystem permissions (and, in deployment, the container's mount set). fs.remove is DESTRUCTIVE: rationale-gated for agent actors at the dispatch boundary and risk-classified ClassDestructive in the corpos gate. There is no read-state precondition (you remove a path, not its content). SUBSTRATE UPGRADE (opt-in, default stays a plain delete): record mode emits an ArtifactRemoved event (intent → event rationale) keyed by the removed absolute path. Emission is fail-open: an event-log error never fails a remove that already committed.",
}

var grepDoc = actionspec.ActionDoc{
	Purpose: "Search file contents with a regular expression (ripgrep). The owned, substrate-native replacement for the harness Grep tool; the default output is faithful to it. Paths are reported relative to the search root.",
	Params: []actionspec.DocParam{
		{Name: "pattern", Required: true, Description: "The regular expression to search for. A pattern beginning with '-' is passed explicitly so it is not parsed as a flag."},
		{Name: "path", Required: false, Description: "File or directory to search; defaults to the working directory. The search root — results are relative to it."},
		{Name: "file_path", Required: false, Description: "Alias of path (accepted when path is absent).", AliasOf: "path"},
		{Name: "glob", Required: false, Description: "Glob filter on filenames (e.g. \"*.go\", \"*.{ts,tsx}\"); whitespace/comma-separated, brace groups preserved."},
		{Name: "type", Required: false, Description: "ripgrep file type to restrict the search (e.g. go, py, rust, js)."},
		{Name: "output_mode", Required: false, Description: "content (matching lines, supports context + line numbers), files_with_matches (paths, the default), or count (per-file match counts)."},
		{Name: "context_before", Required: false, Description: "Lines of context before each match (content mode only)."},
		{Name: "context_after", Required: false, Description: "Lines of context after each match (content mode only)."},
		{Name: "context", Required: false, Description: "Lines of context before AND after each match (content mode only); takes precedence over context_before/context_after."},
		{Name: "show_line_numbers", Required: false, Description: "Show line numbers (content mode only); default true."},
		{Name: "case_insensitive", Required: false, Description: "Case-insensitive search; default false."},
		{Name: "head_limit", Required: false, Description: "Cap output to the first N entries (lines/files/counts); default 250. Pass 0 for unlimited."},
		{Name: "offset", Required: false, Description: "Skip the first N entries before applying head_limit; default 0."},
		{Name: "multiline", Required: false, Description: "Patterns may span lines and '.' matches newlines; default false."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "pattern missing or empty", Message: "fs.grep requires pattern"},
		{Condition: "output_mode is not content/files_with_matches/count", Message: "fs.grep: invalid output_mode <mode> (want content|files_with_matches|count)"},
		{Condition: "the search path does not exist", Message: "fs.grep: path does not exist: <path>"},
		{Condition: "ripgrep is not installed", Message: "fs.grep: ripgrep (rg) not found on PATH"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "GrepResult",
		Description: "mode, num_files, filenames (files_with_matches), content (content/count text), num_lines (content), num_matches (count total), and applied_limit/applied_offset paging markers (set only when they took effect).",
	},
	Notes: "CONTRACT (see go/internal/fs/testdata/parity/GREP_GLOB_CONTRACT.md): ripgrep-backed, hidden-aware, version-control metadata dirs excluded, lines clamped to 500 columns. files_with_matches is sorted newest-first by modification time. In content mode the filename is always emitted (path:line:text) so a single-file search stays addressable. head_limit/offset page the result set; applied_limit is reported only when truncation occurred. Harness carve-outs (plugin-cache exclusions, permission-context ignore patterns, the test-only sort hook) are dropped.",
}

var globDoc = actionspec.ActionDoc{
	Purpose: "Match files by glob pattern (ripgrep file enumeration). The owned replacement for the harness Glob tool; results are sorted newest-first by modification time and capped at 100 files.",
	Params: []actionspec.DocParam{
		{Name: "pattern", Required: true, Description: "The glob (ripgrep dialect: ** spans directories, */? within a segment, {a,b} groups). e.g. \"**/*.go\", \"src/**/*.ts\"."},
		{Name: "path", Required: false, Description: "Directory to search in; defaults to the working directory. Results are relative to it."},
		{Name: "file_path", Required: false, Description: "Alias of path (accepted when path is absent).", AliasOf: "path"},
	},
	Errors: []actionspec.ActionError{
		{Condition: "pattern missing or empty", Message: "fs.glob requires pattern"},
		{Condition: "the search path does not exist", Message: "fs.glob: path does not exist: <path>"},
		{Condition: "the path is not a directory", Message: "fs.glob: <path> is not a directory"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "GlobResult",
		Description: "filenames (relative to the search root, newest-first), num_files, truncated (true when the 100-file cap elided matches), duration_ms.",
	},
	Notes: "CONTRACT (see go/internal/fs/testdata/parity/GREP_GLOB_CONTRACT.md): rides ripgrep's glob dialect (one engine for grep + glob), hidden-aware, excludes version-control metadata dirs, honors .gitignore. A bare *.ext matches at any depth (ripgrep semantics), unlike a shell *.ext — documented, not faithful to a shell glob. Capped at 100 files; truncated flags elision.",
}

var lsDoc = actionspec.ActionDoc{
	Purpose: "List the immediate entries of a directory as structured rows. SELF-DEFINED (the harness has no LS tool — listing there is done via Bash/Glob), so this is a first-principles contract, not a parity match.",
	Params: []actionspec.DocParam{
		{Name: "path", Required: false, Description: "Directory to list; defaults to the working directory."},
		{Name: "file_path", Required: false, Description: "Alias of path (accepted when path is absent).", AliasOf: "path"},
		{Name: "all", Required: false, Description: "Include entries whose name begins with '.' (default false)."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "the path does not exist", Message: "fs.ls: path does not exist: <path>"},
		{Condition: "the path is a file, not a directory", Message: "fs.ls: <path> is not a directory"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "LSResult",
		Description: "path (the listed directory), entries (rows of {name, type: dir|file|symlink, size in bytes — 0 for dirs}), count.",
	},
	Notes: "CONTRACT (see go/internal/fs/testdata/LS_CONTRACT.md): immediate children only (non-recursive), sorted by name (byte order); dotfiles omitted unless all=true. Self-defined — no harness LS counterpart.",
}

// fsActionRegistry is the ordered, co-located descriptor registry — the single
// source of the fs surface's action docs. FsActionSpecs() derives the catalog
// the corpus generator + admin.action_describe consume. ParamStruct is set on
// every action (all are struct-backed), so param Types derive from the param
// struct and the authored Types stay empty.
var fsActionRegistry = []actionspec.ActionEntry{
	{Name: "read", Doc: readDoc, ParamStruct: reflect.TypeOf(ReadParams{})},
	{Name: "write", Doc: writeDoc, ParamStruct: reflect.TypeOf(WriteParams{})},
	{Name: "edit", Doc: editDoc, ParamStruct: reflect.TypeOf(EditParams{})},
	{Name: "move", Doc: moveDoc, ParamStruct: reflect.TypeOf(MoveParams{})},
	{Name: "remove", Doc: removeDoc, ParamStruct: reflect.TypeOf(RemoveParams{})},
	{Name: "grep", Doc: grepDoc, ParamStruct: reflect.TypeOf(GrepParams{})},
	{Name: "glob", Doc: globDoc, ParamStruct: reflect.TypeOf(GlobParams{})},
	{Name: "ls", Doc: lsDoc, ParamStruct: reflect.TypeOf(LSParams{})},
}

// FsActionSpecs returns the fs surface's full action catalog, derived from the
// co-located descriptor registry. This is what the corpus generator projects
// into corpus/fs/*.toml and what admin.action_describe(fs, X) serves.
func FsActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(fsActionRegistry)
}
