package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	serverName    = "mcp-notifications"
	serverVersion = "0.1.0"
)

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type mcpInitializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Capabilities    mcpCapabilities  `json:"capabilities"`
	ServerInfo      mcpServerInfo    `json:"serverInfo"`
	Instructions    string           `json:"instructions,omitempty"`
}

type mcpCapabilities struct {
	Tools mcpToolsCapability `json:"tools,omitempty"`
}

type mcpToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type notifyArgs struct {
	Title     string `json:"title"`
	Message   string `json:"message"`
	Urgency   string `json:"urgency,omitempty"`   // linux: low|normal|critical
	TimeoutMs int    `json:"timeoutMs,omitempty"` // linux: notify-send expire time
}

type messageFraming uint8

const (
	framingLSP messageFraming = iota
	framingRaw
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		msg, framing, err := readMessage(in)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			_ = writeMessage(out, jsonrpcResponse{
				JSONRPC: "2.0",
				Error: &jsonrpcError{
					Code:    -32700,
					Message: "Parse error",
					Data:    err.Error(),
				},
			}, framing)
			_ = out.Flush()
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			_ = writeMessage(out, jsonrpcResponse{
				JSONRPC: "2.0",
				Error: &jsonrpcError{
					Code:    -32700,
					Message: "Parse error",
					Data:    err.Error(),
				},
			}, framing)
			_ = out.Flush()
			continue
		}

		// Notifications have no id; ignore unknown notifications.
		if len(req.ID) == 0 {
			continue
		}

		resp := handleRequest(ctx, req)
		if err := writeMessage(out, resp, framing); err != nil {
			return
		}
		_ = out.Flush()
	}
}

func handleRequest(ctx context.Context, req jsonrpcRequest) jsonrpcResponse {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "initialize":
		var params mcpInitializeParams
		_ = json.Unmarshal(req.Params, &params)
		proto := strings.TrimSpace(params.ProtocolVersion)
		if proto == "" {
			// Be permissive; many clients pass a protocolVersion anyway.
			proto = "2024-11-05"
		}

		resp.Result = mcpInitializeResult{
			ProtocolVersion: proto,
			Capabilities: mcpCapabilities{
				Tools: mcpToolsCapability{ListChanged: false},
			},
			ServerInfo: mcpServerInfo{Name: serverName, Version: serverVersion},
		}
		return resp

	case "tools/list":
		resp.Result = mcpToolsListResult{
			Tools: []mcpTool{
				{
					Name:        "notify",
					Description: "Shows a local desktop notification.",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title": map[string]any{
								"type":        "string",
								"description": "Notification title.",
							},
							"message": map[string]any{
								"type":        "string",
								"description": "Notification body/message.",
							},
							"urgency": map[string]any{
								"type":        "string",
								"enum":        []string{"low", "normal", "critical"},
								"description": "Linux only (notify-send).",
							},
							"timeoutMs": map[string]any{
								"type":        "integer",
								"minimum":     0,
								"description": "Linux only (notify-send).",
							},
						},
						"required":             []string{"title", "message"},
						"additionalProperties": false,
					},
				},
			},
		}
		return resp

	case "tools/call":
		var params mcpToolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &jsonrpcError{Code: -32602, Message: "Invalid params", Data: err.Error()}
			return resp
		}

		switch params.Name {
		case "notify":
			var args notifyArgs
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				resp.Result = mcpToolResult{
					IsError: true,
					Content: []mcpContent{{Type: "text", Text: "Invalid arguments: " + err.Error()}},
				}
				return resp
			}
			if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Message) == "" {
				resp.Result = mcpToolResult{
					IsError: true,
					Content: []mcpContent{{Type: "text", Text: "Missing required fields: title and message."}},
				}
				return resp
			}

			if err := sendNotification(ctx, args); err != nil {
				resp.Result = mcpToolResult{
					IsError: true,
					Content: []mcpContent{{Type: "text", Text: "Failed to send notification: " + err.Error()}},
				}
				return resp
			}

			resp.Result = mcpToolResult{
				Content: []mcpContent{{Type: "text", Text: "OK"}},
			}
			return resp

		default:
			resp.Result = mcpToolResult{
				IsError: true,
				Content: []mcpContent{{Type: "text", Text: "Unknown tool: " + params.Name}},
			}
			return resp
		}

	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "Method not found", Data: req.Method}
		return resp
	}
}

func readMessage(r *bufio.Reader) ([]byte, messageFraming, error) {
	for {
		b, err := r.Peek(1)
		if err != nil {
			return nil, framingLSP, err
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			_, _ = r.ReadByte()
			continue
		case '{', '[':
			var raw json.RawMessage
			dec := json.NewDecoder(r)
			if err := dec.Decode(&raw); err != nil {
				return nil, framingRaw, err
			}
			return raw, framingRaw, nil
		default:
			msg, err := readLSPMessage(r)
			return msg, framingLSP, err
		}
	}
}

func writeMessage(w *bufio.Writer, v any, framing messageFraming) error {
	switch framing {
	case framingRaw:
		enc := json.NewEncoder(w)
		return enc.Encode(v)
	default:
		return writeLSPMessage(w, v)
	}
}

func readLSPMessage(r *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		const prefix = "Content-Length:"
		normalized := strings.ToLower(trimmed)
		if strings.HasPrefix(normalized, strings.ToLower(prefix)) {
			v := strings.TrimSpace(trimmed[len(prefix):])
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", v, err)
			}
			contentLength = n
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing or invalid Content-Length")
	}

	buf := make([]byte, contentLength)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeLSPMessage(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return nil
}

func sendNotification(ctx context.Context, args notifyArgs) error {
	switch runtime.GOOS {
	case "linux":
		return notifyLinux(ctx, args)
	case "darwin":
		return notifyDarwin(ctx, args)
	case "windows":
		return notifyWindows(ctx, args)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func notifyLinux(ctx context.Context, args notifyArgs) error {
	env := envWithSessionBus()

	if hasDisplay() {
		err := notifyLinuxViaNotifySend(ctx, args, env)
		if err == nil {
			return nil
		}
		if isDisplayError(err) || strings.Contains(strings.ToLower(err.Error()), "notify-send was not found") {
			// If notify-send failed due to a missing display (or is unavailable), try DBus directly.
			dbusErr := notifyLinuxViaDBus(ctx, args, env)
			if dbusErr == nil {
				return nil
			}
			if isWSL() {
				if werr := notifyWindows(ctx, args); werr == nil {
					return nil
				}
			}
			return fmt.Errorf("notify-send failed (%v); gdbus fallback failed: %w", err, dbusErr)
		}
		if isWSL() {
			if werr := notifyWindows(ctx, args); werr == nil {
				return nil
			}
		}
		return err
	}

	dbusErr := notifyLinuxViaDBus(ctx, args, env)
	if dbusErr == nil {
		return nil
	}
	if isWSL() {
		// WSLg may fail without a graphical session; fall back to a Windows toast.
		if werr := notifyWindows(ctx, args); werr == nil {
			return nil
		}
	}
	return dbusErr
}

func notifyDarwin(ctx context.Context, args notifyArgs) error {
	path, err := exec.LookPath("osascript")
	if err != nil {
		return fmt.Errorf("osascript was not found in PATH")
	}

	// Pasamos title/message como argv para evitar problemas de escaping.
	cmd := exec.CommandContext(ctx,
		path,
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		args.Title,
		args.Message,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func notifyLinuxViaNotifySend(ctx context.Context, args notifyArgs, env []string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send was not found in PATH")
	}

	urgency := strings.TrimSpace(args.Urgency)
	if urgency == "" {
		urgency = "normal"
	}

	cmdArgs := []string{"--app-name", serverName, "--urgency", urgency}
	if args.TimeoutMs > 0 {
		cmdArgs = append(cmdArgs, "--expire-time", strconv.Itoa(args.TimeoutMs))
	}
	cmdArgs = append(cmdArgs, args.Title, args.Message)

	cmd := exec.CommandContext(ctx, path, cmdArgs...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func notifyLinuxViaDBus(ctx context.Context, args notifyArgs, env []string) error {
	path, err := exec.LookPath("gdbus")
	if err != nil {
		return fmt.Errorf("gdbus was not found in PATH")
	}

	timeout := args.TimeoutMs
	if timeout <= 0 {
		timeout = -1
	}

	cmdArgs := []string{
		"call",
		"--session",
		"--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications",
		"--method", "org.freedesktop.Notifications.Notify",
		serverName,
		"0",
		"",
		args.Title,
		args.Message,
		"[]",
		"{}",
		fmt.Sprintf("%d", timeout),
	}

	cmd := exec.CommandContext(ctx, path, cmdArgs...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func notifyWindows(ctx context.Context, args notifyArgs) error {
	title := sanitizeSingleLine(args.Title)
	message := sanitizeSingleLine(args.Message)

	ps := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null",
		"$template = [Windows.UI.Notifications.ToastTemplateType]::ToastText02",
		"$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent($template)",
		"$textNodes = $xml.GetElementsByTagName('text')",
		fmt.Sprintf("$textNodes.Item(0).AppendChild($xml.CreateTextNode('%s')) | Out-Null", psSingleQuote(title)),
		fmt.Sprintf("$textNodes.Item(1).AppendChild($xml.CreateTextNode('%s')) | Out-Null", psSingleQuote(message)),
		"$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)",
		fmt.Sprintf("$notifier = [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('%s')", psSingleQuote(serverName)),
		"$notifier.Show($toast)",
	}, "; ")

	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		powershell, err = exec.LookPath("pwsh")
		if err != nil {
			return fmt.Errorf("powershell.exe or pwsh was not found in PATH")
		}
	}

	cmd := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", ps)

	// Evita bloquear en entornos raros; toasts deberían ser rápidos.
	timer := time.AfterFunc(5*time.Second, func() {
		_ = cmd.Process.Kill()
	})
	defer timer.Stop()

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func psSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func sanitizeSingleLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func hasDisplay() bool {
	return strings.TrimSpace(os.Getenv("DISPLAY")) != "" ||
		strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) != ""
}

func isDisplayError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "display") || strings.Contains(msg, "x11")
}

func envWithSessionBus() []string {
	env := os.Environ()
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return env
	}

	if addr := defaultSessionBusAddress(); addr != "" {
		return append(env, "DBUS_SESSION_BUS_ADDRESS="+addr)
	}
	return env
}

func defaultSessionBusAddress() string {
	xdg := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if xdg == "" {
		xdg = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	if xdg != "" {
		path := filepath.Join(xdg, "bus")
		if _, err := os.Stat(path); err == nil {
			return "unix:path=" + path
		}
	}
	return ""
}

func isWSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
		return true
	}
	b, err = os.ReadFile("/proc/version")
	if err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
		return true
	}
	return false
}
