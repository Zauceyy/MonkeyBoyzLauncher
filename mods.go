package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ModEntry is one file in an instance's mods folder, as shown in the mod list.
type ModEntry struct {
	File        string `json:"file"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	Enabled     bool   `json:"enabled"`
	HasIcon     bool   `json:"has_icon"`
	Logo        string `json:"logo"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
}

type jarMeta struct {
	Name, Version, Description, Icon string
	mtime, size                      int64
}

var (
	jarCache   = map[string]jarMeta{}
	jarCacheMu sync.Mutex
	tomlName   = regexp.MustCompile(`(?m)^\s*displayName\s*=\s*["']([^"'\r\n]*)["']`)
	tomlVer    = regexp.MustCompile(`(?m)^\s*version\s*=\s*["']([^"'\r\n]*)["']`)
	tomlLogo   = regexp.MustCompile(`(?m)^\s*logoFile\s*=\s*["']([^"'\r\n]*)["']`)
	tomlDesc   = regexp.MustCompile(`(?m)^\s*description\s*=\s*(?:'''|"""|["'])\s*([^\r\n"']*)`)
	jsonNameRe = regexp.MustCompile(`"name"\s*:\s*"([^"]*)"`)
)

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// readJarMeta pulls the mod's display name, version and icon path out of the jar's own metadata.
func readJarMeta(jarPath string) jarMeta {
	st, err := os.Stat(jarPath)
	if err != nil {
		return jarMeta{}
	}
	jarCacheMu.Lock()
	c, ok := jarCache[jarPath]
	jarCacheMu.Unlock()
	if ok && c.mtime == st.ModTime().UnixNano() && c.size == st.Size() {
		return c
	}
	meta := jarMeta{mtime: st.ModTime().UnixNano(), size: st.Size()}
	if zr, err := zip.OpenReader(jarPath); err == nil {
		read := func(name string) string {
			b, err := zipRead(&zr.Reader, name)
			if err != nil || len(b) > 1<<20 {
				return ""
			}
			return string(b)
		}
		if s := read("fabric.mod.json"); s != "" {
			parseFabricLike(s, &meta, false)
		} else if s := read("quilt.mod.json"); s != "" {
			parseFabricLike(s, &meta, true)
		} else if s := read("META-INF/neoforge.mods.toml") + read("META-INF/mods.toml"); s != "" {
			meta.Name, meta.Version = firstMatch(tomlName, s), firstMatch(tomlVer, s)
			meta.Icon, meta.Description = firstMatch(tomlLogo, s), firstMatch(tomlDesc, s)
		} else if s := read("mcmod.info"); s != "" {
			var list []M
			if json.Unmarshal([]byte(s), &list) != nil {
				var wrap M
				if json.Unmarshal([]byte(s), &wrap) == nil {
					for _, x := range asA(wrap["modList"]) {
						list = append(list, asM(x))
					}
				}
			}
			if len(list) > 0 {
				meta.Name, meta.Version = asS(list[0]["name"]), asS(list[0]["version"])
				meta.Description, meta.Icon = asS(list[0]["description"]), asS(list[0]["logoFile"])
			} else {
				meta.Name = firstMatch(jsonNameRe, s)
			}
		}
		if strings.Contains(meta.Version, "${") {
			meta.Version = ""
		}
		meta.Icon = strings.TrimLeft(meta.Icon, "/")
		if meta.Icon != "" {
			if _, err := zipRead(&zr.Reader, meta.Icon); err != nil {
				meta.Icon = ""
			}
		}
		zr.Close()
	}
	if len(meta.Description) > 220 {
		meta.Description = meta.Description[:220] + "..."
	}
	jarCacheMu.Lock()
	jarCache[jarPath] = meta
	jarCacheMu.Unlock()
	return meta
}

func parseFabricLike(s string, meta *jarMeta, quilt bool) {
	var doc M
	if json.Unmarshal([]byte(s), &doc) != nil {
		meta.Name = firstMatch(jsonNameRe, s) // some jars ship slightly broken JSON
		return
	}
	if quilt {
		ql := asM(doc["quilt_loader"])
		md := asM(ql["metadata"])
		meta.Name, meta.Version = asS(md["name"]), asS(ql["version"])
		meta.Description, meta.Icon = asS(md["description"]), asS(md["icon"])
		return
	}
	meta.Name, meta.Version, meta.Description = asS(doc["name"]), asS(doc["version"]), asS(doc["description"])
	switch ic := doc["icon"].(type) {
	case string:
		meta.Icon = ic
	case map[string]any:
		for _, v := range ic {
			meta.Icon = asS(v)
		}
	}
}

func validFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\:`)
}

func loadModIndex(inst *Instance) map[string]ModMeta {
	var metas []ModMeta
	readJSONInto(filepath.Join(inst.Dir, "modlist.json"), &metas)
	out := map[string]ModMeta{}
	for _, m := range metas {
		out[m.FileName] = m
	}
	return out
}

// scanMods lists what is really in the instance's mods folder, enriched with CurseForge details when known.
func scanMods(inst *Instance) []ModEntry {
	dir := filepath.Join(inst.GameDir(), "mods")
	entries, _ := os.ReadDir(dir)
	index := loadModIndex(inst)
	out := []ModEntry{}
	for _, e := range entries {
		name := e.Name()
		lower := strings.ToLower(name)
		enabled := strings.HasSuffix(lower, ".jar")
		if e.IsDir() || !(enabled || strings.HasSuffix(lower, ".jar.disabled")) {
			continue
		}
		base := strings.TrimSuffix(name, ".disabled")
		meta := readJarMeta(filepath.Join(dir, name))
		m := ModEntry{File: base, Name: meta.Name, Version: meta.Version, Description: meta.Description,
			Enabled: enabled, HasIcon: meta.Icon != "", Size: meta.size}
		if cf, ok := index[base]; ok {
			if cf.Name != "" {
				m.Name = cf.Name
			}
			if cf.Summary != "" {
				m.Description = cf.Summary
			}
			m.Logo, m.URL, m.Author = cf.Logo, cf.URL, cf.Author
		}
		if m.Name == "" {
			m.Name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		out = append(out, m)
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out
}

func countFiles(dir string) int {
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func countMods(inst *Instance) int {
	entries, _ := os.ReadDir(filepath.Join(inst.GameDir(), "mods"))
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			n++
		}
	}
	return n
}

// toggleMod switches a mod on or off by renaming it to or from ".jar.disabled".
func toggleMod(inst *Instance, file string, enabled bool) error {
	if !validFileName(file) || !strings.HasSuffix(strings.ToLower(file), ".jar") {
		return errors.New("not a mod file")
	}
	on := filepath.Join(inst.GameDir(), "mods", file)
	off := on + ".disabled"
	if enabled && exists(off) {
		return os.Rename(off, on)
	}
	if !enabled && exists(on) {
		return os.Rename(on, off)
	}
	return nil
}

// modIcon returns the icon image stored inside a mod's jar.
func modIcon(inst *Instance, file string) ([]byte, error) {
	if !validFileName(file) {
		return nil, errors.New("bad file name")
	}
	jar := filepath.Join(inst.GameDir(), "mods", file)
	if !exists(jar) {
		jar += ".disabled"
	}
	meta := readJarMeta(jar)
	if meta.Icon == "" {
		return nil, errors.New("no icon")
	}
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return zipRead(&zr.Reader, path.Clean(meta.Icon))
}
