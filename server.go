package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

type logLine struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

type Notice struct {
	ID       int    `json:"id"`
	Kind     string `json:"kind"` // "ok" or "error"
	Text     string `json:"text"`
	Instance string `json:"instance,omitempty"`
}

type App struct {
	p     *Paths
	token string

	mu        sync.Mutex
	cfg       *Config
	busy      bool
	status    string
	progLabel string
	progDone  int
	progTotal int
	logs      []logLine
	logSeq    int
	notice    Notice
	game      *exec.Cmd
	gameInst  string
	ms        *deviceLogin
	msCancel  chan struct{}
	lastPoll  time.Time
	versions  []string

	gameStart   time.Time
	launched    time.Time
	discordLive bool
	byeAt       time.Time // when the window last said it was closing
}

func newApp(p *Paths, token string) *App {
	return &App{p: p, token: token, cfg: loadConfig(p), status: "Ready", lastPoll: time.Now(), launched: time.Now()}
}

// ---------------------------------------------------------------- logging, progress, tasks

func (a *App) logf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(text, "\r\n"), "\n") {
		a.logSeq++
		a.logs = append(a.logs, logLine{a.logSeq, strings.TrimRight(line, "\r")})
	}
	if len(a.logs) > 5000 {
		a.logs = append([]logLine{}, a.logs[len(a.logs)-4000:]...)
	}
}

func (a *App) progress(label string, done, total int) {
	a.mu.Lock()
	a.progLabel, a.progDone, a.progTotal = label, done, total
	a.mu.Unlock()
}

func (a *App) setNotice(kind, text, inst string) {
	a.mu.Lock()
	a.notice = Notice{a.notice.ID + 1, kind, text, inst}
	a.mu.Unlock()
}

// runTask runs one long job at a time in the background. fn returns a success message and instance id.
func (a *App) runTask(status string, fn func() (string, string, error)) error {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return errors.New("the launcher is still busy with something else")
	}
	a.busy, a.status = true, status
	a.progLabel, a.progDone, a.progTotal = "", 0, 0
	a.mu.Unlock()
	go func() {
		var msg, inst string
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("unexpected problem: %v", r)
				}
			}()
			msg, inst, err = fn()
		}()
		a.mu.Lock()
		a.busy, a.status = false, "Ready"
		a.progLabel, a.progDone, a.progTotal = "", 0, 0
		a.mu.Unlock()
		if err != nil {
			a.logf("ERROR: %v", err)
			a.setNotice("error", err.Error(), inst)
		} else if msg != "" {
			a.setNotice("ok", msg, inst)
		}
	}()
	return nil
}

// builtinCurseForgeKey can be set at build time (-ldflags "-X main.builtinCurseForgeKey=...") so the
// group shares one key without anyone pasting it. It is not kept in the source.
var builtinCurseForgeKey = ""

// curseForgeKey finds the API key: Settings, then curseforge_api_key.txt next to the exe, then the built-in one.
func curseForgeKey(cfg *Config) string {
	if k := strings.TrimSpace(cfg.CurseForgeAPIKey); k != "" {
		return k
	}
	if k := sideFile("curseforge_api_key.txt"); k != "" {
		return k
	}
	return builtinCurseForgeKey
}

// workConfig is the configuration used for doing work, with the shared CurseForge key filled in.
func (a *App) workConfig() Config {
	c := a.configCopy()
	c.CurseForgeAPIKey = curseForgeKey(&c)
	return c
}

func (a *App) configCopy() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := *a.cfg
	c.Accounts = append([]Account{}, a.cfg.Accounts...)
	return c
}

// ---------------------------------------------------------------- game

// playVia says whether Play hands the pack to the official Minecraft Launcher or starts the game itself.
func playVia(cfg *Config) string {
	if cfg.PlayVia == "official" || cfg.PlayVia == "builtin" {
		return cfg.PlayVia
	}
	if officialAvailable() {
		return "official"
	}
	return "builtin"
}

func (a *App) playOfficial(inst *Instance) error {
	return a.runTask("Sending "+inst.Name+" to the Minecraft Launcher", func() (string, string, error) {
		cfg := a.workConfig()
		msg, err := sendToOfficial(a.p, &cfg, inst, a.logf, a.progress)
		return msg, inst.ID, err
	})
}

func (a *App) play(id, via string) error {
	inst := findInstance(a.p, id)
	if inst == nil {
		return errors.New("that instance no longer exists")
	}
	if via == "" {
		cfg := a.workConfig()
		via = playVia(&cfg)
	}
	if via == "official" {
		return a.playOfficial(inst)
	}
	a.mu.Lock()
	if a.game != nil {
		a.mu.Unlock()
		return errors.New("the game is already running")
	}
	if len(a.cfg.Accounts) == 0 {
		a.mu.Unlock()
		return errors.New("no_account")
	}
	if a.cfg.SelectedAccount >= len(a.cfg.Accounts) || a.cfg.SelectedAccount < 0 {
		a.cfg.SelectedAccount = 0
	}
	idx := a.cfg.SelectedAccount
	acct := a.cfg.Accounts[idx]
	a.mu.Unlock()

	return a.runTask("Getting "+inst.Name+" ready", func() (string, string, error) {
		fresh, err := refreshAccount(acct)
		if err != nil {
			return "", id, err
		}
		if fresh.AccessToken != acct.AccessToken {
			a.mu.Lock()
			if idx < len(a.cfg.Accounts) && a.cfg.Accounts[idx].UUID == acct.UUID {
				a.cfg.Accounts[idx] = fresh
				saveConfig(a.p, a.cfg)
			}
			a.mu.Unlock()
		}
		collectManual(inst, a.logf)
		cfg := a.workConfig()
		vjson, java, err := prepareInstance(a.p, &cfg, inst, a.logf, a.progress)
		if err != nil {
			return "", id, err
		}
		a.logf("Starting Minecraft %s (%s) with %d MB of memory as %s...", inst.MCVersion, inst.Loader, instanceMemory(&cfg, inst), fresh.Name)
		cmd, out, err := launchGame(a.p, vjson, java, inst.GameDir(), inst.ID, fresh, instanceMemory(&cfg, inst), strings.Fields(cfg.JVMArgs))
		if err != nil {
			return "", id, err
		}
		started := time.Now()
		inst.LastPlayed = started.Unix()
		inst.Save()
		a.mu.Lock()
		a.game, a.gameInst, a.gameStart = cmd, id, started
		a.mu.Unlock()
		go a.pump(cmd, out, id, started)
		return "", id, nil
	})
}

func (a *App) pump(cmd *exec.Cmd, out io.Reader, id string, started time.Time) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		a.logf("%s", sc.Text())
	}
	io.Copy(io.Discard, out)
	err := cmd.Wait()
	a.mu.Lock()
	a.game, a.gameInst = nil, ""
	a.mu.Unlock()
	if inst := findInstance(a.p, id); inst != nil {
		inst.PlaySeconds += int64(time.Since(started).Seconds())
		inst.Save()
	}
	if err != nil {
		a.logf("The game closed with a problem (%v).", err)
		if time.Since(started) < 3*time.Minute {
			a.setNotice("error", "The game closed unexpectedly. Check the Console tab for the reason; the game's own logs are in the instance's logs and crash-reports folders.", id)
		}
	} else {
		a.logf("The game closed normally.")
	}
}

func (a *App) stopGame() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.game != nil && a.game.Process != nil {
		a.game.Process.Kill()
	}
}

// windowGone reports that the window said goodbye a few seconds ago and has not checked in since.
func (a *App) windowGone() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.byeAt.IsZero() && a.lastPoll.Before(a.byeAt) && time.Since(a.byeAt) > 3*time.Second
}

func (a *App) idle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.busy && a.game == nil
}

// ---------------------------------------------------------------- Microsoft sign-in

func (a *App) startMSLogin() error {
	cfg := a.workConfig()
	a.mu.Lock()
	if a.ms != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	d, err := startDeviceLogin(msClientID(&cfg))
	if err != nil {
		return err
	}
	cancel := make(chan struct{})
	a.mu.Lock()
	a.ms, a.msCancel = d, cancel
	a.mu.Unlock()
	openURL(d.URL)
	go func() {
		acct, err := finishDeviceLogin(d, cancel)
		a.mu.Lock()
		a.ms, a.msCancel = nil, nil
		if err == nil {
			kept := []Account{}
			for _, x := range a.cfg.Accounts {
				if x.UUID != acct.UUID {
					kept = append(kept, x)
				}
			}
			a.cfg.Accounts = append(kept, acct)
			a.cfg.SelectedAccount = len(a.cfg.Accounts) - 1
			saveConfig(a.p, a.cfg)
		}
		a.mu.Unlock()
		if err != nil {
			if err.Error() != "sign-in cancelled" {
				a.setNotice("error", err.Error(), "")
			}
			return
		}
		a.logf("Signed in as %s.", acct.Name)
		a.setNotice("ok", "Signed in as "+acct.Name, "")
	}()
	return nil
}

// ---------------------------------------------------------------- HTTP plumbing

type apiError struct {
	msg  string
	code string
}

func (e *apiError) Error() string { return e.msg }

func (a *App) authorized(r *http.Request) bool {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	return r.Header.Get("X-Token") == a.token || r.URL.Query().Get("t") == a.token
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handle wraps an API handler: checks the session token, decodes a JSON body, encodes the reply.
func (a *App) handle(mux *http.ServeMux, path string, fn func(r *http.Request, body M) (any, error)) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			writeJSONResp(w, 403, M{"error": "forbidden"})
			return
		}
		body := M{}
		if r.Method == "POST" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		}
		out, err := fn(r, body)
		if err != nil {
			resp := M{"error": err.Error()}
			var ae *apiError
			if errors.As(err, &ae) {
				resp["code"] = ae.code
			}
			writeJSONResp(w, 400, resp)
			return
		}
		if out == nil {
			out = M{"ok": true}
		}
		writeJSONResp(w, 200, out)
	})
}

func (a *App) instFrom(r *http.Request, body M) (*Instance, error) {
	id := asS(body["id"])
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	inst := findInstance(a.p, id)
	if inst == nil {
		return nil, errors.New("that instance no longer exists")
	}
	return inst, nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || !a.authorized(r) {
			http.Error(w, "Open MonkeyBoyz Launcher from its exe.", 403)
			return
		}
		b, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	a.handle(mux, "/api/state", a.apiState)
	a.handle(mux, "/api/bye", func(r *http.Request, _ M) (any, error) {
		a.mu.Lock()
		a.byeAt = time.Now()
		a.mu.Unlock()
		return nil, nil
	})

	a.handle(mux, "/api/import", func(r *http.Request, _ M) (any, error) {
		if r.Method != "POST" {
			return nil, errors.New("POST only")
		}
		name, _ := url.QueryUnescape(r.Header.Get("X-Filename"))
		tmp := filepath.Join(a.p.Temp, fmt.Sprintf("import-%d.zip", time.Now().UnixNano()))
		f, err := os.Create(tmp)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(f, r.Body)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return nil, err
		}
		if err := a.startImport(tmp, name, true); err != nil {
			os.Remove(tmp)
			return nil, err
		}
		return nil, nil
	})

	a.handle(mux, "/api/instance/create", func(r *http.Request, b M) (any, error) {
		mc, loader, lv := strings.TrimSpace(asS(b["mc"])), strings.ToLower(asS(b["loader"])), strings.TrimSpace(asS(b["loader_version"]))
		if mc == "" {
			return nil, errors.New("pick a Minecraft version")
		}
		if loader != "" && loader != "vanilla" && lv == "" {
			return nil, errors.New("pick a mod loader version")
		}
		name := strings.TrimSpace(asS(b["name"]))
		if name == "" {
			name = "Minecraft " + mc
		}
		inst, err := createInstance(a.p, name, mc, loader, lv)
		if err != nil {
			return nil, err
		}
		return M{"id": inst.ID}, nil
	})

	a.handle(mux, "/api/instance/update", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		if n, ok := b["name"].(string); ok && strings.TrimSpace(n) != "" {
			inst.Name = strings.TrimSpace(n)
		}
		if _, ok := b["memory_mb"]; ok {
			inst.MemoryMB = asInt(b["memory_mb"]) // 0 = use the launcher default
		}
		return nil, inst.Save()
	})

	a.handle(mux, "/api/instance/delete", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		running := a.gameInst == inst.ID
		a.mu.Unlock()
		if running {
			return nil, errors.New("close the game before deleting this instance")
		}
		removeOfficialProfile(inst)
		return nil, os.RemoveAll(inst.Dir)
	})

	mux.HandleFunc("/api/instance/icon", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		inst := findInstance(a.p, r.URL.Query().Get("id"))
		if inst == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case "GET":
			if inst.Icon == "" || !validFileName(inst.Icon) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "max-age=31536000")
			http.ServeFile(w, r, filepath.Join(inst.Dir, inst.Icon))
		case "POST":
			data, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
			ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[http.DetectContentType(data)]
			if ext == "" {
				writeJSONResp(w, 400, M{"error": "Pick a PNG, JPG, GIF or WebP image."})
				return
			}
			if inst.Icon != "" && validFileName(inst.Icon) {
				os.Remove(filepath.Join(inst.Dir, inst.Icon))
			}
			inst.Icon = fmt.Sprintf("icon-%d%s", time.Now().Unix(), ext)
			os.WriteFile(filepath.Join(inst.Dir, inst.Icon), data, 0o644)
			inst.Save()
			writeJSONResp(w, 200, M{"ok": true})
		case "DELETE":
			if inst.Icon != "" && validFileName(inst.Icon) {
				os.Remove(filepath.Join(inst.Dir, inst.Icon))
			}
			inst.Icon = ""
			inst.Save()
			writeJSONResp(w, 200, M{"ok": true})
		}
	})

	a.handle(mux, "/api/mods", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		var packList []M
		readJSONInto(filepath.Join(inst.Dir, "packlist.json"), &packList)
		manual := inst.Manual
		if manual == nil {
			manual = []ManualItem{}
		}
		return M{"mods": scanMods(inst), "manual": manual, "pack_list": packList,
			"resourcepacks": countFiles(filepath.Join(inst.GameDir(), "resourcepacks")),
			"shaderpacks":   countFiles(filepath.Join(inst.GameDir(), "shaderpacks"))}, nil
	})

	a.handle(mux, "/api/packsearch", func(r *http.Request, b M) (any, error) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		var hits []SearchHit
		var err error
		if r.URL.Query().Get("source") == "curseforge" {
			cfg := a.workConfig()
			hits, err = searchPacksCurseForge(q, cfg.CurseForgeAPIKey)
		} else {
			hits, err = searchPacksModrinth(q)
		}
		if err != nil {
			return nil, err
		}
		return M{"results": hits}, nil
	})

	a.handle(mux, "/api/packsearch/install", func(r *http.Request, b M) (any, error) {
		source, project, name := asS(b["source"]), asS(b["project"]), asS(b["name"])
		return nil, a.runTask("Installing "+name, func() (string, string, error) {
			cfg := a.workConfig()
			var inst *Instance
			var err error
			if source == "curseforge" {
				n, _ := strconv.Atoi(project)
				inst, err = installPackCurseForge(a.p, &cfg, n, a.logf, a.progress)
			} else {
				inst, err = installPackModrinth(a.p, &cfg, project, a.logf, a.progress)
			}
			if err != nil {
				return "", "", err
			}
			if n := len(inst.Manual); n > 0 {
				return fmt.Sprintf("%s is installed, but %d file(s) need a manual download. See the Missing files tab.", inst.Name, n), inst.ID, nil
			}
			return inst.Name + " is ready to play.", inst.ID, nil
		})
	})

	a.handle(mux, "/api/search", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		q, kind := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("kind")
		if err := checkKind(inst, kind); err != nil {
			return nil, err
		}
		var hits []SearchHit
		if r.URL.Query().Get("source") == "curseforge" {
			cfg := a.workConfig()
			hits, err = searchCurseForge(inst, q, kind, cfg.CurseForgeAPIKey)
		} else {
			hits, err = searchModrinth(inst, q, kind)
		}
		if err != nil {
			return nil, err
		}
		return M{"results": hits}, nil
	})

	a.handle(mux, "/api/search/install", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		kind, source, project, name := asS(b["kind"]), asS(b["source"]), asS(b["project"]), asS(b["name"])
		if err := checkKind(inst, kind); err != nil {
			return nil, err
		}
		return nil, a.runTask("Installing "+name, func() (string, string, error) {
			seen := map[string]bool{}
			var err error
			if source == "curseforge" {
				cfg := a.workConfig()
				n, _ := strconv.Atoi(project)
				err = installCurseForge(inst, n, kind, cfg.CurseForgeAPIKey, a.logf, seen, 0)
			} else {
				err = installModrinth(inst, project, kind, a.logf, seen, 0)
			}
			if err != nil {
				return "", inst.ID, err
			}
			msg := name + " was added to " + inst.Name + "."
			if len(seen) > 1 {
				msg = fmt.Sprintf("%s was added to %s, along with %d add-on(s) it needs.", name, inst.Name, len(seen)-1)
			}
			return msg, inst.ID, nil
		})
	})

	a.handle(mux, "/api/mods/toggle", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		enabled, _ := b["enabled"].(bool)
		if all, _ := b["all"].(bool); all {
			for _, m := range scanMods(inst) {
				toggleMod(inst, m.File, enabled)
			}
			return nil, nil
		}
		return nil, toggleMod(inst, asS(b["file"]), enabled)
	})

	mux.HandleFunc("/api/modicon", func(w http.ResponseWriter, r *http.Request) {
		inst := findInstance(a.p, r.URL.Query().Get("id"))
		if !a.authorized(r) || inst == nil {
			http.NotFound(w, r)
			return
		}
		data, err := modIcon(inst, r.URL.Query().Get("file"))
		ct := http.DetectContentType(data)
		if err != nil || !strings.HasPrefix(ct, "image/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(data)
	})

	mux.HandleFunc("/api/official/skin", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.NotFound(w, r)
			return
		}
		data, err := officialSkin(a.p, r.URL.Query().Get("uuid"))
		if err != nil || http.DetectContentType(data) != "image/png" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write(data)
	})

	a.handle(mux, "/api/manual/check", func(r *http.Request, b M) (any, error) {
		inst, err := a.instFrom(r, b)
		if err != nil {
			return nil, err
		}
		before := len(inst.Manual)
		cfg := a.workConfig()
		resolveManualViaModrinth(inst, cfg.CurseForgeAPIKey, a.logf)
		still := collectManual(inst, a.logf)
		return M{"found": before - len(still), "missing": len(still)}, nil
	})

	a.handle(mux, "/api/play", func(r *http.Request, b M) (any, error) {
		err := a.play(asS(b["id"]), asS(b["via"]))
		if err != nil && err.Error() == "no_account" {
			return nil, &apiError{"Add an account first.", "no_account"}
		}
		return nil, err
	})
	a.handle(mux, "/api/stop", func(r *http.Request, b M) (any, error) { a.stopGame(); return nil, nil })

	a.handle(mux, "/api/open", func(r *http.Request, b M) (any, error) {
		dir := a.p.Root
		if asS(b["what"]) != "data" {
			inst, err := a.instFrom(r, b)
			if err != nil {
				return nil, err
			}
			dir = inst.GameDir()
			switch asS(b["what"]) {
			case "mods", "logs", "saves", "resourcepacks", "shaderpacks", "screenshots", "crash-reports":
				dir = filepath.Join(dir, asS(b["what"]))
			}
		}
		os.MkdirAll(dir, 0o755)
		return nil, openPath(dir)
	})

	a.handle(mux, "/api/openurl", func(r *http.Request, b M) (any, error) {
		u := asS(b["url"])
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return nil, errors.New("not a web link")
		}
		return nil, openURL(u)
	})

	a.handle(mux, "/api/account/offline", func(r *http.Request, b M) (any, error) {
		acct, err := offlineAccount(asS(b["name"]))
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, x := range a.cfg.Accounts {
			if x.UUID == acct.UUID {
				a.cfg.SelectedAccount = i
				return nil, saveConfig(a.p, a.cfg)
			}
		}
		a.cfg.Accounts = append(a.cfg.Accounts, acct)
		a.cfg.SelectedAccount = len(a.cfg.Accounts) - 1
		return nil, saveConfig(a.p, a.cfg)
	})

	a.handle(mux, "/api/account/ms/start", func(r *http.Request, b M) (any, error) {
		err := a.startMSLogin()
		if errors.Is(err, errNeedClientID) {
			return nil, &apiError{"Microsoft sign-in needs a one-time setup.", "need_client_id"}
		}
		return nil, err
	})

	a.handle(mux, "/api/account/ms/cancel", func(r *http.Request, b M) (any, error) {
		a.mu.Lock()
		if a.msCancel != nil {
			close(a.msCancel)
			a.msCancel = nil
		}
		a.mu.Unlock()
		return nil, nil
	})

	a.handle(mux, "/api/account/select", func(r *http.Request, b M) (any, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if i := asInt(b["index"]); i >= 0 && i < len(a.cfg.Accounts) {
			a.cfg.SelectedAccount = i
		}
		return nil, saveConfig(a.p, a.cfg)
	})

	a.handle(mux, "/api/account/remove", func(r *http.Request, b M) (any, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if i := asInt(b["index"]); i >= 0 && i < len(a.cfg.Accounts) {
			a.cfg.Accounts = append(a.cfg.Accounts[:i:i], a.cfg.Accounts[i+1:]...)
			a.cfg.SelectedAccount = 0
		}
		return nil, saveConfig(a.p, a.cfg)
	})

	a.handle(mux, "/api/settings", func(r *http.Request, b M) (any, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if m := asInt(b["memory_mb"]); m >= 512 {
			a.cfg.MemoryMB = m
		}
		set := func(key string, dst *string) {
			if v, ok := b[key].(string); ok {
				*dst = strings.TrimSpace(v)
			}
		}
		set("java_path", &a.cfg.JavaPath)
		set("jvm_args", &a.cfg.JVMArgs)
		set("curseforge_api_key", &a.cfg.CurseForgeAPIKey)
		set("ms_client_id", &a.cfg.MSClientID)
		set("discord_app_id", &a.cfg.DiscordAppID)
		if v, ok := b["play_via"].(string); ok && (v == "official" || v == "builtin") {
			a.cfg.PlayVia = v
		}
		if v, ok := b["discord_on"].(bool); ok {
			a.cfg.DiscordOff = !v
		}
		if a.cfg.JavaPath != "" && !exists(a.cfg.JavaPath) {
			bad := a.cfg.JavaPath
			a.cfg.JavaPath = ""
			saveConfig(a.p, a.cfg)
			return nil, fmt.Errorf("there is no Java program at %s, so the launcher will keep choosing Java automatically", bad)
		}
		return nil, saveConfig(a.p, a.cfg)
	})

	a.handle(mux, "/api/mcversions", func(r *http.Request, b M) (any, error) {
		a.mu.Lock()
		cached := a.versions
		a.mu.Unlock()
		if cached == nil {
			all, err := listMinecraftVersions()
			if err != nil {
				return M{"versions": []string{}}, nil
			}
			for _, v := range all {
				if asS(asM(v)["type"]) == "release" {
					cached = append(cached, asS(asM(v)["id"]))
				}
			}
			a.mu.Lock()
			a.versions = cached
			a.mu.Unlock()
		}
		return M{"versions": cached}, nil
	})

	a.handle(mux, "/api/loaderversions", func(r *http.Request, b M) (any, error) {
		return M{"versions": loaderVersions(r.URL.Query().Get("mc"), r.URL.Query().Get("loader"))}, nil
	})

	return mux
}

func (a *App) startImport(zipPath, displayName string, removeAfter bool) error {
	if displayName == "" {
		displayName = filepath.Base(zipPath)
	}
	return a.runTask("Importing "+displayName, func() (string, string, error) {
		if removeAfter {
			defer os.Remove(zipPath)
		}
		cfg := a.workConfig()
		inst, err := importAny(a.p, &cfg, zipPath, a.logf, a.progress)
		if err != nil {
			return "", "", err
		}
		if n := len(inst.Manual); n > 0 {
			return fmt.Sprintf("%s is imported, but %d file(s) need a manual download. See the Missing files tab.", inst.Name, n), inst.ID, nil
		}
		return inst.Name + " is ready to play.", inst.ID, nil
	})
}

// ---------------------------------------------------------------- state

type instView struct {
	*Instance
	Mods        int   `json:"mods"`
	ManualCount int   `json:"manual_count"`
	EffMemory   int   `json:"effective_memory_mb"`
	IconV       int64 `json:"icon_v"`
}

func (a *App) apiState(r *http.Request, _ M) (any, error) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	cfg := a.configCopy()
	insts := []instView{}
	for _, inst := range listInstances(a.p) {
		v := instView{Instance: inst, Mods: countMods(inst), ManualCount: len(inst.Manual), EffMemory: instanceMemory(&cfg, inst)}
		if inst.Icon != "" {
			v.IconV = 1
		}
		inst.Manual = nil
		insts = append(insts, v)
	}
	accounts := []M{}
	for _, x := range cfg.Accounts {
		accounts = append(accounts, M{"name": x.Name, "type": x.Type})
	}

	offName, offUUID := officialAccount()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastPoll = time.Now()
	logs := []logLine{}
	for _, l := range a.logs {
		if l.Seq > since {
			logs = append(logs, l)
		}
	}
	if len(logs) > 1500 {
		logs = logs[len(logs)-1500:]
	}
	var ms any
	if a.ms != nil {
		ms = M{"code": a.ms.UserCode, "url": a.ms.URL}
	}
	return M{
		"app": appName, "version": appVersion, "instances": insts, "accounts": accounts,
		"selected_account": cfg.SelectedAccount, "busy": a.busy, "status": a.status,
		"progress": M{"label": a.progLabel, "done": a.progDone, "total": a.progTotal},
		"logs":     logs, "log_seq": a.logSeq, "running": a.gameInst, "notice": a.notice, "ms_login": ms,
		"settings": M{"memory_mb": cfg.MemoryMB, "java_path": cfg.JavaPath, "jvm_args": cfg.JVMArgs,
			"curseforge_api_key": cfg.CurseForgeAPIKey, "ms_client_id": cfg.MSClientID,
			"ms_ready": msClientID(&cfg) != "", "cf_ready": curseForgeKey(&cfg) != "", "data_dir": a.p.Root,
			"play_via": playVia(&cfg), "official_available": officialAvailable(), "official_name": offName, "official_uuid": offUUID,
			"discord_on": !cfg.DiscordOff, "discord_app_id": cfg.DiscordAppID, "discord_ready": discordAppID(&cfg) != "", "discord_live": a.discordLive},
		"ram_options": ramOptions(), "system_ram_mb": systemRAMMB(),
	}, nil
}

// loaderVersions suggests mod loader versions for the New Instance dialog (best effort; typing one in always works).
func loaderVersions(mc, loader string) []string {
	out := []string{}
	if mc == "" {
		return out
	}
	switch loader {
	case "fabric", "quilt":
		u := "https://meta.fabricmc.net/v2/versions/loader/" + url.PathEscape(mc)
		if loader == "quilt" {
			u = "https://meta.quiltmc.org/v3/versions/loader/" + url.PathEscape(mc)
		}
		if v, err := getJSON(u, nil); err == nil {
			for _, e := range asA(v) {
				if s := asS(asM(asM(e)["loader"])["version"]); s != "" && !strings.Contains(s, "beta") {
					out = append(out, s)
				}
			}
		}
	case "forge":
		if v, err := getJSON("https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json", nil); err == nil {
			promos := asM(asM(v)["promos"])
			for _, k := range []string{mc + "-recommended", mc + "-latest"} {
				if s := asS(promos[k]); s != "" && (len(out) == 0 || out[0] != s) {
					out = append(out, s)
				}
			}
		}
	case "neoforge":
		if v, err := getJSON("https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge", nil); err == nil {
			parts := strings.Split(mc, ".")
			if len(parts) >= 2 && parts[0] == "1" {
				minor := "0"
				if len(parts) > 2 {
					minor = parts[2]
				}
				prefix := parts[1] + "." + minor + "."
				all := asA(asM(v)["versions"])
				for i := len(all) - 1; i >= 0; i-- {
					if s := asS(all[i]); strings.HasPrefix(s, prefix) && !strings.Contains(s, "beta") {
						out = append(out, s)
					}
				}
			}
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
