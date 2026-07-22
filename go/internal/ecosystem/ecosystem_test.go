package ecosystem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/testutil"
)

func mkDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{Pool: testutil.NewTestDB(t)}
}

func mustLearnHost(t *testing.T, d Deps, params string) {
	t.Helper()
	if _, err := d.hostLearn(context.Background(), json.RawMessage(params)); err != nil {
		t.Fatalf("host_learn(%s): %v", params, err)
	}
}

// The canonical scenario: after learning example-host, a cold "do I have access
// to example-host?" resolves deterministically to YES with the ssh detail — and
// the youruser/sophi username trap is answered correctly from the data.
func TestAccessCheck_SophieServerScenario(t *testing.T) {
	d := mkDeps(t)
	mustLearnHost(t, d, `{
		"slug":"example-host","addr":"203.0.113.10","ssh_user":"youruser",
		"ssh_key_path":"~/.ssh/id_ed25519","passwordless_sudo":true,
		"addresses":[
			{"kind":"magicdns","value":"example-host.tailnet.ts.net","preferred":true},
			{"kind":"lan","value":"10.0.0.182"}
		]}`)

	for _, target := range []string{"example-host", "203.0.113.10", "example-host.tailnet.ts.net"} {
		sum, err := d.accessCheck(context.Background(), json.RawMessage(`{"target":"`+target+`"}`))
		if err != nil {
			t.Fatalf("access_check(%q): %v", target, err)
		}
		if sum.Status != StatusYes {
			t.Errorf("target %q: status = %q, want yes (answer=%q)", target, sum.Status, sum.Answer)
		}
		if sum.Kind != "host" || sum.Slug != "example-host" {
			t.Errorf("target %q: resolved to kind=%q slug=%q", target, sum.Kind, sum.Slug)
		}
		var ssh *AccessMethodView
		for i := range sum.AccessMethods {
			if sum.AccessMethods[i].Method == "ssh" {
				ssh = &sum.AccessMethods[i]
			}
		}
		if ssh == nil {
			t.Fatalf("target %q: no ssh method in %+v", target, sum.AccessMethods)
		}
		if ssh.Principal != "youruser" {
			t.Errorf("target %q: ssh principal = %q, want youruser (the youruser/sophi trap)", target, ssh.Principal)
		}
		if ssh.CredentialPointer != "~/.ssh/id_ed25519" {
			t.Errorf("target %q: credential pointer = %q", target, ssh.CredentialPointer)
		}
		if !strings.Contains(sum.Answer, "ssh youruser@") {
			t.Errorf("target %q: answer = %q, want an 'ssh youruser@...' phrasing", target, sum.Answer)
		}
	}
}

// The empty-tenant guardrail: an un-learned target is UNKNOWN, never a
// hallucinated NO.
func TestAccessCheck_UnknownNotNo(t *testing.T) {
	d := mkDeps(t)
	sum, err := d.accessCheck(context.Background(), json.RawMessage(`{"target":"nonexistent-host"}`))
	if err != nil {
		t.Fatalf("access_check: %v", err)
	}
	if sum.Status != StatusUnknown {
		t.Errorf("status = %q, want unknown", sum.Status)
	}
	if sum.Resolved {
		t.Errorf("resolved = true for an un-learned target")
	}
	if !strings.Contains(strings.ToLower(sum.Answer), "unknown") {
		t.Errorf("answer = %q, want it to say unknown", sum.Answer)
	}
}

// Service resolution: falls back to the host's access method when the service has
// none of its own, and reports the endpoint.
func TestAccessCheck_ServiceFallsBackToHostAccess(t *testing.T) {
	d := mkDeps(t)
	mustLearnHost(t, d, `{"slug":"example-host","addr":"203.0.113.10","ssh_user":"youruser","ssh_key_path":"~/.ssh/id_ed25519"}`)
	if _, err := d.serviceLearn(context.Background(), json.RawMessage(
		`{"slug":"jellyfin","host_slug":"example-host","kind":"media","endpoint":"http://example-host:8096","port":8096,"soft_ref":"memory/reference/example-host-jellyfin-media-transfer"}`,
	)); err != nil {
		t.Fatalf("service_learn: %v", err)
	}
	sum, err := d.accessCheck(context.Background(), json.RawMessage(`{"target":"jellyfin"}`))
	if err != nil {
		t.Fatalf("access_check: %v", err)
	}
	if sum.Kind != "service" || sum.Slug != "jellyfin" {
		t.Fatalf("resolved kind=%q slug=%q", sum.Kind, sum.Slug)
	}
	if sum.Status != StatusYes {
		t.Errorf("status = %q, want yes (host ssh should back the service)", sum.Status)
	}
	if sum.Endpoint != "http://example-host:8096" {
		t.Errorf("endpoint = %q", sum.Endpoint)
	}
	if len(sum.SoftRefs) == 0 || sum.SoftRefs[0] != "memory/reference/example-host-jellyfin-media-transfer" {
		t.Errorf("soft_refs = %v, want the jellyfin vault pointer", sum.SoftRefs)
	}
}

// A service-specific access method (gitea API token pointer) is preferred and the
// scope note survives.
func TestAccessCheck_ServiceOwnAccessMethod(t *testing.T) {
	d := mkDeps(t)
	mustLearnHost(t, d, `{"slug":"example-host","addr":"203.0.113.10","ssh_user":"youruser"}`)
	if _, err := d.serviceLearn(context.Background(), json.RawMessage(
		`{"slug":"gitea","host_slug":"example-host","kind":"git","endpoint":"https://example-host.tailnet.ts.net/git/"}`,
	)); err != nil {
		t.Fatalf("service_learn: %v", err)
	}
	if _, err := d.accessLearn(context.Background(), json.RawMessage(
		`{"slug":"gitea-api","target_kind":"service","target_slug":"gitea","method":"https-api","principal":"sophdn","credential_pointer":"~/.git-credentials","scope_note":"repo-scoped not org"}`,
	)); err != nil {
		t.Fatalf("access_learn: %v", err)
	}
	sum, err := d.accessCheck(context.Background(), json.RawMessage(`{"target":"gitea"}`))
	if err != nil {
		t.Fatalf("access_check: %v", err)
	}
	if sum.Status != StatusYes {
		t.Fatalf("status = %q, want yes", sum.Status)
	}
	m := firstEnabled(sum.AccessMethods)
	if m.Method != "https-api" || m.Principal != "sophdn" || m.CredentialPointer != "~/.git-credentials" {
		t.Errorf("access method = %+v", m)
	}
	if m.ScopeNote != "repo-scoped not org" {
		t.Errorf("scope_note = %q", m.ScopeNote)
	}
}

// The credential-pointer invariant: an inline-secret-looking value is rejected;
// legitimate pointers (paths, env names) pass.
func TestCredentialGuard(t *testing.T) {
	d := mkDeps(t)
	// A host with an inline-secret ssh_key_path is rejected.
	_, err := d.hostLearn(context.Background(), json.RawMessage(
		`{"slug":"h1","addr":"10.0.0.9","ssh_key_path":"AKIAIOSFODNN7EXAMPLEabcdef0123456789"}`))
	if err == nil || !strings.Contains(err.Error(), "inline secret") {
		t.Errorf("expected inline-secret rejection, got %v", err)
	}

	cases := []struct {
		val    string
		reject bool
	}{
		{"~/.ssh/id_ed25519", false},
		{"~/.git-credentials", false},
		{"GITEA_TOKEN", false},
		{"/etc/campaign-settings.env", false},
		{"$HOME/.config/token", false},
		{"", false},
		{"ghp_0123456789abcdefABCDEF0123456789abcdef", true},
		{"AKIAIOSFODNN7EXAMPLEabcdefghij0123456789", true},
	}
	for _, c := range cases {
		got := looksLikeInlineSecret(c.val)
		if got != c.reject {
			t.Errorf("looksLikeInlineSecret(%q) = %v, want %v", c.val, got, c.reject)
		}
	}
}

// list + describe round-trip, and AllTokens surfaces host/service/address tokens
// for the refresolve catalog.
func TestListDescribeAndTokens(t *testing.T) {
	d := mkDeps(t)
	mustLearnHost(t, d, `{"slug":"example-host","addr":"203.0.113.10","ssh_user":"youruser","addresses":[{"kind":"lan","value":"10.0.0.182"}]}`)
	if _, err := d.serviceLearn(context.Background(), json.RawMessage(
		`{"slug":"gitea","host_slug":"example-host","endpoint":"https://example-host/git/"}`)); err != nil {
		t.Fatalf("service_learn: %v", err)
	}
	// A retired service is excluded from the default list.
	if _, err := d.serviceLearn(context.Background(), json.RawMessage(
		`{"slug":"dm-manager","host_slug":"example-host","status":"retired"}`)); err != nil {
		t.Fatalf("service_learn retired: %v", err)
	}

	lst, err := d.list(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lst.Hosts) != 1 || lst.Hosts[0].Slug != "example-host" {
		t.Errorf("hosts = %+v", lst.Hosts)
	}
	if lst.Hosts[0].ServiceCount != 1 {
		t.Errorf("service_count = %d, want 1 (retired excluded)", lst.Hosts[0].ServiceCount)
	}
	if len(lst.Services) != 1 || lst.Services[0].Slug != "gitea" {
		t.Errorf("live services = %+v", lst.Services)
	}

	full, err := d.list(context.Background(), json.RawMessage(`{"include_retired":true}`))
	if err != nil {
		t.Fatalf("list include_retired: %v", err)
	}
	if len(full.Services) != 2 {
		t.Errorf("include_retired services = %d, want 2", len(full.Services))
	}

	desc, err := d.describe(context.Background(), json.RawMessage(`{"target":"example-host"}`))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !desc.Found || desc.Host == nil || desc.Host.SSHUser != "youruser" {
		t.Fatalf("describe host = %+v", desc)
	}
	if len(desc.Host.Services) != 2 {
		t.Errorf("describe host services = %d, want 2 (describe shows all)", len(desc.Host.Services))
	}

	tokens, err := AllTokens(context.Background(), d.Pool.DB())
	if err != nil {
		t.Fatalf("AllTokens: %v", err)
	}
	want := map[string]bool{"example-host": false, "gitea": false, "dm-manager": false, "10.0.0.182": false}
	for _, tok := range tokens {
		if _, ok := want[tok]; ok {
			want[tok] = true
		}
	}
	for tok, seen := range want {
		if !seen {
			t.Errorf("AllTokens missing %q (got %v)", tok, tokens)
		}
	}
}

// Learn actions reject unknown targets — you cannot attach a service to a host or
// an access method to a target that was never learned.
func TestLearn_ReferentialGuards(t *testing.T) {
	d := mkDeps(t)
	if _, err := d.serviceLearn(context.Background(), json.RawMessage(
		`{"slug":"gitea","host_slug":"ghost-host"}`)); err == nil || !strings.Contains(err.Error(), "host_not_found") {
		t.Errorf("service_learn on unknown host: got %v", err)
	}
	if _, err := d.accessLearn(context.Background(), json.RawMessage(
		`{"slug":"x","target_kind":"service","target_slug":"ghost-svc","method":"ssh"}`)); err == nil || !strings.Contains(err.Error(), "service_not_found") {
		t.Errorf("access_learn on unknown service: got %v", err)
	}
}

// host_learn is an idempotent upsert: re-learning updates in place, and the
// address set is replaced (declarative), not appended.
func TestHostLearn_UpsertReplacesAddresses(t *testing.T) {
	d := mkDeps(t)
	mustLearnHost(t, d, `{"slug":"h","addr":"1.1.1.1","addresses":[{"kind":"lan","value":"10.0.0.1"},{"kind":"lan","value":"10.0.0.2"}]}`)
	mustLearnHost(t, d, `{"slug":"h","addr":"2.2.2.2","addresses":[{"kind":"tailnet","value":"100.0.0.1","preferred":true}]}`)
	desc, err := d.describe(context.Background(), json.RawMessage(`{"target":"h"}`))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.Host.Addr != "2.2.2.2" {
		t.Errorf("addr = %q, want 2.2.2.2 (upsert)", desc.Host.Addr)
	}
	if len(desc.Host.Addresses) != 1 || desc.Host.Addresses[0].Value != "100.0.0.1" {
		t.Errorf("addresses = %+v, want the replaced single tailnet addr", desc.Host.Addresses)
	}
}
