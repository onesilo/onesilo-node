package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/onesilo/onesilo-node/internal/adminapi"
	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/controlplane"
	"github.com/onesilo/onesilo-node/internal/logging"
	"github.com/onesilo/onesilo-node/internal/node"
	"github.com/onesilo/onesilo-node/internal/tunnel"
)

// nodeLogFile is where the control panel sends the running node's logs
// (inside data_dir), keeping the interactive screen clean.
const nodeLogFile = "node.log"

// panel drives a node running in this process from a cleared-screen menu.
// Every choice acts on the live node through the same ApplyConfigPatch /
// Reconcile path the admin API uses, so changes persist to the config file
// and take effect without a restart.
type panel struct {
	n          *node.Node
	p          *prompter
	adminToken string
	logPath    string
}

// runPanel starts the node and enters the control-panel loop. It returns
// when the operator quits, stdin closes, the process is signalled, or the
// node fails.
func runPanel(ctx context.Context, cfg config.Config, cfgPath, dataDir, adminToken string, p *prompter) int {
	logPath := filepath.Join(dataDir, nodeLogFile)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}
	defer logFile.Close()

	// Libraries that log through the std log package (zeroconf, net/http
	// server errors) would otherwise scribble over the panel.
	log.SetOutput(logFile)
	defer log.SetOutput(os.Stderr)

	n, err := node.New(cfg, cfgPath, adminToken, logging.NewWithWriter(cfg.Log, logFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, "onesilo-node setup: "+err.Error())
		return 1
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- n.Run(runCtx) }()

	// Give the first Reconcile a moment so the opening screen shows
	// capabilities running rather than transient "not started" warnings.
	waitForStartup(runCtx, n, 3*time.Second)

	// Reads happen on a goroutine so the loop can also watch for signals
	// and for the node dying underneath us (e.g. admin port already bound
	// by another onesilo-node). From here on this goroutine is stdin's
	// only reader — prompts inside panel actions must come through it too
	// (via p.readLine), or they'd race it for lines.
	lines := make(chan string)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			s, err := r.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimSpace(s)
		}
	}()
	p.readLine = func() (string, error) {
		select {
		case line, ok := <-lines:
			if !ok {
				return "", io.EOF
			}
			return line, nil
		case <-runCtx.Done():
			return "", runCtx.Err()
		}
	}
	defer func() { p.readLine = nil }()

	pl := &panel{n: n, p: p, adminToken: adminToken, logPath: logPath}

	stopNode := func() {
		cancel()
		<-runDone
		fmt.Fprintf(p.out, "\nNode stopped.\n")
	}

	notice := ""
	for {
		pl.render(notice)
		select {
		case <-ctx.Done():
			stopNode()
			return 0
		case err := <-runDone:
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nonesilo-node setup: node exited: %v\n", err)
				fmt.Fprintf(os.Stderr, "(is another onesilo-node already running? check %s)\n", pl.logPath)
				return 1
			}
			return 0
		case line, ok := <-lines:
			if !ok {
				stopNode()
				return 0
			}
			done := false
			notice, done = pl.handle(runCtx, line)
			if done {
				stopNode()
				return 0
			}
		}
	}
}

// waitForStartup polls until every enabled capability reports running (or
// the timeout passes) so the first render reflects the steady state.
func waitForStartup(ctx context.Context, n *node.Node, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		allRunning := true
		for _, c := range n.Status(ctx).Capabilities {
			if c.Enabled && !c.Running {
				allRunning = false
				break
			}
		}
		if allRunning {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// render clears the terminal and draws the panel from the node's live
// status.
func (pl *panel) render(notice string) {
	cfg := pl.n.ConfigSnapshot()
	st := pl.n.Status(context.Background())
	pending := len(pl.n.PendingPairings())
	fmt.Fprint(pl.p.out, "\x1b[2J\x1b[H")
	fmt.Fprint(pl.p.out, renderPanelScreen(cfg, st, pending, pl.logPath, notice))
}

// renderPanelScreen renders the whole panel as a string; pure so tests can
// pin the layout.
func renderPanelScreen(cfg config.Config, st adminapi.Status, pendingPairings int, logPath, notice string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Welcome to One Silo Node\n\n")
	fmt.Fprintf(&b, "Current Configuration:\n")
	fmt.Fprintf(&b, "  Mode:          %s\n", modeName(cfg))
	fmt.Fprintf(&b, "  Capabilities:  %s\n", capabilitiesLine(cfg))
	fmt.Fprintf(&b, "  Admin:         http://127.0.0.1:%d\n", cfg.Admin.Port)
	fmt.Fprintf(&b, "  Remote access: %s\n", remoteLine(cfg, st))
	fmt.Fprintf(&b, "  Logs:          %s\n", logPath)

	for _, c := range st.Capabilities {
		if c.Enabled && !c.Healthy && c.Detail != "" {
			fmt.Fprintf(&b, "\n  ! %s: %s\n", c.Name, c.Detail)
		}
	}
	if pendingPairings > 0 {
		fmt.Fprintf(&b, "\n  * %d device(s) waiting for pairing confirmation — approve in the admin interface\n", pendingPairings)
	}

	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "  1. Launch Admin Interface (127.0.0.1:%d)\n", cfg.Admin.Port)
	fmt.Fprintf(&b, "  2. %s\n", modeMenuLabel(cfg))
	fmt.Fprintf(&b, "  3. %s\n", remoteMenuLabel(cfg))
	fmt.Fprintf(&b, "  q. Quit (stops the node)\n\n")
	if notice != "" {
		fmt.Fprintf(&b, "%s\n\n", notice)
	}
	fmt.Fprintf(&b, "Select an option (Enter refreshes): ")
	return b.String()
}

// modeName is the product name for the configured mode.
func modeName(cfg config.Config) string {
	if cfg.Mode == config.ModeGateway {
		return "Local Relay"
	}
	return "Local"
}

// capabilitiesLine lists what the node is configured to provide. LAN means
// the node advertises itself to Silo apps on the local network.
func capabilitiesLine(cfg config.Config) string {
	var caps []string
	if cfg.Capabilities.Compute {
		caps = append(caps, "Compute")
	}
	if cfg.Capabilities.Memory {
		caps = append(caps, "Memory")
	}
	if cfg.LAN.Enabled {
		caps = append(caps, "LAN")
	}
	if len(caps) == 0 {
		return "none"
	}
	return strings.Join(caps, ", ")
}

// modeMenuLabel is menu item 2: the mode the node would switch to.
func modeMenuLabel(cfg config.Config) string {
	if cfg.Mode == config.ModeGateway {
		return "Switch to Local Node"
	}
	return "Switch to Local Relay"
}

// remoteMenuLabel is menu item 3: toggling the exposure axis.
func remoteMenuLabel(cfg config.Config) string {
	if cfg.Exposed() {
		return "Disable access from anywhere"
	}
	return "Enable access from anywhere"
}

// remoteLine describes the live remote-access state.
func remoteLine(cfg config.Config, st adminapi.Status) string {
	if !cfg.Exposed() {
		return "off (LAN/localhost only)"
	}
	if st.Tunnel.URL != "" {
		return st.Tunnel.URL
	}
	if cfg.Tunnel.Mode == config.TunnelModeExternal {
		return cfg.Tunnel.ExternalURL + " (external)"
	}
	return "enabling — waiting for the tunnel (Enter refreshes; progress in the admin interface)"
}

// handle runs one menu selection. It returns the notice to show above the
// next prompt and whether the panel should quit.
func (pl *panel) handle(ctx context.Context, line string) (notice string, quit bool) {
	switch strings.ToLower(line) {
	case "":
		return "", false
	case "1":
		return pl.openAdmin(), false
	case "2":
		return pl.toggleMode(ctx), false
	case "3":
		return pl.toggleRemote(ctx), false
	case "q", "quit", "exit":
		return "", true
	default:
		return fmt.Sprintf("Unknown option %q.", line), false
	}
}

// openAdmin opens the admin UI in the default browser, handing the token
// over in the URL fragment so the operator lands signed in. The fragment
// stays in the browser (never sent over the network) and the UI scrubs it
// from the URL and history immediately.
func (pl *panel) openAdmin() string {
	cfg := pl.n.ConfigSnapshot()
	url := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Admin.Port)
	openBrowser(url + "#token=" + pl.adminToken)
	return "Admin interface opened in your browser — " + url
}

// toggleMode switches Local Node <-> Local Relay. A relay talks to the
// control plane on behalf of local agents, so switching to it first makes
// sure this node holds a One Silo credential.
func (pl *panel) toggleMode(ctx context.Context) string {
	cfg := pl.n.ConfigSnapshot()
	if cfg.Mode == config.ModeGateway {
		mode := config.ModeLocal
		if _, err := pl.n.ApplyConfigPatch(ctx, adminapi.ConfigPatch{Mode: &mode}); err != nil {
			return "! " + err.Error()
		}
		return "Now running as a Local Node."
	}

	if msg := pl.ensureSignedIn(ctx,
		"A Local Relay signs in to One Silo to relay your cloud silos, connectors,\nand MCP to local agents."); msg != "" {
		return msg
	}
	mode := config.ModeGateway
	if _, err := pl.n.ApplyConfigPatch(ctx, adminapi.ConfigPatch{Mode: &mode}); err != nil {
		return "! " + err.Error()
	}
	return "Now running as a Local Relay."
}

// toggleRemote flips the "access from anywhere" axis. Enabling pairs the
// node with the One Silo control plane when it isn't already (OAuth in the
// browser — the flow also carries the subscription requirement), makes sure
// cloudflared is available, and switches the tunnel to the managed mode
// where One Silo provisions a stable hostname for this node.
func (pl *panel) toggleRemote(ctx context.Context) string {
	cfg := pl.n.ConfigSnapshot()
	if cfg.Exposed() {
		off := config.TunnelModeOff
		if _, err := pl.n.ApplyConfigPatch(ctx, adminapi.ConfigPatch{
			Tunnel: &adminapi.TunnelPatch{Mode: &off},
		}); err != nil {
			return "! " + err.Error()
		}
		return "Access from anywhere disabled — the node is reachable on this network only."
	}

	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return "! " + err.Error()
	}

	pl.p.printf("\nRemote access is end-to-end encrypted and reachable only by your One Silo\n")
	pl.p.printf("authenticated devices. It requires a One Silo subscription — running the\n")
	pl.p.printf("node locally is always free.\n")

	if msg := pl.ensureSignedIn(ctx,
		"Enabling remote access pairs this node with the One Silo control plane."); msg != "" {
		return msg
	}

	bin, err := ensureCloudflared(ctx, cfg.Tunnel.CloudflaredPath, dataDir, pl.p)
	if err != nil {
		return "! " + err.Error() + " — remote access left off; the node still works locally."
	}

	managed := config.TunnelModeManaged
	patch := adminapi.ConfigPatch{Tunnel: &adminapi.TunnelPatch{Mode: &managed}}
	if bin != cfg.Tunnel.CloudflaredPath {
		patch.Tunnel.CloudflaredPath = &bin
	}
	if _, err := pl.n.ApplyConfigPatch(ctx, patch); err != nil {
		return "! " + err.Error()
	}
	return "Access from anywhere enabling — One Silo is provisioning a stable hostname\nfor this node. Press Enter to refresh; the URL appears under Remote access."
}

// ensureSignedIn makes sure the node can authenticate to the control
// plane, running the browser OAuth sign-in when no credential is stored
// yet. Returns "" when authenticated, else the notice explaining why not.
// A node already configured with an sc_ API key counts as signed in.
func (pl *panel) ensureSignedIn(ctx context.Context, why string) string {
	cfg := pl.n.ConfigSnapshot()
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return "! " + err.Error()
	}

	if _, err := controlplane.LoadOAuthCredential(dataDir); err == nil {
		return pl.ensureAuthMode(ctx, config.AuthModeOAuth)
	}
	if cfg.ControlPlane.AuthMode == config.AuthModeAPIKey {
		return ""
	}

	pl.p.printf("\n%s\n", why)
	if !pl.p.confirm("Sign in to One Silo now (opens your browser; new users can create an account there)?", true) {
		return "Sign-in declined — nothing changed."
	}
	if err := signIn(ctx, cfg.ControlPlane.URL, dataDir, deviceNameFor(&cfg), pl.p.out); err != nil {
		if errors.Is(err, errUserNotFound) {
			return "! No One Silo account found — create one at https://onesilo.com, then try again."
		}
		return "! Sign-in failed: " + err.Error()
	}
	if msg := pl.ensureAuthMode(ctx, config.AuthModeOAuth); msg != "" {
		return msg
	}
	pl.p.printf("\nSigned in — this node now appears at https://dashboard.onesilo.com/connections\n")
	return ""
}

// ensureAuthMode patches control_plane.auth_mode when it differs.
func (pl *panel) ensureAuthMode(ctx context.Context, mode string) string {
	if pl.n.ConfigSnapshot().ControlPlane.AuthMode == mode {
		return ""
	}
	if _, err := pl.n.ApplyConfigPatch(ctx, adminapi.ConfigPatch{
		ControlPlane: &adminapi.ControlPlanePatch{AuthMode: &mode},
	}); err != nil {
		return "! " + err.Error()
	}
	return ""
}

// ensureCloudflared returns a usable cloudflared binary path, downloading
// the official release into data_dir when none is installed. The returned
// path may equal the configured one (including ""), meaning nothing needs
// patching.
func ensureCloudflared(ctx context.Context, configuredPath, dataDir string, p *prompter) (string, error) {
	if bin, err := tunnel.FindBinary(configuredPath); err == nil {
		p.printf("  found cloudflared at %s\n", bin)
		return configuredPath, nil
	}
	url, archive, err := cloudflaredAssetURL(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	destDir := filepath.Join(dataDir, "cloudflared")
	if !p.confirm(fmt.Sprintf("  cloudflared is needed for remote access. Download it into %s?", destDir), true) {
		return "", fmt.Errorf("cloudflared is required for remote access")
	}
	p.printf("  downloading %s ...\n", url)
	var bin string
	if archive {
		bin, err = installFromArchive(ctx, url, destDir, "cloudflared")
	} else {
		bin, err = installBinary(ctx, url, destDir, "cloudflared")
	}
	if err != nil {
		return "", err
	}
	p.printf("  installed %s\n", bin)
	return bin, nil
}
