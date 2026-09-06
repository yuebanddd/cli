package publicapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	canvas "github.com/Pippit-dev/pippit-cli/internal/canvas"
)

// RunCanvasCommand keeps the JS SDK, but all native/API/persistence operations
// are delegated back into this request's Go tenant boundary. No credential,
// database URL, keyring path or general shell capability is passed to Node.
func (p *requestPolicy) RunCanvasCommand(ctx context.Context, args []string) ([]byte, error) {
	if p.app.cfg.CanvasScript == "" {
		return nil, errors.New("public Canvas runtime is not configured")
	}
	script, e := filepath.Abs(p.app.cfg.CanvasScript)
	if e != nil {
		return nil, e
	}
	canvasID := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--canvas-id" {
			canvasID = args[i+1]
		}
	}
	if canvasID != "" {
		if e = p.owns(ctx, resourceRef{"asset", canvasID, ""}); e != nil {
			return nil, e
		}
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	secret := randomToken()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || !equal(r.Header.Get("Authorization"), "Bearer "+secret) {
			http.Error(w, "forbidden", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		var body struct {
			Op   string          `json:"op"`
			Data json.RawMessage `json:"data"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		out, e := p.canvasOperation(ctx, canvasID, body.Op, body.Data)
		if e != nil {
			http.Error(w, "canvas operation failed", 400)
			return
		}
		writeJSON(w, 200, out)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, MaxHeaderBytes: 8192}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	input, _ := json.Marshal(map[string]any{"args": append([]string{"canvas", "command"}, args...), "url": "http://" + listener.Addr().String() + "/rpc", "token": secret})
	command := exec.CommandContext(ctx, p.app.cfg.NodeBinary, script)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "NODE_NO_WARNINGS=1"}
	command.Stdin = bytes.NewReader(input)
	var output boundedBuffer
	output.limit = 2 << 20
	command.Stdout = &output
	command.Stderr = io.Discard
	command.WaitDelay = 5 * time.Second
	if e = command.Run(); e != nil {
		return nil, errors.New("canvas_command_failed: query current state before retrying")
	}
	if !json.Valid(output.Bytes()) {
		return nil, errors.New("invalid_canvas_result")
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("output too large")
	}
	return b.Buffer.Write(p)
}
func (p *requestPolicy) canvasOperation(ctx context.Context, canvasID, op string, raw json.RawMessage) (any, error) {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("invalid arguments")
	}
	// Re-check revocation and credential before every operation in the child.
	if _, e := p.runner().Auth.ResolveAccessKey(ctx); e != nil {
		return nil, e
	}
	if e := p.checkInput(ctx, args); e != nil {
		return nil, e
	}
	var out any
	var e error
	switch op {
	case "get":
		ids := []string{}
		for _, v := range args["asset_ids"].([]any) {
			ids = append(ids, v.(string))
		}
		r, err := canvas.Get(ctx, canvas.GetOptions{AssetIDs: ids}, p.runner())
		if err != nil {
			return nil, err
		}
		out = r
	case "allocate":
		count, ok := args["count"].(float64)
		if !ok || count != float64(int(count)) {
			return nil, errors.New("invalid count")
		}
		out, e = canvas.Allocate(ctx, int(count), p.runner())
	case "apply":
		var request canvas.ApplyRequest
		if e = json.Unmarshal(raw, &request); e != nil {
			return nil, e
		}
		out, e = canvas.Apply(ctx, canvas.ApplyOptions{Request: request, AllowNonAcknowledgedResults: true}, p.runner())
	case "state.read":
		if canvasID == "" {
			return nil, errors.New("canvas required")
		}
		var state []byte
		e = p.app.store.DB.QueryRowContext(ctx, `SELECT state FROM canvas_state WHERE user_id=$1 AND account_id=$2 AND canvas_id=$3`, p.principal.UserID, p.accountID, canvasID).Scan(&state)
		if errors.Is(e, sql.ErrNoRows) {
			return map[string]any{}, nil
		}
		return json.RawMessage(state), e
	case "state.write":
		if canvasID == "" {
			return nil, errors.New("canvas required")
		}
		state := args["state"]
		encoded, e := json.Marshal(state)
		if e != nil || len(encoded) > 1<<20 {
			return nil, errors.New("canvas state too large")
		}
		_, e = p.app.store.DB.ExecContext(ctx, `INSERT INTO canvas_state(user_id,account_id,canvas_id,state) VALUES($1,$2,$3,$4) ON CONFLICT(user_id,account_id,canvas_id) DO UPDATE SET state=EXCLUDED.state,updated_at=now()`, p.principal.UserID, p.accountID, canvasID, encoded)
		return map[string]bool{"ok": e == nil}, e
	default:
		return nil, fmt.Errorf("unsupported canvas operation %s", strings.TrimSpace(op))
	}
	if e != nil {
		return nil, e
	}
	b, e := json.Marshal(out)
	if e != nil {
		return nil, e
	}
	var value any
	if e = json.Unmarshal(b, &value); e != nil {
		return nil, e
	}
	tx, e := p.app.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = p.remember(ctx, tx, resourceRefs(value)); e != nil {
		return nil, e
	}
	return out, tx.Commit()
}
