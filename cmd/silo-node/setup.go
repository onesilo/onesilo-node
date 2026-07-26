package main

import (
	"bufio"
	"context"
	"errors"
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

	"github.com/onesilo/silo-node/internal/compute/ollama"
	"github.com/onesilo/silo-node/internal/config"
	"github.com/onesilo/silo-node/internal/controlplane"
	"github.com/onesilo/silo-node/internal/gateway"
	"github.com/onesilo/silo-node/internal/tunnel"
)

// runSetup implements `silo-node setup`: an interactive wizard that takes a
// standalone machine from nothing to a runnable node in one command. It
// asks which mode the node runs in (Local Node: private memory + LLM; Local
// Relay: also relays the control-plane cloud surface) and whether to enable
// remote access (a Cloudflare quick tunnel, downloading cloudflared if
// needed), provisions the admin token, sets up local inference (finding or
// downloading Ollama and pulling the default model), optionally enables
// device memory, and — for a relay or an exposed node — control-plane
// credentials. Everything persists to the config file. Every step is
// idempotent, so re-running setup is always safe.
func runSetup(args []string) int {
	fs := flag.NewFlagSet("silo-node setup", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file (default ~/.silo-node/config.toml)")
	yes := fs.Bool("yes", false, "non-interactive: accept every default")
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

	// File + defaults only: transient env overrides must not be baked into
	// the config file setup writes.
	cfg, err := config.Load(config.LoadOptions{
		Path:      path,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "silo-node setup: "+err.Error())
		return 1
	}

	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "silo-node setup: "+err.Error())
		return 1
	}

	p := &prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout, assumeYes: *yes}
	p.printf("silo-node setup\n\n")
	p.printf("  data dir: %s\n", dataDir)
	p.printf("  config:   %s\n\n", path)

	// Node mode.
	p.printf("One Silo Node is a self-hosted node in the One Silo control plane.\n")
	p.printf("Memory and compute happen on this device.\n\n")
	p.printf("This node can be set up as a local node, or a secure gateway into the\n")
	p.printf("control plane for local agents.\n\n")
	modeDef := 1
	if cfg.Mode == config.ModeGateway {
		modeDef = 2
	}
	choice := p.choose("Which mode do you want?", []string{
		"Local Node  — memory + LLM served on this device",
		"Local Relay — also relays One Silo cloud silos, connectors, and MCP to local agents",
	}, modeDef)
	isGateway := choice == 2
	if isGateway {
		cfg.Mode = config.ModeGateway
	} else {
		cfg.Mode = config.ModeLocal
	}

	// Exposure — reachable from anywhere. Orthogonal to mode: either kind of
	// node can be exposed so the owner's authenticated apps reach its local
	// compute/memory remotely.
	p.printf("\nRemote access\n")
	p.printf("  Your One Silo Node can be accessed from anywhere. This tunnel is\n")
	p.printf("  encrypted end-to-end and can only be accessed by your One Silo\n")
	p.printf("  authenticated devices such as the One Silo iOS app and web.\n")
	p.printf("  Remote access requires a One Silo subscription — running the node\n")
	p.printf("  locally is always free.\n")
	wantExposed := cfg.Exposed()
	if p.confirm("  Enable access to this node from anywhere?", wantExposed) {
		if err := setupTunnel(ctx, &cfg, dataDir, p); err != nil {
			p.printf("  ! %s\n  ! remote access left off — the node still works locally\n", err)
			cfg.Tunnel.Mode = config.TunnelModeOff
		}
	} else {
		// "No" means not exposed — turn off any existing tunnel (quick or
		// external), not just a quick one.
		cfg.Tunnel.Mode = config.TunnelModeOff
	}
	exposed := cfg.Exposed()

	// Admin token — generated once, loaded automatically at start.
	p.printf("\nAdmin API token\n")
	if _, created, err := ensureAdminToken(dataDir); err != nil {
		fmt.Fprintln(os.Stderr, "silo-node setup: "+err.Error())
		return 1
	} else if created {
		p.printf("  generated %s (0600); silo-node loads it automatically\n", filepath.Join(dataDir, adminTokenFile))
	} else {
		p.printf("  using existing %s\n", filepath.Join(dataDir, adminTokenFile))
	}
	if env := os.Getenv("SILO_NODE_ADMIN_TOKEN"); env != "" {
		p.printf("  note: SILO_NODE_ADMIN_TOKEN is set in this shell and takes precedence at runtime\n")
	}

	// Compute — local LLM inference via Ollama. The point of a local node;
	// optional extra on a gateway.
	p.printf("\nCompute (local LLM inference via Ollama)\n")
	computeDefault := !isGateway || cfg.Capabilities.Compute
	if p.confirm("  Enable local LLM inference?", computeDefault) {
		if err := setupOllama(ctx, &cfg, dataDir, p); err != nil {
			p.printf("  ! %s\n  ! compute left disabled — fix the above and re-run setup\n", err)
			cfg.Capabilities.Compute = false
		} else {
			cfg.Capabilities.Compute = true
		}
	} else {
		cfg.Capabilities.Compute = false
	}

	// Memory — silos homed on this device. Same split as compute.
	p.printf("\nMemory (silos stored on this device)\n")
	memoryDefault := !isGateway || cfg.Capabilities.Memory
	if p.confirm("  Store silo memories on this device?", memoryDefault) {
		cfg.Capabilities.Memory = true
		if cfg.Capabilities.Compute {
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
	} else {
		cfg.Capabilities.Memory = false
	}

	// A node that relays the control plane (gateway) or is exposed as a
	// destination must authenticate to One Silo. A purely local, unexposed
	// node needs no control-plane credential.
	if isGateway || exposed {
		setupSignIn(ctx, &cfg, dataDir, p)
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "silo-node setup: "+err.Error())
		return 1
	}
	if err := config.Save(cfg, path); err != nil {
		fmt.Fprintln(os.Stderr, "silo-node setup: "+err.Error())
		return 1
	}

	p.printf("\nSetup complete — config written to %s.\n\n", path)
	p.printf("  mode:    %s\n", modeSummary(cfg))
	p.printf("  compute: %s\n", computeSummary(cfg))
	p.printf("  memory:  %s\n", enabledWord(cfg.Capabilities.Memory))
	if isGateway {
		p.printf("  relay:   http://127.0.0.1:%d%s/... (X-Silo-Node-Key authenticated)\n",
			cfg.LAN.Port, gateway.RoutePrefix)
	}
	p.printf("  remote:  %s\n", remoteSummary(cfg))
	if isGateway || exposed {
		p.printf("  auth:    %s\n", authSummary(cfg))
	}
	p.printf("\nStart the node:\n\n")
	if (isGateway || exposed) && cfg.ControlPlane.AuthMode == config.AuthModeAPIKey {
		p.printf("  export SILO_API_KEY=\"sc_...\"   # from your One Silo account\n")
	}
	p.printf("  silo-node\n")
	return 0
}

// modeSummary renders the node mode with its product name.
func modeSummary(cfg config.Config) string {
	if cfg.Mode == config.ModeGateway {
		return "Local Relay (gateway)"
	}
	return "Local Node"
}

// remoteSummary describes the exposure axis for the setup summary.
func remoteSummary(cfg config.Config) string {
	switch cfg.Tunnel.Mode {
	case config.TunnelModeQuick:
		return "reachable from anywhere (Cloudflare quick tunnel)"
	case config.TunnelModeExternal:
		return "reachable from anywhere (external URL: " + cfg.Tunnel.ExternalURL + ")"
	default:
		return "LAN/localhost only"
	}
}

// setupSignIn authenticates a node that talks to One Silo — a Local Relay
// or an exposed node — to the control plane. The default is the OAuth
// sign-in (the node gets its own credential and shows up in the dashboard,
// like the Silo iOS app); an sc_ API key is the fallback for headless
// machines or when sign-in fails.
func setupSignIn(ctx context.Context, cfg *config.Config, dataDir string, p *prompter) {
	p.printf("\nControl plane sign-in\n")
	if _, err := controlplane.LoadOAuthCredential(dataDir); err == nil {
		cfg.ControlPlane.AuthMode = config.AuthModeOAuth
		p.printf("  already signed in (%s); this node appears in your dashboard connections\n",
			filepath.Join(dataDir, controlplane.OAuthCredentialFile))
		return
	}
	p.printf("  This node connects with its own One Silo credential, like the Silo iOS app.\n")

	if p.assumeYes {
		// Non-interactive runs can't complete a browser flow.
		cfg.ControlPlane.AuthMode = config.AuthModeAPIKey
		p.printf("  non-interactive: skipping browser sign-in — using an sc_ API key instead\n")
		p.printf("  (re-run `silo-node setup` without -yes to sign in)\n")
		return
	}

	if p.confirm("  Sign in to One Silo now (opens your browser; new users can create an account there)?", true) {
		err := signIn(ctx, cfg.ControlPlane.URL, dataDir, deviceNameFor(cfg), p.out)
		if err == nil {
			cfg.ControlPlane.AuthMode = config.AuthModeOAuth
			p.printf("  signed in — credential stored at %s (0600)\n",
				filepath.Join(dataDir, controlplane.OAuthCredentialFile))
			p.printf("  this node now appears in your dashboard: https://dashboard.onesilo.com/connections\n")
			return
		}
		if errors.Is(err, errUserNotFound) {
			p.printf("  ! user not found — please create an account at https://onesilo.com first,\n")
			p.printf("  ! then re-run `silo-node setup` to sign in\n")
		} else {
			p.printf("  ! sign-in failed: %s\n", err)
			p.printf("  ! no One Silo account? Create one at https://onesilo.com and re-run `silo-node setup`\n")
		}
	}

	if p.confirm("  Use an sc_ API key instead?", true) {
		cfg.ControlPlane.AuthMode = config.AuthModeAPIKey
	}
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
	return "silo-node"
}

func authSummary(cfg config.Config) string {
	switch cfg.ControlPlane.AuthMode {
	case config.AuthModeOAuth:
		return "signed in (oauth credential in data dir)"
	case config.AuthModeAPIKey:
		return "sc_ API key (SILO_API_KEY env)"
	default:
		return "jwt (pushed by the desktop app)"
	}
}

func computeSummary(cfg config.Config) string {
	if !cfg.Capabilities.Compute {
		return "disabled"
	}
	if cfg.Ollama.BinaryPath != "" {
		return fmt.Sprintf("enabled (managed ollama at %s)", cfg.Ollama.BinaryPath)
	}
	if cfg.Ollama.Manage {
		return "enabled (managed ollama from $PATH)"
	}
	return fmt.Sprintf("enabled (existing server at %s)", cfg.Ollama.Host)
}

func enabledWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// setupOllama makes local inference work: prefer a running server, then a
// user-installed binary, then download the official release into data_dir.
// Finally offers to pull the default model so first inference doesn't fail.
func setupOllama(ctx context.Context, cfg *config.Config, dataDir string, p *prompter) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, probeErr := ollama.NewClient(cfg.Ollama.Host).Version(probeCtx)
	cancel()

	switch {
	case probeErr == nil:
		p.printf("  found a running Ollama server at %s\n", cfg.Ollama.Host)
	default:
		bin, err := ollama.FindBinary(cfg.Ollama.BinaryPath)
		if err == nil {
			p.printf("  found ollama at %s — the node will run `ollama serve` itself\n", bin)
			cfg.Ollama.Manage = true
		} else {
			url, err := ollamaAssetURL(runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return err
			}
			if !p.confirm(fmt.Sprintf("  Ollama is not installed. Download it into %s?", filepath.Join(dataDir, "ollama")), true) {
				return fmt.Errorf("ollama is required for compute — install it from https://ollama.com/download and re-run setup")
			}
			p.printf("  downloading %s ...\n", url)
			bin, err := installFromArchive(ctx, url, filepath.Join(dataDir, "ollama"), "ollama")
			if err != nil {
				return err
			}
			p.printf("  installed %s\n", bin)
			cfg.Ollama.BinaryPath = bin
			cfg.Ollama.Manage = true
		}
	}

	if p.confirm(fmt.Sprintf("  Pull the default model %s now?", cfg.Ollama.DefaultModel), true) {
		if err := ensureModel(ctx, cfg.Ollama, cfg.Ollama.DefaultModel, p.out); err != nil {
			p.printf("  ! %s\n  ! you can pull it later; inference fails until a model is installed\n", err)
		}
	}
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

// setupTunnel switches the tunnel to managed mode (a stable, One Silo-
// provisioned hostname), downloading cloudflared into data_dir when no
// install is found. A pre-existing explicit quick/external choice is kept.
func setupTunnel(ctx context.Context, cfg *config.Config, dataDir string, p *prompter) error {
	if bin, err := tunnel.FindBinary(cfg.Tunnel.CloudflaredPath); err == nil {
		p.printf("  found cloudflared at %s\n", bin)
	} else {
		url, archive, err := cloudflaredAssetURL(runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return err
		}
		destDir := filepath.Join(dataDir, "cloudflared")
		if !p.confirm(fmt.Sprintf("  cloudflared is not installed. Download it into %s?", destDir), true) {
			return fmt.Errorf("cloudflared is required for the quick tunnel")
		}
		p.printf("  downloading %s ...\n", url)
		var bin string
		if archive {
			bin, err = installFromArchive(ctx, url, destDir, "cloudflared")
		} else {
			bin, err = installBinary(ctx, url, destDir, "cloudflared")
		}
		if err != nil {
			return err
		}
		p.printf("  installed %s\n", bin)
		cfg.Tunnel.CloudflaredPath = bin
	}
	// Prefer the managed named tunnel: One Silo provisions a stable
	// hostname for this node (paid feature; the node explains the upgrade
	// path at runtime if the account is free). Keep an explicit
	// quick/external choice a user has made in the config.
	if cfg.Tunnel.Mode == config.TunnelModeOff || cfg.Tunnel.Mode == "" ||
		cfg.Tunnel.Mode == config.TunnelModeManaged {
		cfg.Tunnel.Mode = config.TunnelModeManaged
		p.printf("  remote access will use a stable One Silo hostname for this node\n")
	} else {
		p.printf("  keeping existing tunnel mode %q from config\n", cfg.Tunnel.Mode)
	}
	return nil
}

// prompter asks yes/no questions on the terminal; assumeYes (the -yes flag)
// answers every question with its default, and EOF on stdin does the same,
// so `silo-node setup -yes` and piped input both stay non-blocking.
type prompter struct {
	in        *bufio.Reader
	out       io.Writer
	assumeYes bool
}

func (p *prompter) printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

// choose presents a numbered list and returns the selected 1-based index.
// def (1-based) is used on enter, EOF, and -yes runs.
func (p *prompter) choose(question string, options []string, def int) int {
	for i, opt := range options {
		p.printf("  %d) %s\n", i+1, opt)
	}
	if p.assumeYes {
		p.printf("%s [%d] %d\n", question, def, def)
		return def
	}
	for {
		p.printf("%s [%d] ", question, def)
		line, err := p.in.ReadString('\n')
		if err != nil {
			p.printf("%d\n", def)
			return def
		}
		s := strings.TrimSpace(line)
		if s == "" {
			return def
		}
		for i := range options {
			if s == fmt.Sprintf("%d", i+1) {
				return i + 1
			}
		}
	}
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
		line, err := p.in.ReadString('\n')
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
