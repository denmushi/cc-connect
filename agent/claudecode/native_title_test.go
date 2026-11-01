package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestNativeTitleProcess(t *testing.T) {
	path := os.Getenv("CC_NATIVE_TITLE_ARGS")
	if path == "" {
		return
	}
	data, _ := json.Marshal(os.Args)
	if err := os.WriteFile(path, data, 0600); err != nil {
		os.Exit(2)
	}
	fmt.Println(`{"type":"system","subtype":"init","session_id":"fixture"}`)
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestNativeTitleOnNewSessionPreservesResume(t *testing.T) {
	for _, id := range []string{"", "existing-session"} {
		t.Run("session="+id, func(t *testing.T) {
			root := t.TempDir()
			argsFile := filepath.Join(root, "args.json")
			a := &Agent{workDir: root, cmd: os.Args[0], cliExtraArgs: []string{"-test.run=^TestNativeTitleProcess$", "--"}, activeIdx: -1, ccDataDir: root, configEnv: []string{"CC_NATIVE_TITLE_ARGS=" + argsFile}}
			ctx, cancel := context.WithCancel(core.WithNativeSessionName(context.Background(), "测试 · Bot-A · Claude Code"))
			defer cancel()
			s, err := a.StartSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var raw []byte
			for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
				raw, err = os.ReadFile(argsFile)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			var args []string
			if err := json.Unmarshal(raw, &args); err != nil {
				t.Fatal(err)
			}
			name := ""
			for i, arg := range args {
				if arg == "--name" && i+1 < len(args) {
					name = args[i+1]
				}
			}
			if id == "" && name != "测试 · Bot-A · Claude Code" {
				t.Fatalf("new title=%q", name)
			}
			if id != "" && name != "" {
				t.Fatalf("resume overwrote native title: %q", name)
			}
		})
	}
}
