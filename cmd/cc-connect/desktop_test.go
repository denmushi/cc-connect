package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestDesktopCLIUsesSelectedLocalSocket(t *testing.T) {
	// macOS Unix socket 路径长度受限，不使用长测试函数名作为临时目录前缀。
	dir, err := os.MkdirTemp("", "cc-desktop-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "run", "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan core.DesktopRequest, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/desktop" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode([]core.DesktopSession{{SessionID: "native-id", Agent: "codex", Name: "群聊标题", WorkDir: "/workspace"}})
			return
		}
		var request core.DesktopRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		requests <- request
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})}
	go server.Serve(listener)
	defer server.Close()
	var out bytes.Buffer
	if err := desktopCommand([]string{"list", "--data-dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "native-id  codex  群聊标题") {
		t.Fatalf("list=%s", out.String())
	}
	if err := desktopCommand([]string{"open", "native-id", "--data-dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	req := <-requests
	if req.Action != "open" || req.SessionID != "native-id" {
		t.Fatalf("request=%+v", req)
	}
	if err := desktopCommand([]string{"open", "--data-dir", dir}, &out); err == nil {
		t.Fatal("missing native ID accepted")
	}
}
