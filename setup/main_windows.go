//go:build windows

// Setup program for MonkeyBoyz Launcher. It copies the launcher into the user's own programs
// folder (no administrator rights needed), adds Desktop and Start Menu shortcuts and an entry in
// Windows' installed-apps list. A copy of this program named Uninstall.exe removes all of that.
package main

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed MonkeyBoyzLauncher.exe
var payload []byte

const (
	appTitle   = "MonkeyBoyz Launcher"
	appVersion = "2.4.3"
	regKey     = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\MonkeyBoyzLauncher`

	mbYesNo, mbQuestion, mbInfo, mbError, idYes = 0x4, 0x20, 0x40, 0x10, 6
)

func msgBox(text string, flags uintptr) int {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(appTitle + " Setup")
	r, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
	return int(r)
}

func hidden(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}

func installDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "Programs", "MonkeyBoyzLauncher")
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// shortcuts creates (or removes) the Desktop and Start Menu shortcuts through Windows' own script host.
func shortcuts(target, dir string, create bool) error {
	script := `$ErrorActionPreference='Stop';$s=New-Object -ComObject WScript.Shell;` +
		`foreach($d in @([Environment]::GetFolderPath('Desktop'),[Environment]::GetFolderPath('Programs'))){` +
		`$p=Join-Path $d 'MonkeyBoyz Launcher.lnk';`
	if create {
		script += `$l=$s.CreateShortcut($p);$l.TargetPath=` + psQuote(target) + `;$l.WorkingDirectory=` + psQuote(dir) +
			`;$l.Description='Modded Minecraft launcher';$l.Save()}`
	} else {
		script += `if(Test-Path $p){Remove-Item $p -Force}}`
	}
	return hidden("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

func regAdd(name, kind, value string) {
	hidden("reg", "add", regKey, "/v", name, "/t", kind, "/d", value, "/f").Run()
}

func install() {
	if msgBox("Install "+appTitle+" "+appVersion+"?\n\nIt goes into your own programs folder, so no administrator password is needed. "+
		"Shortcuts are added to the Desktop and the Start Menu.", mbYesNo|mbQuestion) != idYes {
		return
	}
	dir := installDir()
	target := filepath.Join(dir, "MonkeyBoyzLauncher.exe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		msgBox("Could not create the install folder:\n"+err.Error(), mbError)
		return
	}
	// An older copy that is still running (even in the background) locks its file, so close it first.
	err := os.WriteFile(target, payload, 0o755)
	for try := 0; err != nil && try < 10; try++ {
		hidden("taskkill", "/F", "/IM", "MonkeyBoyzLauncher.exe").Run()
		time.Sleep(700 * time.Millisecond)
		err = os.WriteFile(target, payload, 0o755)
	}
	if err != nil {
		msgBox("Could not copy the launcher. Restart the computer and run Setup again.\n\n"+err.Error(), mbError)
		return
	}
	// Carry over ID files shared next to Setup, so friends sign in without typing anything.
	self, _ := os.Executable()
	for _, name := range []string{"ms_client_id.txt", "discord_app_id.txt"} {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(self), name)); err == nil {
			os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
	}
	uninstaller := filepath.Join(dir, "Uninstall.exe")
	if data, err := os.ReadFile(self); err == nil {
		os.WriteFile(uninstaller, data, 0o755)
	}
	shortcutErr := shortcuts(target, dir, true)

	regAdd("DisplayName", "REG_SZ", appTitle)
	regAdd("DisplayVersion", "REG_SZ", appVersion)
	regAdd("Publisher", "REG_SZ", "MonkeyBoyz")
	regAdd("InstallLocation", "REG_SZ", dir)
	regAdd("DisplayIcon", "REG_SZ", target)
	regAdd("UninstallString", "REG_SZ", `"`+uninstaller+`"`)
	regAdd("NoModify", "REG_DWORD", "1")
	regAdd("NoRepair", "REG_DWORD", "1")

	note := ""
	if shortcutErr != nil {
		note = "\n\n(The shortcuts could not be created. You can start it from:\n" + target + ")"
	}
	if msgBox(appTitle+" is installed."+note+"\n\nStart it now?", mbYesNo|mbInfo) == idYes {
		cmd := exec.Command(target)
		cmd.Dir = dir
		cmd.Start()
	}
}

func uninstall() {
	if msgBox("Remove "+appTitle+" from this computer?\n\nYour packs, worlds and settings are kept.", mbYesNo|mbQuestion) != idYes {
		return
	}
	dir := installDir()
	shortcuts("", "", false)
	hidden("reg", "delete", regKey, "/f").Run()
	os.Remove(filepath.Join(dir, "MonkeyBoyzLauncher.exe"))
	data := filepath.Join(os.Getenv("APPDATA"), "MonkeyBoyzLauncher")
	msgBox(appTitle+" was removed.\n\nYour packs and worlds are still in:\n"+data+"\nDelete that folder too if you want them gone.", mbInfo)
	// This program cannot delete itself while running, so a short delayed command removes the folder.
	cmd := exec.Command("cmd")
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000,
		CmdLine: `cmd /c ping 127.0.0.1 -n 3 >nul & rmdir /s /q "` + dir + `"`}
	cmd.Start()
}

func main() {
	self, _ := os.Executable()
	if strings.HasPrefix(strings.ToLower(filepath.Base(self)), "uninstall") {
		uninstall()
		return
	}
	install()
}
