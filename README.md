# mcp-notifications

MCP server (stdio, JSON-RPC) to send local desktop notifications from Codex or other agents.

## OS requirements

- Linux (GNOME/KDE/etc): `notify-send` on `PATH` (typically `libnotify-bin`). If no DISPLAY/WAYLAND is available or `notify-send` fails, the server falls back to `gdbus` (from `glib2.0-bin`) and uses the session bus (`DBUS_SESSION_BUS_ADDRESS` or `/run/user/<uid>/bus`).
- macOS: `osascript` (preinstalled).
- Windows: `powershell.exe` or `pwsh` on `PATH` (requires an interactive desktop session for toasts).
- WSL (optional): if Linux notification paths fail, the server falls back to Windows toasts via `powershell.exe`.

## Build

```sh
go build -o mcp-notifications ./cmd/mcp-notifications
```

## Configure in Codex

Add an MCP server entry in `~/.codex/config.toml` pointing at the built binary:

```toml
[mcp_servers.notifications]
command = "/ABSOLUTE/PATH/TO/mcp-notifications"
```

If it is available on your `PATH` (e.g. `~/.local/bin`), you can use:

```toml
[mcp_servers.notifications]
command = "mcp-notifications"
```

## Exposed tool

- `notify`
  - Input: `{ "title": string, "message": string, "urgency"?: "low"|"normal"|"critical", "timeoutMs"?: number }`
  - Notes:
    - `urgency` and `timeoutMs` apply only on Linux (via `notify-send`).
