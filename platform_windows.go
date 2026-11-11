//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// hiddenCommand runs a console program without flashing a console window.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}

func systemRAMMB() int {
	var st struct {
		Length                                                                     uint32
		MemoryLoad                                                                 uint32
		TotalPhys, AvailPhys, TotalPage, AvailPage, TotalVirt, AvailVirt, AvailExt uint64
	}
	st.Length = uint32(unsafe.Sizeof(st))
	r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0
	}
	return int(st.TotalPhys / (1024 * 1024))
}

func openPath(path string) error { return exec.Command("explorer", path).Start() }

func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func appBrowsers() []string {
	var out []string
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
		if base == "" {
			continue
		}
		out = append(out, filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"))
	}
	return out
}

// dialDiscord opens the Discord desktop app's local named pipe.
func dialDiscord(n int) (io.ReadWriteCloser, error) {
	return os.OpenFile(`\\.\pipe\discord-ipc-`+strconv.Itoa(n), os.O_RDWR, 0)
}

var officialLauncherRunning = func() bool {
	out, err := hiddenCommand("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	s := strings.ToLower(string(out))
	return strings.Contains(s, `"minecraftlauncher.exe"`) || strings.Contains(s, `"minecraft.exe"`)
}

var openOfficialLauncher = func() bool {
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		exe := filepath.Join(base, "Minecraft Launcher", "MinecraftLauncher.exe")
		if base != "" && exists(exe) {
			return exec.Command(exe).Start() == nil
		}
	}
	if exe := `C:\XboxGames\Minecraft Launcher\Content\Minecraft.exe`; exists(exe) {
		return exec.Command(exe).Start() == nil
	}
	// Any other install (Microsoft Store, Xbox app): ask Windows for the Start Menu entry and open that.
	script := `$a=Get-StartApps | Where-Object {$_.Name -like 'Minecraft Launcher*'} | Select-Object -First 1;` +
		`if($a){Start-Process ('shell:AppsFolder\'+$a.AppID); exit 0}; exit 1`
	if hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run() == nil {
		return true
	}
	const pkg = "Microsoft.4297127D64EC6_8wekyb3d8bbwe"
	if exists(filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", pkg)) {
		return exec.Command("explorer.exe", `shell:AppsFolder\`+pkg+`!Minecraft`).Start() == nil
	}
	return false
}

// closeOfficialLauncher asks the Minecraft Launcher to close (a normal close, so it saves its files).
var closeOfficialLauncher = func() {
	hiddenCommand("taskkill", "/IM", "MinecraftLauncher.exe").Run()
	hiddenCommand("taskkill", "/IM", "Minecraft.exe").Run()
}
