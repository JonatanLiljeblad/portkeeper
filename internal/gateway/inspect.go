package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const toolsCallMethod = "tools/call"

var (
	errMCPMalformed = errors.New("request body is not a single well-formed JSON-RPC message")
	errMCPBatch     = errors.New("JSON-RPC batches are not supported")
)

// mcpRequest is the part of a client JSON-RPC message that policy depends on.
type mcpRequest struct {
	Method string
	Tool   string          // set only for tools/call
	ID     json.RawMessage // nil for notifications and responses
}

// inspectMCPRequest extracts the method and tool name from one JSON-RPC
// message. It is deliberately stricter than a backend parser: policy is only
// sound if every backend reads the same method and tool that the gateway did.
// Duplicate members and members that differ from a policy-relevant name only
// by case or Unicode folding are rejected, because JSON parsers disagree on
// which one wins (Go's encoding/json matches member names case-insensitively).
func inspectMCPRequest(body []byte) (mcpRequest, error) {
	var out mcpRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	first, err := dec.Token()
	if err != nil {
		return out, errMCPMalformed
	}
	if first == json.Delim('[') {
		return out, errMCPBatch
	}
	if first != json.Delim('{') {
		return out, errMCPMalformed
	}

	var name json.RawMessage
	seen := map[string]bool{}
	for dec.More() {
		key, err := memberName(dec, seen, "jsonrpc", "id", "method", "params")
		if err != nil {
			return out, err
		}
		switch key {
		case "method":
			if dec.Decode(&out.Method) != nil {
				return out, errMCPMalformed
			}
		case "id":
			if dec.Decode(&out.ID) != nil || !validRequestID(out.ID) {
				return out, errMCPMalformed
			}
		case "params":
			if name, err = paramsName(dec); err != nil {
				return out, err
			}
		default:
			if skipValue(dec) != nil {
				return out, errMCPMalformed
			}
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return out, errMCPMalformed
	}
	if _, err := dec.Token(); err != io.EOF {
		return out, errMCPMalformed
	}

	if out.Method != toolsCallMethod {
		return out, nil
	}
	// A tools/call that cannot be attributed to a tool or answered is refused
	// rather than forwarded unaudited.
	if out.ID == nil || name == nil || json.Unmarshal(name, &out.Tool) != nil || out.Tool == "" {
		return out, errMCPMalformed
	}
	return out, nil
}

// paramsName returns the raw "name" member of an object-valued params, or nil.
func paramsName(dec *json.Decoder) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, errMCPMalformed
	}
	inner := json.NewDecoder(bytes.NewReader(raw))
	if first, err := inner.Token(); err != nil || first != json.Delim('{') {
		return nil, nil // array or scalar params carry no tool name
	}
	var name json.RawMessage
	seen := map[string]bool{}
	for inner.More() {
		key, err := memberName(inner, seen, "name")
		if err != nil {
			return nil, err
		}
		if key == "name" {
			if inner.Decode(&name) != nil {
				return nil, errMCPMalformed
			}
			if name == nil || string(name) == "null" {
				return nil, errMCPMalformed
			}
			continue
		}
		if skipValue(inner) != nil {
			return nil, errMCPMalformed
		}
	}
	return name, nil
}

// memberName reads the next object member name. For a name that folds to one
// of the reserved names it requires the exact spelling and a single occurrence.
func memberName(dec *json.Decoder, seen map[string]bool, reserved ...string) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", errMCPMalformed
	}
	key, ok := tok.(string)
	if !ok {
		return "", errMCPMalformed
	}
	for _, want := range reserved {
		if !strings.EqualFold(key, want) {
			continue
		}
		if key != want || seen[want] {
			return "", errMCPMalformed
		}
		seen[want] = true
		return want, nil
	}
	return "", nil
}

func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}

// validRequestID accepts JSON strings and numbers; MCP forbids null ids.
func validRequestID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	return id[0] == '"' || id[0] == '-' || (id[0] >= '0' && id[0] <= '9')
}
