// Package mcp is the SDK-free core of Aperture's MCP surface. It defines the
// typed In/Out contract for every registered tool, reflects an input + output
// JSON Schema for each at package-init time (carried as json.RawMessage), and
// holds the handlers that drive the single service.Service facade.
//
// The package imports NO MCP SDK. It depends only on the decision facade
// (github.com/frankbardon/aperture/service) and the domain types the facade
// returns (engine, model), the coded-errors package, the embedded skill docs
// (mcp/skills, stdlib-only), and the schema reflector
// github.com/google/jsonschema-go. A thin adapter (mcp/gosdk) mounts these
// descriptors onto a caller-supplied go-sdk server via the low-level
// Server.AddTool path, which accepts any value that JSON-marshals to a valid
// 2020-12 schema — exactly the json.RawMessage this package emits. Keeping the
// schema carrier type-erased is what lets the firewall guarantee hold: external
// code can import this contract without pulling an MCP SDK. firewall_test.go
// makes that load-bearing.
//
// READ-ONLY: every tool calls a facade READ or DECISION method (Check /
// Enumerate / Explain / Simulate / Get* / List*). No handler calls a mutator, and
// surface_test.go asserts no registered tool name carries a mutating verb.
//
// Recursive-type note (Lattice rule): jsonschema-go's reflector returns an
// error on a Go-level type cycle. The contract types below (the decision Query /
// Result, engine.Trace, the flat model entities) are all non-cyclic, so direct
// reflection succeeds; schema.go records any reflection error per tool so
// schema_test.go fails loudly rather than the package panicking. Were a future
// field to introduce a cycle, type it as `any` in the In/Out struct so reflection
// stays error-free.
package mcp

import (
	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/mcp/skills"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"
)

// --- Decision inputs (alias the facade's surface-neutral query types) --------

// CheckIn is the input for aperture_check.
type CheckIn = service.Query

// EnumerateIn is the input for aperture_enumerate.
type EnumerateIn = service.EnumerateQuery

// SearchIn is the input for aperture_search.
type SearchIn = service.SearchQuery

// ExplainIn is the input for aperture_explain.
type ExplainIn = service.Query

// CheckBatchIn is the input for aperture_check_batch.
type CheckBatchIn struct {
	Queries []service.Query `json:"queries" jsonschema:"The authorization questions to decide, in order"`
}

// SearchBatchIn is the input for aperture_search_batch.
type SearchBatchIn struct {
	Queries []service.SearchQuery `json:"queries" jsonschema:"The name-resolution questions to answer, in order"`
}

// EnumerateBatchIn is the input for aperture_enumerate_batch.
type EnumerateBatchIn struct {
	Queries []service.EnumerateQuery `json:"queries" jsonschema:"The enumeration questions to answer, in order"`
}

// ExplainBatchIn is the input for aperture_explain_batch.
type ExplainBatchIn struct {
	Queries []service.Query `json:"queries" jsonschema:"The questions to trace, in order"`
}

// --- Simulate input ----------------------------------------------------------

// SimulateIn is the input for aperture_simulate: a question plus the hypothetical
// overlay to evaluate it against. Nothing in the overlay is ever persisted.
type SimulateIn struct {
	Overlay service.Overlay `json:"overlay" jsonschema:"Hypothetical principals/groups/permissions/grants/memberships to layer over the live model for this evaluation only"`
	Query   service.Query   `json:"query" jsonschema:"The authorization question to evaluate under the overlay"`
}

// --- Model-inspection inputs -------------------------------------------------

// nameIn is the input for the by-name lookups (object types).
type GetObjectTypeIn struct {
	Name string `json:"name" jsonschema:"Object-type name (the identity-segment type, e.g. 'document')"`
}

// idIn is the input for the by-id entity lookups.
type GetPermissionIn struct {
	ID string `json:"id" jsonschema:"Permission id"`
}

// GetRoleIn is the input for aperture_get_role.
type GetRoleIn struct {
	ID string `json:"id" jsonschema:"Role id"`
}

// GetGroupIn is the input for aperture_get_group.
type GetGroupIn struct {
	ID string `json:"id" jsonschema:"Group id"`
}

// GetPrincipalIn is the input for aperture_get_principal.
type GetPrincipalIn struct {
	ID string `json:"id" jsonschema:"Principal id"`
}

// GetGrantIn is the input for aperture_get_grant.
type GetGrantIn struct {
	ID string `json:"id" jsonschema:"Grant id"`
}

// ListGrantsIn is the input for aperture_list_grants: grants are account-scoped.
type ListGrantsIn struct {
	Account string `json:"account" jsonschema:"The account whose grants to list"`
}

// emptyIn is the (empty) input for the parameterless list tools.
type emptyIn struct{}

// SkillsGetIn is the input for aperture_skills_get.
type SkillsGetIn struct {
	Name string `json:"name" jsonschema:"Skill doc name (e.g. 'mcp-surface')"`
}

// --- Outputs -----------------------------------------------------------------

// CheckOut is the output for aperture_check.
type CheckOut = service.Result

// ExplainOut is the output for aperture_explain (the public decision trace).
type ExplainOut = engine.Trace

// EnumerateOut is the output for aperture_enumerate.
type EnumerateOut struct {
	Objects []string `json:"objects" jsonschema:"Object ids the principal may take the action on, within the pattern"`
}

// SearchOut is the output for aperture_search: the ranked candidates, best
// first. Each carries the object's metadata inline, so naming what was found
// costs no further call.
type SearchOut struct {
	Matches []service.SearchMatch `json:"matches" jsonschema:"Ranked candidates, best match first. Each is an object the principal may act on; the score ranks, it does not authorize"`
}

// BatchItem is the JSON-friendly form of engine.BatchResult: a per-item result
// plus an error STRING (engine.BatchResult carries a Go error, which does not
// round-trip through JSON). Exactly one of Result / Error is meaningful: when
// Error is non-empty the item failed and Result is the zero value.
type BatchItem[T any] struct {
	Result T      `json:"result"`
	Error  string `json:"error,omitempty" jsonschema:"The item's error message when it failed; empty on success"`
}

// CheckBatchOut is the output for aperture_check_batch.
type CheckBatchOut struct {
	Results []BatchItem[service.Result] `json:"results" jsonschema:"Per-query results, aligned with the input queries"`
}

// EnumerateBatchOut is the output for aperture_enumerate_batch.
type EnumerateBatchOut struct {
	Results []BatchItem[[]string] `json:"results" jsonschema:"Per-query object-id lists, aligned with the input queries"`
}

// SearchBatchOut is the output for aperture_search_batch.
type SearchBatchOut struct {
	Results []BatchItem[[]service.SearchMatch] `json:"results" jsonschema:"Per-query ranked candidate lists, aligned with the input queries"`
}

// ExplainBatchOut is the output for aperture_explain_batch.
type ExplainBatchOut struct {
	Results []BatchItem[engine.Trace] `json:"results" jsonschema:"Per-query decision traces, aligned with the input queries"`
}

// SimulateOut is the output for aperture_simulate: the full trace under the
// overlay.
type SimulateOut = engine.Trace

// ListObjectTypesOut is the output for aperture_list_object_types.
type ListObjectTypesOut struct {
	ObjectTypes []model.ObjectType `json:"object_types" jsonschema:"Every object type in the model"`
}

// GetObjectTypeOut is the output for aperture_get_object_type.
type GetObjectTypeOut = model.ObjectType

// ListPermissionsOut is the output for aperture_list_permissions.
type ListPermissionsOut struct {
	Permissions []model.Permission `json:"permissions" jsonschema:"Every permission in the model"`
}

// GetPermissionOut is the output for aperture_get_permission.
type GetPermissionOut = model.Permission

// ListRolesOut is the output for aperture_list_roles.
type ListRolesOut struct {
	Roles []model.Role `json:"roles" jsonschema:"Every role in the model"`
}

// GetRoleOut is the output for aperture_get_role.
type GetRoleOut = model.Role

// ListGroupsOut is the output for aperture_list_groups.
type ListGroupsOut struct {
	Groups []model.Group `json:"groups" jsonschema:"Every group in the model"`
}

// GetGroupOut is the output for aperture_get_group.
type GetGroupOut = model.Group

// ListPrincipalsOut is the output for aperture_list_principals.
type ListPrincipalsOut struct {
	Principals []model.Principal `json:"principals" jsonschema:"Every principal in the model"`
}

// GetPrincipalOut is the output for aperture_get_principal.
type GetPrincipalOut = model.Principal

// ListGrantsOut is the output for aperture_list_grants.
type ListGrantsOut struct {
	Grants []model.Grant `json:"grants" jsonschema:"Every grant stamped to the account"`
}

// GetGrantOut is the output for aperture_get_grant.
type GetGrantOut = model.Grant

// SkillsListOut is the output for aperture_skills_list.
type SkillsListOut struct {
	Skills []skills.Metadata `json:"skills" jsonschema:"Embedded MCP skill-doc entries (name, description, applies_to)"`
}

// SkillsGetOut is the output for aperture_skills_get.
type SkillsGetOut struct {
	Body string `json:"body" jsonschema:"Markdown body of the named skill doc"`
}
