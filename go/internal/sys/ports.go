package sys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PortRow is one listening/bound socket in a sys.ports listing.
type PortRow struct {
	Proto     string `json:"proto"`
	State     string `json:"state"`
	LocalAddr string `json:"local_addr"`
	LocalPort int    `json:"local_port"`
	PID       int    `json:"pid"`
	Process   string `json:"process"`
}

// PortsParams filters a sys.ports listing.
type PortsParams struct {
	Proto string `json:"proto,omitempty"` // tcp | udp
}

// PortsResult is the success shape for sys.ports.
type PortsResult struct {
	Ports []PortRow `json:"ports"`
	Count int       `json:"count"`
}

// ssUsersRe extracts the owning process name + pid from ss's process column:
// users:(("name",pid=N,fd=M)).
var ssUsersRe = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// parseSSLine parses one `ss -tulnpH` row into a PortRow. The address column is
// split on its LAST ':' so IPv6 ([::]:8080) and scoped (127.0.0.53%lo:53)
// addresses parse correctly. Returns false for a line that is not a tcp/udp row.
func parseSSLine(line string) (PortRow, bool) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return PortRow{}, false
	}
	proto := f[0]
	if proto != "tcp" && proto != "udp" {
		return PortRow{}, false
	}
	local := f[4]
	i := strings.LastIndexByte(local, ':')
	if i < 0 {
		return PortRow{}, false
	}
	port, err := strconv.Atoi(local[i+1:])
	if err != nil {
		return PortRow{}, false
	}
	row := PortRow{Proto: proto, State: f[1], LocalAddr: local[:i], LocalPort: port}
	if m := ssUsersRe.FindStringSubmatch(line); m != nil {
		row.Process = m[1]
		row.PID, _ = strconv.Atoi(m[2])
	}
	return row, true
}

// HandlePorts lists listening TCP and bound UDP sockets via `ss -tulnpH` (see
// testdata/INTROSPECTION_CONTRACT.md). Read-only.
func HandlePorts(ctx context.Context, params json.RawMessage) (PortsResult, error) {
	var p PortsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return PortsResult{}, fmt.Errorf("sys.ports: invalid params: %w", err)
		}
	}
	out, err := runHostCmd(ctx, "ss", "-tulnpH")
	if err != nil {
		return PortsResult{}, fmt.Errorf("sys.ports: %w", err)
	}
	rows := []PortRow{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		row, ok := parseSSLine(line)
		if !ok {
			continue
		}
		if p.Proto != "" && row.Proto != p.Proto {
			continue
		}
		rows = append(rows, row)
	}
	return PortsResult{Ports: rows, Count: len(rows)}, nil
}

// runHostCmd runs a host introspection tool and returns combined stdout. It is
// the thin IO boundary the pure parsers sit behind.
func runHostCmd(ctx context.Context, name string, args ...string) (string, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
