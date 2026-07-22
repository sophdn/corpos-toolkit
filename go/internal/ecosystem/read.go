package ecosystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// --- describe ---------------------------------------------------------------

type describeParams struct {
	Target string `json:"target"`
}

// HostRecord is the full stored record for a host (describe).
type HostRecord struct {
	Slug             string             `json:"slug"`
	Addr             string             `json:"addr"`
	SSHUser          string             `json:"ssh_user"`
	SSHPort          int64              `json:"ssh_port"`
	SSHKeyPath       string             `json:"ssh_key_path,omitempty"`
	PasswordlessSudo bool               `json:"passwordless_sudo"`
	Notes            string             `json:"notes,omitempty"`
	Addresses        []AddressRecord    `json:"addresses,omitempty"`
	Services         []ServiceRecord    `json:"services,omitempty"`
	AccessMethods    []AccessMethodView `json:"access_methods,omitempty"`
}

// AddressRecord is one alternate address (describe).
type AddressRecord struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	Preferred bool   `json:"preferred"`
}

// ServiceRecord is a stored service (describe / list).
type ServiceRecord struct {
	Slug     string `json:"slug"`
	HostSlug string `json:"host_slug"`
	Kind     string `json:"kind,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Port     *int64 `json:"port,omitempty"`
	Status   string `json:"status"`
	SoftRef  string `json:"soft_ref,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// DescribeResult carries whichever record kind matched.
type DescribeResult struct {
	Found   bool           `json:"found"`
	Kind    string         `json:"kind,omitempty"` // host | service
	Host    *HostRecord    `json:"host,omitempty"`
	Service *ServiceRecord `json:"service,omitempty"`
	Message string         `json:"message,omitempty"`
}

// describe returns the full stored record for a host or service.
func (d Deps) describe(ctx context.Context, params json.RawMessage) (DescribeResult, error) {
	var p describeParams
	if len(params) == 0 {
		return DescribeResult{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return DescribeResult{}, err
	}
	norm := strings.ToLower(strings.TrimSpace(p.Target))
	if norm == "" {
		return DescribeResult{}, errors.New("params.target is required")
	}
	h := d.Pool.DB()

	if slug, _, err := matchHost(ctx, h, norm); err != nil {
		return DescribeResult{}, err
	} else if slug != "" {
		rec, err := hostRecord(ctx, h, slug)
		if err != nil {
			return DescribeResult{}, err
		}
		return DescribeResult{Found: true, Kind: "host", Host: rec}, nil
	}
	if slug, err := matchService(ctx, h, norm); err != nil {
		return DescribeResult{}, err
	} else if slug != "" {
		rec, err := serviceRecord(ctx, h, slug)
		if err != nil {
			return DescribeResult{}, err
		}
		return DescribeResult{Found: true, Kind: "service", Service: rec}, nil
	}
	return DescribeResult{Found: false, Message: fmt.Sprintf("%q is not in the learned ecosystem", p.Target)}, nil
}

func hostRecord(ctx context.Context, h *sql.DB, slug string) (*HostRecord, error) {
	var rec HostRecord
	var sshKey sql.NullString
	var sudo int64
	if err := h.QueryRowContext(ctx,
		`SELECT slug, addr, ssh_user, ssh_port, ssh_key_path, passwordless_sudo, notes
		   FROM hosts WHERE slug = ?`, slug,
	).Scan(&rec.Slug, &rec.Addr, &rec.SSHUser, &rec.SSHPort, &sshKey, &sudo, &rec.Notes); err != nil {
		return nil, fmt.Errorf("read host %q: %w", slug, err)
	}
	rec.SSHKeyPath = sshKey.String
	rec.PasswordlessSudo = sudo != 0

	rows, err := h.QueryContext(ctx,
		`SELECT kind, value, preferred FROM ecosystem_host_addresses WHERE host_slug = ? ORDER BY preferred DESC, value`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AddressRecord
		var pref int64
		if err := rows.Scan(&a.Kind, &a.Value, &pref); err != nil {
			return nil, err
		}
		a.Preferred = pref != 0
		rec.Addresses = append(rec.Addresses, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	svcs, err := servicesOnHost(ctx, h, slug)
	if err != nil {
		return nil, err
	}
	rec.Services = svcs

	methods, _, err := accessMethodsFor(ctx, h, "host", slug)
	if err != nil {
		return nil, err
	}
	rec.AccessMethods = methods
	return &rec, nil
}

func serviceRecord(ctx context.Context, h *sql.DB, slug string) (*ServiceRecord, error) {
	var rec ServiceRecord
	var port sql.NullInt64
	if err := h.QueryRowContext(ctx,
		`SELECT slug, host_slug, kind, endpoint, port, status, soft_ref, notes
		   FROM ecosystem_services WHERE slug = ?`, slug,
	).Scan(&rec.Slug, &rec.HostSlug, &rec.Kind, &rec.Endpoint, &port, &rec.Status, &rec.SoftRef, &rec.Notes); err != nil {
		return nil, fmt.Errorf("read service %q: %w", slug, err)
	}
	if port.Valid {
		rec.Port = &port.Int64
	}
	return &rec, nil
}

func servicesOnHost(ctx context.Context, h *sql.DB, hostSlug string) ([]ServiceRecord, error) {
	rows, err := h.QueryContext(ctx,
		`SELECT slug, host_slug, kind, endpoint, port, status, soft_ref, notes
		   FROM ecosystem_services WHERE host_slug = ? ORDER BY slug`, hostSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRecord
	for rows.Next() {
		var rec ServiceRecord
		var port sql.NullInt64
		if err := rows.Scan(&rec.Slug, &rec.HostSlug, &rec.Kind, &rec.Endpoint, &port, &rec.Status, &rec.SoftRef, &rec.Notes); err != nil {
			return nil, err
		}
		if port.Valid {
			rec.Port = &port.Int64
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// --- list -------------------------------------------------------------------

type listParams struct {
	IncludeRetired bool `json:"include_retired"`
}

// HostSummaryRow is a compact host row for the list action.
type HostSummaryRow struct {
	Slug         string `json:"slug"`
	Addr         string `json:"addr"`
	SSHUser      string `json:"ssh_user"`
	ServiceCount int    `json:"service_count"`
}

// ListResult enumerates the learned ecosystem.
type ListResult struct {
	Hosts    []HostSummaryRow `json:"hosts"`
	Services []ServiceRecord  `json:"services"`
	Count    int              `json:"count"`
}

// list enumerates the learned ecosystem (hosts + services).
func (d Deps) list(ctx context.Context, params json.RawMessage) (ListResult, error) {
	var p listParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	h := d.Pool.DB()
	res := ListResult{Hosts: []HostSummaryRow{}, Services: []ServiceRecord{}}

	hostSQL := `SELECT slug, addr, ssh_user FROM hosts`
	if !p.IncludeRetired {
		hostSQL += ` WHERE retired_at IS NULL`
	}
	hostSQL += ` ORDER BY slug`
	rows, err := h.QueryContext(ctx, hostSQL)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var r HostSummaryRow
		if err := rows.Scan(&r.Slug, &r.Addr, &r.SSHUser); err != nil {
			return res, err
		}
		res.Hosts = append(res.Hosts, r)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	svcSQL := `SELECT slug, host_slug, kind, endpoint, port, status, soft_ref, notes FROM ecosystem_services`
	if !p.IncludeRetired {
		svcSQL += ` WHERE status = 'live'`
	}
	svcSQL += ` ORDER BY host_slug, slug`
	srows, err := h.QueryContext(ctx, svcSQL)
	if err != nil {
		return res, err
	}
	defer srows.Close()
	counts := map[string]int{}
	for srows.Next() {
		var rec ServiceRecord
		var port sql.NullInt64
		if err := srows.Scan(&rec.Slug, &rec.HostSlug, &rec.Kind, &rec.Endpoint, &port, &rec.Status, &rec.SoftRef, &rec.Notes); err != nil {
			return res, err
		}
		if port.Valid {
			rec.Port = &port.Int64
		}
		res.Services = append(res.Services, rec)
		counts[rec.HostSlug]++
	}
	if err := srows.Err(); err != nil {
		return res, err
	}
	for i := range res.Hosts {
		res.Hosts[i].ServiceCount = counts[res.Hosts[i].Slug]
	}
	res.Count = len(res.Hosts) + len(res.Services)
	return res, nil
}
