package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Account struct {
	Type         string `json:"type"` // "offline" or "microsoft"
	Name         string `json:"name"`
	UUID         string `json:"uuid"`
	AccessToken  string `json:"access_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}

type Config struct {
	Accounts         []Account `json:"accounts"`
	SelectedAccount  int       `json:"selected_account"`
	MemoryMB         int       `json:"memory_mb"`
	JavaPath         string    `json:"java_path"`
	JVMArgs          string    `json:"jvm_args"`
	CurseForgeAPIKey string    `json:"curseforge_api_key"`
	MSClientID       string    `json:"ms_client_id"`
	PlayVia          string    `json:"play_via"` // "official", "builtin" or "" (official when available)
	DiscordOff       bool      `json:"discord_off"`
	DiscordAppID     string    `json:"discord_app_id"`
}

func loadConfig(p *Paths) *Config {
	cfg := &Config{}
	readJSONInto(p.Config, cfg)
	if cfg.MemoryMB < 512 {
		cfg.MemoryMB = defaultMemoryMB()
	}
	return cfg
}

func saveConfig(p *Paths, cfg *Config) error { return writeJSON(p.Config, cfg) }

var ramChoices = []int{2048, 3072, 4096, 5120, 6144, 8192, 10240, 12288, 16384, 24576, 32768}

// ramOptions lists the memory choices that fit in this computer.
func ramOptions() []int {
	total := systemRAMMB()
	var out []int
	for _, c := range ramChoices {
		if total == 0 || c <= total-1024 || c == ramChoices[0] {
			out = append(out, c)
		}
	}
	return out
}

// defaultMemoryMB picks about half the computer's memory, between 2 and 8 GB.
func defaultMemoryMB() int {
	total := systemRAMMB()
	if total == 0 {
		return 4096
	}
	best := ramChoices[0]
	for _, c := range ramOptions() {
		if c <= total/2 && c <= 8192 {
			best = c
		}
	}
	return best
}

type PackInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Author  string `json:"author"`
	Files   int    `json:"files"`
}

type ManualItem struct {
	Project  int    `json:"project"`
	File     int    `json:"file"`
	Name     string `json:"name"`
	FileName string `json:"file_name"`
	Folder   string `json:"folder"`
	URL      string `json:"url"`
	Reason   string `json:"reason"`
	SHA1     string `json:"sha1,omitempty"`
}

type Instance struct {
	Dir           string       `json:"-"`
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	MCVersion     string       `json:"mc_version"`
	Loader        string       `json:"loader"`
	LoaderVersion string       `json:"loader_version"`
	VersionID     string       `json:"version_id"`
	MemoryMB      int          `json:"memory_mb"`
	Pack          *PackInfo    `json:"pack,omitempty"`
	Manual        []ManualItem `json:"manual_downloads"`
	Created       int64        `json:"created"`
	LastPlayed    int64        `json:"last_played"`
	PlaySeconds   int64        `json:"play_seconds"`
	Icon          string       `json:"icon"`
}

func (i *Instance) GameDir() string { return filepath.Join(i.Dir, "minecraft") }
func (i *Instance) Save() error     { return writeJSON(filepath.Join(i.Dir, "instance.json"), i) }

func loadInstance(dir string) *Instance {
	inst := &Instance{}
	if err := readJSONInto(filepath.Join(dir, "instance.json"), inst); err != nil {
		return nil
	}
	inst.Dir = dir
	inst.ID = filepath.Base(dir)
	if inst.Name == "" {
		inst.Name = inst.ID
	}
	if inst.Loader == "" {
		inst.Loader = "vanilla"
	}
	return inst
}

func listInstances(p *Paths) []*Instance {
	entries, _ := os.ReadDir(p.Instances)
	var out []*Instance
	for _, e := range entries {
		if e.IsDir() {
			if inst := loadInstance(filepath.Join(p.Instances, e.Name())); inst != nil {
				out = append(out, inst)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].LastPlayed != out[b].LastPlayed {
			return out[a].LastPlayed > out[b].LastPlayed
		}
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out
}

func findInstance(p *Paths, id string) *Instance {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil
	}
	return loadInstance(filepath.Join(p.Instances, id))
}

func createInstance(p *Paths, name, mc, loader, loaderVersion string) (*Instance, error) {
	base := safeName(name)
	dir := filepath.Join(p.Instances, base)
	for n := 2; exists(dir); n++ {
		dir = filepath.Join(p.Instances, fmt.Sprintf("%s (%d)", base, n))
	}
	if err := os.MkdirAll(filepath.Join(dir, "minecraft"), 0o755); err != nil {
		return nil, err
	}
	if loader == "" {
		loader = "vanilla"
	}
	inst := &Instance{Dir: dir, ID: filepath.Base(dir), Name: strings.TrimSpace(name), MCVersion: mc,
		Loader: loader, LoaderVersion: loaderVersion, Created: time.Now().Unix()}
	if inst.Name == "" {
		inst.Name = inst.ID
	}
	return inst, inst.Save()
}

// prepareInstance installs the mod loader, game files and Java. It returns the merged version JSON and java path.
func prepareInstance(p *Paths, cfg *Config, inst *Instance, logf Logger, prog Progress) (M, string, error) {
	vid, err := installLoader(p, inst.MCVersion, inst.Loader, inst.LoaderVersion, cfg.JavaPath, logf, prog)
	if err != nil {
		return nil, "", err
	}
	if inst.VersionID != vid {
		inst.VersionID = vid
		inst.Save()
	}
	vjson, err := installGameFiles(p, vid, inst.GameDir(), logf, prog)
	if err != nil {
		return nil, "", err
	}
	java := cfg.JavaPath
	if java == "" {
		if java, err = ensureJava(p, vjson, logf, prog); err != nil {
			return nil, "", err
		}
	}
	return vjson, java, nil
}

func instanceMemory(cfg *Config, inst *Instance) int {
	if inst.MemoryMB >= 512 {
		return inst.MemoryMB
	}
	return cfg.MemoryMB
}
