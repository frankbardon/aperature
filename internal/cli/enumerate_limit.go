package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// The configured enumeration bound belongs to the PROCESS, not to one command.
//
// It lives in its own file for that reason. It started on `serve`, beside
// --enforce-membership, and that was very nearly a bug: buildDecisionStack takes
// per-command engine options exactly so `serve` can add membership enforcement
// without forcing it on `check` / `enumerate` / `identifiers` / `explain`, so a
// bound wired the same way would have given the server 1500 while the CLI kept
// answering 1000 — two surfaces of ONE binary disagreeing about the same
// question, with nothing anywhere reporting the disagreement.
//
// Membership enforcement really is a serve-only posture. A bound is not: it is
// the deployment's answer to "how many ids may one enumeration return", and an
// operator who raises it expects `aperture enumerate` to say the same number the
// server does. So the flag is attached to every command that decides
// (enumerateLimitFlag) and it is applied in buildDecisionStack's SHARED options,
// where no command can fail to inherit it.

// enumerateLimitFlagName is the single spelling of the flag. Every command that
// declares it and every reader of it names this constant, so a command cannot
// declare one spelling while buildDecisionStack reads another — which would fail
// silently, as a flag nobody reads and a bound nobody configured.
const enumerateLimitFlagName = "enumerate-limit"

// envEnumerateLimit is the environment variable --enumerate-limit reads when the
// operator did not type the flag. It is named in the flag's usage text (see
// TestServeFlagsNameTheirEnvVars), so the constant is the single spelling the
// flag, the help output and the tests all share.
const envEnumerateLimit = "APERTURE_ENUMERATE_LIMIT"

// enumerateLimitFlag is the one declaration of --enumerate-limit, constructed
// fresh per command because a ucli.Flag carries parse state and must not be
// shared between two commands in one tree.
//
// It is a ucli.StringFlag carrying an env source and parsed by hand
// (enumerateLimit), NOT a ucli.IntFlag. See enumerateLimit for why; changing the
// type is a contract change, and TestEnumerateLimitIsNotAnIntFlag pins it.
//
// It is attached to every command that builds a decision stack AND can enumerate
// objects: serve, check, enumerate, identifiers, explain and mcp. `attributes`
// builds the same stack but reads attribute DIRECTORIES, which are paged by the
// attribute registry's own cap and never by this bound, so it does not carry a
// knob that would not move anything it prints.
func enumerateLimitFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name: enumerateLimitFlagName,
		Usage: "maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. " +
			"It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value " +
			"(a whole number greater than zero; default " + strconv.Itoa(engine.DefaultEnumerateLimit) + "; overrides " + envEnumerateLimit + ")",
		Sources: ucli.EnvVars(envEnumerateLimit),
	}
}

// enumerateLimit resolves the enumeration bound this process decides with, in
// precedence order: the engine's own DefaultEnumerateLimit, overridden by
// APERTURE_ENUMERATE_LIMIT, overridden by a --enumerate-limit the operator
// actually typed. ok is false when neither was given, which leaves the engine on
// its documented default and every command behaving exactly as it did before the
// flag existed.
//
// A cmd that does not declare the flag reads the empty string and therefore
// resolves to "unset". That is why the flag is attached per command rather than
// read from the environment directly: reading os.Getenv here would honour the
// variable on commands whose help never mentions it and whose --help could not
// be used to discover it.
//
// The flag is a StringFlag that carries an EnvVars source and is parsed HERE,
// rather than a ucli.IntFlag that would parse itself. That is deliberate, and it
// is the opposite trade from the --manage-* flags in serve.go, so both halves of
// the reasoning belong together:
//
//   - An IntFlag with an env source hands the parse to urfave, which fails the
//     command with its own uncoded "could not parse ... from environment" error
//     before the action ever runs. APERTURE_ENUMERATE_LIMIT=banana would then
//     report something other than APERTURE_CONFIG_INVALID — the same hazard that
//     kept the --manage-* bools off EnvVars entirely.
//   - A StringFlag has no parse to fail: urfave's value setter accepts any
//     string verbatim, so a malformed value reaches this function and becomes a
//     coded error the operator can act on.
//
// Keeping the EnvVars source (which --manage-* could not) is worth the manual
// Atoi: precedence stays urfave's NATIVE flag > env > default — the env source is
// applied only when the flag was not set on the command line — so there is no
// hand-rolled resolution order here that could drift from the one every other
// flag obeys. The --manage-* flags needed their own reader for a second reason
// that does not apply to a string: an env-sourced BoolFlag also sets
// hasBeenSet, which would make cmd.IsSet stop meaning "the operator typed this".
//
// This reads the value and checks it is SAYABLE — a whole number greater than
// zero. It does not decide what an accepted number MEANS: the clamping is the
// engine's (engine.WithEnumerateLimit), which is why every value that survives
// the two checks below is handed over unexamined.
func enumerateLimit(cmd *ucli.Command) (int, bool, error) {
	raw := strings.TrimSpace(cmd.String(enumerateLimitFlagName))
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, badEnumerateLimit(raw, "is not a whole number")
	}
	if n <= 0 {
		// engine.WithEnumerateLimit NORMALISES a non-positive bound to
		// DefaultEnumerateLimit rather than storing it, and that is correct for the
		// library: an Option cannot report an error, and a Go embedder handing over
		// a computed 0 should get a sane engine instead of a zero bound that reads
		// as "no access". It is wrong for a human, though — an operator who typed
		// -5 would be served 1000 while believing the bound is what they wrote,
		// which is the exact invisibility this flag exists to remove. So the
		// boundary refuses what the library would have absorbed, and the value
		// never reaches the option. Lenient normalisation in the library, input
		// validation at the boundary; both are right.
		return 0, false, badEnumerateLimit(raw, "must be greater than zero")
	}
	return n, true, nil
}

// badEnumerateLimit builds the single refusal both --enumerate-limit rejections
// share, so a malformed value and an out-of-range one read identically apart
// from the reason.
//
// The setting and the rejected value go in the MESSAGE, not only in the context
// map: nothing on the CLI path renders a CodedError's Context, so an operator
// who mistyped one of two spellings would otherwise be told which code failed
// but not which knob or what it read. The value is the operator's own input and
// carries no account data.
func badEnumerateLimit(raw, why string) error {
	return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
		fmt.Sprintf("cli: --%s / %s %s: %q", enumerateLimitFlagName, envEnumerateLimit, why, raw),
		map[string]any{
			"setting": "--" + enumerateLimitFlagName + " / " + envEnumerateLimit,
			"value":   raw,
			"valid":   "a whole number greater than zero, e.g. 1500",
			"default": strconv.Itoa(engine.DefaultEnumerateLimit) + " — the setting may simply be omitted",
		})
}

// sharedEngineOptions are the engine options EVERY surface that decides applies,
// resolved from the flags that configure the process rather than the command.
// buildDecisionStack calls it, so a new decision command inherits them by being
// built through the shared builder — there is nothing per-command to remember.
//
// It is pure flag reading with no I/O, and it produces NO option when nothing is
// configured, which leaves the engine on engine.DefaultEnumerateLimit rather
// than restating 1000 here.
func sharedEngineOptions(cmd *ucli.Command) ([]engine.Option, error) {
	limit, ok, err := enumerateLimit(cmd)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	// Hand the configured number to the engine and let the engine decide what the
	// number MEANS: it clamps every enumeration against it and stamps it into the
	// scope deps, so the member gather and the result cap stay one value.
	return []engine.Option{engine.WithEnumerateLimit(limit)}, nil
}
