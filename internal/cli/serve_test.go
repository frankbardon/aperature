package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// resolveManaged runs the real `serve` flag set over args and returns what
// managedEntities makes of it, without booting a server.
func resolveManaged(t *testing.T, args ...string) (service.ManagedEntities, error) {
	t.Helper()
	var (
		got service.ManagedEntities
		err error
	)
	cmd := &ucli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			got, err = managedEntities(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return got, err
}

// TestManagedEntities_DefaultsToManaged asserts an operator who passes nothing
// and sets nothing gets Aperture's historical behaviour.
func TestManagedEntities_DefaultsToManaged(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if !got.Accounts.Enabled() || !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("unconfigured serve is not fully managed: %+v", got)
	}
}

// TestManagedEntities_FlagsAreBooleansOfPositivePolarity asserts --manage-X=false
// disables exactly X. Positive polarity: the flag names the ENABLED state.
func TestManagedEntities_FlagsAreBooleansOfPositivePolarity(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	for _, tc := range []struct {
		flag string
		read func(service.ManagedEntities) service.Managed
	}{
		{"--manage-accounts=false", func(m service.ManagedEntities) service.Managed { return m.Accounts }},
		{"--manage-principals=false", func(m service.ManagedEntities) service.Managed { return m.Principals }},
		{"--manage-memberships=false", func(m service.ManagedEntities) service.Managed { return m.Memberships }},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got, err := resolveManaged(t, tc.flag)
			if err != nil {
				t.Fatalf("managedEntities: %v", err)
			}
			if tc.read(got).Enabled() {
				t.Fatalf("%s left the kind managed: %+v", tc.flag, got)
			}
		})
	}
}

// TestManagedEntities_EnvIsRead asserts the env var alone (no flag) is enough.
func TestManagedEntities_EnvIsRead(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "false")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if got.Accounts.Enabled() {
		t.Fatalf("%s=false was not read: %+v", service.EnvManageAccounts, got)
	}
	if !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("one env var disabled more than its own kind: %+v", got)
	}
}

// TestManagedEntities_FlagBeatsEnv pins the precedence in both directions, so
// the flag is a real override and not merely a second way to say the same thing.
func TestManagedEntities_FlagBeatsEnv(t *testing.T) {
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	t.Run("flag disables what env enabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "true")
		got, err := resolveManaged(t, "--manage-accounts=false")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})

	t.Run("flag enables what env disabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "false")
		got, err := resolveManaged(t, "--manage-accounts=true")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if !got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})
}

// TestManagedEntities_MalformedEnvIsConfigError asserts the coded error survives
// the CLI layer. This is the reason the flags carry no ucli.EnvVars source:
// urfave would fail the command with its own uncoded parse error first.
func TestManagedEntities_MalformedEnvIsConfigError(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "yes-please")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	_, err := resolveManaged(t)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
}

// TestServeEngineOptionsCarryOnlyTheServeOnlyPosture pins what belongs in the
// per-command half of the split. --enforce-membership is a posture `serve` takes
// and the one-shot commands deliberately do not, so it is the ONLY option this
// function produces; a configured value that describes the deployment — the
// enumeration bound — belongs in sharedEngineOptions, where every command
// inherits it. Asserting the count is what catches a shared knob being added
// here by habit, which would leave `aperture enumerate` configured differently
// from the server it is meant to agree with.
func TestServeEngineOptionsCarryOnlyTheServeOnlyPosture(t *testing.T) {
	t.Setenv(envEnumerateLimit, "1500")
	t.Setenv("APERTURE_ENFORCE_MEMBERSHIP", "")

	opts, err := resolveServeOptions(t)
	if err != nil {
		t.Fatalf("serveEngineOptions: %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("serve with no posture flag produced %d engine option(s), want none — a configured bound must not be wired here", len(opts))
	}

	opts, err = resolveServeOptions(t, "--enforce-membership")
	if err != nil {
		t.Fatalf("serveEngineOptions: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("--enforce-membership produced %d engine option(s), want exactly one", len(opts))
	}
}

// resolveServeOptions runs the real `serve` flag set over args and returns the
// SERVE-ONLY engine options serveEngineOptions makes of it, without booting a
// server. It is the SAME function runServe calls, so a test driving it cannot
// pass against a serve command that never wired the flag.
func resolveServeOptions(t *testing.T, args ...string) ([]engine.Option, error) {
	t.Helper()
	var (
		opts []engine.Option
		err  error
	)
	cmd := &ucli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			opts, err = serveEngineOptions(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return opts, err
}

// TestServeFlagsNameTheirEnvVars asserts each --manage-* flag's usage text names
// the environment variable it pairs with. The usage string is the only shipped
// explanation of the flag, so an operator reading `aperture serve --help` must
// be able to find the env var from it.
func TestServeFlagsNameTheirEnvVars(t *testing.T) {
	usage := map[string]string{}
	for _, f := range serveCommand().Flags {
		bf, ok := f.(*ucli.BoolFlag)
		if !ok {
			continue
		}
		usage[bf.Name] = bf.Usage
		if bf.Name == "manage-accounts" || bf.Name == "manage-principals" || bf.Name == "manage-memberships" {
			if !bf.Value {
				t.Errorf("--%s defaults to false; all three manage flags default to true", bf.Name)
			}
		}
	}
	for flag, env := range map[string]string{
		"manage-accounts":    service.EnvManageAccounts,
		"manage-principals":  service.EnvManagePrincipals,
		"manage-memberships": service.EnvManageMemberships,
	} {
		u, ok := usage[flag]
		if !ok {
			t.Fatalf("serve has no --%s bool flag", flag)
		}
		if !strings.Contains(u, env) {
			t.Errorf("--%s usage does not name %s: %q", flag, env, u)
		}
	}

	// The convention is about the usage TEXT, not about the flag's Go type, so it
	// covers the string flags that pair with a variable too. --enumerate-limit
	// carries a real ucli.EnvVars source, which makes its usage text the only
	// place `aperture serve --help` says the variable exists at all.
	strUsage := map[string]string{}
	for _, f := range serveCommand().Flags {
		if sf, ok := f.(*ucli.StringFlag); ok {
			strUsage[sf.Name] = sf.Usage
		}
	}
	for flag, env := range map[string]string{
		"auth":            auth.EnvMode,
		"enumerate-limit": envEnumerateLimit,
	} {
		u, ok := strUsage[flag]
		if !ok {
			t.Fatalf("serve has no --%s string flag", flag)
		}
		if !strings.Contains(u, env) {
			t.Errorf("--%s usage does not name %s: %q", flag, env, u)
		}
	}
}

// TestEnumerateLimitIsNotAnIntFlag pins the shape choice, not just the
// behaviour. A ucli.IntFlag carrying the same EnvVars source parses the
// environment inside urfave and fails the command with its own UNCODED parse
// error before the action ever runs, so APERTURE_ENUMERATE_LIMIT=banana would
// report something other than APERTURE_CONFIG_INVALID. The StringFlag is what
// keeps the parse — and therefore the coded error — on this side of the
// boundary, while still leaving precedence to urfave's native flag > env >
// default. Changing the type is a contract change, not a tidy-up.
func TestEnumerateLimitIsNotAnIntFlag(t *testing.T) {
	for _, f := range serveCommand().Flags {
		if !slices.Contains(f.Names(), "enumerate-limit") {
			continue
		}
		if _, ok := f.(*ucli.StringFlag); !ok {
			t.Fatalf("--enumerate-limit is a %T; it must stay a *ucli.StringFlag so a malformed value is APERTURE_CONFIG_INVALID", f)
		}
		return
	}
	t.Fatal("serve has no --enumerate-limit flag")
}
