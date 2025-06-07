package embedexec

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
)

//go:embed bin/*.exe
var exeFS embed.FS

// List returns the embedded exe filenames, e.g. ["helper1.exe", "helper2.exe"].
func List() ([]string, error) {
	entries, err := fs.ReadDir(exeFS, "bin")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Extract writes the named exe to a temp dir, marks it executable,
// and returns (path, cleanupFn, err).
func Extract(name string) (string, func(), error) {
	data, err := exeFS.ReadFile(path.Join("bin", name))
	if err != nil {
		return "", nil, fmt.Errorf("read embedded exe: %w", err)
	}

	tmpFile, err := os.CreateTemp("", name+"-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	cleanup := func() { os.Remove(tmpFile.Name()) } // remove when caller is done

	if _, err := tmpFile.Write(data); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write temp exe: %w", err)
	}
	tmpFile.Close()

	// mark executable (harmless on Windows, required on UNIX)
	if err := os.Chmod(tmpFile.Name(), 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chmod temp exe: %w", err)
	}

	return tmpFile.Name(), cleanup, nil
}

// Run is convenience glue tying Extract + exec.CommandContext together.
func Run(ctx context.Context, base string, args ...string) (*exec.Cmd, func(), error) {
	exe := base + ".exe"

	path, cleanup, err := Extract(exe)
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.CommandContext(ctx, path, args...)
	return cmd, cleanup, nil
}
