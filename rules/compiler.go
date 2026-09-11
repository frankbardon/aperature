package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/expr-lang/expr/vm/runtime"

	aerr "github.com/frankbardon/aperture/errors"
)

// evalEnv is the typed expression environment every rule compiles and evaluates
// against. It is a struct (not a bare map) so the expression type-checker treats
// the four roots as the ONLY valid top-level identifiers: a reference to anything
// else is an unknown name at compile time. The metadata roots are map[string]any
// so a rule reads host-defined fields dynamically (object.classification), while
// action is typed string so misusing it (action.foo) is a type error.
//
// The expr struct tags fix the identifier each field is exposed under.
// Notes is the per-evaluation notes sink the guarded dispatcher writes to. It is
// exposed under an identifier no rule can reference (allowedRoots does not list
// it), so it is reachable only from the expression the renderer emits. It may be
// nil — the decision hot path leaves it unset, and every NoteCollector method is
// nil-safe.
//
// Now is the decision instant (now.go) the same way: a per-decision snapshot of
// the engine's clock, exposed under an identifier that is not a variable root, so
// only a renderer-emitted expression can read it. Keeping it OUT of allowedRoots
// is a security boundary, not a style choice — reflective method calls survive
// expr.DisableAllBuiltins, so an exposed `NOW` root would make
// `NOW.AddDate(0, -3, 0)` a well-formed var path. It is always UTC, and its zero
// value means no reference instant was supplied (a directly-built Input); a date
// operator that needs one denies rather than resolving against year 1.
type evalEnv struct {
	Object    map[string]any `expr:"object"`
	Principal map[string]any `expr:"principal"`
	Account   map[string]any `expr:"account"`
	Action    string         `expr:"action"`
	Notes     *NoteCollector `expr:"__notes"`
	Now       time.Time      `expr:"__now"`
}

// notesVar is the identifier evalEnv.Notes is exposed under, and the name
// renderGuarded passes to the dispatcher.
const notesVar = "__notes"

// nowVar is the identifier evalEnv.Now is exposed under — the name a renderer
// passes to a dispatcher that needs the decision instant. Like notesVar it is
// absent from allowedRoots, so no rule can name it.
const nowVar = "__now"

// Input is the per-evaluation context a compiled rule reads. Object is the
// object's metadata snapshot (treated read-only — never mutated), Principal and
// Account are attribute bags, and Action is the action verb. A nil map is read as
// empty.
//
// Now is the decision's reference instant — the value a rule's date arithmetic
// resolves against. An Engine fills it from its injected Clock exactly once per
// decision (see now.go), so a rule that refers to "now" twice cannot straddle a
// tick and an Explain trace can be reproduced against a recorded instant. A
// caller building an Input by hand may leave it zero: that means "no reference
// instant is available", which any date operator needing one treats as deny-safe
// rather than resolving against year 1. It is normalised to UTC when the
// evaluation environment is built, so a non-UTC value cannot reach a comparison.
type Input struct {
	Object    map[string]any
	Principal map[string]any
	Account   map[string]any
	Action    string
	Now       time.Time
}

// env builds the tagged evaluation environment from the public Input, binding the
// per-evaluation notes sink (nil on the decision hot path) and the decision
// instant.
//
// The UTC conversion is repeated here rather than trusted from the snapshot
// boundary: env is the last point before evaluation and it is also the path a
// direct library caller takes with a hand-built Input, so normalising here means
// no zone reaches a comparison by any route. It costs nothing — Time.UTC only
// rewrites the value's location pointer.
func (in Input) env(notes *NoteCollector) evalEnv {
	return evalEnv{
		Object:    in.Object,
		Principal: in.Principal,
		Account:   in.Account,
		Action:    in.Action,
		Notes:     notes,
		Now:       in.Now.UTC(),
	}
}

// Compiled is a rule compiled to a reusable program. It is immutable and safe for
// concurrent evaluation, and carries the canonical source + hash the cache keys
// on. Build one with Compiler.Compile (or get a cached one from an Engine).
type Compiled struct {
	program *vm.Program
	source  string
	// key is the cache key: the raw sha256 of source. An array, not its hex
	// string, because a lookup runs on EVERY evaluation and a [32]byte is
	// directly map-comparable without allocating.
	key  [32]byte
	hash string
}

// Source returns the canonical expr-lang expression the rule rendered to.
func (c *Compiled) Source() string { return c.source }

// Hash returns the rule's canonical hash — the cache key (sha256 of Source).
func (c *Compiled) Hash() string { return c.hash }

// Eval runs the compiled rule against in and returns its boolean result.
// Evaluation is pure: it reads only in, mutates nothing, and exposes no
// nondeterministic function. A runtime failure or a non-boolean result is an
// APERTURE_RULE_EVAL coded error.
//
// The context carries the evaluation-notes collector (notes.go). When one is
// installed — Explain does that; Check and Enumerate do not — any diagnostic the
// evaluation records is published to it. Without one, nothing is recorded and
// nothing is allocated. Notes never influence the result.
func (c *Compiled) Eval(ctx context.Context, in Input) (bool, error) {
	return c.eval(in, NoteCollectorFrom(ctx))
}

// EvalWithNotes runs the compiled rule and RETURNS the notes the evaluation
// recorded instead of publishing them to the context's collector. It is the
// direct-library entry point (and what the Engine uses so it can stamp each note
// with the rule reference before publishing). The context is accepted for
// symmetry with Eval; expression evaluation itself does not block on it.
func (c *Compiled) EvalWithNotes(_ context.Context, in Input) (bool, []Note, error) {
	sink := &NoteCollector{}
	b, err := c.eval(in, sink)
	return b, sink.Notes(), err
}

// eval is the single evaluation body. notes may be nil, which is the hot path:
// the guarded dispatcher then records nothing and behaves identically.
func (c *Compiled) eval(in Input, notes *NoteCollector) (bool, error) {
	out, err := expr.Run(c.program, in.env(notes))
	if err != nil {
		return false, aerr.Wrapf(aerr.APERTURE_RULE_EVAL, err,
			"rule: evaluation failed for %q", c.source)
	}
	b, ok := out.(bool)
	if !ok {
		return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
			"rule: expression did not evaluate to a boolean",
			map[string]any{"source": c.source})
	}
	return b, nil
}

// Compiler turns a validated AST into a Compiled program. It holds the expression
// options — the curated pure functions plus any a host registers — that every
// rule it compiles shares. A zero Compiler is not usable; build one with
// NewCompiler. Compiler is safe for concurrent use (its options are read-only
// after construction).
type Compiler struct {
	options []expr.Option
}

// CompilerOption configures a Compiler at construction.
type CompilerOption func(*Compiler)

// Function registers a pure function callable from rules under name. It uses
// expr-lang's Function option: the function joins the expression environment and
// is type-checked at the call site. Hosts should only
// register deterministic, side-effect-free functions so rule evaluation stays
// pure.
func Function(name string, fn func(args ...any) (any, error)) CompilerOption {
	return func(c *Compiler) {
		c.options = append(c.options, expr.Function(name, fn))
	}
}

// NewCompiler builds a Compiler. By default the only callable functions are the
// curated pure set (lower, upper, contains, startsWith, endsWith, len, plus the
// collection-operator backings hasAll, hasAny, hasNone, subsetOf, isEmpty,
// isNotEmpty); all of expr-lang's builtins are disabled, so no wall-clock or
// random function is reachable and evaluation stays deterministic. Host functions
// added with Function join that set.
//
// That determinism survives the date operators because the reference instant is
// DATA, not a function: it arrives as Input.Now and is exposed to the program as
// the __now environment entry, fixed for the whole evaluation. Nothing a rule can
// call reads a clock.
func NewCompiler(opts ...CompilerOption) *Compiler {
	c := &Compiler{}
	// The typed env, a required boolean result, and a disabled builtin library
	// are fixed for every rule. The curated functions are registered first so a
	// host Function option can shadow or extend them.
	c.options = append(c.options,
		expr.Env(evalEnv{}),
		expr.AsBool(),
		expr.DisableAllBuiltins(),
	)
	c.options = append(c.options, defaultFunctions()...)
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compile validates the AST, renders it to an expr-lang expression, and compiles
// that to a reusable program. Validation failures surface APERTURE_RULE_INVALID /
// APERTURE_RULE_UNKNOWN_VARIABLE; type-check failures surface
// APERTURE_RULE_TYPE_ERROR — all before any evaluation.
func (c *Compiler) Compile(n *Node) (*Compiled, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	src, err := n.Expr()
	if err != nil {
		return nil, err
	}
	return c.compileSource(src)
}

// compileSource compiles an already-rendered expression. The engine reaches this
// directly after a cache miss, having computed the source (and its hash) to probe
// the cache.
func (c *Compiler) compileSource(src string) (*Compiled, error) {
	program, err := expr.Compile(src, c.options...)
	if err != nil {
		return nil, classifyCompileError(err, src)
	}
	key := hashSource(src)
	return &Compiled{program: program, source: src, key: key, hash: hexHash(key)}, nil
}

// classifyCompileError maps an expr-lang compile error to an Aperture code. The
// AST validator has already proven every variable root is exposed, so a remaining
// failure is a type mismatch, a non-boolean result, or a call to an unregistered
// function — all APERTURE_RULE_TYPE_ERROR. The evaluator's message is preserved
// in context for the operator.
func classifyCompileError(err error, src string) error {
	return aerr.WithContext(aerr.APERTURE_RULE_TYPE_ERROR,
		"rule: expression failed type checking",
		map[string]any{"source": src, "detail": strings.TrimSpace(err.Error())})
}

// hashSource is the canonical-hash function the compiled-rule cache keys on: two
// ASTs that render to the same expression share a compiled program.
func hashSource(src string) [32]byte {
	return sha256.Sum256([]byte(src))
}

// hexHash renders a cache key for DISPLAY — Compiled.Hash's exported string
// form. It is computed once per compilation, never on a cache lookup: hex
// encoding a digest allocates, and a lookup happens on every evaluation while a
// compilation happens once per distinct rule.
func hexHash(key [32]byte) string {
	return hex.EncodeToString(key[:])
}

// Names of the backing functions the collection operators render to. They are
// constants rather than string literals so opSpecs (ast.go) and defaultFunctions
// (below) cannot drift apart.
const (
	fnHasAll     = "hasAll"
	fnHasAny     = "hasAny"
	fnHasNone    = "hasNone"
	fnSubsetOf   = "subsetOf"
	fnIsEmpty    = "isEmpty"
	fnIsNotEmpty = "isNotEmpty"

	// fnCollectionOp is the guarded dispatcher every shape-sensitive collection
	// comparison renders to (renderGuarded). The '$' is load-bearing: varPath
	// rejects it, so no NodeCall can name this function — the guard is
	// structurally compiler-only rather than denylisted.
	fnCollectionOp = "$op"

	// fnDateOp is the guarded dispatcher every DATE comparison renders to
	// (renderDateOp), unconditionally — a date operator has no native spelling
	// that could be correct. The '$' is load-bearing for the same reason as
	// fnCollectionOp's.
	fnDateOp = "$date"

	// fnRelativeDate is the dispatcher a relative-date OPERAND renders to
	// (renderRelativeDate). Unlike the other two it returns a value rather than a
	// bool: it resolves the decision's reference instant plus a literal
	// anchor/offset/snap into a canonical date string, which the enclosing date
	// operator then compares like any other operand. Its '$' is load-bearing in
	// the same way, and it is the ONLY dispatcher that reads the instant — $date
	// never sees it.
	fnRelativeDate = "$rel"
)

// defaultFunctions is the curated pure function set every Compiler exposes. They
// are deterministic and side-effect-free, the safe baseline a rule can rely on
// regardless of host configuration.
//
// The last six back the collection operators that have no native expr spelling
// under DisableAllBuiltins (see opSpecs in ast.go). Each declares an explicit
// signature so the type-checker knows it returns a bool — that is what lets a
// bare `isEmpty(object.tags)` satisfy expr.AsBool as a whole rule. All six read
// their arguments and nothing else: no state, no clock, no I/O.
func defaultFunctions() []expr.Option {
	return []expr.Option{
		expr.Function("lower", func(args ...any) (any, error) {
			return strings.ToLower(argString(args)), nil
		}),
		expr.Function("upper", func(args ...any) (any, error) {
			return strings.ToUpper(argString(args)), nil
		}),
		expr.Function("contains", func(args ...any) (any, error) {
			s, sub := arg2String(args)
			return strings.Contains(s, sub), nil
		}),
		expr.Function("startsWith", func(args ...any) (any, error) {
			s, p := arg2String(args)
			return strings.HasPrefix(s, p), nil
		}),
		expr.Function("endsWith", func(args ...any) (any, error) {
			s, p := arg2String(args)
			return strings.HasSuffix(s, p), nil
		}),
		expr.Function("len", func(args ...any) (any, error) {
			if len(args) != 1 {
				return 0, nil
			}
			switch v := args[0].(type) {
			case string:
				return len(v), nil
			case []any:
				return len(v), nil
			case map[string]any:
				return len(v), nil
			case nil:
				return 0, nil
			default:
				return 0, nil
			}
		}),

		// hasAll(coll, items): every element of items is in coll.
		binaryCollectionFunc(fnHasAll, OpHasAll),
		// hasAny(coll, items): at least one element of items is in coll.
		binaryCollectionFunc(fnHasAny, OpHasAny),
		// hasNone(coll, items): no element of items is in coll. An absent coll
		// reads as empty, so this is TRUE for an object missing the field — the
		// same "negative operator grants on missing data" shape as `nin`.
		binaryCollectionFunc(fnHasNone, OpHasNone),
		// subsetOf(coll, super): every element of coll is in super. An absent
		// coll reads as empty, and the empty set is a subset of everything, so
		// this is TRUE for an object missing the field.
		binaryCollectionFunc(fnSubsetOf, OpSubsetOf),
		// isEmpty(v): v is absent, an empty array, an empty object, or an empty
		// string.
		unaryCollectionFunc(fnIsEmpty, OpIsEmpty),
		// isNotEmpty(v): the exact negation of isEmpty, registered separately so
		// the rendered expression stays a single atomic call.
		unaryCollectionFunc(fnIsNotEmpty, OpIsNotEmpty),

		// $op is the guarded dispatcher (renderGuarded). Its arguments are the
		// operator, the notes sink read out of the environment, and each
		// operand's path + value. It applies the deny-safe shape policy, so a
		// wrong-shaped operand yields false and a note rather than an error.
		expr.Function(fnCollectionOp, func(args ...any) (any, error) {
			if len(args) != 6 {
				return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
					"rule: guarded collection operator takes exactly six operands",
					map[string]any{"args": len(args)})
			}
			op, _ := args[0].(string)
			sink, _ := args[1].(*NoteCollector)
			leftPath, _ := args[2].(string)
			rightPath, _ := args[4].(string)
			return evalCollectionOp(op, sink, leftPath, args[3], rightPath, args[5]), nil
		}, new(func(string, any, string, any, string, any) bool)),

		// $date is the guarded date dispatcher (renderDateOp). Its arguments are
		// the operator, the notes sink read out of the environment, and THREE
		// path/value pairs: the left operand, the first right operand, and the
		// second right operand (between's upper bound, ""/nil otherwise). One
		// fixed arity covers both the binary and the ternary operators, so there
		// is a single registered signature for the type-checker to see.
		expr.Function(fnDateOp, func(args ...any) (any, error) {
			if len(args) != 8 {
				return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
					"rule: guarded date operator takes exactly eight operands",
					map[string]any{"args": len(args)})
			}
			op, _ := args[0].(string)
			sink, _ := args[1].(*NoteCollector)
			leftPath, _ := args[2].(string)
			rightPath, _ := args[4].(string)
			right2Path, _ := args[6].(string)
			return evalDateOp(op, sink,
				leftPath, args[3], rightPath, args[5], right2Path, args[7]), nil
		}, new(func(string, any, string, any, string, any, string, any) bool)),

		// $rel resolves a relative-date OPERAND (renderRelativeDate). Its
		// arguments are the decision's reference instant — the only place __now
		// is ever read — followed by the node's four literal fields: anchor,
		// offset count, offset unit, snap.
		//
		// It returns a canonical date STRING, which is byte-for-byte what a date
		// literal in the same position would be, so `$date` parses both operands
		// through one path and a relative date is interchangeable with a fixed
		// one. When the date cannot be resolved — no reference instant, or an
		// offset that leaves the representable year range — it returns nil, and
		// the enclosing operator applies its ordinary deny-safe policy (false, with a
		// shape note). It never raises: a rule must not fail to evaluate because
		// of the date it is measured against.
		expr.Function(fnRelativeDate, func(args ...any) (any, error) {
			if len(args) != 5 {
				return nil, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
					"rule: relative date takes exactly five operands",
					map[string]any{"args": len(args)})
			}
			now, _ := args[0].(time.Time)
			anchor, _ := args[1].(string)
			count, _ := args[2].(int)
			unit, _ := args[3].(string)
			snap, _ := args[4].(string)
			v, ok := resolveRelativeDate(now, anchor, count, unit, snap)
			if !ok {
				return nil, nil
			}
			return v.String(), nil
		}, new(func(time.Time, string, int, string, string) any)),
	}
}

// binaryCollectionFunc registers a binary collection operator's backing function.
// A rule reaches it only when neither operand can be the wrong shape (see
// guardNeeded) or when an author calls it directly with a NodeCall, so it carries
// no notes sink — but it applies the same policy through evalCollectionOp, so a
// direct call to hasAll over a string is false, never an error.
func binaryCollectionFunc(name, op string) expr.Option {
	return expr.Function(name, func(args ...any) (any, error) {
		if len(args) != 2 {
			return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
				"rule: collection operator takes exactly two operands",
				map[string]any{"op": op, "args": len(args)})
		}
		return evalCollectionOp(op, nil, "", args[0], "", args[1]), nil
	}, new(func(any, any) bool))
}

// unaryCollectionFunc is binaryCollectionFunc's one-operand twin (isEmpty /
// isNotEmpty).
func unaryCollectionFunc(name, op string) expr.Option {
	return expr.Function(name, func(args ...any) (any, error) {
		if len(args) != 1 {
			return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
				"rule: unary collection operator takes exactly one operand",
				map[string]any{"op": op, "args": len(args)})
		}
		return evalCollectionOp(op, nil, "", args[0], "", nil), nil
	}, new(func(any) bool))
}

// containsValue reports whether coll holds v, using expr-lang's own equality so
// element matching is identical to the native `in` operator `has` renders to —
// including its numeric cross-type handling and its refusal to coerce "1" to 1.
func containsValue(coll []any, v any) bool {
	for _, e := range coll {
		if runtime.Equal(e, v) {
			return true
		}
	}
	return false
}

func argString(args []any) string {
	if len(args) == 0 {
		return ""
	}
	s, _ := args[0].(string)
	return s
}

func arg2String(args []any) (string, string) {
	var a, b string
	if len(args) > 0 {
		a, _ = args[0].(string)
	}
	if len(args) > 1 {
		b, _ = args[1].(string)
	}
	return a, b
}
