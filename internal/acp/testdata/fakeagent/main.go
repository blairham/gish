// A minimal ACP agent, for testing the client (#167).
//
// It is a real process speaking real JSON-RPC over stdio, because the
// property under test is concurrency: it calls back into the client
// with terminal/create *while* the client is waiting for its
// session/prompt answer. A mock that answered inline would pass while
// the real shape deadlocked.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

var (
	out     = bufio.NewWriter(os.Stdout)
	in      = bufio.NewReader(os.Stdin)
	nextID  = 100
	replies = map[string]chan message{}
)

func send(m message) {
	m.JSONRPC = "2.0"
	data, _ := json.Marshal(m)
	out.Write(append(data, '\n')) //nolint:errcheck
	out.Flush()                   //nolint:errcheck
}

// call makes a request of the client and waits for its answer.
func call(method string, params any) json.RawMessage {
	nextID++
	id, _ := json.Marshal(nextID)
	encoded, _ := json.Marshal(params)
	ch := make(chan message, 1)
	replies[string(id)] = ch
	send(message{ID: id, Method: method, Params: encoded})
	return (<-ch).Result
}

func main() {
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var msg message
			if json.Unmarshal(line, &msg) == nil {
				handle(msg)
			}
		}
		if err != nil {
			return
		}
	}
}

func handle(msg message) {
	// An answer to something we asked.
	if msg.Method == "" && len(msg.ID) > 0 {
		if ch, ok := replies[string(msg.ID)]; ok {
			delete(replies, string(msg.ID))
			ch <- msg
		}
		return
	}
	switch msg.Method {
	case "initialize":
		send(message{ID: msg.ID, Result: raw(map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "fake-agent", "version": "0.1"},
			"agentCapabilities": map[string]any{
				"promptCapabilities": map[string]any{"embeddedContext": false},
			},
			"authMethods": []any{},
		})})
	case "session/new":
		send(message{ID: msg.ID, Result: raw(map[string]any{"sessionId": "sess_test"})})
	case "session/prompt":
		go prompt(msg)
	default:
		send(message{ID: msg.ID, Result: raw(map[string]any{})})
	}
}

// prompt is the turn. It runs on its own goroutine because it calls
// back into the client, and the reader loop has to keep reading to
// deliver those answers.
func prompt(msg message) {
	var params struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	json.Unmarshal(msg.Params, &params) //nolint:errcheck

	text := ""
	if len(params.Prompt) > 0 {
		text = params.Prompt[0].Text
	}

	if strings.Contains(text, "command") {
		var created struct {
			TerminalID string `json:"terminalId"`
		}
		json.Unmarshal(call("terminal/create", map[string]any{ //nolint:errcheck
			"sessionId": params.SessionID,
			"command":   echoCommand(),
			"args":      echoArgs("command-output-marker"),
		}), &created)

		call("terminal/wait_for_exit", map[string]any{
			"sessionId": params.SessionID, "terminalId": created.TerminalID,
		})
		var output struct {
			Output string `json:"output"`
		}
		json.Unmarshal(call("terminal/output", map[string]any{ //nolint:errcheck
			"sessionId": params.SessionID, "terminalId": created.TerminalID,
		}), &output)
		call("terminal/release", map[string]any{
			"sessionId": params.SessionID, "terminalId": created.TerminalID,
		})

		update(params.SessionID, fmt.Sprintf("ran it, saw: %s", strings.TrimSpace(output.Output)))
	} else {
		update(params.SessionID, "hello from the agent")
	}
	send(message{ID: msg.ID, Result: raw(map[string]any{"stopReason": "end_turn"})})
}

func update(session, text string) {
	send(message{Method: "session/update", Params: raw(map[string]any{
		"sessionId": session,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		},
	})})
}

func raw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
