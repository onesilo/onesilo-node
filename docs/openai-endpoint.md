# OpenAI-compatible endpoint

The node can expose its local model as a standard OpenAI-compatible API, so
any tool that accepts an OpenAI base URL and API key — Cursor, Continue,
Cline, aider, the OpenAI SDKs — can use your node's compute directly.

```
/v1/chat/completions   chat (streaming and non-streaming)
/v1/completions        legacy completions
/v1/models             installed models
/v1/embeddings         embeddings
```

The endpoints are served on `lan.port` next to the Silo app protocol and are
reverse-proxied to Ollama's native OpenAI-compatible API. With a tunnel
configured they are reachable at the node's public hostname, e.g.
`https://<your-node>.onesilo.net/v1`.

## Enabling

```toml
[capabilities]
compute = true

[openai]
enabled = true
```

Or through the admin API: `PUT /v1/config` with
`{"openai": {"enabled": true}}`.

## API keys

Requests authenticate with `Authorization: Bearer <key>`. Keys are minted
through the admin API (localhost, `ONESILO_NODE_ADMIN_TOKEN`-authenticated):

```
POST   /v1/openai/keys          {"name": "cursor"}   → key shown once
GET    /v1/openai/keys          list (metadata only)
DELETE /v1/openai/keys/{id}     revoke
```

Keys look like `silo_sk_…`. Only their SHA-256 is stored
(`openai_keys.json` in the data dir), so a stolen store file cannot
authenticate; losing a key means minting a new one.

## Example: Cursor

1. Enable the surface and a tunnel, note the node's hostname.
2. Mint a key: `curl -H "Authorization: Bearer $ONESILO_NODE_ADMIN_TOKEN" \
   -d '{"name":"cursor"}' http://127.0.0.1:8766/v1/openai/keys`
3. In Cursor: Settings → Models → *Override OpenAI Base URL* →
   `https://<your-node>.onesilo.net/v1`, paste the key, add the model name
   as it appears in `/v1/models` (e.g. `llama3.2:3b`), Verify.

Note that only some Cursor features honor custom endpoints (chat does; tab
autocomplete does not), and Cursor routes requests via its own backend.
Clients that connect directly (Continue, Cline, aider) keep the traffic
between them and your node.

## Security model

This surface is deliberately separate from the Silo app protocol:

- The app protocol is end-to-end encrypted device pairing; nothing on this
  surface can touch memory or pairing, and keys grant inference only.
- This surface is standard TLS-to-the-edge HTTP. Through a managed or quick
  tunnel, the tunnel provider terminates TLS and can observe the traffic —
  the same trust model as any hosted inference API. If that is not
  acceptable, use `tunnel.mode = "external"` behind your own ingress, or
  keep the surface LAN-only.
- The surface is off by default, returns 404 while disabled, and requires
  `capabilities.compute`.
