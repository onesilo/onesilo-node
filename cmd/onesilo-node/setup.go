package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/onesilo/onesilo-node/internal/compute/ollama"
	"github.com/onesilo/onesilo-node/internal/config"
)

// runSetup implements `onesilo-node setup`: launch the node with default
// settings, then hand control to a terminal control panel.
//
// On first run (no config file) it applies the product defaults — Local
// Node with Compute, Memory, and LAN on — and bootstraps local inference
// (finding or downloading Ollama and pulling the default model), the one
// part of a working node that can't be silent. Everything else that the old
// question-by-question wizard asked now lives on the panel: launching the
// admin interface, switching Local Node <-> Local Relay, and enabling
// access from anywhere (which pairs with the One Silo control plane via the
// browser OAuth flow on demand).
//
// -yes keeps the non-interactive contract: initialize the config with
// defaults (accepting the download prompts) and exit without starting the
// node — Docker and scripts then run `onesilo-node` as before.
func runSetup(args []string) int {
	fs := flag.NewFlagSet("onesilo-node setup", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file (default ~/.onesilo-node/config.toml)")
	yes := fs.Bool("yes", false, "non-interactive: initialize config with defaults and exit")
	serve := fs.String("serve", "", "who this node serves: agents (loopback only), network (LAN discovery), anywhere (LAN discovery; enable remote access from the panel, which needs sign-in). Skips the question.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	path := *configPath
	if path == "" {
		path = os.Getenv("SILO_NODE_CONFIG")
	}
	if path == "" {
		path = config.DefaultPath()
	}
	_, statErr := os.Stat(path)
	firstRun := os.IsNotExist(statErr)

	// File + defaults only: transient env overrides must not be baked into
	// the config file setup writes.
	cfg, err := config.Load(config.LoadOptions{
		Path:      path,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}
	if firstRun {
		applyProductDefaults(&cfg)
	}

	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}

	p := &prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout, assumeYes: *yes}
	p.printf("Welcome to One Silo Node\n\n")
	p.printf("  data dir: %s\n", dataDir)
	p.printf("  config:   %s\n\n", path)

	// Reach, before anything slow: it is the consequential answer, and
	// asking it after a multi-minute model download buries it.
	if requested := strings.TrimSpace(*serve); requested != "" {
		shape, ok := parseShape(requested)
		if !ok {
			fmt.Fprintf(os.Stderr, "onesilo-node setup: -serve must be one of %s (got %q)\n", shapeNames(), requested)
			return 2
		}
		applyShape(&cfg, shape)
		announceShape(p, cfg, shape)
	} else if firstRun {
		shape := askShape(p, ShapeAgents)
		applyShape(&cfg, shape)
		announceShape(p, cfg, shape)
	}

	// Admin token — generated once, loaded automatically at start.
	adminToken, created, err := ensureAdminToken(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}
	if created {
		p.printf("  generated admin token %s (0600)\n\n", filepath.Join(dataDir, adminTokenFile))
	}

	// Compute bootstrap: local inference needs Ollama and a model on disk,
	// which can't happen silently. Availability is re-checked on every run
	// (cheap when installed, and it heals a deleted install); the model
	// pulls only happen on first run — they need a temporary `ollama
	// serve` just to check, which is too slow for every launch.
	if cfg.Capabilities.Compute {
		p.printf("Local LLM inference (Ollama):\n")
		if err := ensureOllamaAvailable(ctx, &cfg, dataDir, p); err != nil {
			p.printf("  ! %s\n  ! compute disabled — enable it from the admin interface once Ollama is installed\n", err)
			cfg.Capabilities.Compute = false
		} else if firstRun {
			if p.confirm(fmt.Sprintf("  Pull the default model %s now?", cfg.Ollama.DefaultModel), true) {
				if err := ensureModel(ctx, cfg.Ollama, cfg.Ollama.DefaultModel, p.out); err != nil {
					p.printf("  ! %s\n  ! you can pull it later; inference fails until a model is installed\n", err)
				}
			}
			if cfg.Capabilities.Memory {
				embed := cfg.Memory.EmbedModel
				if embed == "" {
					embed = "nomic-embed-text"
				}
				if p.confirm(fmt.Sprintf("  Pull the embedding model %s for hybrid recall?", embed), true) {
					if err := ensureModel(ctx, cfg.Ollama, embed, p.out); err != nil {
						p.printf("  ! %s\n  ! memory falls back to keyword-only recall until the model is pulled\n", err)
					}
				}
			}
		}
		p.printf("\n")
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}
	if err := config.Save(cfg, path); err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}

	if *yes {
		p.printf("Config written to %s.\n\n", path)
		p.printf("  mode:          %s\n", modeName(cfg))
		p.printf("  capabilities:  %s\n", capabilitiesLine(cfg))
		p.printf("  serving:       %s\n", servingLine(cfg))
		p.printf("  admin:         http://127.0.0.1:%d\n", cfg.Admin.Port)
		p.printf("\nStart the node:\n\n  onesilo-node\n")
		return 0
	}

	return runPanel(ctx, cfg, path, dataDir, adminToken, p)
}

// applyProductDefaults is what "launch with default settings" means on a
// machine that has never run a node: a Local Node providing Compute and
// Memory. Only ever applied when no config file exists — an existing config
// is the operator's.
//
// Capabilities only. Reach — who can talk to this node — is asked rather
// than defaulted; see askShape. Turning LAN discovery on here meant a fresh
// node advertised itself over Bonjour and accepted connections from the
// whole local network without anyone being asked.
func applyProductDefaults(cfg *config.Config) {
	cfg.Mode = config.ModeLocal
	cfg.Capabilities.Compute = true
	cfg.Capabilities.Memory = true
}

// deviceNameFor mirrors the node's device-name fallback (config, then
// hostname) for the sign-in client name.
func deviceNameFor(cfg *config.Config) string {
	if cfg.ControlPlane.DeviceName != "" {
		return cfg.ControlPlane.DeviceName
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return "onesilo-node"
}

// ensureOllamaAvailable makes local inference possible: prefer a running
// server, then a user-installed binary, then download the official release
// into data_dir. Cheap when Ollama is already present.
func ensureOllamaAvailable(ctx context.Context, cfg *config.Config, dataDir string, p *prompter) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, probeErr := ollama.NewClient(cfg.Ollama.Host).Version(probeCtx)
	cancel()

	if probeErr == nil {
		p.printf("  found a running Ollama server at %s\n", cfg.Ollama.Host)
		return nil
	}
	if bin, err := ollama.FindBinary(cfg.Ollama.BinaryPath); err == nil {
		p.printf("  found ollama at %s — the node will run `ollama serve` itself\n", bin)
		cfg.Ollama.Manage = true
		return nil
	}
	url, err := ollamaAssetURL(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if !p.confirm(fmt.Sprintf("  Ollama is not installed. Download it into %s?", filepath.Join(dataDir, "ollama")), true) {
		return fmt.Errorf("ollama is required for compute — install it from https://ollama.com/download")
	}
	p.printf("  downloading %s ...\n", url)
	bin, err := installFromArchive(ctx, url, filepath.Join(dataDir, "ollama"), "ollama")
	if err != nil {
		return err
	}
	p.printf("  installed %s\n", bin)
	cfg.Ollama.BinaryPath = bin
	cfg.Ollama.Manage = true
	return nil
}

// ensureModel makes sure a model is installed locally, spawning a temporary
// `ollama serve` (stopped afterwards) when no server is running.
func ensureModel(ctx context.Context, settings config.Ollama, model string, out io.Writer) error {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := ollama.NewManager(func() ollama.Settings {
		return ollama.Settings{
			Host:         settings.Host,
			Manage:       settings.Manage,
			BinaryPath:   settings.BinaryPath,
			DefaultModel: settings.DefaultModel,
		}
	}, quiet)
	wasRunning := mgr.Reachable(ctx)
	if err := mgr.EnsureRunning(ctx); err != nil {
		return err
	}
	if !wasRunning {
		defer mgr.Stop()
	}

	client := ollama.NewClient(settings.Host)
	if models, err := client.Tags(ctx); err == nil && hasModel(models, model) {
		fmt.Fprintf(out, "  model %s is already installed\n", model)
		return nil
	}

	fmt.Fprintf(out, "  pulling %s ...\n", model)
	last := ""
	err := client.Pull(ctx, model, func(pr ollama.PullProgress) {
		if pr.Total > 0 {
			fmt.Fprintf(out, "\r    %s %d%%   ", pr.Status, pr.Completed*100/pr.Total)
			last = pr.Status
			return
		}
		if pr.Status != last {
			fmt.Fprintf(out, "\r    %s          \n", pr.Status)
			last = pr.Status
		}
	})
	fmt.Fprintln(out)
	return err
}

// hasModel reports whether want is in the installed model list, treating a
// missing tag as :latest on both sides (Ollama's own convention).
func hasModel(models []ollama.Model, want string) bool {
	norm := func(name string) string {
		if !strings.Contains(name, ":") {
			return name + ":latest"
		}
		return name
	}
	for _, m := range models {
		if norm(m.Name) == norm(want) {
			return true
		}
	}
	return false
}

// prompter asks yes/no questions on the terminal; assumeYes (the -yes flag)
// answers every question with its default, and EOF on stdin does the same,
// so `onesilo-node setup -yes` and piped input both stay non-blocking.
//
// Exactly one thing may read stdin at a time: once the control panel's
// reader goroutine owns stdin, it installs itself as readLine so prompts
// inside panel actions receive lines through it instead of racing it.
type prompter struct {
	in        *bufio.Reader
	readLine  func() (string, error)
	out       io.Writer
	assumeYes bool
}

func (p *prompter) printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

func (p *prompter) line() (string, error) {
	if p.readLine != nil {
		return p.readLine()
	}
	return p.in.ReadString('\n')
}

func (p *prompter) confirm(question string, def bool) bool {
	suffix, answer := " [y/N] ", "n"
	if def {
		suffix, answer = " [Y/n] ", "y"
	}
	if p.assumeYes {
		p.printf("%s%s%s\n", question, suffix, answer)
		return def
	}
	for {
		p.printf("%s%s", question, suffix)
		line, err := p.line()
		if err != nil {
			p.printf("%s\n", answer)
			return def
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}
