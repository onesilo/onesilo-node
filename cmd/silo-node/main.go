// Command silo-node runs an open-source Silo node: it contributes Memory
// and Compute capabilities to the Silo control plane, driven locally over a
// localhost admin API (used by the Silo Desktop app) or a TOML config file
// (Docker / headless).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/onesilo/silo-node/internal/adminapi"
	"github.com/onesilo/silo-node/internal/config"
	"github.com/onesilo/silo-node/internal/logging"
	"github.com/onesilo/silo-node/internal/node"
	"github.com/onesilo/silo-node/internal/version"
)

func main() {
	// Subcommands (currently just `silo-node healthcheck`).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("silo-node", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file (default ~/.silo-node/config.toml)")
	showVersion := fs.Bool("version", false, "print version and exit")
	registerConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Printf("silo-node %s (%s)\n", version.Version, version.Commit)
		return 0
	}

	cfg, path, err := loadConfig(fs, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "silo-node: "+err.Error())
		return 1
	}

	logger := logging.New(cfg.Log)

	adminToken := os.Getenv(adminapi.AdminTokenEnvVar)
	if adminToken == "" {
		logger.Warn("SILO_NODE_ADMIN_TOKEN is not set; admin API refuses all authenticated requests (only /healthz is served)")
	}

	n, err := node.New(cfg, path, adminToken, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := n.Run(ctx); err != nil {
		logger.Error("node exited with error", "error", err)
		return 1
	}
	return 0
}

// registerConfigFlags exposes every config override from the settings
// table as a string flag (booleans take -flag=true / -flag=false).
func registerConfigFlags(fs *flag.FlagSet) {
	for _, s := range config.Settings() {
		fs.String(s.Flag, "", s.Usage+" [env "+s.Env+"]")
	}
}

// loadConfig resolves the config file path (flag > SILO_NODE_CONFIG env >
// default) and loads with full precedence.
func loadConfig(fs *flag.FlagSet, pathFlag string) (config.Config, string, error) {
	path := pathFlag
	if path == "" {
		path = os.Getenv("SILO_NODE_CONFIG")
	}
	if path == "" {
		path = config.DefaultPath()
	}

	flagValues := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		flagValues[f.Name] = f.Value.String()
	})
	delete(flagValues, "config")
	delete(flagValues, "version")

	cfg, err := config.Load(config.LoadOptions{
		Path:       path,
		FlagValues: flagValues,
	})
	return cfg, path, err
}
