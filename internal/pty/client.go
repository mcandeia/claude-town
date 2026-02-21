/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pty

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ttyd protocol message types.
const (
	// msgInput is prepended to input data sent to the terminal.
	msgInput byte = '0'
	// msgOutput is prepended to output data received from the terminal.
	msgOutput byte = '1'
	// msgResize is prepended to resize messages.
	msgResize byte = '2'
)

// Client is a WebSocket PTY client for connecting to ttyd servers.
// It implements io.Closer.
type Client struct {
	url  string
	conn *websocket.Conn
	mu   sync.Mutex
}

// NewClient creates a new Client that will connect to the ttyd server
// running at the given pod IP and port.
func NewClient(podIP string, port int) *Client {
	return &Client{
		url: fmt.Sprintf("ws://%s:%d/ws", podIP, port),
	}
}

// Connect dials the ttyd WebSocket endpoint.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to ttyd at %s: %w", c.url, err)
	}
	c.conn = conn
	return nil
}

// SendCommand sends an input message to the terminal. The command is prefixed
// with the ttyd input message type byte ('0') and a newline is appended.
func (c *Client) SendCommand(command string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	msg := append([]byte{msgInput}, []byte(command+"\n")...)
	if err := c.conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
		return fmt.Errorf("failed to send command: %w", err)
	}
	return nil
}

// ReadOutput continuously reads output messages from the terminal and calls
// handler for each output chunk. It blocks until the context is cancelled or
// an error occurs.
func (c *Client) ReadOutput(ctx context.Context, handler func(output string)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("failed to read message: %w", err)
		}

		if len(msg) < 1 {
			continue
		}

		// Only process output messages (type '1').
		if msg[0] == msgOutput {
			handler(string(msg[1:]))
		}
	}
}

// RunAndWaitForMarker sends a command and accumulates output until the given
// marker string is found or the context times out. It returns the accumulated
// output up to and including the marker line.
func (c *Client) RunAndWaitForMarker(ctx context.Context, command, marker string) (string, error) {
	if err := c.SendCommand(command); err != nil {
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	var (
		buf    strings.Builder
		done   = make(chan struct{})
		result string
	)

	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		defer close(done)
		_ = c.ReadOutput(readCtx, func(output string) {
			buf.WriteString(output)
			if strings.Contains(buf.String(), marker) {
				result = buf.String()
				cancel()
			}
		})
	}()

	select {
	case <-done:
		if result != "" {
			return result, nil
		}
		return buf.String(), fmt.Errorf("read ended without finding marker %q", marker)
	case <-ctx.Done():
		cancel()
		<-done
		if result != "" {
			return result, nil
		}
		return buf.String(), fmt.Errorf("timed out waiting for marker %q after %v", marker, time.Since(time.Time{}))
	}
}

// Close closes the underlying WebSocket connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// ParsePRURL extracts a pull request URL from output that contains the
// ":::PR_URL:::" marker. It returns the URL found after the marker, or an
// empty string if no marker is present.
func ParsePRURL(output string) string {
	const marker = ":::PR_URL:::"
	idx := strings.Index(output, marker)
	if idx == -1 {
		return ""
	}

	rest := output[idx+len(marker):]
	rest = strings.TrimSpace(rest)

	// Take the first whitespace-delimited token as the URL.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
