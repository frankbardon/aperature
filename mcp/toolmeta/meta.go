// Package toolmeta holds the metadata table for Aperture's MCP tools: the single
// public source of truth for tool identity (name + description). It is imported
// by BOTH the SDK-free mcp/ core (for its catalog and schema reflection) and the
// mcp/gosdk adapter (whose RegisteredTools mirrors Names), so the two never drift.
//
// The package is pure data: it imports NO MCP SDK and no Aperture execution
// package, so it sits at the bottom of the firewall'd dep graph. The firewall
// test covers it alongside the core.
//
// Every tool here is READ-ONLY: the Aperture MCP surface decides (Check /
// Enumerate / Explain), simulates (what-if), and inspects the model. No tool
// mutates — the mutator verbs the no-mutation test bans (put / delete / create /
// update / bestow / revoke / grant / set / remove / write) never appear as a tool
// name. The decision facade's mutators are simply not surfaced.
package toolmeta

// Tool name constants. Stable, snake_cased, aperture_-prefixed.
const (
	// Decision API (single + bulk).
	ToolCheck          = "aperture_check"
	ToolCheckBatch     = "aperture_check_batch"
	ToolEnumerate      = "aperture_enumerate"
	ToolEnumerateBatch = "aperture_enumerate_batch"
	ToolSearch         = "aperture_search"
	ToolSearchBatch    = "aperture_search_batch"
	ToolExplain        = "aperture_explain"
	ToolExplainBatch   = "aperture_explain_batch"

	// What-if / simulate (read-only, never persisted).
	ToolSimulate = "aperture_simulate"

	// Model inspection (read the schema + grants).
	ToolListObjectTypes = "aperture_list_object_types"
	ToolGetObjectType   = "aperture_get_object_type"
	ToolListPermissions = "aperture_list_permissions"
	ToolGetPermission   = "aperture_get_permission"
	ToolListRoles       = "aperture_list_roles"
	ToolGetRole         = "aperture_get_role"
	ToolListGroups      = "aperture_list_groups"
	ToolGetGroup        = "aperture_get_group"
	ToolListPrincipals  = "aperture_list_principals"
	ToolGetPrincipal    = "aperture_get_principal"
	ToolListGrants      = "aperture_list_grants"
	ToolGetGrant        = "aperture_get_grant"

	// Embedded skill docs describing the surface.
	ToolSkillsList = "aperture_skills_list"
	ToolSkillsGet  = "aperture_skills_get"
)

// Description constants.
const (
	DescCheck           = "Decide whether a principal may take an action on an object, scoped to an account. Returns the verdict (allow/deny), a human-readable reason naming the deciding grant(s), and the deciding grant ids. Fail-closed: any operational failure (unknown principal, storage fault) renders as a DENY rather than an error; only an ill-formed question (bad identity) is an error. Use this for a single authorization question; for the full derivation use aperture_explain."
	DescCheckBatch      = "Decide many (account, principal, action, object) questions in one round-trip. Returns results aligned with the input queries (results[i] answers queries[i]); a single ill-formed query carries its error in that item without failing the batch. Use when authorizing a set of objects at once."
	DescEnumerate       = "List the object ids under a pattern that a principal may take an action on, in an account — the inverse of aperture_check. Every returned id is one aperture_check would allow (deny-overrides and specificity are honoured, so a denied object is never returned). Bounded by limit. Use to answer 'which of these may alice read?'."
	DescEnumerateBatch  = "Enumerate accessible objects for many queries in one round-trip, aligned with the input queries. A query that errors carries its error in its item; the rest are unaffected."
	DescSearch          = "Resolve a NAME to an object id: rank the objects a principal may take an action on, in an account, by how well their metadata matches free text — the lookup to do when a question arrives in a person's own words. Every returned object is one aperture_check would allow, because candidates are decided BEFORE they are scored: the result is always a subset of aperture_enumerate's for the same subject, action and pattern. Matching is case- and punctuation-insensitive and tolerates a typo. A score RANKS candidates for choosing between; it is never a permission, so still check an id before acting on it. Use this to turn 'Nike' into 'account:acme/brand:42'; use aperture_enumerate when you want the whole entitled set."
	DescSearchBatch     = "Resolve many names to object ids in one round-trip, aligned with the input queries (results[i] answers queries[i]). A query that errors carries its error in its item; the rest are unaffected. Use when one sentence names several things — a brand and a category, say."
	DescExplain         = "Return the full structured decision trace for a single question: the principal's expanded subject set, every grant considered with its per-grant outcome (action match, coverage, specificity), which grants decided the verdict, and the final decision. Use when you need to understand WHY a decision came out the way it did, not just the verdict."
	DescExplainBatch    = "Return decision traces for many questions in one round-trip, aligned with the input queries. A query that errors carries its error in its item."
	DescSimulate        = "What-if: render the decision (full trace) for a question as it WOULD be under a hypothetical overlay of principals, groups, permissions, grants, and memberships — WITHOUT writing anything. The overlay is additive; an overlay entity with the same id as a stored one shadows it, so you can model 'what if I bestowed this grant' or 'what if alice had this role'. Nothing is persisted and nothing is audited. Use to preview the effect of a change before making it."
	DescListObjectTypes = "List every object type in the model: the protected resource types (the 'type' half of an identity segment, e.g. document, project) with their declared action verb sets. Read-only schema inspection."
	DescGetObjectType   = "Fetch one object type by name, including its declared action verb set. Read-only."
	DescListPermissions = "List every permission: the (object-type, action, scope-strategy) records grants reference, including the delegatable flag. Read-only schema inspection."
	DescGetPermission   = "Fetch one permission by id. Read-only."
	DescListRoles       = "List every role: named bundles of permissions principals are assigned. Read-only schema inspection."
	DescGetRole         = "Fetch one role by id, including its permission bundle. Read-only."
	DescListGroups      = "List every group: collections of principals that can themselves be grant subjects. Read-only schema inspection."
	DescGetGroup        = "Fetch one group by id, including its member principal ids. Read-only."
	DescListPrincipals  = "List every principal (user or machine), including assigned role ids and identity strings. Read-only schema inspection."
	DescGetPrincipal    = "Fetch one principal by id, including its assigned roles. Read-only."
	DescListGrants      = "List every grant stamped to an account: the bindings of a subject (principal/role/group) to a permission, scoped to an object pattern with an allow/deny effect. Account-scoped, so a grant in another account is never returned. Read-only inspection of the authorization graph."
	DescGetGrant        = "Fetch one grant by id, including its subject, permission, object pattern, effect, and account. Read-only."
	DescSkillsList      = "List the embedded skill docs describing the Aperture MCP surface — how the decision, simulate, and inspection tools fit together. Fetch a doc body with aperture_skills_get."
	DescSkillsGet       = "Fetch the markdown body of a named MCP skill doc (e.g. 'mcp-surface'). The skill pack is the authoritative reference for how to drive this surface."
)

// ToolMeta is the canonical (name, description) record for one registered tool.
type ToolMeta struct {
	Name        string
	Description string
}

// Meta returns the canonical list of MCP tool metadata in stable registration
// order. The mcp/gosdk adapter's RegisteredTools mirrors Names() so server
// registration stays in lockstep with this table.
func Meta() []ToolMeta {
	return []ToolMeta{
		{Name: ToolCheck, Description: DescCheck},
		{Name: ToolCheckBatch, Description: DescCheckBatch},
		{Name: ToolEnumerate, Description: DescEnumerate},
		{Name: ToolEnumerateBatch, Description: DescEnumerateBatch},
		{Name: ToolSearch, Description: DescSearch},
		{Name: ToolSearchBatch, Description: DescSearchBatch},
		{Name: ToolExplain, Description: DescExplain},
		{Name: ToolExplainBatch, Description: DescExplainBatch},
		{Name: ToolSimulate, Description: DescSimulate},
		{Name: ToolListObjectTypes, Description: DescListObjectTypes},
		{Name: ToolGetObjectType, Description: DescGetObjectType},
		{Name: ToolListPermissions, Description: DescListPermissions},
		{Name: ToolGetPermission, Description: DescGetPermission},
		{Name: ToolListRoles, Description: DescListRoles},
		{Name: ToolGetRole, Description: DescGetRole},
		{Name: ToolListGroups, Description: DescListGroups},
		{Name: ToolGetGroup, Description: DescGetGroup},
		{Name: ToolListPrincipals, Description: DescListPrincipals},
		{Name: ToolGetPrincipal, Description: DescGetPrincipal},
		{Name: ToolListGrants, Description: DescListGrants},
		{Name: ToolGetGrant, Description: DescGetGrant},
		{Name: ToolSkillsList, Description: DescSkillsList},
		{Name: ToolSkillsGet, Description: DescSkillsGet},
	}
}

// Names returns the tool identifiers in the same order as Meta(). Stable.
func Names() []string {
	meta := Meta()
	out := make([]string, len(meta))
	for i, m := range meta {
		out[i] = m.Name
	}
	return out
}
