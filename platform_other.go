//go:build !windows

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func hiddenCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func systemRAMMB() int {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.Atoi(f[1])
					return kb / 1024
				}
			}
		}
	}
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return int(n / (1024 * 1024))
	}
	return 0
}

func opener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func openPath(path string) error { return exec.Command(opener(), path).Start() }
func openURL(url string) error   { return exec.Command(opener(), url).Start() }

func appBrowsers() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
	}
	var out []string
	for _, n := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(n); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// dialDiscord opens the Discord desktop app's local socket.
func dialDiscord(n int) (io.ReadWriteCloser, error) {
	var last error = errors.New("Discord is not running")
	for _, dir := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("TMPDIR"), "/tmp"} {
		if dir == "" {
			continue
		}
		c, err := net.DialTimeout("unix", filepath.Join(dir, "discord-ipc-"+strconv.Itoa(n)), 2*time.Second)
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

var officialLauncherRunning = func() bool {
	return exec.Command("pgrep", "-if", "minecraft.launcher|minecraft-launcher").Run() == nil
}

var openOfficialLauncher = func() bool {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-a", "Minecraft").Start() == nil
	}
	if p, err := exec.LookPath("minecraft-launcher"); err == nil {
		return exec.Command(p).Start() == nil
	}
	return false
}

var closeOfficialLauncher = func() {
	exec.Command("pkill", "-if", "minecraft.launcher|minecraft-launcher").Run()
}
