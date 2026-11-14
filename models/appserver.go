package models

// A harness's own model server: codex app-server, which speaks JSON-RPC, one
// JSON object a line, on its standard input and output, and lists the
// models its sign-in may use with model/list. The exchange, as codex-cli
// 0.164.0 answered it on 2026-11-14 (run with an empty CODEX_HOME and no
// sign-in, its list-models call sent to a local listener), and as the
// protocol's schema at rust-v0.164.0 says
// (codex-rs/app-server-protocol/schema/json, ClientRequest.json,
// v2/ModelListResponse.json):
//
//	-> {"id":1,"method":"initialize","params":{"clientInfo":{"name":"agentbus","version":"..."}}}
//	<- {"id":1,"result":{"userAgent":...,"codexHome":...,"platformFamily":...,"platformOs":...}}
//	-> {"method":"initialized"}
//	-> {"id":2,"method":"model/list","params":{}}
//	<- {"id":2,"result":{"data":[{"id":"gpt-6.1-sol","model":"gpt-6.1-sol","displayName":"GPT-6.1-Sol","hidden":false,"isDefault":true,...}],"nextCursor":null}}
//
// The lines carry no "jsonrpc" member. Between the answers the server may
// send notifications (remoteControl/status/changed) and requests of its
// own; neither is read. A page holds the models the picker shows (no
// includeHidden); nextCursor, when set, asks for the next page. Only each
// model's name and display name are kept: nothing else of the answers.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// ErrServer is a model server that did not list its models: it could not
// be started, it ended, or it answered with an error or a shape that is
// not a list. In fixed words: what the harness printed is never passed on.
var ErrServer = errors.New("the harness could not list its models just now")

// maxLine bounds one line the server sends.
const maxLine = 8 << 20

type rpcRequest struct {
	ID     int    `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcAnswer struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// AppServer lists the models of a model server whose input is in and
// output is out, as client (the name and version the server is told). It
// returns when the list is read, ctx ends, or the server fails; the caller
// stops the server.
func AppServer(ctx context.Context, in io.Writer, out io.Reader, client, version string) ([]Model, error) {
	type result struct {
		ms  []Model
		err error
	}
	done := make(chan result, 1)
	go func() {
		ms, err := appServer(in, out, client, version)
		done <- result{ms, err}
	}()
	select {
	case r := <-done:
		return r.ms, r.err
	case <-ctx.Done():
		return nil, ErrServer
	}
}

func appServer(in io.Writer, out io.Reader, client, version string) ([]Model, error) {
	enc := json.NewEncoder(in)
	lines := bufio.NewReaderSize(out, 64<<10)
	send := func(r rpcRequest) error {
		if enc.Encode(r) != nil {
			return ErrServer
		}
		return nil
	}
	// answer is the result of request id, the lines before it left unread.
	answer := func(id int) (json.RawMessage, error) {
		for {
			line, err := readLine(lines)
			if err != nil {
				return nil, ErrServer
			}
			var a rpcAnswer
			if json.Unmarshal(line, &a) != nil {
				return nil, ErrServer
			}
			if a.ID == nil || *a.ID != id || a.Method != "" {
				continue // a notification, or a request of the server's
			}
			if (len(a.Error) > 0 && string(a.Error) != "null") || len(a.Result) == 0 {
				return nil, ErrServer
			}
			return a.Result, nil
		}
	}
	type clientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := send(rpcRequest{ID: 1, Method: "initialize", Params: map[string]any{"clientInfo": clientInfo{client, version}}}); err != nil {
		return nil, err
	}
	if _, err := answer(1); err != nil {
		return nil, err
	}
	if err := send(rpcRequest{Method: "initialized"}); err != nil {
		return nil, err
	}
	var ms []Model
	cursor := ""
	for page := range maxPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := 2 + page
		if err := send(rpcRequest{ID: id, Method: "model/list", Params: params}); err != nil {
			return nil, err
		}
		raw, err := answer(id)
		if err != nil {
			return nil, err
		}
		var list struct {
			Data *[]struct {
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
			} `json:"data"`
			Next *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &list) != nil || list.Data == nil {
			return nil, ErrServer
		}
		for _, m := range *list.Data {
			ms = append(ms, Model{ID: m.Model, Label: m.DisplayName})
		}
		if list.Next == nil || *list.Next == "" || *list.Next == cursor {
			break
		}
		cursor = *list.Next
	}
	return keep(ms), nil
}

// readLine is one line of r, at most maxLine long, without its end.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, ErrServer
		}
		if !more {
			return line, nil
		}
	}
}
