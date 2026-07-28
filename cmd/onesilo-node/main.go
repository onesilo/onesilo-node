// Command onesilo-node runs an open-source Silo node: it contributes Memory
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
	"path/filepath"
	"syscall"

	"github.com/onesilo/onesilo-node/internal/adminapi"
	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/logging"
	"github.com/onesilo/onesilo-node/internal/node"
	"github.com/onesilo/onesilo-node/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthcheck(os.Args[2:]))
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		}
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("onesilo-node", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file (default ~/.onesilo-node/config.toml)")
	showVersion := fs.Bool("version", false, "print version and exit")
	registerConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Printf("onesilo-node %s (%s)\n", version.Version, version.Commit)
		return 0
	}

	cfg, path, err := loadConfig(fs, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node: "+err.Error())
		return 1
	}

	logger := logging.New(cfg.Log)

	// Admin token: SILO_NODE_ADMIN_TOKEN wins; otherwise the token file
	// written by `onesilo-node setup` is loaded.
	dataDir, dirErr := cfg.ResolvedDataDir()
	if dirErr != nil {
		dataDir = ""
	}
	adminToken, tokenSource := resolveAdminToken(dataDir, os.LookupEnv)
	switch tokenSource {
	case "":
		logger.Warn("no admin token (" + adminapi.AdminTokenEnvVar + " unset, no " + adminTokenFile + " in data dir); admin API refuses all authenticated requests — run `onesilo-node setup`")
	case "file":
		logger.Info("admin token loaded", "path", filepath.Join(dataDir, adminTokenFile))
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
