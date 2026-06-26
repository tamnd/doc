package doc

import (
	"os"
	"path/filepath"
	"strings"
)

// DirSink is a WALSink backed by a directory on the local filesystem. Each segment
// is one file; List returns the segment file names. It is the default sink for WAL
// archiving and the one doc restore --wal-source reads.
type DirSink struct {
	dir string
}

// NewDirSink opens (creating if needed) a directory sink at dir.
func NewDirSink(dir string) (*DirSink, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DirSink{dir: dir}, nil
}

const segmentExt = ".seg"

// Put writes a segment file atomically: it writes a temp file and renames it, so a
// reader never sees a half-written segment.
func (d *DirSink) Put(name string, data []byte) error {
	final := filepath.Join(d.dir, name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// List returns the names of every segment file in the directory.
func (d *DirSink) List() ([]string, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), segmentExt) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Get reads back a segment file.
func (d *DirSink) Get(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.dir, name))
}
