package sys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// UnitRow is one systemd-user unit in a sys.units listing.
type UnitRow struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// UnitsParams filters a sys.units listing.
type UnitsParams struct {
	Type       string `json:"type,omitempty"`        // unit type, default "service"
	ActiveOnly bool   `json:"active_only,omitempty"` // omit --all when true
}

// UnitsResult is the success shape for sys.units.
type UnitsResult struct {
	Units []UnitRow `json:"units"`
	Count int       `json:"count"`
}

// parseSystemctlUnits unmarshals `systemctl … -o json` output into rows.
func parseSystemctlUnits(b []byte) ([]UnitRow, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return []UnitRow{}, nil
	}
	var rows []UnitRow
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse systemctl json: %w", err)
	}
	return rows, nil
}

// HandleUnits lists systemd-user units via `systemctl --user … -o json` (see
// testdata/INTROSPECTION_CONTRACT.md). Read-only.
func HandleUnits(ctx context.Context, params json.RawMessage) (UnitsResult, error) {
	var p UnitsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return UnitsResult{}, fmt.Errorf("sys.units: invalid params: %w", err)
		}
	}
	unitType := strings.TrimSpace(p.Type)
	if unitType == "" {
		unitType = "service"
	}
	args := []string{"--user", "list-units", "--type=" + unitType, "-o", "json", "--no-pager"}
	if !p.ActiveOnly {
		args = append(args, "--all")
	}
	out, err := runHostCmd(ctx, "systemctl", args...)
	if err != nil {
		return UnitsResult{}, fmt.Errorf("sys.units: %w", err)
	}
	rows, err := parseSystemctlUnits([]byte(out))
	if err != nil {
		return UnitsResult{}, fmt.Errorf("sys.units: %w", err)
	}
	if rows == nil {
		rows = []UnitRow{}
	}
	return UnitsResult{Units: rows, Count: len(rows)}, nil
}
