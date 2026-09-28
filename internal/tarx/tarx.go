package tarx

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func Pack(w io.Writer, dir, prefix string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		// The root itself is an entry only when it has a name to carry;
		// downloading a root asks for its contents instead.
		name := prefix
		if rel != "." {
			name = filepath.Join(prefix, rel)
		}
		if name == "" {
			return nil
		}

		var link string
		if entry.Type()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		if closeErr := f.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return fmt.Errorf("pack %s: %w", path, copyErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func Unpack(r io.Reader, root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Every name is resolved by the Root, which refuses to follow a symlink out
	// of the tree. A lexical clean cannot see that: an entry named "link/x"
	// looks contained until link turns out to point above root.
	rt, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()

	type dirMode struct {
		rel  string
		mode fs.FileMode
	}
	var dirs []dirMode

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		// The leading slash makes Clean collapse any ".." at the root, so the
		// name cannot address anything above it before the Root checks it too.
		rel := strings.TrimPrefix(filepath.Clean("/"+hdr.Name), "/")
		dest := filepath.Join(root, rel)
		if rel == "" {
			if hdr.Typeflag == tar.TypeDir {
				continue // the root itself, which already exists
			}
			return fmt.Errorf("entry %q has no name", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := rt.MkdirAll(rel, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
			dirs = append(dirs, dirMode{rel, fs.FileMode(hdr.Mode).Perm()})
		case tar.TypeReg:
			if err := mkdirParent(rt, rel, dest); err != nil {
				return err
			}
			if err := writeFile(rt, tr, rel, dest, fs.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := mkdirParent(rt, rel, dest); err != nil {
				return err
			}
			_ = rt.Remove(rel) // replacing a file or an older link
			if err := rt.Symlink(hdr.Linkname, rel); err != nil {
				return fmt.Errorf("symlink %s: %w", dest, err)
			}
		default:
			return fmt.Errorf("entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}

	for _, d := range dirs {
		if err := rt.Chmod(d.rel, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", filepath.Join(root, d.rel), err)
		}
	}
	return nil
}

func mkdirParent(rt *os.Root, rel, dest string) error {
	dir := filepath.Dir(rel)
	if dir == "." {
		return nil
	}
	if err := rt.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Join(filepath.Dir(dest), ""), err)
	}
	return nil
}

func writeFile(rt *os.Root, r io.Reader, rel, dest string, mode fs.FileMode) error {
	f, err := rt.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	_, copyErr := io.Copy(f, r)
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		// A half-written file is worse than none: a later run would read it as
		// if it were complete.
		_ = rt.Remove(rel)
		return fmt.Errorf("write %s: %w", dest, copyErr)
	}
	if err := rt.Chmod(rel, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	return nil
}
