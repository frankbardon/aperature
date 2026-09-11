package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// This file holds the ONE test that asserts the engine's leniency and the CLI's
// strictness about a non-positive enumeration bound are two halves of a single
// decision, rather than two independent behaviours that happen to differ today.
//
// Why it lives in package cli, and not anywhere else
//
// The test has to see both halves. The CLI half is unexported (sharedEngineOptions,
// enumerateLimitFlag, envEnumerateLimit), so only package cli can reach it; the
// engine half is reachable from here because internal/cli ALREADY imports engine.
// The mirror placement — a test in package engine reaching into internal/cli —
// is not available and must not be made available: engine is a public root
// package, internal/cli is internal and imports engine, so the edge would be
// both an import cycle and a public package depending on an internal one.
//
// No new import edge was created for this test.
//
// The engine half is observed through EXPORTED surface only, since package cli
// cannot read engine's unexported fields: an engine built with the bound under
// test reports its effective ceiling as `configured_bound` on the on-bound
// warning (engine.warnAtEnumerateBound), captured here through
// engine.WithLogger. engine/enumerate_limit_test.go pins the field directly and
// engine/enumerate_bound_warning_test.go pins the warning's wording; this test
// depends on neither of those being the only statement of the behaviour.

// enumerateBoundContract is the prose a reader who breaks this test must be
// handed. A failure here is almost always someone "fixing" one half to match the
// other — which is the regression, not the repair — so the message states what
// the divergence IS, why each side is right, and which documents go silently
// wrong if the behaviour moves without them.
const enumerateBoundContract = `
  THE CONTRACT — the two halves are deliberately divergent, and both are correct:

    engine.WithEnumerateLimit(n) with n <= 0 NORMALISES to engine.DefaultEnumerateLimit.
      An Option cannot report an error, and a Go embedder handing over a computed 0
      must get a sane engine rather than a zero bound, which would make every
      enumeration return nothing and read as "no access".

    --enumerate-limit / APERTURE_ENUMERATE_LIMIT with n <= 0 is REFUSED with
      APERTURE_CONFIG_INVALID, and the value never reaches the option.
      A human who typed -5 made a mistake; serving them DefaultEnumerateLimit while
      they believe the bound is -5 is the exact invisibility the flag exists to remove.

  Lenient normalisation in the library, input validation at the boundary. Neither half
  is the bug. Changing EITHER one without the other is what this test exists to catch.

  THREE documents state this divergence and go silently wrong if a half moves:
    - skills/decision-api.md
    - docs/src/cli/serve.md
    - the ` + "`n <= 0`" + ` comment in internal/cli/enumerate_limit.go
  Plus the enumeration-bound Update-Demand row in CLAUDE.md, which names this test.

  If you believe one half is wrong, change it deliberately: move all three documents
  and this test in the same commit. Do not edit this test until it passes.`

// nonPositiveBounds are the values the two halves answer differently. 0 is the
// computed-zero an embedder really produces; -5 is the operator typo. Both must
// be normalised by the library and refused by the boundary.
var nonPositiveBounds = []int{0, -5}

// TestTheLibraryNormalisesWhatTheBoundaryRefuses is the gate the enumeration
// bound's Update-Demand row names.
//
// engine/enumerate_limit_test.go already pins the normalisation and
// internal/cli/enumerate_limit_test.go already pins the refusal. Neither one
// fails if the OTHER half changes: drop the normalisation and the CLI tests stay
// green; drop the refusal and the engine tests stay green. Both halves would
// still be "tested", and three documents describing their relationship would be
// wrong with nothing red.
//
// So the claim asserted here is the PAIR, from the same two values, in one test:
// the library absorbs them and the boundary rejects them.
func TestTheLibraryNormalisesWhatTheBoundaryRefuses(t *testing.T) {
	for _, n := range nonPositiveBounds {
		raw := strconv.Itoa(n)

		t.Run("the library absorbs "+raw, func(t *testing.T) {
			got := effectiveEngineBound(t, n)
			if got != engine.DefaultEnumerateLimit {
				t.Fatalf("engine.WithEnumerateLimit(%d) yielded an effective bound of %d, want %d "+
					"(engine.DefaultEnumerateLimit).\n"+
					"The library half of the contract is gone: a non-positive bound is being STORED "+
					"rather than normalised.%s",
					n, got, engine.DefaultEnumerateLimit, enumerateBoundContract)
			}
		})

		t.Run("the boundary refuses "+raw, func(t *testing.T) {
			for _, src := range []struct {
				name string
				env  string
				args []string
			}{
				{name: "from the environment", env: raw},
				{name: "from the flag", args: []string{"--" + enumerateLimitFlagName + "=" + raw}},
			} {
				t.Run(src.name, func(t *testing.T) {
					// Empty, never unset: whichever source is not under test must be
					// incapable of supplying the value that fails.
					t.Setenv(envEnumerateLimit, src.env)

					opts, err := resolveSharedOptions(t, serveCommand().Flags, src.args...)
					if err == nil {
						t.Fatalf("--%s=%s was ACCEPTED (%d engine option(s) produced).\n"+
							"The boundary half of the contract is gone: the value will now reach "+
							"engine.WithEnumerateLimit and be normalised away, and the operator will be "+
							"served %d while believing the bound is %s.%s",
							enumerateLimitFlagName, raw, len(opts),
							engine.DefaultEnumerateLimit, raw, enumerateBoundContract)
					}
					if code := aerr.CodeOf(err); code != aerr.APERTURE_CONFIG_INVALID {
						t.Fatalf("--%s=%s was refused with %s, want %s (err=%v).\n"+
							"The refusal is the boundary half of the contract and must stay an operator-"+
							"actionable configuration error.%s",
							enumerateLimitFlagName, raw, code, aerr.APERTURE_CONFIG_INVALID, err,
							enumerateBoundContract)
					}
					if len(opts) != 0 {
						t.Fatalf("--%s=%s was refused but still produced %d engine option(s).\n"+
							"A refused value that reaches the engine is normalised away, which is the "+
							"silent fallback the refusal exists to prevent.%s",
							enumerateLimitFlagName, raw, len(opts), enumerateBoundContract)
					}
				})
			}
		})
	}
}

// effectiveEngineBound is the bound an engine built with WithEnumerateLimit(n)
// actually decides with, read back through exported surface only.
//
// The engine is built by the REAL builder (buildDecisionStack), with no flag and
// no environment variable set, so the bound under test is the only one in play
// and the CLI's own refusal cannot mask the library's behaviour. n is passed as
// a per-command engine option, which is applied after the (absent) shared ones.
//
// The reading itself: an enumeration that comes back holding exactly as many ids
// as it was allowed to hold is warned about, and the warning names the engine's
// configured ceiling. Requesting a limit of 1 against a fixture of three
// documents guarantees the warning fires, whatever the ceiling turns out to be —
// so a wrong ceiling is reported as a wrong number rather than as silence.
func effectiveEngineBound(t *testing.T, n int) int {
	t.Helper()

	// Neither source configured: the library half must be observed on its own.
	t.Setenv(envEnumerateLimit, "")

	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)
	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logs := &bytes.Buffer{}
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: enumerateCommand().Flags,
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			stack, err := buildDecisionStack(cmd, store, seedPath,
				engine.WithEnumerateLimit(n),
				engine.WithLogger(slog.New(slog.NewTextHandler(logs,
					&slog.HandlerOptions{Level: slog.LevelWarn}))))
			if err != nil {
				return err
			}
			defer func() { _ = stack.Close() }()

			ids, err := stack.eng.Enumerate(ctx, engine.EnumerateRequest{
				Account:   "acme",
				Principal: "alice",
				Action:    "list",
				Pattern:   "account:acme/**",
				Limit:     1,
			})
			if err != nil {
				return err
			}
			if len(ids) != 1 {
				t.Fatalf("an enumeration limited to 1 returned %d ids (%v); "+
					"the fixture can no longer put a result on its bound, so the engine's "+
					"configured ceiling is unobservable from here", len(ids), ids)
			}
			return nil
		},
	}
	if err := cmd.Run(ctx, []string{"probe"}); err != nil {
		t.Fatalf("enumerating with WithEnumerateLimit(%d): %v", n, err)
	}
	return configuredBound(t, logs.String())
}

// configuredBound reads the engine's configured ceiling out of the on-bound
// warning. The warning is the only exported report of that number, so its
// absence is a failure rather than a zero: silence here would let every
// assertion above pass against an engine that never applied a bound at all.
func configuredBound(t *testing.T, logs string) int {
	t.Helper()
	const key = "configured_bound="
	i := strings.Index(logs, key)
	if i < 0 {
		t.Fatalf("the enumeration raised no on-bound warning naming %q, so the engine's "+
			"effective ceiling cannot be read.\nlogs = %q\n"+
			"engine.warnAtEnumerateBound is how an operator — and this test — learns the "+
			"configured bound; see engine/enumerate_bound_warning_test.go.", key, logs)
	}
	rest := logs[i+len(key):]
	if cut := strings.IndexAny(rest, " \n"); cut >= 0 {
		rest = rest[:cut]
	}
	bound, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("the on-bound warning reported %s%q, which is not a number: %v", key, rest, err)
	}
	return bound
}
