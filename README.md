# mcp-http-bridge

[![CI](https://github.com/kirkanos/mcp-http-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/kirkanos/mcp-http-bridge/actions/workflows/ci.yml)

A single-binary bridge that lets an MCP client which only speaks **stdio** talk to a
remote **Streamable HTTP** MCP server.

Go standard library only — no runtime, no `node`, no `npx`.

## Why

Some MCP clients only accept stdio servers in their config file. Claude Desktop is one
of them: the schema behind `claude_desktop_config.json` requires a `command` and has no
`type`/`url` fields, so a remote HTTP endpoint pasted in there is rejected at startup
with *"not valid MCP server configurations and were skipped"*. Remote servers are
supposed to be added as connectors in the UI instead — which is not always available,
for example when an organisation policy disables custom connectors.

This bridge closes that gap: the client spawns it as an ordinary stdio server, and it
forwards everything to the remote endpoint over HTTP.

Connecting from your own machine rather than via a vendor's cloud has two side effects
worth knowing: geo-blocking or IP allowlists on your reverse proxy can stay as they are,
and a URL that embeds an API key never leaves your device.

## Install

Download a prebuilt archive from the
[latest release](https://github.com/kirkanos/mcp-http-bridge/releases/latest) —
macOS and Linux (arm64/amd64) and Windows (amd64):

```sh
tar -xzf mcp-http-bridge_*_darwin_arm64.tar.gz
install -m 755 mcp-http-bridge ~/.local/bin/
mcp-http-bridge -version
```

Each release ships a `SHA256SUMS` file; verify with `shasum -a 256 -c SHA256SUMS`.
On macOS, Gatekeeper may quarantine the downloaded binary because it is unsigned —
clear it with `xattr -d com.apple.quarantine mcp-http-bridge`.

With a Go toolchain:

```sh
go install github.com/kirkanos/mcp-http-bridge@latest
```

Or build from a checkout:

```sh
go build -o ~/.local/bin/mcp-http-bridge .
```

## Release

Tagging is what publishes a release; the workflow builds every target and uploads the
archives:

```sh
git tag v0.2.0
git push origin v0.2.0
```

`./scripts/build-release.sh v0.2.0` produces the same archives locally in `dist/`.

## Usage

```sh
mcp-http-bridge <url> [-H "Header: value"]...
```

The URL may also be given via `MCP_HTTP_URL`. Repeat `-H` for each extra header, e.g.
`-H "Authorization: Bearer $TOKEN"`.

### Claude Desktop

In `claude_desktop_config.json` (macOS:
`~/Library/Application Support/Claude/`, Windows: `%APPDATA%\Claude\`):

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/Users/you/.local/bin/mcp-http-bridge",
      "args": ["https://example.com/mcp"]
    }
  }
}
```

Use an **absolute path** for `command` — the client does not spawn the process through
your shell, so `PATH` and shell aliases do not apply. Restart the client completely
afterwards; the config file is only read at startup.

If the endpoint needs a token, prefer `env` over `args` so it does not show up in the
process list:

```json
{
  "mcpServers": {
    "my-server": {
      "command": "/Users/you/.local/bin/mcp-http-bridge",
      "args": ["https://example.com/mcp", "-H", "Authorization: Bearer ${TOKEN}"],
      "env": { "TOKEN": "..." }
    }
  }
}
```

Note that `${TOKEN}` expansion depends on the client; when in doubt, pass
`MCP_HTTP_URL` via `env` and keep the secret in the URL.

## What it handles

- **Session management** — captures `Mcp-Session-Id` from the server's first response
  and sends it on every later request, plus `MCP-Protocol-Version` once the handshake
  has settled on a version. Issues a `DELETE` to release the session on exit.
- **Streaming** — parses `text/event-stream` responses incrementally and forwards each
  payload as it arrives, so progress notifications reach the client during a long tool
  call instead of all at once at the end. Plain `application/json` responses work too.
- **Concurrency** — each incoming message is forwarded in its own goroutine, so one slow
  tool call does not block anything else. Writes to stdout are serialised, and messages
  are compacted to a single line as the stdio transport requires.
- **Failures** — a transport error is turned into a JSON-RPC error response for the
  matching request id, so the client fails fast instead of waiting forever for a reply
  that can no longer arrive. Notifications carry no id and are only logged to stderr.

Diagnostics go to stderr, which MCP clients typically capture in their logs.

## Limitations

- Only the client→server direction is polled. The bridge does not open a standalone
  `GET` event stream, so a server that pushes requests to the client outside of a
  response cycle will not be able to reach it.
- No automatic re-initialisation if the server expires the session; the resulting HTTP
  error is passed through to the client.
- No retries. A failed request surfaces as an error rather than being repeated, since
  MCP requests are not generally idempotent.

## License

[MIT](LICENSE)
