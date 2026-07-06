package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

// runHealthcheck implements `silo-node healthcheck`: probe the local admin
// API's /healthz and exit 0 (healthy) or 1. Intended for Docker:
//
//	HEALTHCHECK CMD ["/silo-node", "healthcheck"]
//
// The admin port is resolved with the same precedence as the main command
// (flags > env > config file > default), so a container that overrides
// SILO_NODE_ADMIN_PORT health-checks the right port automatically.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("silo-node healthcheck", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file")
	fs.String("admin-port", "", "admin API port to probe")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, _, err := loadConfig(fs, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: "+err.Error())
		return 1
	}

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Admin.Port)
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %s unreachable: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	fmt.Println("ok")
	return 0
}
