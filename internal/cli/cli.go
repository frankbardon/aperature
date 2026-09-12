// Package cli assembles Aperture's urfave/cli/v3 command tree. It is the seam
// cmd/aperture/main.go delegates to, keeping the binary entrypoint a pure
// adapter. Each command is thin: it parses flags/args, hand-wires the
// dependency graph (storage -> engine -> service -> surface), translates one
// call into the engine, and maps the result to output + an exit code. No
// business logic lives here — that is the library's job.
package cli

import (
	ucli "github.com/urfave/cli/v3"
)

// NewApp builds the root command tree. version is stamped onto --version.
func NewApp(version string) *ucli.Command {
	return &ucli.Command{
		Name:    "aperture",
		Usage:   "Fine-grained access control engine",
		Version: version,
		Commands: []*ucli.Command{
			// Decision API.
			checkCommand(),
			enumerateCommand(),
			searchCommand(),
			explainCommand(),
			identifiersCommand(),
			// Mutations (the same facade path the Twirp surface drives).
			putCommand(),
			getCommand(),
			listCommand(),
			deleteCommand(),
			bestowCommand(),
			revokeCommand(),
			impersonateCommand(),
			// Provisioning: parameterized templates + transactional bulk apply.
			templateCommand(),
			bulkCommand(),
			// Declarative state: whole-model export/import (system-admin tier).
			exportCommand(),
			importCommand(),
			// Attribute directories: wiring listing, gated bulk read, cache
			// invalidation.
			attributesCommand(),
			// Server.
			serveCommand(),
			// Read-only MCP surface (stdio).
			mcpCommand(),
		},
	}
}
