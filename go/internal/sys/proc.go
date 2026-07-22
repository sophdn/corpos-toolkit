package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ProcessRow is one process in a sys.ps listing.
type ProcessRow struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	State   string `json:"state"`
	Command string `json:"command"`
	RSSKB   int64  `json:"rss_kb"`
}

// PSParams filters a sys.ps listing.
type PSParams struct {
	Contains string `json:"contains,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// PSResult is the success shape for sys.ps.
type PSResult struct {
	Processes []ProcessRow `json:"processes"`
	Count     int          `json:"count"`
}

// procStat is the parsed subset of a /proc/<pid>/stat line.
type procStat struct {
	PID      int
	PPID     int
	State    string
	Comm     string
	RSSPages int64
}

// parseProcStat parses a /proc/<pid>/stat line. The comm field (parenthesized,
// may contain spaces and parens) is split off at the LAST ')'; state, ppid, and
// the rss page count (field 24) are read from the fixed-offset remainder.
func parseProcStat(content string) (procStat, bool) {
	open := strings.IndexByte(content, '(')
	closeIdx := strings.LastIndexByte(content, ')')
	if open < 0 || closeIdx < 0 || closeIdx < open {
		return procStat{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(content[:open]))
	if err != nil {
		return procStat{}, false
	}
	comm := content[open+1 : closeIdx]
	rest := strings.Fields(content[closeIdx+1:])
	if len(rest) < 22 {
		return procStat{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return procStat{}, false
	}
	rss, _ := strconv.ParseInt(rest[21], 10, 64)
	return procStat{PID: pid, PPID: ppid, State: rest[0], Comm: comm, RSSPages: rss}, true
}

// parseCmdline renders /proc/<pid>/cmdline (NUL-delimited args) as a space-joined
// command string; empty for kernel threads.
func parseCmdline(b []byte) string {
	s := strings.Trim(string(b), "\x00")
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(s, "\x00", " ")
}

// HandlePS enumerates processes by reading /proc directly (see
// testdata/INTROSPECTION_CONTRACT.md). Read-only.
func HandlePS(_ context.Context, params json.RawMessage) (PSResult, error) {
	var p PSParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return PSResult{}, fmt.Errorf("sys.ps: invalid params: %w", err)
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return PSResult{}, fmt.Errorf("sys.ps: read /proc: %w", err)
	}
	pageKB := int64(os.Getpagesize()) / 1024
	rows := []ProcessRow{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // only numeric pid dirs
		}
		statB, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // process exited between readdir and read
		}
		st, ok := parseProcStat(string(statB))
		if !ok {
			continue
		}
		cmd := parseCmdline(readProcFile(e.Name(), "cmdline"))
		if cmd == "" {
			cmd = "[" + st.Comm + "]"
		}
		if p.Contains != "" && !strings.Contains(cmd, p.Contains) {
			continue
		}
		rows = append(rows, ProcessRow{
			PID: st.PID, PPID: st.PPID, State: st.State, Command: cmd, RSSKB: st.RSSPages * pageKB,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PID < rows[j].PID })
	if p.Limit > 0 && len(rows) > p.Limit {
		rows = rows[:p.Limit]
	}
	return PSResult{Processes: rows, Count: len(rows)}, nil
}

func readProcFile(pid, name string) []byte {
	b, err := os.ReadFile(filepath.Join("/proc", pid, name))
	if err != nil {
		return nil
	}
	return b
}
