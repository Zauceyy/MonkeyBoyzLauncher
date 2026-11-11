package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Playing through the official Minecraft Launcher: the pack's mod loader is installed into the
// official launcher's own folder and the pack is added there as an installation that points at
// our instance folder. Sign-in then happens in the official launcher with a normal Microsoft account.

var launcherCloseWait = 500 * time.Millisecond

var minecraftDir = func() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, ".minecraft")
		}
		return filepath.Join(home, "AppData", "Roaming", ".minecraft")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "minecraft")
	}
	return filepath.Join(home, ".minecraft")
}

// The classic launcher and the Microsoft Store launcher keep their installations in different files.
var profileFiles = []string{"launcher_profiles.json", "launcher_profiles_microsoft_store.json"}

func officialProfileFiles() []string {
	var out []string
	for _, f := range profileFiles {
		if p := filepath.Join(minecraftDir(), f); exists(p) {
			out = append(out, p)
		}
	}
	return out
}

func officialAvailable() bool { return len(officialProfileFiles()) > 0 }

// officialPaths makes the installer code write into the official launcher's folder, while Java
// (used only to run mod loader setup steps) and temporary files stay in ours.
func officialPaths(p *Paths) *Paths {
	mc := minecraftDir()
	op := &Paths{Root: mc, Versions: filepath.Join(mc, "versions"), Libraries: filepath.Join(mc, "libraries"),
		Assets: filepath.Join(mc, "assets"), Runtime: p.Runtime, Instances: p.Instances, Natives: p.Natives, Temp: p.Temp, Config: p.Config}
	os.MkdirAll(op.Versions, 0o755)
	os.MkdirAll(op.Libraries, 0o755)
	return op
}

func profileIcon(inst *Instance) string {
	if strings.HasSuffix(inst.Icon, ".png") && validFileName(inst.Icon) {
		if b, err := os.ReadFile(filepath.Join(inst.Dir, inst.Icon)); err == nil && len(b) < 200*1024 {
			return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
		}
	}
	return "Jungle_Log"
}

// writeOfficialProfile adds or updates this pack's entry in one launcher profiles file, keeping everything else.
func writeOfficialProfile(file string, inst *Instance, versionID string, memoryMB int, extraJVM string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("could not read %s: %v", filepath.Base(file), err)
	}
	profiles, _ := doc["profiles"].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
	}
	key := "monkeyboyz-" + inst.ID
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	entry, _ := profiles[key].(map[string]any)
	if entry == nil {
		entry = map[string]any{"created": now, "type": "custom"}
	}
	javaArgs := fmt.Sprintf("-Xmx%dM -XX:+UnlockExperimentalVMOptions -XX:+UseG1GC -XX:G1NewSizePercent=20 -XX:G1ReservePercent=20 -XX:MaxGCPauseMillis=50 -XX:G1HeapRegionSize=32M", memoryMB)
	if extraJVM = strings.TrimSpace(extraJVM); extraJVM != "" {
		javaArgs += " " + extraJVM
	}
	entry["name"] = inst.Name
	entry["lastVersionId"] = versionID
	entry["gameDir"] = inst.GameDir()
	entry["javaArgs"] = javaArgs
	entry["lastUsed"] = now // newest first, so the official launcher has it selected
	entry["icon"] = profileIcon(inst)
	profiles[key] = entry
	doc["profiles"] = profiles
	if !exists(file + ".monkeyboyz-backup") {
		os.WriteFile(file+".monkeyboyz-backup", raw, 0o644)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(file, out)
}

func removeOfficialProfile(inst *Instance) {
	for _, file := range officialProfileFiles() {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var doc map[string]any
		if dec.Decode(&doc) != nil {
			continue
		}
		profiles, _ := doc["profiles"].(map[string]any)
		if _, ok := profiles["monkeyboyz-"+inst.ID]; ok {
			delete(profiles, "monkeyboyz-"+inst.ID)
			if out, err := json.MarshalIndent(doc, "", "  "); err == nil {
				writeFileAtomic(file, out)
			}
		}
	}
}

// sendToOfficial prepares the pack inside the official launcher and opens it.
func sendToOfficial(p *Paths, cfg *Config, inst *Instance, logf Logger, prog Progress) (string, error) {
	files := officialProfileFiles()
	if len(files) == 0 {
		return "", errors.New("the Minecraft Launcher has not been used on this computer yet. Install it from minecraft.net, open it once, sign in, close it, then press Play again")
	}
	if officialLauncherRunning() {
		// It rewrites its installation list when it closes, so it has to be closed before we add the pack.
		logf("Closing the Minecraft Launcher so the pack can be added...")
		closeOfficialLauncher()
		for i := 0; i < 24 && officialLauncherRunning(); i++ {
			time.Sleep(launcherCloseWait)
		}
		if officialLauncherRunning() {
			return "", errors.New("the Minecraft Launcher is still open (is a game running from it?). Close it completely, then press Play again")
		}
		time.Sleep(2 * launcherCloseWait) // let it finish saving its files
	}
	collectManual(inst, logf)
	op := officialPaths(p)
	logf("Installing %s into the Minecraft Launcher's folder (%s)...", inst.Name, op.Root)
	vid, err := installLoader(op, inst.MCVersion, inst.Loader, inst.LoaderVersion, cfg.JavaPath, logf, prog)
	if err != nil {
		return "", err
	}
	if inst.VersionID != vid {
		inst.VersionID = vid
		inst.Save()
	}
	os.MkdirAll(inst.GameDir(), 0o755)
	for _, f := range files {
		if err := writeOfficialProfile(f, inst, vid, instanceMemory(cfg, inst), cfg.JVMArgs); err != nil {
			return "", err
		}
	}
	logf("Added \"%s\" to the Minecraft Launcher (version %s, %d MB).", inst.Name, vid, instanceMemory(cfg, inst))
	inst.LastPlayed = time.Now().Unix()
	inst.Save()
	if openOfficialLauncher() {
		return fmt.Sprintf("The Minecraft Launcher is opening with \"%s\" selected. Press Play there.", inst.Name), nil
	}
	return fmt.Sprintf("\"%s\" was added to the Minecraft Launcher. Open the Minecraft Launcher, pick it next to the Play button and press Play.", inst.Name), nil
}

// ---------------------------------------------------------------- who is signed in over there

var accountFiles = []string{"launcher_accounts.json", "launcher_accounts_microsoft_store.json"}

// officialAccount reads the player name and id of the account signed in to the Minecraft Launcher.
// Only the public profile is read; sign-in tokens are never touched.
func officialAccount() (name, uuid string) {
	var newest time.Time
	for _, f := range accountFiles {
		path := filepath.Join(minecraftDir(), f)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		raw, err := readJSON(path)
		if err != nil {
			continue
		}
		doc := asM(raw)
		accounts := asM(doc["accounts"])
		profile := asM(asM(accounts[asS(doc["activeAccountLocalId"])])["minecraftProfile"])
		if asS(profile["name"]) == "" {
			for _, acc := range accounts {
				if p := asM(asM(acc)["minecraftProfile"]); asS(p["name"]) != "" {
					profile = p
					break
				}
			}
		}
		if n := asS(profile["name"]); n != "" && (name == "" || st.ModTime().After(newest)) {
			name, uuid, newest = n, strings.ReplaceAll(asS(profile["id"]), "-", ""), st.ModTime()
		}
	}
	return
}

var sessionProfileURL = "https://sessionserver.mojang.com/session/minecraft/profile/"

// officialSkin downloads the player's skin image from Mojang (cached for a day) so the interface can show their face.
func officialSkin(p *Paths, uuid string) ([]byte, error) {
	for _, c := range uuid {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return nil, errors.New("bad id")
		}
	}
	if len(uuid) != 32 {
		return nil, errors.New("bad id")
	}
	cache := filepath.Join(p.Temp, "skin-"+uuid+".png")
	if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return os.ReadFile(cache)
	}
	fallback := func(err error) ([]byte, error) {
		if b, e := os.ReadFile(cache); e == nil {
			return b, nil
		}
		return nil, err
	}
	raw, err := getJSON(sessionProfileURL+uuid, nil)
	if err != nil {
		return fallback(err)
	}
	for _, prop := range asA(asM(raw)["properties"]) {
		if asS(asM(prop)["name"]) != "textures" {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(asS(asM(prop)["value"]))
		if err != nil {
			return fallback(err)
		}
		var tex M
		json.Unmarshal(dec, &tex)
		u := asS(asM(asM(tex["textures"])["SKIN"])["url"])
		if !strings.Contains(u, "://textures.minecraft.net/") {
			break
		}
		img, err := httpBytes("GET", strings.Replace(u, "http://", "https://", 1), nil, nil)
		if err != nil {
			return fallback(err)
		}
		writeFileAtomic(cache, img)
		return img, nil
	}
	return fallback(errors.New("no skin"))
}
