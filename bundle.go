package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// inAppBundle reports whether the binary runs from a macOS .app bundle,
// i.e. it was probably started from Finder or the Dock.
func inAppBundle() bool {
	exe, err := os.Executable()
	return err == nil && runtime.GOOS == "darwin" && strings.Contains(exe, ".app/Contents/MacOS/")
}

// prepareBundleEnv gives Finder-launched apps the tool paths a terminal has:
// Finder starts apps with PATH=/usr/bin:/bin:/usr/sbin:/sbin, which misses
// Homebrew's ffmpeg.
func prepareBundleEnv() {
	if !inAppBundle() {
		return
	}
	path := os.Getenv("PATH")
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(":"+path+":", ":"+dir+":") {
			path += ":" + dir
		}
	}
	os.Setenv("PATH", path)
}

// screenshotDir is the working directory for terminal use and
// ~/Pictures/Scout for the app bundle, whose working directory is /.
func screenshotDir() string {
	if !inAppBundle() {
		return "."
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, "Pictures", "Scout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "."
	}
	return dir
}
