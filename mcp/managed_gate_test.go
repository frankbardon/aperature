package mcp

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// E1-S4, the MCP half. The APERTURE_MANAGE_* switches have NO MCP surface, and
// that is a decision rather than an oversight: mcp/handlers.go exposes only
// decide / simulate / inspect handlers, so there is nothing here for the gate to
// refuse. An absence is the one kind of design decision that leaves no trace in
// the code, which is exactly why it is written down as a test — a comment saying
// "no mutation tools exist" quietly becomes false the day someone adds one,
// while these fail.
//
// mcp/surface_test.go already bans mutating VERBS in tool NAMES. That is a check
// on vocabulary; a tool named aperture_manage_tenant would sail through it. What
// follows checks the two things that actually matter: that no handler calls a
// gated facade mutator, and that no tool in the catalog can produce the coded
// refusal at all.

// gatedFacadeMethods are the seven service.Service methods the entity-management
// gate guards. If an MCP handler ever calls one, the MCP surface has acquired a
// write path — and with it an obligation this story's other tests would not
// cover, because they only know about Twirp and the CLI.
var gatedFacadeMethods = []string{
	"PutAccount", "DeleteAccount",
	"PutPrincipal", "DeletePrincipal",
	"PutMembership", "DeleteMembership",
	"Import",
}

// readOnlyHandlerPrefixes are the shapes a READ handler's name may take. The
// list is deliberately short: adding a prefix to it is the moment to ask whether
// the new handler writes, and if it does, whether the gate reaches it.
var readOnlyHandlerPrefixes = []string{
	"handleCheck", "handleEnumerate", "handleExplain", "handleSearch", "handleSimulate",
	"handleList", "handleGet", "handleSkills",
}

// mcpSourceFiles returns the package's non-test Go files, failing (never
// skipping) if the directory cannot be read — a scan that silently covers
// nothing is worse than no scan.
func mcpSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("no non-test Go files found in mcp/ — the scan below would assert nothing")
	}
	sort.Strings(out)
	return out
}

// TestMCPExposesNoMutationHandler is the durable form of "MCP has nothing to
// gate": it parses every non-test file in the package and fails if any of them
// calls one of the gated facade mutators, or defines a handler whose name is not
// one of the read-only shapes.
//
// The scan is on the CALL, not on the tool name, because that is where a bypass
// would actually live. A future aperture_provision tool would trip this on the
// day it is written — and the fix is not to relax the test, it is to gate the
// new tool and add it to E1-S4's coverage.
func TestMCPExposesNoMutationHandler(t *testing.T) {
	fset := token.NewFileSet()
	banned := make(map[string]bool, len(gatedFacadeMethods))
	for _, m := range gatedFacadeMethods {
		banned[m] = true
	}

	var handlers int
	for _, name := range mcpSourceFiles(t) {
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if ok && banned[sel.Sel.Name] {
					t.Errorf("%s calls the gated facade mutator %s — the MCP surface must expose no mutation, "+
						"and a new write path here needs its own entity-management gate",
						fset.Position(node.Pos()), sel.Sel.Name)
				}
			case *ast.FuncDecl:
				if node.Recv != nil || !strings.HasPrefix(node.Name.Name, "handle") {
					return true
				}
				handlers++
				for _, p := range readOnlyHandlerPrefixes {
					if strings.HasPrefix(node.Name.Name, p) {
						return true
					}
				}
				t.Errorf("%s: handler %s is not one of the read-only shapes %v — if it mutates, it needs a gate",
					fset.Position(node.Pos()), node.Name.Name, readOnlyHandlerPrefixes)
			}
			return true
		})
	}
	if handlers == 0 {
		t.Fatal("found no handle* functions — the scan matched nothing and would pass vacuously")
	}
}

// lockedService wires the read stack every MCP tool needs, with ALL THREE entity
// kinds explicitly unmanaged: the harshest posture an operator can configure.
func lockedService(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.Setup(ctx))
	must(store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(store.PutPermission(ctx, model.Permission{ID: "p-read", ObjectType: "document", Action: "read"}))
	must(store.PutRole(ctx, model.Role{ID: "reader", Name: "Reader", PermissionIDs: []string{"p-read"}}))
	must(store.PutGroup(ctx, model.Group{ID: "readers", Name: "Readers", MemberPrincipalIDs: []string{"alice"}}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-read", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	return service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithManagedEntities(service.ManagedEntities{
			Accounts:    service.ManagedNo,
			Principals:  service.ManagedNo,
			Memberships: service.ManagedNo,
		}),
	)
}

// TestMCPCatalogCannotProduceTheUnmanagedRefusal is the behavioural companion to
// the source scan: it invokes EVERY tool in the catalog against a facade with
// all three kinds locked, and asserts none of them ever answers
// APERTURE_ENTITY_UNMANAGED.
//
// Two things follow from a green run. The obvious one is that the switches have
// no MCP surface — an agent's view of Aperture is identical whether the operator
// locked every entity kind or none. The less obvious one is the reason: a tool
// that CAN return that code is by definition a tool that reaches a gated
// mutator, so this fails the moment MCP grows a write, whatever it is called and
// whichever handler shape it uses.
func TestMCPCatalogCannotProduceTheUnmanagedRefusal(t *testing.T) {
	svc := lockedService(t)
	ctx := context.Background()

	// A representative argument blob per tool. Tools not listed are invoked with
	// no arguments, which the parameterless ones expect and the rest reject as a
	// decode/validation error — either way, never as an unmanaged refusal.
	args := map[string]string{
		"aperture_check":           `{"account":"acme","principal":"alice","action":"read","object":"account:acme/document:1"}`,
		"aperture_explain":         `{"account":"acme","principal":"alice","action":"read","object":"account:acme/document:1"}`,
		"aperture_enumerate":       `{"account":"acme","principal":"alice","action":"read","pattern":"account:acme/**"}`,
		"aperture_get_object_type": `{"name":"document"}`,
		"aperture_get_permission":  `{"id":"p-read"}`,
		"aperture_get_role":        `{"id":"reader"}`,
		"aperture_get_group":       `{"id":"readers"}`,
		"aperture_get_principal":   `{"id":"alice"}`,
		"aperture_get_grant":       `{"id":"g-read"}`,
		"aperture_list_grants":     `{"account":"acme"}`,
		"aperture_skills_get":      `{"name":"mcp-surface"}`,
	}

	tools := Tools(Config{Version: "test"})
	if len(tools) == 0 {
		t.Fatal("empty tool catalog — this test would assert nothing")
	}
	for _, d := range tools {
		t.Run(d.Name, func(t *testing.T) {
			var raw json.RawMessage
			if a, ok := args[d.Name]; ok {
				raw = json.RawMessage(a)
			}
			_, err := d.Invoke(ctx, svc, raw)
			if got := aerr.CodeOf(err); got == aerr.APERTURE_ENTITY_UNMANAGED {
				t.Fatalf("tool %s returned %s — the MCP surface must expose no gated mutation",
					d.Name, aerr.APERTURE_ENTITY_UNMANAGED)
			}
		})
	}
}

// TestMCPReadsAreUnaffectedByALockedPosture is the other half of "no MCP
// surface": not merely that the tools cannot report the refusal, but that they
// still WORK. Locking an entity kind is about who writes it, never about who may
// look at it, so an agent inspecting a fully-locked deployment must see the same
// model it would see in an open one.
func TestMCPReadsAreUnaffectedByALockedPosture(t *testing.T) {
	ctx := context.Background()
	locked := lockedService(t)

	out, err := invokerFor(t, "aperture_list_principals")(ctx, locked, nil)
	if err != nil {
		t.Fatalf("listing principals under a locked posture: %v", err)
	}
	list, ok := out.(ListPrincipalsOut)
	if !ok {
		t.Fatalf("unexpected output type %T", out)
	}
	if len(list.Principals) != 1 || list.Principals[0].ID != "alice" {
		t.Fatalf("locked posture changed what an agent can read: %+v", list.Principals)
	}

	dec, err := invokerFor(t, "aperture_check")(ctx, locked,
		json.RawMessage(`{"account":"acme","principal":"alice","action":"read","object":"account:acme/document:1"}`))
	if err != nil {
		t.Fatalf("check under a locked posture: %v", err)
	}
	if !dec.(CheckOut).Allow {
		t.Fatalf("the decision path must not consult the entity switches: %+v", dec)
	}
}
