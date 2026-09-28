package tests

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndx-technologies/lean-sandbox/api"
	"github.com/ndx-technologies/lean-sandbox/internal/agent"
	"github.com/ndx-technologies/lean-sandbox/sdk"
)

func sandbox(t *testing.T) *sdk.Sandbox {
	t.Helper()

	id := api.NewSandboxID()
	agentSrv, err := agent.NewServer(id, "")
	if err != nil {
		t.Fatal(err)
	}
	agentHTTP := httptest.NewServer(agentSrv.Handler())
	t.Cleanup(agentHTTP.Close)

	cpHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cpHTTP.Close)

	return &sdk.Sandbox{
		Sandbox:      api.Sandbox{ID: id, Endpoint: agentHTTP.URL},
		HTTPClient:   agentHTTP.Client(),
		ControlPlane: &sdk.ControlPlane{BaseURL: cpHTTP.URL, HTTPClient: cpHTTP.Client()},
	}
}

func TestRun(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping sandbox test")
	}
	sb := sandbox(t)
	ctx := t.Context()

	res, err := sb.Run(ctx, "export GREETING=hello && echo $GREETING")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}

	res, err = sb.Run(ctx, "echo GREETING=$GREETING")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "GREETING=hello") {
		t.Errorf("env did not persist: stdout=%q", res.Stdout)
	}

	res, err = sb.Run(ctx, "exit 7")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit=%d want 7", res.ExitCode)
	}
}

func TestFileCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping sandbox test")
	}
	sb := sandbox(t)
	ctx := t.Context()

	content := make([]byte, 1<<20+512)
	for i := range content {
		content[i] = byte(i)
	}

	path := filepath.Join(t.TempDir(), "a", "b", "payload.bin")
	if err := sb.WriteFile(ctx, path, content); err != nil {
		t.Fatal(err)
	}

	got, err := sb.ReadFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read back %d bytes, want %d", len(got), len(content))
	}
}

func TestDirCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping sandbox test")
	}
	const dirMode = 0o705

	sb := sandbox(t)
	ctx := t.Context()

	src := t.TempDir()
	if err := os.Chmod(src, dirMode); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "keep", "a.txt"), "hello", 0o644)
	writeFile(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho from-script\n", 0o755)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("keep", "a.txt"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := sb.UploadDir(ctx, root, src); err != nil {
		t.Fatal(err)
	}

	remote := filepath.Join(root, filepath.Base(src))
	checkTree(t, remote)
	fi, err := os.Stat(remote)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != dirMode {
		t.Errorf("uploaded dir mode = %o, want %o", fi.Mode().Perm(), dirMode)
	}

	res, err := sb.Run(ctx, remote+"/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "from-script") {
		t.Errorf("uploaded script did not run: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	back := t.TempDir()
	if err := sb.DownloadDir(ctx, remote, back); err != nil {
		t.Fatal(err)
	}
	checkTree(t, back)
}

func checkTree(t *testing.T, dir string) {
	t.Helper()

	if got, err := os.ReadFile(filepath.Join(dir, "keep", "a.txt")); err != nil || string(got) != "hello" {
		t.Errorf("%s: a.txt = %q, %v; want %q", dir, got, err, "hello")
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty")); err != nil {
		t.Errorf("%s: stat empty: %v", dir, err)
	} else if !fi.IsDir() {
		t.Errorf("%s: empty is not a directory", dir)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != filepath.Join("keep", "a.txt") {
		t.Errorf("%s: link = %q, %v; want %q", dir, target, err, filepath.Join("keep", "a.txt"))
	}
	if fi, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		t.Errorf("%s: stat run.sh: %v", dir, err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("%s: run.sh mode = %v, want 0755", dir, fi.Mode().Perm())
	}
}

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
