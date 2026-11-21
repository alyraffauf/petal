package agentmcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"petal/internal/routing"
	"petal/internal/sessions"
)

const TokenEnv = "PETAL_DELEGATION_TOKEN"
const deliveryAckMethod = "petal/ackResultDelivery"

type server struct {
	cwd       string
	bound     func() (sessions.Record, bool)
	delivered func(string)
}

func Listen(ctx context.Context, cwd string, tools Tools, bound func() (sessions.Record, bool), delivered func(string)) (routing.MCPServer, error) {
	command, err := os.Executable()
	if err != nil {
		return routing.MCPServer{}, err
	}
	dir, err := os.MkdirTemp("", "petal-mcp-")
	if err != nil {
		return routing.MCPServer{}, err
	}
	socket := filepath.Join(dir, "m")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Remove(dir)
		return routing.MCPServer{}, err
	}
	token := rand.Text()
	config := routing.MCPServer{Name: routing.DelegationMCPServerName, Command: command, Args: []string{"delegation-mcp", socket}, Env: []routing.MCPEnvVariable{{Name: TokenEnv, Value: token}}}
	context.AfterFunc(ctx, func() {
		_ = listener.Close()
		_ = os.Remove(dir)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				proof := make([]byte, len(token))
				if _, err := io.ReadFull(conn, proof); err != nil || subtle.ConstantTimeCompare(proof, []byte(token)) != 1 {
					return
				}
				_ = conn.SetReadDeadline(time.Time{})
				server{cwd: cwd, bound: bound, delivered: delivered}.serve(ctx, conn, tools)
			}()
		}
	}()
	return config, nil
}

func Relay(ctx context.Context, socket, token string, in io.Reader, out io.Writer) error {
	if token == "" {
		return errors.New("missing delegation credential")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, token); err != nil {
		return err
	}
	// Serialize adapter requests with private delivery acknowledgments on the socket.
	var writes sync.Mutex
	send := func(data []byte) error {
		writes.Lock()
		defer writes.Unlock()
		_, err := conn.Write(data)
		return err
	}
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			var req mcpRequest
			if json.Unmarshal(line, &req) == nil && req.Method == deliveryAckMethod {
				continue
			}
			if send(append(line, '\n')) != nil {
				break
			}
		}
		writes.Lock()
		defer writes.Unlock()
		_ = conn.(*net.UnixConn).CloseWrite()
	}()
	decoder := json.NewDecoder(conn)
	for {
		var frame delegationRelayFrame
		if err := decoder.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		data := append(frame.Response, '\n')
		n, err := out.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		if frame.Delivery != "" {
			payload, _ := json.Marshal(frame.Delivery)
			ack := mcpRequest{Version: "2.0", Method: deliveryAckMethod, Params: payload}
			data, _ := json.Marshal(ack)
			if err := send(append(data, '\n')); err != nil {
				return err
			}
		}
	}
}

type mcpRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// The delivery token stays on the private bridge, out of the MCP response.
type delegationRelayFrame struct {
	Response json.RawMessage `json:"response"`
	Delivery string          `json:"delivery,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (endpoint server) serve(lifetime context.Context, conn net.Conn, tools Tools) {
	ctx, cancel := context.WithCancel(lifetime)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(conn)
	var mu sync.Mutex
	active := make(map[string]context.CancelFunc)
	pending := make(map[string]string)
	recording := 0
	recordDelivery := func(child string) {
		defer func() {
			mu.Lock()
			recording--
			mu.Unlock()
		}()
		// A complete relay write remains delivered even if the socket closes.
		recordCtx, stop := context.WithTimeout(lifetime, 10*time.Second)
		defer stop()
		record, ok := endpoint.bound()
		if !ok {
			return
		}
		binding := sessions.ChildBinding{ParentID: record.ID, ChildID: child}
		if err := sessions.ConsumeChildResult(recordCtx, endpoint.cwd, binding); err != nil {
			if lifetime.Err() == nil {
				slog.Warn("Could not record child result delivery", "childId", child, "error", err)
			}
			return
		}
		endpoint.delivered(child)
	}
	write := func(response mcpResponse, delivery string) {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		data, err := json.Marshal(response)
		if err == nil {
			err = encoder.Encode(delegationRelayFrame{Response: data, Delivery: delivery})
		}
		if err != nil {
			cancel()
		}
	}
	writeResponse := func(response mcpResponse) {
		mu.Lock()
		defer mu.Unlock()
		write(response, "")
	}

	initialized := false
	for scanner.Scan() {
		var req mcpRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeResponse(mcpResponse{"2.0", json.RawMessage("null"), nil, &mcpError{-32700, "Parse error"}})
			continue
		}
		if req.Version != "2.0" || req.Method == "" {
			writeResponse(mcpResponse{"2.0", json.RawMessage("null"), nil, &mcpError{-32600, "Invalid request"}})
			continue
		}
		if req.ID == nil && req.Method == deliveryAckMethod {
			var token string
			if json.Unmarshal(req.Params, &token) == nil {
				mu.Lock()
				child := pending[token]
				if child != "" {
					delete(pending, token)
					recording++
					go recordDelivery(child)
				}
				mu.Unlock()
			}
			continue
		}
		if req.ID == nil {
			if req.Method == "notifications/cancelled" {
				var params struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &params) == nil {
					mu.Lock()
					key := string(params.RequestID)
					if stop := active[key]; stop != nil {
						stop()
					}
					mu.Unlock()
				}
			}
			continue
		}
		response := mcpResponse{Version: "2.0", ID: req.ID}
		switch {
		case req.Method == "initialize" && !initialized:
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
				response.Error = &mcpError{-32602, "protocolVersion is required"}
				break
			}
			initialized = true
			switch params.ProtocolVersion {
			case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
			default:
				params.ProtocolVersion = "2025-11-25"
			}
			response.Result = map[string]any{
				"protocolVersion": params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": struct{}{}},
				"instructions":    "Call petal_capabilities before choosing a child model. Use its exact model ID. Give the child a self-contained task with the context, file scope and output you need. The child shares your working directory and starts with fresh conversation history. Keep childId. Finish your turn when you have no independent work. Petal wakes the connected parent for unread child results. Follow next on each response. For questions about earlier work, including what a completed child observed or returned, search the archive with petal_search_history. Open the original message or tool result with petal_read_transcript and cite its numeric reference when asked for a source. Use the child activity journal to follow a run in progress.",
				"serverInfo":      map[string]string{"name": routing.DelegationMCPServerName, "version": "1.0.0"},
			}
		case req.Method == "ping":
			response.Result = struct{}{}
		case !initialized:
			response.Error = &mcpError{-32002, "Server not initialized"}
		case req.Method == "tools/list":
			response.Result = delegationToolList(tools.Embeddings.Provider != "off")
		case req.Method == "tools/call":
			key := string(req.ID)
			mu.Lock()
			if len(active)+len(pending)+recording >= 16 || active[key] != nil {
				mu.Unlock()
				response.Error = &mcpError{-32600, "Duplicate request ID or too many active requests"}
				break
			}
			callCtx, stop := context.WithCancel(ctx)
			active[key] = stop
			mu.Unlock()
			go func() {
				defer stop()
				result, child := endpoint.call(callCtx, tools, req.Params)
				mu.Lock()
				defer mu.Unlock()
				delete(active, key)
				if callCtx.Err() != nil {
					return
				}
				var delivery string
				if child != "" {
					delivery = rand.Text()
					pending[delivery] = child
				}
				response.Result = result
				write(response, delivery)
			}()
			continue
		default:
			response.Error = &mcpError{-32601, "Method not found"}
		}
		writeResponse(response)
	}
}
