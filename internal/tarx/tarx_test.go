package tarx

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	write(t, filepath.Join(src, "keep", "a.txt"), "hello", 0o644)
	write(t, filepath.Join(src, "script.sh"), "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("keep", "a.txt"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Pack(&buf, src, "src"); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(base, "root")
	if err := Unpack(&buf, root); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "src")
	if got, err := os.ReadFile(filepath.Join(dest, "keep", "a.txt")); err != nil || string(got) != "hello" {
		t.Errorf("a.txt = %q, %v; want %q", got, err, "hello")
	}
	if fi, err := os.Stat(filepath.Join(dest, "script.sh")); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("script.sh mode = %v, want 0755", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(dest, "empty")); err != nil {
		t.Error(err)
	} else if !fi.IsDir() {
		t.Errorf("empty is not a directory")
	}
	if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != filepath.Join("keep", "a.txt") {
		t.Errorf("link = %q, %v; want %q", target, err, filepath.Join("keep", "a.txt"))
	}
}

func TestUnpackAppliesDirModesLast(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	write(t, filepath.Join(src, "ro", "a.txt"), "hello", 0o644)
	if err := os.Chmod(filepath.Join(src, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "ro"), 0o755) })

	var buf bytes.Buffer
	if err := Pack(&buf, src, "src"); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(base, "root")
	if err := Unpack(&buf, root); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "src", "ro")
	t.Cleanup(func() { _ = os.Chmod(dest, 0o755) })
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "hello" {
		t.Errorf("a.txt = %q, %v; want %q", got, err, "hello")
	}
	if fi, err := os.Stat(dest); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o555 {
		t.Errorf("dir mode = %v, want 0555", fi.Mode().Perm())
	}
}

func TestUnpackClampsEntryNames(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")

	if err := Unpack(tarStream(t, "../escape.txt", "nope"), root); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(base, "escape.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("entry escaped root: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "escape.txt")); err != nil || string(got) != "nope" {
		t.Errorf("clamped file = %q, %v; want %q", got, err, "nope")
	}
}

func TestUnpackDoesNotFollowSymlinksOutOfRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	link := &tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../outside"}
	if err := tw.WriteHeader(link); err != nil {
		t.Fatal(err)
	}
	content := "escaped"
	reg := &tar.Header{Name: "link/escaped.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Unpack(&buf, root); err == nil {
		t.Error("Unpack wrote through a symlink pointing out of root")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file escaped root: %v", err)
	}
}

func TestUnpackRejectsUnsupportedType(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "pipe", Mode: 0o644, Typeflag: tar.TypeFifo}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Unpack(&buf, t.TempDir()); err == nil {
		t.Fatal("Unpack accepted a fifo entry")
	}
}

func tarStream(t *testing.T, name, content string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func write(t *testing.T, path, content string, mode fs.FileMode) {
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
