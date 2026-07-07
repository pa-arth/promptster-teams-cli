package version

// Version is the CLI version, set at build time via
// -ldflags "-X github.com/pa-arth/promptster-teams-cli/internal/version.Version=<tag>".
// It defaults to "dev" for local builds.
var Version = "dev"
