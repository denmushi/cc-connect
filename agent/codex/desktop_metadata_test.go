package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type metadataWriter struct {
	s       *appServerSession
	mu      sync.Mutex
	methods []string
	reply   func(string, map[string]any) (any, *rpcError)
}

func (w *metadataWriter) Close() error { return nil }
func (w *metadataWriter) Write(data []byte) (int, error) {
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.methods = append(w.methods, req.Method)
	w.mu.Unlock()
	result, rpcErr := w.reply(req.Method, req.Params)
	raw, _ := json.Marshal(result)
	w.s.pendingMu.Lock()
	ch := w.s.pending[req.ID]
	delete(w.s.pending, req.ID)
	w.s.pendingMu.Unlock()
	ch <- rpcResponseEnvelope{ID: req.ID, Result: raw, Error: rpcErr}
	return len(data), nil
}

func TestNativeProjectMatching(t *testing.T) {
	root := t.TempDir()
	makeProject := func(id, path string) nativeProject {
		p := nativeProject{ID: id}
		p.Roots = append(p.Roots, struct {
			Path string `json:"path"`
		}{path})
		return p
	}
	projects := []nativeProject{makeProject("home", root), makeProject("src", filepath.Join(root, "src")), makeProject("sibling", filepath.Join(root, "src-other"))}
	if got := matchingProject(projects, filepath.Join(root, "src", "repo")); got != "src" {
		t.Fatalf("matched %q", got)
	}
	if got := matchingProject(projects[1:2], filepath.Join(root, "src-other")); got != "" {
		t.Fatalf("sibling incorrectly matched %q", got)
	}
}

func TestNativeMetadataProtocol(t *testing.T) {
	for _, scenario := range []string{"new", "resume-missing", "resume-preserve", "old-server"} {
		t.Run(scenario, func(t *testing.T) {
			cwd := t.TempDir()
			s := &appServerSession{ctx: core.WithNativeSessionName(context.Background(), "测试 · Bot-A · Codex"), workDir: cwd}
			w := &metadataWriter{s: s}
			w.reply = func(method string, p map[string]any) (any, *rpcError) {
				switch method {
				case "project/list":
					if scenario == "old-server" {
						return nil, &rpcError{Code: -32601, Message: "method not found"}
					}
					if p["cursor"] == nil {
						return map[string]any{"data": []any{}, "nextCursor": "next"}, nil
					}
					return map[string]any{"data": []any{map[string]any{"id": "src-id", "roots": []any{map[string]string{"path": cwd}}}}}, nil
				case "thread/start":
					if p["cwd"] != cwd {
						t.Errorf("missing cwd: %v", p)
					}
					if scenario != "old-server" && p["projectId"] != "src-id" {
						t.Errorf("missing project: %v", p)
					}
					return map[string]any{"thread": map[string]any{"id": "thread-1"}}, nil
				case "thread/resume":
					thread := map[string]any{"id": "thread-1"}
					if scenario == "resume-preserve" {
						thread["name"] = "用户改名"
						thread["projectId"] = "user-project"
					}
					return map[string]any{"thread": thread}, nil
				case "thread/metadata/update":
					if p["threadId"] != "thread-1" || p["projectId"] != "src-id" {
						t.Errorf("wrong repair: %v", p)
					}
				case "thread/name/set":
					if p["name"] != "测试 · Bot-A · Codex" {
						t.Errorf("wrong title: %v", p)
					}
				default:
					t.Errorf("unexpected method %s", method)
				}
				return map[string]any{}, nil
			}
			s.stdin = w
			id := ""
			if scenario == "resume-missing" || scenario == "resume-preserve" {
				id = "thread-1"
			}
			if err := s.ensureThread(id); err != nil {
				t.Fatal(err)
			}
			w.mu.Lock()
			methods := fmt.Sprint(w.methods)
			w.mu.Unlock()
			if scenario == "resume-preserve" && methods != "[thread/resume]" {
				t.Fatalf("overwrote user metadata: %s", methods)
			}
			if scenario == "old-server" && s.StartupWarning() == "" {
				t.Fatal("missing compatibility warning")
			}
			if scenario != "old-server" && s.NativeMetadataError() != nil {
				t.Fatal(s.NativeMetadataError())
			}
		})
	}
}
