package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const searchLimit = 32 << 10 // bytes of search results a call returns

// MCP is a minimal client for a stdio MCP server that offers web search. The
// server starts on the first search and stays up for the session; searches
// run one at a time.
type MCP struct {
	Command []string // the server, e.g. uvx minimax-coding-plan-mcp
	Env     []string // KEY=VALUE pairs added to its environment
	Tool    string   // its search tool, which takes a query

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	id    int
}

// Search runs one web search.
func (m *MCP) Search(ctx context.Context, query string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil {
		if err := m.start(ctx); err != nil {
			m.stop()
			return "", fmt.Errorf("search server: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	raw, err := m.request(ctx, "tools/call", map[string]any{"name": m.Tool, "arguments": map[string]any{"query": query}})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	text := b.String()
	if len(text) > searchLimit {
		text = text[:searchLimit] + "\n[results cut]"
	}
	if res.IsError {
		return "", errors.New(text)
	}
	return text, nil
}

// Close stops the server.
func (m *MCP) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop()
}

func (m *MCP) start(ctx context.Context) error {
	if len(m.Command) == 0 {
		return errors.New("no command")
	}
	cmd := exec.Command(m.Command[0], m.Command[1:]...)
	cmd.Env = append(os.Environ(), m.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.cmd, m.stdin, m.lines = cmd, stdin, make(chan []byte, 16)
	go func(lines chan<- []byte) {
		in := bufio.NewScanner(stdout)
		in.Buffer(make([]byte, 0, 1<<20), 32<<20)
		for in.Scan() {
			lines <- append([]byte(nil), in.Bytes()...)
		}
		close(lines)
	}(m.lines)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a first run downloads the server
	defer cancel()
	if _, err := m.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "council", "version": "1"},
	}); err != nil {
		return err
	}
	return m.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (m *MCP) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = m.stdin.Write(append(b, '\n'))
	return err
}

// request sends one call and waits for its answer, skipping whatever else the
// server writes. A server that does not answer in time is stopped, and the
// next search starts a fresh one.
func (m *MCP) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m.id++
	id := m.id
	if err := m.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			m.stop()
			return nil, ctx.Err()
		case line, ok := <-m.lines:
			if !ok {
				m.stop()
				return nil, errors.New("the search server exited")
			}
			var msg struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &msg) != nil || msg.Method != "" || msg.ID == nil || *msg.ID != id {
				continue
			}
			if msg.Error != nil {
				return nil, errors.New(msg.Error.Message)
			}
			return msg.Result, nil
		}
	}
}

func (m *MCP) stop() {
	if m.cmd == nil {
		return
	}
	_ = m.stdin.Close()
	_ = m.cmd.Process.Kill()
	_ = m.cmd.Wait()
	m.cmd = nil
}
