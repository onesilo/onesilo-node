// onesilo-node admin UI. Dependency-free SPA: token gate, hash router,
// three views (Silos / Models / Settings) over the admin API.
"use strict";

const TOKEN_KEY = "onesilo-node-admin-token";

// One-time token handoff from `onesilo-node setup`: the control panel opens
// /#token=<hex> so the operator lands signed in instead of pasting the
// token. The fragment never leaves the browser (it is not sent with any
// request); consume it and scrub it from the URL and history immediately.
{
  const handoff = location.hash.match(/^#token=([A-Za-z0-9._~-]+)$/);
  if (handoff) {
    localStorage.setItem(TOKEN_KEY, handoff[1]);
    history.replaceState(null, "", location.pathname + location.search);
  }
}

const $ = (sel, el) => (el || document).querySelector(sel);
const main = $("#main");

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (v !== null && v !== undefined) node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    node.append(c.nodeType ? c : document.createTextNode(c));
  }
  return node;
}

// --- API ---

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(path, opts) {
  const token = localStorage.getItem(TOKEN_KEY) || "";
  const res = await fetch(path, {
    ...opts,
    headers: {
      Authorization: "Bearer " + token,
      ...(opts && opts.body ? { "Content-Type": "application/json" } : {}),
      ...(opts && opts.headers),
    },
  });
  if (res.status === 401) {
    showGate(true);
    throw new ApiError(401, "unauthorized");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch (_) { /* non-JSON error body */ }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

function toast(message, isError) {
  const t = el("div", { class: "toast" + (isError ? " error" : "") }, message);
  document.body.append(t);
  setTimeout(() => t.remove(), isError ? 6000 : 3000);
}

// --- token gate ---

function showGate(rejected) {
  $("#app").classList.add("hidden");
  $("#token-gate").classList.remove("hidden");
  $("#token-error").classList.toggle("hidden", !rejected);
  $("#token-input").focus();
}

async function tryOpen() {
  try {
    const status = await api("/v1/status");
    $("#node-version").textContent = "v" + status.version;
  } catch (err) {
    if (err.status === 401) return; // gate already shown
    // API down or other error: still open the shell; views report errors.
  }
  $("#token-gate").classList.add("hidden");
  $("#app").classList.remove("hidden");
  route();
}

$("#token-save").addEventListener("click", () => {
  localStorage.setItem(TOKEN_KEY, $("#token-input").value.trim());
  tryOpen();
});
$("#token-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") $("#token-save").click();
});

// --- router ---

let pollTimer = null;
// Aborted on every navigation. The log console holds a response open for as
// long as it is on screen, so without this, leaving and returning to the
// page would stack a second stream on top of the first.
let viewAbort = null;

function route() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
  if (viewAbort) {
    viewAbort.abort();
    viewAbort = null;
  }
  const hash = location.hash || "#/silos";
  const parts = hash.slice(2).split("/").filter(Boolean);
  const page = parts[0] || "silos";

  document.querySelectorAll(".sidebar nav a").forEach((a) => {
    a.classList.toggle("active", a.dataset.nav === page);
  });

  main.replaceChildren();
  if (page === "silos" && parts[1]) renderSiloDetail(decodeURIComponent(parts[1]));
  else if (page === "models") renderModels();
  else if (page === "logs") renderLogs();
  else if (page === "settings") renderSettings();
  else renderSilos();
}

window.addEventListener("hashchange", route);

// --- Silos ---

async function renderSilos() {
  main.append(
    el("div", { class: "page-title" }, "Silos"),
    el("div", { class: "page-sub" }, "Memory stored on this node, grouped by silo."),
  );
  const list = el("div", { class: "card-grid" });
  main.append(list);
  try {
    const silos = await api("/v1/silos");
    if (!silos.length) {
      list.replaceWith(el("div", { class: "card empty" },
        "No silos yet. Memories written through the node's memory API will show up here."));
      return;
    }
    for (const s of silos) {
      list.append(el("div", {
        class: "card clickable",
        onclick: () => { location.hash = "#/silos/" + encodeURIComponent(s.silo_id); },
      },
        el("div", { class: "row-title" }, s.silo_id),
        el("div", { class: "row-sub" }, s.count + (s.count === 1 ? " memory" : " memories")),
      ));
    }
  } catch (err) {
    if (err.status === 401) return;
    list.replaceWith(el("div", { class: "card" },
      el("p", { class: "error" }, "Couldn't load silos: " + err.message),
      el("p", { class: "muted" }, "Is the memory capability enabled and running?"),
    ));
  }
}

async function exportSilo(siloID) {
  const token = localStorage.getItem(TOKEN_KEY) || "";
  const res = await fetch("/v1/silos/" + encodeURIComponent(siloID) + "/export", {
    headers: { Authorization: "Bearer " + token },
  });
  if (res.status === 401) {
    // Match the rest of the UI: a rejected token re-opens the gate.
    showGate(true);
    return;
  }
  if (!res.ok) {
    toast("Export failed (" + res.status + ")", true);
    return;
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = el("a", { href: url, download: siloID + ".silo" });
  document.body.append(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

const KIND_BADGES = {
  decision: "badge-orange",
  fact: "badge-blue",
  preference: "badge-green",
  action_item: "badge-red",
  explicit: "badge-gray",
};

async function renderSiloDetail(siloID) {
  main.append(
    el("div", { class: "toolbar" },
      el("a", { class: "btn btn-outline btn-sm", href: "#/silos" }, "← Silos"),
      el("div", { class: "spacer" }),
      el("button", {
        class: "btn btn-primary",
        onclick: () => exportSilo(siloID),
      }, "Export .silo"),
    ),
    el("div", { class: "page-title" }, siloID),
  );
  const card = el("div", { class: "card" }, el("p", { class: "muted" }, "Loading…"));
  main.append(card);

  let data;
  try {
    data = await api("/v1/silos/" + encodeURIComponent(siloID) + "/memories");
  } catch (err) {
    if (err.status === 401) return;
    card.replaceChildren(el("p", { class: "error" }, "Couldn't load memories: " + err.message));
    return;
  }

  const memories = data.memories || [];
  main.insertBefore(
    el("div", { class: "page-sub" },
      memories.length + (memories.length === 1 ? " memory" : " memories")),
    card,
  );
  if (!memories.length) {
    card.replaceChildren(el("div", { class: "empty" }, "This silo is empty."));
    return;
  }

  card.replaceChildren(...memories.map((m) => {
    const kind = m.metadata && (m.metadata.kind || m.metadata.type);
    const row = el("div", { class: "row" },
      el("div", { class: "grow" },
        el("div", { class: "mem-content" }, m.content),
        el("div", { class: "mem-meta" },
          kind ? el("span", { class: "badge " + (KIND_BADGES[kind] || "badge-gray") }, kind) : null,
          el("span", null, (m.created_at || "").slice(0, 19).replace("T", " ")),
          el("span", null, m.id.slice(0, 12)),
        ),
      ),
      el("button", {
        class: "btn btn-danger btn-sm",
        onclick: async (e) => {
          const btn = e.currentTarget;
          if (btn.dataset.armed !== "1") {
            btn.dataset.armed = "1";
            btn.textContent = "Confirm delete";
            setTimeout(() => { btn.dataset.armed = ""; btn.textContent = "Delete"; }, 3000);
            return;
          }
          try {
            await api("/v1/silos/" + encodeURIComponent(siloID) +
              "/memories/" + encodeURIComponent(m.id), { method: "DELETE" });
            row.remove();
            toast("Memory deleted");
          } catch (err) {
            if (err.status !== 401) toast("Delete failed: " + err.message, true);
          }
        },
      }, "Delete"),
    );
    return row;
  }));
}

// --- Models ---

function fmtBytes(n) {
  if (!n) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(n >= 10 || i === 0 ? 0 : 1) + " " + units[i];
}

async function renderModels() {
  main.append(
    el("div", { class: "page-title" }, "Models"),
    el("div", { class: "page-sub" }, "Local LLM models served by this node through Ollama."),
  );
  const container = el("div");
  main.append(container);
  await drawModels(container);
  // Poll while a pull is running so the progress bar moves.
  pollTimer = setInterval(async () => {
    if ($(".progress", container)) await drawModels(container);
  }, 2000);
}

async function drawModels(container) {
  let info;
  try {
    info = await api("/v1/models");
  } catch (err) {
    if (err.status === 401) return;
    container.replaceChildren(el("div", { class: "card" },
      el("p", { class: "error" }, "Couldn't load models: " + err.message)));
    return;
  }

  const listCard = el("div", { class: "card" }, el("h2", null, "Installed"));
  if (info.error) {
    listCard.append(el("p", { class: "error" }, info.error),
      el("p", { class: "muted" }, "Is the compute capability enabled and Ollama reachable?"));
  } else if (!info.models || !info.models.length) {
    listCard.append(el("div", { class: "empty" }, "No models installed yet — pull one below."));
  } else {
    for (const m of info.models) {
      listCard.append(el("div", { class: "row" },
        el("div", { class: "grow" },
          el("div", { class: "row-title" }, m.name, " ",
            m.active ? el("span", { class: "badge badge-green" }, "active") : null, " ",
            m.default && !m.active ? el("span", { class: "badge badge-blue" }, "default") : null,
          ),
          el("div", { class: "row-sub" },
            [fmtBytes(m.size_bytes), (m.modified_at || "").slice(0, 10)]
              .filter(Boolean).join(" · ")),
        ),
        m.default ? null : el("button", {
          class: "btn btn-outline btn-sm",
          onclick: async () => {
            try {
              await api("/v1/config", {
                method: "PUT",
                body: JSON.stringify({ ollama: { default_model: m.name } }),
              });
              toast(m.name + " is now the default model");
              await drawModels(container);
            } catch (err) {
              if (err.status !== 401) toast("Activate failed: " + err.message, true);
            }
          },
        }, "Activate"),
      ));
    }
  }

  const pullCard = el("div", { class: "card" }, el("h2", null, "Pull a model"));
  const pull = info.pull;
  if (pull && pull.status === "pulling") {
    const pct = pull.total > 0 ? Math.round((pull.completed / pull.total) * 100) : null;
    pullCard.append(
      el("div", { class: "row-title" }, "Pulling ", el("code", null, pull.model), " ",
        el("span", { class: "badge badge-blue" }, pct !== null ? pct + "%" : "starting")),
      pull.detail ? el("div", { class: "row-sub" }, pull.detail) : null,
      el("div", { class: "progress" },
        el("div", { style: "width:" + (pct !== null ? pct : 5) + "%" })),
    );
  } else {
    if (pull && pull.status === "error") {
      pullCard.append(el("p", { class: "error" },
        "Pull of " + pull.model + " failed: " + pull.detail));
    } else if (pull && pull.status === "done") {
      pullCard.append(el("p", null,
        el("span", { class: "badge badge-green" }, "done"), " Pulled ", el("code", null, pull.model), "."));
    }
    const input = el("input", { type: "text", placeholder: "e.g. llama3.2:3b" });
    pullCard.append(
      el("div", { class: "field-row", style: "align-items:flex-end" },
        el("div", { class: "field", style: "margin-bottom:0" },
          el("label", null, "Model name"), input),
        el("button", {
          class: "btn btn-primary",
          onclick: async () => {
            const model = input.value.trim();
            if (!model) return;
            try {
              await api("/v1/models/pull", {
                method: "POST",
                body: JSON.stringify({ model }),
              });
              await drawModels(container);
            } catch (err) {
              if (err.status !== 401) toast("Pull failed: " + err.message, true);
            }
          },
        }, "Pull"),
      ),
      el("p", { class: "hint muted", style: "margin-top:8px" },
        "Any model from the Ollama library. The download continues in the background."),
    );
  }

  container.replaceChildren(listCard, pullCard);
}

// --- Settings ---

/**
 * Connect / disconnect the node's One Silo account.
 *
 * This used to read "run onesilo-node setup to sign in", which is an odd
 * thing to tell someone already looking at the node's admin UI — and on a
 * headless box there may be no terminal to go to.
 *
 * The sign-in itself happens in a new tab against the control plane, which
 * redirects back to the node's own callback. Nothing is polled here: the
 * callback tab reports its own outcome, and this view refreshes when the
 * operator returns to it, so there is no timer left running against a tab
 * that may never come back.
 */
async function renderControlPlaneAuth() {
  const host = document.querySelector("#cp-auth");
  if (!host) return;
  host.textContent = "";

  let auth;
  try {
    auth = await api("/v1/controlplane/auth");
  } catch (err) {
    host.appendChild(el("span", { class: "error" }, "couldn't read sign-in state: " + err.message));
    return;
  }

  if (auth.connected) {
    host.appendChild(el("span", { class: "badge badge-green" }, "connected"));
    if (!auth.can_refresh) {
      // Worth saying: without a refresh token this connection expires and
      // needs a fresh sign-in, and the operator would otherwise only find
      // out when registration started failing.
      host.appendChild(el("span", { class: "hint" },
        " no refresh token — this will need signing in again when it expires"));
    }
    host.appendChild(el("button", {
      class: "btn btn-secondary",
      onclick: async (ev) => {
        ev.target.disabled = true;
        try {
          await api("/v1/controlplane/auth", { method: "DELETE" });
          await renderControlPlaneAuth();
        } catch (err) {
          ev.target.disabled = false;
          host.appendChild(el("span", { class: "error" }, " " + err.message));
        }
      },
    }, "Disconnect"));
    return;
  }

  host.appendChild(el("span", { class: "badge badge-gray" }, "not connected"));
  const connect = el("button", {
    class: "btn",
    onclick: async (ev) => {
      ev.target.disabled = true;
      ev.target.textContent = "Contacting One Silo…";
      try {
        const started = await api("/v1/controlplane/auth/start", { method: "POST" });
        // Opened rather than navigated: leaving the console would lose any
        // unsaved state on this page, and the callback tab is
        // self-describing.
        window.open(started.authorization_url, "_blank", "noopener");
        ev.target.textContent = "Waiting for sign-in…";
        host.appendChild(el("span", { class: "hint" },
          " finish in the new tab, then "));
        host.appendChild(el("button", {
          class: "btn btn-secondary",
          onclick: () => renderControlPlaneAuth(),
        }, "Check again"));
      } catch (err) {
        ev.target.disabled = false;
        ev.target.textContent = "Connect to One Silo";
        host.appendChild(el("span", { class: "error" }, " " + err.message));
      }
    },
  }, "Connect to One Silo");
  host.appendChild(connect);
}

async function renderSettings() {
  main.append(
    el("div", { class: "page-title" }, "Settings"),
    el("div", { class: "page-sub" }, "Node configuration and live status."),
  );
  const container = el("div");
  main.append(container);

  let cfg, status;
  try {
    [cfg, status] = await Promise.all([api("/v1/config"), api("/v1/status")]);
  } catch (err) {
    if (err.status === 401) return;
    container.append(el("div", { class: "card" },
      el("p", { class: "error" }, "Couldn't load settings: " + err.message)));
    return;
  }

  $("#node-version").textContent = "v" + status.version;

  // -- status card --
  const reg = status.registration || {};
  const statusCard = el("div", { class: "card" },
    el("h2", null, "Status"),
    el("dl", { class: "kv" },
      el("dt", null, "Version"), el("dd", null, status.version + " (" + (status.commit || "?").slice(0, 8) + ")"),
      el("dt", null, "Mode"),
      el("dd", null, el("span", {
        class: "badge " + (status.mode === "gateway" ? "badge-blue" : "badge-green"),
      }, status.mode)),
      el("dt", null, "Uptime"), el("dd", null, fmtUptime(status.uptime_seconds)),
      // Not gated on gateway mode: signing in is a PREREQUISITE for going
      // remote, so hiding it until the node is already a gateway would leave
      // a local node with no way to connect — which is exactly the gap this
      // fills for a headless box with no terminal to run setup in.
      el("dt", null, "One Silo account"),
      el("dd", { id: "cp-auth" }, "…"),
      ...(status.mode === "gateway" ? [
        el("dt", null, "Control plane"),
        el("dd", null,
          reg.registered
            ? el("span", { class: "badge badge-green" }, "registered")
            : el("span", { class: "badge badge-gray" }, "not registered"),
          reg.device_id ? " " + reg.device_id : ""),
        el("dt", null, "Tunnel"),
        el("dd", null, status.tunnel && status.tunnel.url
          ? status.tunnel.url
          : (status.tunnel ? status.tunnel.mode : "off")),
      ] : []),
      ...((reg.subscription_required && status.tunnel && status.tunnel.mode !== "off") ? [
        el("dt", null, "Remote access"),
        el("dd", null,
          el("span", { class: "badge badge-orange" }, "subscription required"),
          " — this node is set to be reachable from anywhere, but that needs a ",
          el("a", { href: "https://onesilo.com/pricing", target: "_blank", rel: "noopener" }, "One Silo subscription"),
          ". It still works on your local network."),
      ] : []),
      el("dt", null, "Capabilities"),
      el("dd", null, ...(status.capabilities || []).map((c) =>
        el("span", {
          class: "badge " + (c.healthy ? "badge-green" : c.enabled ? "badge-orange" : "badge-gray"),
          style: "margin-right:6px",
          title: c.detail || "",
        }, c.name)),
      ),
      el("dt", null, "Node key"),
      el("dd", null, nodeKeyEl(status.node_key)),
    ),
  );

  // -- config form --
  const f = {};
  const field = (label, input, hint) => el("div", { class: "field" },
    el("label", null, label), input,
    hint ? el("div", { class: "hint" }, hint) : null);
  const select = (name, value, options) => {
    const s = el("select", null, ...options.map(([v, label]) =>
      el("option", { value: v, ...(v === value ? { selected: "" } : {}) }, label)));
    f[name] = s;
    return s;
  };
  const text = (name, value, placeholder) => {
    const i = el("input", { type: "text", value: value || "", placeholder: placeholder || "" });
    f[name] = i;
    return i;
  };
  const check = (name, checked, label) => {
    const i = el("input", { type: "checkbox", ...(checked ? { checked: "" } : {}) });
    f[name] = i;
    return el("label", { class: "check" }, i, label);
  };

  const formCard = el("div", { class: "card" },
    el("h2", null, "Configuration"),
    field("Node mode",
      select("mode", cfg.mode, [
        ["local", "Local Node — memory + LLM served on this device"],
        ["gateway", "Local Relay — also relays cloud silos, connectors, and MCP to local agents"],
      ]),
      "Relay adds the One Silo cloud surface. Remote access (below) is a separate choice."),
    el("div", { class: "field-row" },
      field("Memory capability", check("cap_memory", cfg.capabilities.memory, "Enabled")),
      field("Compute capability", check("cap_compute", cfg.capabilities.compute, "Enabled")),
    ),
    field("Device name", text("device_name", cfg.control_plane.device_name, "defaults to hostname"),
      "How this node appears in the One Silo dashboard."),
    field("Control plane URL", text("cp_url", cfg.control_plane.url)),
    field("Auth mode", select("auth_mode", cfg.control_plane.auth_mode, [
      ["oauth", "oauth — signed in via onesilo-node setup"],
      ["jwt", "jwt — token pushed by the desktop app"],
      ["api_key", "api_key — from SILO_NODE_API_KEY"],
    ])),
    el("div", { class: "field-row" },
      field("Ollama host", text("ollama_host", cfg.ollama.host)),
      field("Default model", text("default_model", cfg.ollama.default_model, "e.g. llama3.2:3b")),
    ),
    field("Manage Ollama", check("ollama_manage", cfg.ollama.manage,
      "Node starts and supervises its own Ollama server")),
    el("div", { class: "field-row" },
      field("Remote access", select("tunnel_mode", cfg.tunnel.mode, [
        ["off", "off — LAN/localhost only"],
        ["quick", "on — Cloudflare quick tunnel (reachable from anywhere)"],
        ["external", "on — bring your own external URL"],
      ]), "Exposes this node to your authenticated apps. Works in either mode."),
      field("External URL", text("external_url", cfg.tunnel.external_url, "https://…")),
    ),
    el("div", { class: "field-row" },
      field("LAN server", check("lan_enabled", cfg.lan.enabled, "Advertise on the local network (Bonjour)")),
      field("LAN port", text("lan_port", String(cfg.lan.port))),
    ),
    el("button", {
      class: "btn btn-primary",
      onclick: async (e) => {
        const btn = e.currentTarget;
        btn.disabled = true;
        const patch = {
          mode: f.mode.value,
          capabilities: { memory: f.cap_memory.checked, compute: f.cap_compute.checked },
          control_plane: {
            url: f.cp_url.value.trim(),
            auth_mode: f.auth_mode.value,
            device_name: f.device_name.value.trim(),
          },
          ollama: {
            host: f.ollama_host.value.trim(),
            manage: f.ollama_manage.checked,
            default_model: f.default_model.value.trim(),
          },
          tunnel: { mode: f.tunnel_mode.value, external_url: f.external_url.value.trim() },
          lan: { enabled: f.lan_enabled.checked, port: parseInt(f.lan_port.value, 10) || cfg.lan.port },
        };
        try {
          await api("/v1/config", { method: "PUT", body: JSON.stringify(patch) });
          toast("Settings saved — the node is reconfiguring");
          main.replaceChildren();
          renderSettings();
        } catch (err) {
          if (err.status !== 401) toast("Save failed: " + err.message, true);
        } finally {
          btn.disabled = false;
        }
      },
    }, "Save changes"),
  );

  const pairingCard = el("div", { class: "card" });
  container.append(statusCard, pairingCard, formCard);
  // After the append: renderControlPlaneAuth() looks the row up by id.
  void renderControlPlaneAuth();
  renderPendingPairings(pairingCard);
}

// renderPendingPairings shows automated-pairing sessions awaiting a Short
// Authentication String (SAS) confirmation. The operator compares the code
// shown here with the one in the Silo app, then clicks Verify — that trusts
// the app's identity key so its (and future) sessions can run inference.
async function renderPendingPairings(card) {
  card.replaceChildren(
    el("h2", null, "Device pairing"),
    el("p", { class: "hint" },
      "New devices pairing over the relay show a security code here. Compare it with the code in the Silo app, then confirm — this proves no one is intercepting the connection."),
  );
  let data;
  try {
    data = await api("/v1/pairing/pending");
  } catch (err) {
    if (err.status === 401) return;
    card.append(el("p", { class: "muted" }, "Pairing status unavailable: " + err.message));
    return;
  }
  const pending = (data && data.pending) || [];
  if (pending.length === 0) {
    card.append(el("p", { class: "muted" }, "No devices are waiting to be verified."));
    return;
  }
  for (const p of pending) {
    const row = el("div", { class: "field-row", style: "align-items:center" },
      el("div", null,
        el("div", { style: "font-size:1.4em;letter-spacing:2px;font-family:monospace" },
          (p.sas || "").replace(/(\d{4})(\d{4})/, "$1 $2")),
        el("div", { class: "hint" }, "account " + shortId(p.account_id))),
      el("button", {
        class: "btn btn-primary btn-sm",
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            await api("/v1/pairing/verify", {
              method: "POST",
              body: JSON.stringify({ account_id: p.account_id, app_id_pub: p.app_id_pub }),
            });
            toast("Device verified — it can now use this node");
            renderPendingPairings(card);
          } catch (err) {
            if (err.status !== 401) toast("Verify failed: " + err.message, true);
            e.currentTarget.disabled = false;
          }
        },
      }, "Verify"));
    card.append(row);
  }
}

function shortId(id) {
  if (!id) return "?";
  return id.length > 12 ? id.slice(0, 8) + "…" : id;
}

function nodeKeyEl(key) {
  if (!key) return el("span", { class: "muted" }, "—");
  const code = el("code", null, "••••••••" + key.slice(-6));
  let shown = false;
  const btn = el("button", {
    class: "btn btn-outline btn-sm",
    style: "margin-left:8px",
    onclick: () => {
      shown = !shown;
      code.textContent = shown ? key : "••••••••" + key.slice(-6);
      btn.textContent = shown ? "Hide" : "Reveal";
    },
  }, "Reveal");
  return el("span", null, code, btn);
}

// --- Logs ---

const LOG_LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];
// Bounds the DOM, not the server's ring: an all-day console would otherwise
// grow until the tab is unusable.
const LOG_VIEW_MAX = 2000;

async function renderLogs() {
  let minLevel = "DEBUG";
  let follow = true;
  let paused = false;

  const status = el("span", { class: "muted log-status" }, "connecting…");
  const stream = el("div", { class: "log-stream" });
  // Records that arrived while paused, replayed on resume so pausing to
  // read something doesn't silently lose what happened meanwhile.
  const heldWhilePaused = [];
  const levelSelect = el("select", {
    class: "select",
    onchange: (e) => {
      minLevel = e.target.value;
      applyLevel();
    },
  }, ...LOG_LEVELS.map((l) => el("option", { value: l }, l + " and above")));
  const pauseBtn = el("button", {
    class: "btn btn-outline btn-sm",
    onclick: () => {
      paused = !paused;
      pauseBtn.textContent = paused ? "Resume" : "Pause";
      // Resuming without catching up would leave a silent hole, so the
      // buffered lines are flushed rather than discarded.
      if (!paused) {
        for (const line of heldWhilePaused) appendLine(line);
        heldWhilePaused.length = 0;
      }
    },
  }, "Pause");

  main.append(
    el("div", { class: "page-title" }, "Logs"),
    el("div", { class: "page-sub" },
      "What the node is doing right now. Streamed live; the last " +
      "1000 lines are kept, so a restart clears the history."),
    el("div", { class: "toolbar" },
      levelSelect,
      el("label", { class: "check" },
        el("input", {
          type: "checkbox",
          checked: "checked",
          onchange: (e) => {
            follow = e.target.checked;
            if (follow) stream.scrollTop = stream.scrollHeight;
          },
        }),
        "Follow",
      ),
      pauseBtn,
      el("button", {
        class: "btn btn-outline btn-sm",
        onclick: () => stream.replaceChildren(),
      }, "Clear"),
      el("div", { class: "spacer" }),
      status,
    ),
    stream,
  );

  // Scrolling up is an explicit "let me read this" — stop yanking the view
  // to the bottom until the operator returns there.
  stream.addEventListener("scroll", () => {
    const atBottom = stream.scrollHeight - stream.scrollTop - stream.clientHeight < 24;
    if (follow !== atBottom) {
      follow = atBottom;
      $(".check input", main).checked = atBottom;
    }
  });

  function levelRank(level) {
    const i = LOG_LEVELS.indexOf((level || "INFO").toUpperCase());
    return i === -1 ? 1 : i; // unknown levels behave like INFO
  }

  function applyLevel() {
    const min = levelRank(minLevel);
    for (const row of stream.children) {
      row.classList.toggle("hidden", levelRank(row.dataset.level) < min);
    }
  }

  function appendLine(record) {
    const level = (record.level || "INFO").toUpperCase();
    const attrs = Object.entries(record)
      .filter(([k]) => k !== "time" && k !== "level" && k !== "msg")
      .map(([k, v]) => k + "=" + (typeof v === "string" ? v : JSON.stringify(v)))
      .join(" ");
    const row = el("div", { class: "log-row log-" + level.toLowerCase() },
      el("span", { class: "log-time" }, fmtLogTime(record.time)),
      el("span", { class: "log-level" }, level),
      el("span", { class: "log-msg" }, record.msg || ""),
      attrs ? el("span", { class: "log-attrs" }, attrs) : null,
    );
    row.dataset.level = level;
    if (levelRank(level) < levelRank(minLevel)) row.classList.add("hidden");
    stream.append(row);
    while (stream.childElementCount > LOG_VIEW_MAX) stream.firstElementChild.remove();
    if (follow) stream.scrollTop = stream.scrollHeight;
  }

  function handleRecord(json) {
    let record;
    try {
      record = JSON.parse(json);
    } catch (_) {
      record = { msg: json }; // never drop a line just because it didn't parse
    }
    if (paused) {
      heldWhilePaused.push(record);
      // Bound the hold too, or a long pause becomes a memory leak.
      if (heldWhilePaused.length > LOG_VIEW_MAX) heldWhilePaused.shift();
      return;
    }
    appendLine(record);
  }

  viewAbort = new AbortController();
  // Deliberately fetch + read the body rather than EventSource: EventSource
  // cannot set an Authorization header, and the alternative is putting the
  // admin token in the query string where it lands in history.
  try {
    const res = await fetch("/v1/logs/stream", {
      headers: { Authorization: "Bearer " + (localStorage.getItem(TOKEN_KEY) || "") },
      signal: viewAbort.signal,
    });
    if (res.status === 401) {
      showGate(true);
      return;
    }
    if (!res.ok) {
      status.textContent = "stream failed (" + res.status + ")";
      status.className = "error log-status";
      return;
    }
    status.textContent = "live";
    status.className = "log-status log-live";

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      // SSE events are separated by a blank line; keep any partial tail.
      const events = buffer.split("\n\n");
      buffer = events.pop();
      for (const event of events) {
        const data = event
          .split("\n")
          .filter((l) => l.startsWith("data:"))
          .map((l) => l.slice(5).trimStart())
          .join("\n");
        if (data) handleRecord(data);
      }
    }
    status.textContent = "disconnected — reload to reconnect";
    status.className = "muted log-status";
  } catch (err) {
    if (err.name === "AbortError") return; // navigated away
    status.textContent = "stream error: " + err.message;
    status.className = "error log-status";
  }
}

function fmtLogTime(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  if (isNaN(d)) return "";
  const p = (n, w) => String(n).padStart(w || 2, "0");
  return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds()) +
    "." + p(d.getMilliseconds(), 3);
}

function fmtUptime(secs) {
  if (!secs && secs !== 0) return "—";
  const d = Math.floor(secs / 86400), h = Math.floor((secs % 86400) / 3600),
    m = Math.floor((secs % 3600) / 60);
  if (d > 0) return d + "d " + h + "h";
  if (h > 0) return h + "h " + m + "m";
  return m + "m";
}

// --- boot ---

if (!localStorage.getItem(TOKEN_KEY)) showGate(false);
else tryOpen();
