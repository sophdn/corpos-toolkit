package ecosystem

// action_doc.go is the descriptor-registry seam for the ecosystem surface's
// action docs (the per-surface instantiation of docs/ACTION_DOC_CONTRACT.md).
// Each param's TYPE is DERIVED from the handler's typed param struct; only the
// irreducible semantics are authored here. The generated corpus
// (corpus/ecosystem/*.toml) + admin.action_describe(ecosystem, X) derive from
// this registry via EcosystemActionSpecs().

import (
	"reflect"

	"toolkit/internal/actionspec"
)

// standardRationaleEnvelope documents the dispatch-policy rationale gate the
// mutating learn actions carry (action-manifests/dispatch-policy.toml). Parity
// with the policy is asserted by
// actiondocs.TestActionDocs_DispatchPolicyParity_RationaleEnvelopeRequirement:
// a requires_rationale=true policy entry MUST have this envelope requirement so
// agents reading action_describe see the gate. Read-only; shared by reference.
var standardRationaleEnvelope = []actionspec.ActionEnvelopeReq{{
	Field:               "rationale",
	Required:            true,
	Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Lives at the call envelope level (next to action/params/project), NOT inside params. Rejected on empty / whitespace / boilerplate / <6-char rationales with error=rationale_required.",
	AppliesToActorKinds: []string{"agent"},
}}

var accessCheckDoc = actionspec.ActionDoc{
	Purpose: "Deterministically answer \"do I have access to X?\" for a host or service. The single source of truth for ecosystem access — same answer every call, no RAG, no correction loop.",
	Params: []actionspec.DocParam{
		{Name: "target", Required: true, Description: "A host slug, service slug, or a host address (e.g. 'example-host', 'gitea', '203.0.113.10')."},
		{Name: "intent", Required: false, Description: "access | locate | describe (default access). Currently advisory; the full summary is returned regardless."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "AccessSummary",
		Description: "status (yes|no|unknown), access_methods (method/principal/address/credential_pointer), addresses, endpoint (services), soft_refs (vault how-to pointers), and a composed one-line answer.",
	},
	Notes: "status='unknown' means the target is NOT learned — never a hallucinated 'no'; learn it first. credential_pointer values are POINTERS to where a secret lives (a path/env name), never the secret. The procedural how-to prose stays soft in the vault; soft_refs points there.",
}

var describeDoc = actionspec.ActionDoc{
	Purpose: "Return the full stored record for a host (with its addresses, services, and access methods) or a service.",
	Params: []actionspec.DocParam{
		{Name: "target", Required: true, Description: "A host slug, service slug, or host address to describe."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "DescribeResult",
		Description: "found + kind (host|service) + the matching host or service record.",
	},
}

var listDoc = actionspec.ActionDoc{
	Purpose: "Enumerate the learned ecosystem: all hosts (with service counts) and services.",
	Params: []actionspec.DocParam{
		{Name: "include_retired", Required: false, Description: "Include retired hosts and retired services (default false → live only)."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "ListResult",
		Description: "hosts, services, and a total count.",
	},
}

var hostLearnDoc = actionspec.ActionDoc{
	Purpose: "Learn (upsert) a host into the shared-infra host registry, plus its alternate addresses. The inline ssh_user/ssh_key_path ARE the host's SSH access method. Idempotent; rationale-gated.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "Stable host identifier, e.g. 'example-host'."},
		{Name: "addr", Required: true, Description: "Canonical address (IP or hostname)."},
		{Name: "ssh_user", Required: false, Description: "SSH principal (the login user — NOT necessarily the local box user)."},
		{Name: "ssh_port", Required: false, Description: "SSH port (default 22)."},
		{Name: "ssh_key_path", Required: false, Description: "POINTER to the SSH key, e.g. '~/.ssh/id_ed25519' — never an inline key."},
		{Name: "passwordless_sudo", Required: false, Description: "Whether the host grants passwordless sudo."},
		{Name: "notes", Required: false, Description: "Freeform notes."},
		{Name: "addresses", Required: false, Description: "Alternate reachable addresses: [{kind: tailnet|lan|magicdns|hostname|other, value, preferred, notes}]. Replaces the host's address set."},
	},
	Returns: &actionspec.ActionReturn{Shape: "LearnResult", Description: "ok + kind='host' + slug."},
	Errors: []actionspec.ActionError{
		{Condition: "ssh_key_path looks like an inline secret", Message: "ssh_key_path looks like an inline secret; store a POINTER instead"},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
	Notes:                "Reuses the `hosts` table (migration 003). Ships empty; nothing is hardcoded — this is how a tenant's ecosystem is populated as data.",
}

var serviceLearnDoc = actionspec.ActionDoc{
	Purpose: "Learn (upsert) a service running on an already-learned host. Idempotent; rationale-gated.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "Stable service identifier, e.g. 'gitea', 'jellyfin'."},
		{Name: "host_slug", Required: true, Description: "The host it runs on (must already be learned)."},
		{Name: "kind", Required: false, Description: "Freeform kind: http | db | git | media | ..."},
		{Name: "endpoint", Required: false, Description: "URL or 'host:port'."},
		{Name: "port", Required: false, Description: "Numeric port, if applicable."},
		{Name: "status", Required: false, Description: "live | retired (default live)."},
		{Name: "soft_ref", Required: false, Description: "Pointer to the vault how-to note, e.g. 'memory/reference/...'."},
		{Name: "notes", Required: false, Description: "Freeform notes."},
	},
	Returns: &actionspec.ActionReturn{Shape: "LearnResult", Description: "ok + kind='service' + slug."},
	Errors: []actionspec.ActionError{
		{Condition: "the host_slug is not learned", Message: "host_not_found: <slug> (learn the host first)"},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var accessLearnDoc = actionspec.ActionDoc{
	Purpose: "Learn (upsert) an access method for a host or service. Idempotent; rationale-gated.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "Stable access-method identifier, e.g. 'gitea-api'."},
		{Name: "target_kind", Required: true, Description: "host | service — what this method unlocks."},
		{Name: "target_slug", Required: true, Description: "The host or service slug (must already be learned)."},
		{Name: "method", Required: true, Description: "ssh | https-api | https-basic | token | none."},
		{Name: "principal", Required: false, Description: "The login/API user, e.g. 'youruser', 'sophdn'."},
		{Name: "credential_pointer", Required: false, Description: "POINTER to where the secret lives (a path/env name like '~/.git-credentials'), NEVER the secret."},
		{Name: "scope_note", Required: false, Description: "Scope/caveat, e.g. 'admin', 'repo-scoped not org'."},
		{Name: "enabled", Required: false, Description: "Whether the method is currently usable (default true)."},
		{Name: "soft_ref", Required: false, Description: "Pointer to the vault how-to note."},
		{Name: "notes", Required: false, Description: "Freeform notes."},
	},
	Returns: &actionspec.ActionReturn{Shape: "LearnResult", Description: "ok + kind='access_method' + slug."},
	Errors: []actionspec.ActionError{
		{Condition: "credential_pointer looks like an inline secret", Message: "credential_pointer looks like an inline secret; store a POINTER instead"},
		{Condition: "the target is not learned", Message: "<kind>_not_found: <slug> (learn the target first)"},
	},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var canonResolveDoc = actionspec.ActionDoc{
	Purpose: "Deterministically resolve any token — a canonical name, a retired alias, an old path, or an old port — to its current canonical artifact identity. Answers \"what is X really called / where does it live, and is this name current or retired?\"",
	Params: []actionspec.DocParam{
		{Name: "token", Required: true, Description: "A canonical name, retired alias, path, or port, e.g. 'mcp-servers', 'corpos-toolkit', '~/dev/mcp-servers', '3000', ':3001'."},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "CanonRecord",
		Description: "resolved + kind + canonical value + status (current|retired) + replacement (for retired) + gitea_owner/local_path/port + a composed answer.",
	},
	Notes: "status='retired' means the token names a superseded artifact; `replacement` gives the current form. An un-learned token returns resolved=false with an 'unknown' answer, never a wrong canonical. soft_ref points at the vault retirement/history prose.",
}

var canonLearnDoc = actionspec.ActionDoc{
	Purpose: "Learn (upsert) a canonical artifact entry + its retired-alias set. Idempotent; rationale-gated. This is how the canonical-names map is populated as data.",
	Params: []actionspec.DocParam{
		{Name: "slug", Required: true, Description: "Stable canonical id, e.g. 'corpos-toolkit', 'toolkit-db', 'toolkit-port'."},
		{Name: "kind", Required: true, Description: "repo | path | project | db | port | service | other."},
		{Name: "canonical", Required: true, Description: "The canonical value, e.g. 'sophdn/corpos-toolkit', '~/.local/share/toolkit/data/toolkit.db', '3001'."},
		{Name: "status", Required: false, Description: "current | retired (default current)."},
		{Name: "replacement", Required: false, Description: "For a retired entry: the current canonical form/slug that supersedes it."},
		{Name: "gitea_owner", Required: false, Description: "For repo/service: 'sophdn' | 'shared' (the org-owner facet)."},
		{Name: "local_path", Required: false, Description: "For repo: '~/dev/corpos-toolkit'."},
		{Name: "port", Required: false, Description: "For port/service: the numeric port."},
		{Name: "aliases", Required: false, Description: "Retired/alternate tokens that resolve to this entry, e.g. ['mcp-servers','sophdn/toolkit','~/dev/mcp-servers']. Replaces the entry's alias set."},
		{Name: "notes", Required: false, Description: "Freeform notes."},
		{Name: "soft_ref", Required: false, Description: "Pointer to the vault retirement/history note."},
	},
	Returns:              &actionspec.ActionReturn{Shape: "LearnResult", Description: "ok + kind='canon_entry' + slug."},
	EnvelopeRequirements: standardRationaleEnvelope,
}

var canonListDoc = actionspec.ActionDoc{
	Purpose: "Enumerate the canonical-names map: every entry (slug, kind, canonical value, status) with its aliases.",
	Returns: &actionspec.ActionReturn{Shape: "CanonListResult", Description: "entries (with aliases) and a count."},
}

// ecosystemActionRegistry is the ordered, co-located descriptor registry — the
// single source of the ecosystem surface's action docs.
var ecosystemActionRegistry = []actionspec.ActionEntry{
	{Name: "access_check", Doc: accessCheckDoc, ParamStruct: reflect.TypeOf(accessCheckParams{})},
	{Name: "describe", Doc: describeDoc, ParamStruct: reflect.TypeOf(describeParams{})},
	{Name: "list", Doc: listDoc, ParamStruct: reflect.TypeOf(listParams{})},
	{Name: "host_learn", Doc: hostLearnDoc, ParamStruct: reflect.TypeOf(hostLearnParams{})},
	{Name: "service_learn", Doc: serviceLearnDoc, ParamStruct: reflect.TypeOf(serviceLearnParams{})},
	{Name: "access_learn", Doc: accessLearnDoc, ParamStruct: reflect.TypeOf(accessLearnParams{})},
	{Name: "canon_resolve", Doc: canonResolveDoc, ParamStruct: reflect.TypeOf(canonResolveParams{})},
	{Name: "canon_learn", Doc: canonLearnDoc, ParamStruct: reflect.TypeOf(canonLearnParams{})},
	{Name: "canon_list", Doc: canonListDoc, ParamStruct: reflect.TypeOf(struct{}{})},
}

// EcosystemActionSpecs returns the ecosystem surface's full action catalog,
// derived from the co-located descriptor registry. Projected into
// corpus/ecosystem/*.toml by cmd/action-docs-corpus-gen and served by
// admin.action_describe(ecosystem, X).
func EcosystemActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(ecosystemActionRegistry)
}
