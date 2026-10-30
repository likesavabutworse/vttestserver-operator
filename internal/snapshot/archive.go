package snapshot

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	// MarkerFile is written last by a restore. Its presence means the restore finished.
	MarkerFile   = ".vttest-restored"
	lostAndFound = "lost+found"
)

// Digest is the sha256 and size of the compressed archive stream.
type Digest struct {
	SHA256    string
	SizeBytes int64
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// WriteArchive writes dir as a zstd compressed tar stream to w. It skips the
// restore marker, a root lost+found, sockets, and other special files that tar
// cannot hold. The digest covers the bytes written to w.
func WriteArchive(w io.Writer, dir string) (Digest, error) {
	h := sha256.New()
	c := &counter{}
	zw, err := zstd.NewWriter(io.MultiWriter(w, h, c))
	if err != nil {
		return Digest{}, err
	}
	tw := tar.NewWriter(zw)

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == MarkerFile || rel == lostAndFound {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		var link string
		switch {
		case mode.IsRegular(), mode.IsDir():
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			// sockets, pipes and devices: not archived
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if mode.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return Digest{}, fmt.Errorf("archive %s: %w", dir, walkErr)
	}
	if err := tw.Close(); err != nil {
		return Digest{}, err
	}
	if err := zw.Close(); err != nil {
		return Digest{}, err
	}
	return Digest{SHA256: hex.EncodeToString(h.Sum(nil)), SizeBytes: c.n}, nil
}

// ExtractArchive reads a stream written by WriteArchive into dir. It rejects
// absolute paths, ".." segments and symlinks that point out of dir, because the
// stream comes from a remote store. It sets owners only when it runs as root.
// It reads the stream to its end, so the digest covers all bytes.
func ExtractArchive(r io.Reader, dir string) (Digest, error) {
	h := sha256.New()
	c := &counter{}
	tee := io.TeeReader(r, io.MultiWriter(h, c))
	zr, err := zstd.NewReader(tee)
	if err != nil {
		return Digest{}, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	root, err := filepath.Abs(dir)
	if err != nil {
		return Digest{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Digest{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Digest{}, err
	}
	asRoot := os.Geteuid() == 0

	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Digest{}, fmt.Errorf("read archive: %w", err)
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return Digest{}, err
		}
		if err := checkParent(root, target); err != nil {
			return Digest{}, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return Digest{}, err
			}
			dirs = append(dirs, dirMode{target, fs.FileMode(hdr.Mode).Perm()})
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return Digest{}, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return Digest{}, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return Digest{}, fmt.Errorf("extract %s: %w", hdr.Name, err)
			}
			if err := f.Close(); err != nil {
				return Digest{}, err
			}
			if err := os.Chmod(target, fs.FileMode(hdr.Mode).Perm()); err != nil {
				return Digest{}, err
			}
		case tar.TypeSymlink:
			if err := checkSymlink(root, target, hdr.Linkname); err != nil {
				return Digest{}, err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return Digest{}, err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return Digest{}, err
			}
		default:
			return Digest{}, fmt.Errorf("archive entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
		if asRoot {
			if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
				return Digest{}, err
			}
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return Digest{}, err
	}
	// Directory modes are set last: a mode without the write bit would block the children.
	slices.SortFunc(dirs, func(a, b dirMode) int { return len(b.path) - len(a.path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return Digest{}, err
		}
	}
	return Digest{SHA256: hex.EncodeToString(h.Sum(nil)), SizeBytes: c.n}, nil
}

func safeJoin(root, name string) (string, error) {
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q is outside the data dir", name)
	}
	if clean == "." {
		return root, nil
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// checkParent makes sure no symlink on the way leads out of root.
func checkParent(root, target string) error {
	parent := filepath.Dir(target)
	for {
		if parent == root || len(parent) <= len(root) {
			return nil
		}
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			if !within(root, resolved) {
				return fmt.Errorf("archive entry %q goes through a symlink out of the data dir", target)
			}
			return nil
		}
		parent = filepath.Dir(parent)
	}
}

func checkSymlink(root, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("symlink %q has an absolute target", target)
	}
	if !within(root, filepath.Join(filepath.Dir(target), linkname)) {
		return fmt.Errorf("symlink %q points out of the data dir", target)
	}
	return nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ClearDir removes everything in dir except lost+found. It does not remove dir.
func ClearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == lostAndFound {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
