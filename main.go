// mcp-http-bridge translates between the MCP stdio transport and the
// Streamable HTTP transport, so that clients which only support stdio servers
// (e.g. Claude Desktop's claude_desktop_config.json) can talk to a remote
// MCP endpoint.
//
// Usage:
//
//	mcp-http-bridge <url> [-H "Header: value"]...
//
// The URL may also be supplied via MCP_HTTP_URL.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	stdout   = bufio.NewWriter(os.Stdout)
	stdoutMu sync.Mutex
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[mcp-http-bridge] "+format+"\n", args...)
}

// emit writes one JSON-RPC message to stdout as a single line, which is what
// the stdio transport requires: messages are newline-delimited and must not
// contain embedded newlines.
func emit(msg []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, msg); err != nil {
		logf("dropping malformed message from server: %v", err)
		return
	}
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	stdout.Write(buf.Bytes())
	stdout.WriteByte('\n')
	stdout.Flush()
}

type bridge struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu        sync.RWMutex
	sessionID string
	protocol  string
}

func main() {
	url := os.Getenv("MCP_HTTP_URL")
	headers := map[string]string{}

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-H", "--header":
			if i+1 >= len(args) {
				logf("missing value after %s", args[i])
				os.Exit(2)
			}
			i++
			name, value, ok := strings.Cut(args[i], ":")
			if !ok {
				logf("header must be \"Name: value\", got %q", args[i])
				os.Exit(2)
			}
			headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
		default:
			url = args[i]
		}
	}
	if url == "" {
		logf("usage: mcp-http-bridge <url> [-H \"Header: value\"]...")
		os.Exit(2)
	}

	b := &bridge{
		url:     url,
		headers: headers,
		client: &http.Client{
			// No client-level timeout: tool calls may legitimately run long.
			// The transport still bounds connection setup and idle reuse.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   15 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
	b.run()
}

// run reads newline-delimited JSON-RPC messages from stdin until EOF. The
// client (not this bridge) serialises initialize, so it is safe to forward
// later messages concurrently — a slow tool call must not block notifications.
func (b *bridge) run() {
	in := bufio.NewReader(os.Stdin)
	var wg sync.WaitGroup

	for {
		line, err := in.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			msg := append([]byte(nil), trimmed...)
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.forward(msg)
			}()
		}
		if err != nil {
			if err != io.EOF {
				logf("reading stdin: %v", err)
			}
			break
		}
	}

	wg.Wait()
	b.endSession()
}

func (b *bridge) forward(msg []byte) {
	req, err := http.NewRequest(http.MethodPost, b.url, bytes.NewReader(msg))
	if err != nil {
		b.replyError(msg, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range b.headers {
		req.Header.Set(name, value)
	}
	b.mu.RLock()
	session, protocol := b.sessionID, b.protocol
	b.mu.RUnlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		b.replyError(msg, err)
		return
	}
	defer resp.Body.Close()

	if id := resp.Header.Get("Mcp-Session-Id"); id != "" && id != session {
		b.mu.Lock()
		b.sessionID = id
		b.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if len(detail) > 300 {
			detail = detail[:300] + "..."
		}
		b.replyError(msg, fmt.Errorf("HTTP %d from server: %s", resp.StatusCode, detail))
		return
	}

	// 202 Accepted with no body is the normal reply to a notification.
	switch contentType := resp.Header.Get("Content-Type"); {
	case strings.Contains(contentType, "text/event-stream"):
		b.pumpSSE(resp.Body)
	case strings.Contains(contentType, "application/json"):
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			b.replyError(msg, err)
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			b.deliver(body)
		}
	}
}

// pumpSSE forwards each SSE data payload as it arrives, so streamed progress
// notifications reach the client before the final response.
func (b *bridge) pumpSSE(body io.Reader) {
	reader := bufio.NewReader(body)
	var data strings.Builder

	flush := func() {
		if data.Len() > 0 {
			b.deliver([]byte(data.String()))
			data.Reset()
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			field := strings.TrimRight(line, "\r\n")
			switch {
			case field == "":
				flush()
			case strings.HasPrefix(field, ":"): // comment / keep-alive
			case strings.HasPrefix(field, "data:"):
				value := strings.TrimPrefix(strings.TrimPrefix(field, "data:"), " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
		}
		if err != nil {
			flush()
			if err != io.EOF {
				logf("reading event stream: %v", err)
			}
			return
		}
	}
}

// deliver passes a server message to the client, noting the negotiated
// protocol version so later requests can carry the required header.
func (b *bridge) deliver(msg []byte) {
	var probe struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg, &probe); err == nil && probe.Result.ProtocolVersion != "" {
		b.mu.Lock()
		b.protocol = probe.Result.ProtocolVersion
		b.mu.Unlock()
	}
	emit(msg)
}

// replyError turns a transport failure into a JSON-RPC error response, so the
// client fails fast instead of waiting forever for a reply that cannot come.
// Notifications carry no id and get no response, only a stderr log.
func (b *bridge) replyError(request []byte, cause error) {
	logf("%v", cause)

	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(request, &probe); err != nil || len(probe.ID) == 0 || string(probe.ID) == "null" {
		return
	}

	response, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      probe.ID,
		"error": map[string]any{
			"code":    -32603,
			"message": cause.Error(),
		},
	})
	if err != nil {
		return
	}
	emit(response)
}

func (b *bridge) endSession() {
	b.mu.RLock()
	session := b.sessionID
	b.mu.RUnlock()
	if session == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, b.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	for name, value := range b.headers {
		req.Header.Set(name, value)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
