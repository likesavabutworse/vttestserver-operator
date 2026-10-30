package snapshot

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const (
	myCnfBegin = "# BEGIN vttestserver-operator"
	myCnfEnd   = "# END vttestserver-operator"
)

// ApplyMyCnf puts the settings of extra into every vt_*/my.cnf in the data dir, as one marked block
// at the end. A repeated option group is valid in a MySQL option file, and a later value wins over
// an earlier one, so the block overrides what the golden source wrote. Vitess writes my.cnf only
// when it first initialises a data dir, and a snapshot carries the old file, so without this step
// spec.mysqlConfig has no effect on a restored server.
//
// A block from an earlier call is replaced, so the step is safe to run on every pod start. Empty
// extra removes the block. A data dir without my.cnf (a fresh server) is not an error: Vitess
// reads --extra-my-cnf when it writes the file. It returns the number of files it changed.
func ApplyMyCnf(dataDir string, extra []byte) (int, error) {
	files, err := filepath.Glob(filepath.Join(dataDir, "vt_*", "my.cnf"))
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, f := range files {
		old, err := os.ReadFile(f)
		if err != nil {
			return changed, err
		}
		updated := withMyCnfBlock(old, extra)
		if bytes.Equal(old, updated) {
			continue
		}
		info, err := os.Stat(f)
		if err != nil {
			return changed, err
		}
		tmp := f + ".tmp"
		if err := os.WriteFile(tmp, updated, info.Mode().Perm()); err != nil {
			return changed, err
		}
		if err := os.Rename(tmp, f); err != nil {
			return changed, fmt.Errorf("replace %s: %w", f, err)
		}
		changed++
	}
	return changed, nil
}

func withMyCnfBlock(cnf, extra []byte) []byte {
	base := stripMyCnfBlock(cnf)
	extra = bytes.TrimSpace(extra)
	if len(extra) == 0 {
		return base
	}
	var out bytes.Buffer
	out.Write(base)
	if len(base) > 0 && !bytes.HasSuffix(base, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString(myCnfBegin + "\n")
	out.Write(extra)
	out.WriteString("\n" + myCnfEnd + "\n")
	return out.Bytes()
}

// stripMyCnfBlock removes a marked block and the blank line before it.
func stripMyCnfBlock(cnf []byte) []byte {
	start := bytes.Index(cnf, []byte(myCnfBegin))
	if start < 0 {
		return cnf
	}
	end := bytes.Index(cnf[start:], []byte(myCnfEnd))
	if end < 0 {
		// A block that was never closed: keep everything before it.
		return bytes.TrimRight(cnf[:start], "\n")
	}
	rest := cnf[start+end+len(myCnfEnd):]
	rest = bytes.TrimPrefix(rest, []byte("\n"))
	return append(append([]byte{}, cnf[:start]...), rest...)
}
