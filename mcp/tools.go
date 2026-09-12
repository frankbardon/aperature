package mcp

import (
	"context"
	"encoding/json"

	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/service"
)

// Config carries the runtime configuration baked into the tool catalog at
// construction time, so the catalog has NO dependency on process globals. Version
// is the server/build identity string; the adapter (mcp/gosdk) reads it to
// populate the server's advertised version. Reserved here so the single Config
// value is the one place runtime identity is injected.
type Config struct {
	Version string
}

// InvokeFunc is the type-erased entry point for one tool: it decodes raw
// arguments into the tool's typed In, calls the typed handler, and returns the
// typed Out as any. Coded errors from the facade are returned verbatim — the
// adapter renders a *errors.CodedError as the structured {code, message, details}
// envelope.
type InvokeFunc func(ctx context.Context, s *service.Service, raw json.RawMessage) (any, error)

// ToolDescriptor is the SDK-free, type-erased descriptor for one registered tool.
// It pairs the reflected input/output JSON Schemas (json.RawMessage, so no
// consumer needs the schema reflector) with an Invoke closure that round-trips
// raw JSON arguments through the typed handler. The go-sdk adapter mounts these
// onto a server via the low-level Server.AddTool path.
type ToolDescriptor struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Invoke       InvokeFunc
}

// decode unmarshals raw into In, tolerating an empty argument blob (the
// parameterless tools) by leaving the zero value.
func decode[In any](raw json.RawMessage) (In, error) {
	var in In
	if len(raw) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err
	}
	return in, nil
}

// makeInvoke composes the JSON decode with a typed handler into the type-erased
// InvokeFunc. Errors (decode or handler) are returned verbatim.
func makeInvoke[In, Out any](h func(context.Context, *service.Service, In) (Out, error)) InvokeFunc {
	return func(ctx context.Context, s *service.Service, raw json.RawMessage) (any, error) {
		in, err := decode[In](raw)
		if err != nil {
			return nil, err
		}
		out, err := h(ctx, s, in)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
}

// invokers maps each tool name to its type-erased Invoke. cfg is threaded in so
// future config-dependent closures can capture it; today the catalog is
// config-independent.
func invokers(_ Config) map[string]InvokeFunc {
	return map[string]InvokeFunc{
		toolmeta.ToolCheck:           makeInvoke(handleCheck),
		toolmeta.ToolCheckBatch:      makeInvoke(handleCheckBatch),
		toolmeta.ToolEnumerate:       makeInvoke(handleEnumerate),
		toolmeta.ToolEnumerateBatch:  makeInvoke(handleEnumerateBatch),
		toolmeta.ToolSearch:          makeInvoke(handleSearch),
		toolmeta.ToolSearchBatch:     makeInvoke(handleSearchBatch),
		toolmeta.ToolExplain:         makeInvoke(handleExplain),
		toolmeta.ToolExplainBatch:    makeInvoke(handleExplainBatch),
		toolmeta.ToolSimulate:        makeInvoke(handleSimulate),
		toolmeta.ToolListObjectTypes: makeInvoke(handleListObjectTypes),
		toolmeta.ToolGetObjectType:   makeInvoke(handleGetObjectType),
		toolmeta.ToolListPermissions: makeInvoke(handleListPermissions),
		toolmeta.ToolGetPermission:   makeInvoke(handleGetPermission),
		toolmeta.ToolListRoles:       makeInvoke(handleListRoles),
		toolmeta.ToolGetRole:         makeInvoke(handleGetRole),
		toolmeta.ToolListGroups:      makeInvoke(handleListGroups),
		toolmeta.ToolGetGroup:        makeInvoke(handleGetGroup),
		toolmeta.ToolListPrincipals:  makeInvoke(handleListPrincipals),
		toolmeta.ToolGetPrincipal:    makeInvoke(handleGetPrincipal),
		toolmeta.ToolListGrants:      makeInvoke(handleListGrants),
		toolmeta.ToolGetGrant:        makeInvoke(handleGetGrant),
		toolmeta.ToolSkillsList:      makeInvoke(handleSkillsList),
		toolmeta.ToolSkillsGet:       makeInvoke(handleSkillsGet),
	}
}

// Tools returns the full type-erased tool catalog in stable order (matching
// toolmeta.Names() and Schemas()). Each descriptor carries the reflected
// input/output schema (from the init-time registry) and a config-baked Invoke.
func Tools(cfg Config) []ToolDescriptor {
	inv := invokers(cfg)
	names := toolmeta.Names()
	out := make([]ToolDescriptor, 0, len(names))
	for _, name := range names {
		ts, ok := SchemaFor(name)
		if !ok {
			continue
		}
		out = append(out, ToolDescriptor{
			Name:         name,
			Description:  ts.Description,
			InputSchema:  ts.InputSchema,
			OutputSchema: ts.OutputSchema,
			Invoke:       inv[name],
		})
	}
	return out
}
