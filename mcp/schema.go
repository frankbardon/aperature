package mcp

import (
	"encoding/json"
	"reflect"

	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/google/jsonschema-go/jsonschema"
)

// ToolSchema is the reflected, SDK-free descriptor for one registered tool: its
// name, description, and the input/output JSON Schemas (draft 2020-12) reflected
// from the typed In/Out contract structs. The schemas are carried as
// json.RawMessage so consumers — and the thin go-sdk adapter — never need to
// import the schema reflector or any MCP SDK.
type ToolSchema struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// registry holds the reflected schema per tool name, plus a stable order
// mirroring toolmeta.Names(). reflectErrors records any reflection failure so
// schema_test.go can assert the set is empty rather than the package panicking at
// init (the recursive-type guard).
var (
	registry      = map[string]ToolSchema{}
	order         []string
	reflectErrors = map[string]error{}
)

// reflectSchema reflects a JSON Schema for t and marshals it to json.RawMessage.
// A fallback open-object schema is returned on error; the error is surfaced for
// recording. AddTool requires a non-nil object input schema, so the fallback is
// itself a valid object schema.
func reflectSchema(t reflect.Type) (json.RawMessage, error) {
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`), err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`), err
	}
	return json.RawMessage(body), nil
}

// register reflects the input/output schemas for one tool and stores the
// descriptor. Reflection errors are recorded (not panicked) so a single bad
// contract surfaces in the test rather than crashing every importer.
func register(name, description string, in, out reflect.Type) {
	inSchema, inErr := reflectSchema(in)
	outSchema, outErr := reflectSchema(out)
	if inErr != nil {
		reflectErrors[name+":in"] = inErr
	}
	if outErr != nil {
		reflectErrors[name+":out"] = outErr
	}
	registry[name] = ToolSchema{
		Name:         name,
		Description:  description,
		InputSchema:  inSchema,
		OutputSchema: outSchema,
	}
	order = append(order, name)
}

func init() {
	m := toolmeta.Meta()
	desc := make(map[string]string, len(m))
	for _, e := range m {
		desc[e.Name] = e.Description
	}
	d := func(name string) string { return desc[name] }

	register(toolmeta.ToolCheck, d(toolmeta.ToolCheck), reflect.TypeFor[CheckIn](), reflect.TypeFor[CheckOut]())
	register(toolmeta.ToolCheckBatch, d(toolmeta.ToolCheckBatch), reflect.TypeFor[CheckBatchIn](), reflect.TypeFor[CheckBatchOut]())
	register(toolmeta.ToolEnumerate, d(toolmeta.ToolEnumerate), reflect.TypeFor[EnumerateIn](), reflect.TypeFor[EnumerateOut]())
	register(toolmeta.ToolEnumerateBatch, d(toolmeta.ToolEnumerateBatch), reflect.TypeFor[EnumerateBatchIn](), reflect.TypeFor[EnumerateBatchOut]())
	register(toolmeta.ToolSearch, d(toolmeta.ToolSearch), reflect.TypeFor[SearchIn](), reflect.TypeFor[SearchOut]())
	register(toolmeta.ToolSearchBatch, d(toolmeta.ToolSearchBatch), reflect.TypeFor[SearchBatchIn](), reflect.TypeFor[SearchBatchOut]())
	register(toolmeta.ToolExplain, d(toolmeta.ToolExplain), reflect.TypeFor[ExplainIn](), reflect.TypeFor[ExplainOut]())
	register(toolmeta.ToolExplainBatch, d(toolmeta.ToolExplainBatch), reflect.TypeFor[ExplainBatchIn](), reflect.TypeFor[ExplainBatchOut]())
	register(toolmeta.ToolSimulate, d(toolmeta.ToolSimulate), reflect.TypeFor[SimulateIn](), reflect.TypeFor[SimulateOut]())
	register(toolmeta.ToolListObjectTypes, d(toolmeta.ToolListObjectTypes), reflect.TypeFor[emptyIn](), reflect.TypeFor[ListObjectTypesOut]())
	register(toolmeta.ToolGetObjectType, d(toolmeta.ToolGetObjectType), reflect.TypeFor[GetObjectTypeIn](), reflect.TypeFor[GetObjectTypeOut]())
	register(toolmeta.ToolListPermissions, d(toolmeta.ToolListPermissions), reflect.TypeFor[emptyIn](), reflect.TypeFor[ListPermissionsOut]())
	register(toolmeta.ToolGetPermission, d(toolmeta.ToolGetPermission), reflect.TypeFor[GetPermissionIn](), reflect.TypeFor[GetPermissionOut]())
	register(toolmeta.ToolListRoles, d(toolmeta.ToolListRoles), reflect.TypeFor[emptyIn](), reflect.TypeFor[ListRolesOut]())
	register(toolmeta.ToolGetRole, d(toolmeta.ToolGetRole), reflect.TypeFor[GetRoleIn](), reflect.TypeFor[GetRoleOut]())
	register(toolmeta.ToolListGroups, d(toolmeta.ToolListGroups), reflect.TypeFor[emptyIn](), reflect.TypeFor[ListGroupsOut]())
	register(toolmeta.ToolGetGroup, d(toolmeta.ToolGetGroup), reflect.TypeFor[GetGroupIn](), reflect.TypeFor[GetGroupOut]())
	register(toolmeta.ToolListPrincipals, d(toolmeta.ToolListPrincipals), reflect.TypeFor[emptyIn](), reflect.TypeFor[ListPrincipalsOut]())
	register(toolmeta.ToolGetPrincipal, d(toolmeta.ToolGetPrincipal), reflect.TypeFor[GetPrincipalIn](), reflect.TypeFor[GetPrincipalOut]())
	register(toolmeta.ToolListGrants, d(toolmeta.ToolListGrants), reflect.TypeFor[ListGrantsIn](), reflect.TypeFor[ListGrantsOut]())
	register(toolmeta.ToolGetGrant, d(toolmeta.ToolGetGrant), reflect.TypeFor[GetGrantIn](), reflect.TypeFor[GetGrantOut]())
	register(toolmeta.ToolSkillsList, d(toolmeta.ToolSkillsList), reflect.TypeFor[emptyIn](), reflect.TypeFor[SkillsListOut]())
	register(toolmeta.ToolSkillsGet, d(toolmeta.ToolSkillsGet), reflect.TypeFor[SkillsGetIn](), reflect.TypeFor[SkillsGetOut]())
}

// Schemas returns the reflected descriptor for every registered tool in stable
// registration order (matching toolmeta.Names()).
func Schemas() []ToolSchema {
	out := make([]ToolSchema, 0, len(order))
	for _, name := range order {
		out = append(out, registry[name])
	}
	return out
}

// SchemaFor returns the reflected descriptor for the named tool.
func SchemaFor(name string) (ToolSchema, bool) {
	ts, ok := registry[name]
	return ts, ok
}
