package mcp

import (
	"context"
	"errors"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/mcp/skills"
	"github.com/frankbardon/aperture/service"
)

// This file holds the SDK-free typed tool handlers: one
// func(ctx, *service.Service, In) (Out, error) per registered tool. Each handler
// calls the decision facade's READ or DECISION methods exclusively and returns
// the facade's errors verbatim — coded errors are never flattened to strings
// here; the adapter (mcp/gosdk) renders a *errors.CodedError as the structured
// {code, message, details} envelope. The type-erased ToolDescriptor.Invoke
// (tools.go) wraps these with the JSON decode. NO handler calls a mutator.

// errMissingArg is the plain validation error a tool returns for an absent
// required argument. It is intentionally NOT a coded error: it is surfaced to the
// LLM as a tool-error result so it can self-correct.
func errMissingArg(name string) error {
	return errors.New("missing or invalid '" + name + "'")
}

// --- Decision ---------------------------------------------------------------

func handleCheck(ctx context.Context, s *service.Service, in CheckIn) (CheckOut, error) {
	return s.Check(ctx, in)
}

func handleEnumerate(ctx context.Context, s *service.Service, in EnumerateIn) (EnumerateOut, error) {
	ids, err := s.Enumerate(ctx, in)
	if err != nil {
		return EnumerateOut{}, err
	}
	if ids == nil {
		ids = []string{}
	}
	return EnumerateOut{Objects: ids}, nil
}

func handleSearch(ctx context.Context, s *service.Service, in SearchIn) (SearchOut, error) {
	res, err := s.Search(ctx, in)
	if err != nil {
		return SearchOut{}, err
	}
	if res == nil {
		res = []service.SearchMatch{}
	}
	return SearchOut{Matches: res}, nil
}

func handleSearchBatch(ctx context.Context, s *service.Service, in SearchBatchIn) (SearchBatchOut, error) {
	res := s.SearchBatch(ctx, in.Queries)
	out := make([]BatchItem[[]service.SearchMatch], len(res))
	for i, r := range res {
		ms := r.Result
		if ms == nil {
			ms = []service.SearchMatch{}
		}
		out[i] = BatchItem[[]service.SearchMatch]{Result: ms, Error: errText(r.Err)}
	}
	return SearchBatchOut{Results: out}, nil
}

func handleExplain(ctx context.Context, s *service.Service, in ExplainIn) (ExplainOut, error) {
	return s.Explain(ctx, in)
}

func handleCheckBatch(ctx context.Context, s *service.Service, in CheckBatchIn) (CheckBatchOut, error) {
	res := s.CheckBatch(ctx, in.Queries)
	return CheckBatchOut{Results: toBatchItems(res)}, nil
}

func handleEnumerateBatch(ctx context.Context, s *service.Service, in EnumerateBatchIn) (EnumerateBatchOut, error) {
	res := s.EnumerateBatch(ctx, in.Queries)
	out := make([]BatchItem[[]string], len(res))
	for i, r := range res {
		ids := r.Result
		if ids == nil {
			ids = []string{}
		}
		out[i] = BatchItem[[]string]{Result: ids, Error: errText(r.Err)}
	}
	return EnumerateBatchOut{Results: out}, nil
}

func handleExplainBatch(ctx context.Context, s *service.Service, in ExplainBatchIn) (ExplainBatchOut, error) {
	res := s.ExplainBatch(ctx, in.Queries)
	return ExplainBatchOut{Results: toBatchItems(res)}, nil
}

// --- Simulate (what-if, read-only) ------------------------------------------

func handleSimulate(ctx context.Context, s *service.Service, in SimulateIn) (SimulateOut, error) {
	return s.SimulateExplain(ctx, in.Overlay, in.Query)
}

// --- Model inspection -------------------------------------------------------

func handleListObjectTypes(ctx context.Context, s *service.Service, _ emptyIn) (ListObjectTypesOut, error) {
	ots, err := s.ListObjectTypes(ctx)
	return ListObjectTypesOut{ObjectTypes: ots}, err
}

func handleGetObjectType(ctx context.Context, s *service.Service, in GetObjectTypeIn) (GetObjectTypeOut, error) {
	if in.Name == "" {
		return GetObjectTypeOut{}, errMissingArg("name")
	}
	return s.GetObjectType(ctx, in.Name)
}

func handleListPermissions(ctx context.Context, s *service.Service, _ emptyIn) (ListPermissionsOut, error) {
	ps, err := s.ListPermissions(ctx)
	return ListPermissionsOut{Permissions: ps}, err
}

func handleGetPermission(ctx context.Context, s *service.Service, in GetPermissionIn) (GetPermissionOut, error) {
	if in.ID == "" {
		return GetPermissionOut{}, errMissingArg("id")
	}
	return s.GetPermission(ctx, in.ID)
}

func handleListRoles(ctx context.Context, s *service.Service, _ emptyIn) (ListRolesOut, error) {
	rs, err := s.ListRoles(ctx)
	return ListRolesOut{Roles: rs}, err
}

func handleGetRole(ctx context.Context, s *service.Service, in GetRoleIn) (GetRoleOut, error) {
	if in.ID == "" {
		return GetRoleOut{}, errMissingArg("id")
	}
	return s.GetRole(ctx, in.ID)
}

func handleListGroups(ctx context.Context, s *service.Service, _ emptyIn) (ListGroupsOut, error) {
	gs, err := s.ListGroups(ctx)
	return ListGroupsOut{Groups: gs}, err
}

func handleGetGroup(ctx context.Context, s *service.Service, in GetGroupIn) (GetGroupOut, error) {
	if in.ID == "" {
		return GetGroupOut{}, errMissingArg("id")
	}
	return s.GetGroup(ctx, in.ID)
}

func handleListPrincipals(ctx context.Context, s *service.Service, _ emptyIn) (ListPrincipalsOut, error) {
	ps, err := s.ListPrincipals(ctx, service.Actor{})
	return ListPrincipalsOut{Principals: ps}, err
}

func handleGetPrincipal(ctx context.Context, s *service.Service, in GetPrincipalIn) (GetPrincipalOut, error) {
	if in.ID == "" {
		return GetPrincipalOut{}, errMissingArg("id")
	}
	return s.GetPrincipal(ctx, in.ID)
}

func handleListGrants(ctx context.Context, s *service.Service, in ListGrantsIn) (ListGrantsOut, error) {
	if in.Account == "" {
		return ListGrantsOut{}, errMissingArg("account")
	}
	gs, err := s.ListGrants(ctx, service.Actor{}, in.Account)
	return ListGrantsOut{Grants: gs}, err
}

func handleGetGrant(ctx context.Context, s *service.Service, in GetGrantIn) (GetGrantOut, error) {
	if in.ID == "" {
		return GetGrantOut{}, errMissingArg("id")
	}
	return s.GetGrant(ctx, service.Actor{}, in.ID)
}

// --- Skill docs -------------------------------------------------------------

func handleSkillsList(_ context.Context, _ *service.Service, _ emptyIn) (SkillsListOut, error) {
	return SkillsListOut{Skills: skills.List()}, nil
}

func handleSkillsGet(_ context.Context, _ *service.Service, in SkillsGetIn) (SkillsGetOut, error) {
	if in.Name == "" {
		return SkillsGetOut{}, errMissingArg("name")
	}
	body, ok := skills.Get(in.Name)
	if !ok {
		return SkillsGetOut{}, errors.New("skill \"" + in.Name + "\" not found")
	}
	return SkillsGetOut{Body: body}, nil
}

// --- Batch helpers ----------------------------------------------------------

// toBatchItems maps engine.BatchResult items to JSON-friendly BatchItems,
// folding each item's Go error into an error string.
func toBatchItems[T any](res []engine.BatchResult[T]) []BatchItem[T] {
	out := make([]BatchItem[T], len(res))
	for i, r := range res {
		out[i] = BatchItem[T]{Result: r.Result, Error: errText(r.Err)}
	}
	return out
}

// errText returns err's message, or "" when err is nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
