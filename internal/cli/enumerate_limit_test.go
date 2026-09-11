package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// boundCommands are the commands that build a decision stack AND can enumerate
// objects through it. Every one of them must carry --enumerate-limit and resolve
// it identically: the bound describes the deployment, not the command, so a
// binary whose server answers 1500 while its CLI answers 1000 is broken even
// though neither half is individually wrong.
//
// `attributes` is deliberately absent. It builds the same stack, but everything
// it prints is paged by the attribute registry's own cap and never by this
// bound, so carrying the flag there would advertise a knob that moves nothing.
//
// Built fresh on every call: a ucli.Flag carries parse state, so a shared
// instance would let one subtest's value leak into the next.
func boundCommands() map[string][]ucli.Flag {
	return map[string][]ucli.Flag{
		"serve":       serveCommand().Flags,
		"check":       checkCommand().Flags,
		"enumerate":   enumerateCommand().Flags,
		"identifiers": identifiersCommand().Flags,
		"explain":     explainCommand().Flags,
		"mcp":         mcpCommand().Flags,
	}
}

// resolveSharedOptions runs a real command's flag set over args and returns what
// sharedEngineOptions makes of it, without booting anything. It is the SAME
// function buildDecisionStack calls, so a test driving it cannot pass against a
// command tree that never wired the flag.
func resolveSharedOptions(t *testing.T, flags []ucli.Flag, args ...string) ([]engine.Option, error) {
	t.Helper()
	var (
		opts []engine.Option
		err  error
	)
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			opts, err = sharedEngineOptions(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"probe"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return opts, err
}

// enumerateUnderFlags is the enumeration a command with these flags would
// answer, built through the REAL builder from a command whose flags were really
// parsed. The fixture holds exactly three documents and alice may list all of
// them, so the returned count IS the bound whenever the bound is below three —
// which is what makes the configured value observable rather than merely stored.
func enumerateUnderFlags(t *testing.T, flags []ucli.Flag, args ...string) []string {
	t.Helper()

	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)
	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var ids []string
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: flags,
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			stack, err := buildDecisionStack(cmd, store, seedPath)
			if err != nil {
				return err
			}
			defer func() { _ = stack.Close() }()
			ids, err = stack.eng.Enumerate(ctx, engine.EnumerateRequest{
				Account:   "acme",
				Principal: "alice",
				Action:    "list",
				Pattern:   "account:acme/**",
			})
			return err
		},
	}
	if err := cmd.Run(ctx, append([]string{"probe"}, args...)); err != nil {
		t.Fatalf("enumerating under %v: %v", args, err)
	}
	slices.Sort(ids)
	return ids
}

// TestTheSameConfigurationBoundsServeAndAOneShotCommand is this story's gate.
//
// `serve` and the one-shot commands share buildDecisionStack, which takes
// per-command engine options precisely so `serve` can add --enforce-membership
// without forcing it on `check` / `enumerate` / `identifiers` / `explain`.
// Wiring the bound that way — the obvious way, the way the flag beside it is
// wired — would have given the server the configured ceiling and left the CLI on
// 1000, two surfaces of ONE binary disagreeing about the same question with
// nothing anywhere reporting the disagreement.
//
// So the claim asserted here is EQUALITY between the surfaces, from one
// configuration, through the real flag sets and the real builder. The exact
// counts are pinned as well: equality alone would pass against a build where
// neither surface honours the bound.
func TestTheSameConfigurationBoundsServeAndAOneShotCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want int
	}{
		{name: "unconfigured", env: "", want: 3},
		{name: "from the environment", env: "2", want: 2},
		{name: "from the flag", env: "", args: []string{"--enumerate-limit=1"}, want: 1},
		{name: "the flag beats the environment", env: "1", args: []string{"--enumerate-limit=2"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envEnumerateLimit, tc.env)

			server := enumerateUnderFlags(t, serveCommand().Flags, tc.args...)
			oneShot := enumerateUnderFlags(t, enumerateCommand().Flags, tc.args...)

			if !slices.Equal(server, oneShot) {
				t.Fatalf("serve and `aperture enumerate` disagree under the same configuration: serve=%v, enumerate=%v",
					server, oneShot)
			}
			if len(server) != tc.want {
				t.Fatalf("both surfaces returned %d ids (%v), want %d — the configured bound is not being honoured anywhere",
					len(server), server, tc.want)
			}
		})
	}
}

// TestEveryDecisionCommandCarriesTheConfiguredBound is the other half. The
// equality test above pins two commands; this one pins the set, so a decision
// command added without the flag cannot pass by never being compared. A command
// missing it reads the empty string from cmd.String and silently resolves to the
// default — the failure is invisible from inside that command.
func TestEveryDecisionCommandCarriesTheConfiguredBound(t *testing.T) {
	for name, flags := range boundCommands() {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envEnumerateLimit, "1500")

			var found *ucli.StringFlag
			for _, f := range flags {
				if !slices.Contains(f.Names(), enumerateLimitFlagName) {
					continue
				}
				sf, ok := f.(*ucli.StringFlag)
				if !ok {
					t.Fatalf("`aperture %s` declares --%s as a %T; it must stay a *ucli.StringFlag", name, enumerateLimitFlagName, f)
				}
				found = sf
			}
			if found == nil {
				t.Fatalf("`aperture %s` decides but carries no --%s, so it would silently keep the default while the rest of the binary honours the operator's value",
					name, enumerateLimitFlagName)
			}
			if !strings.Contains(found.Usage, envEnumerateLimit) {
				t.Errorf("`aperture %s --help` never names %s: %q", name, envEnumerateLimit, found.Usage)
			}

			// The declaration is not enough: the env source has to be attached too,
			// which only the resolved value proves.
			opts, err := resolveSharedOptions(t, flags)
			if err != nil {
				t.Fatalf("sharedEngineOptions: %v", err)
			}
			if len(opts) != 1 {
				t.Fatalf("`aperture %s` resolved %d shared option(s) from %s=1500, want exactly one",
					name, len(opts), envEnumerateLimit)
			}
		})
	}
}

// TestEnumerateHelpTellsTheTwoBoundsApart keeps the one place an operator learns
// that --limit and --enumerate-limit are different things from drifting out of
// the command. --limit's old usage text said "<=0 means the default", which
// stopped being the whole truth the moment the default became configurable: it
// is the DEPLOYMENT's ceiling that a request receives and is clamped down to.
func TestEnumerateHelpTellsTheTwoBoundsApart(t *testing.T) {
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	if err := app.Run(context.Background(), []string{"aperture", "enumerate", "--help"}); err != nil {
		t.Fatalf("enumerate --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"--limit",
		"--enumerate-limit",
		envEnumerateLimit,
		"clamped down", // what happens to a --limit larger than the ceiling
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("`aperture enumerate --help` never mentions %q:\n%s", want, help)
		}
	}
}

// TestEnumerateLimit_UnconfiguredIsTheEngineDefault asserts an operator who
// passes nothing and sets nothing adds NO option, which is what leaves the
// engine on DefaultEnumerateLimit and every command behaving exactly as it did
// before the flag existed. Asserting the absence of the option is the precise
// claim: with a three-object fixture, a bound of 1000 and a bound of 1_000_000
// look identical.
func TestEnumerateLimit_UnconfiguredIsTheEngineDefault(t *testing.T) {
	t.Setenv(envEnumerateLimit, "")

	opts, err := resolveSharedOptions(t, serveCommand().Flags)
	if err != nil {
		t.Fatalf("sharedEngineOptions: %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("an unconfigured command carries %d shared engine options, want none", len(opts))
	}
	if got := enumerateUnderFlags(t, enumerateCommand().Flags); len(got) != 3 {
		t.Fatalf("unconfigured enumerate returned %v, want all three documents", got)
	}
}

// TestEnumerateLimit_MalformedIsConfigError asserts a value that is not a number
// is an Aperture-coded error from BOTH sources. This is what the StringFlag
// buys: a ucli.IntFlag carrying the same EnvVars source would have failed the
// command with urfave's own uncoded parse error before the action ran.
func TestEnumerateLimit_MalformedIsConfigError(t *testing.T) {
	t.Run("from the environment", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "banana")
		_, err := resolveSharedOptions(t, serveCommand().Flags)
		if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
			t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
		}
		if !strings.Contains(err.Error(), "banana") || !strings.Contains(err.Error(), envEnumerateLimit) {
			t.Fatalf("the error names neither the setting nor the value: %v", err)
		}
	})

	t.Run("from the flag", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "")
		_, err := resolveSharedOptions(t, serveCommand().Flags, "--enumerate-limit=banana")
		if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
			t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
		}
	})

	// The refusal is the SAME on a one-shot command, and it comes back through
	// buildDecisionStack rather than through a per-command reader — which is what
	// makes it impossible for one surface to accept a value another rejects.
	t.Run("through the shared builder", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "banana")
		ctx := context.Background()
		seedPath := writeRuleBackedSeed(t)
		store, err := buildStore(ctx, "", seedPath)
		if err != nil {
			t.Fatalf("buildStore: %v", err)
		}
		defer func() { _ = store.Close() }()

		cmd := &ucli.Command{
			Name:  "enumerate",
			Flags: enumerateCommand().Flags,
			Action: func(_ context.Context, cmd *ucli.Command) error {
				stack, err := buildDecisionStack(cmd, store, seedPath)
				if err == nil {
					_ = stack.Close()
					t.Fatal("a malformed bound built a decision stack; it must fail the command")
				}
				if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
					t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
				}
				if d := codedDepth(err); d != 1 {
					t.Errorf("coded chain depth = %d, want exactly 1 (err=%v)", d, err)
				}
				return nil
			},
		}
		if err := cmd.Run(ctx, []string{"enumerate"}); err != nil {
			t.Fatalf("running the probe: %v", err)
		}
	})
}

// TestEnumerateLimit_NonPositiveIsConfigError asserts a value that parses but
// cannot be a ceiling — zero, or any negative — is refused at the boundary from
// BOTH sources, rather than reaching engine.WithEnumerateLimit and being
// normalised away.
//
// This is the half a type check cannot catch: -5 is a perfectly good int, so the
// StringFlag/IntFlag argument buys nothing here. The hazard is the silent
// fallback — engine.WithEnumerateLimit deliberately normalises a non-positive
// bound to DefaultEnumerateLimit (correct for a Go embedder passing a computed
// number; see engine.TestEnumerateLimit* for the library side), which would
// leave an operator who typed -5 being served 1000 while believing otherwise.
// Lenient normalisation in the library, input validation at the boundary. Do not
// "fix" this by changing the engine.
func TestEnumerateLimit_NonPositiveIsConfigError(t *testing.T) {
	for _, raw := range []string{"0", "-1", "-5", "-1500"} {
		t.Run("from the environment/"+raw, func(t *testing.T) {
			t.Setenv(envEnumerateLimit, raw)
			opts, err := resolveSharedOptions(t, serveCommand().Flags)
			assertRejectedEnumerateLimit(t, opts, err, raw)
		})

		t.Run("from the flag/"+raw, func(t *testing.T) {
			// Empty, not unset: the flag must be what fails, not a leftover variable.
			t.Setenv(envEnumerateLimit, "")
			opts, err := resolveSharedOptions(t, serveCommand().Flags, "--enumerate-limit="+raw)
			assertRejectedEnumerateLimit(t, opts, err, raw)
		})
	}
}

// assertRejectedEnumerateLimit is the shared claim behind every refusal of a
// configured bound: the command fails with one APERTURE_CONFIG_INVALID naming
// both spellings of the setting and the value it rejected, and NO engine option
// is produced — an option carrying a value the engine would normalise is exactly
// the silent fallback these tests exist to forbid.
func assertRejectedEnumerateLimit(t *testing.T, opts []engine.Option, err error, raw string) {
	t.Helper()
	if err == nil {
		t.Fatalf("--enumerate-limit=%s was accepted; it must fail the command", raw)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
	if d := codedDepth(err); d != 1 {
		t.Errorf("coded chain depth = %d, want exactly 1 (err=%v)", d, err)
	}
	msg := err.Error()
	for _, want := range []string{raw, "--enumerate-limit", envEnumerateLimit} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	if len(opts) != 0 {
		t.Errorf("a refused bound still produced %d engine option(s); the value must never reach the engine", len(opts))
	}
}

// codedDepth counts the Aperture-coded errors in a chain. A same-code re-stamp
// is invisible to CodeOf, so depth is what proves the refusal is constructed
// once rather than wrapped on the way out.
func codedDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}
