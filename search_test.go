package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pathRT answers by host+path so tests can inspect the query the launcher sent.
type pathRT struct {
	handlers map[string]func(req *http.Request) (int, []byte)
	seen     []string
}

func (m *pathRT) RoundTrip(req *http.Request) (*http.Response, error) {
	m.seen = append(m.seen, req.URL.String())
	status, data := 404, []byte("not mocked")
	if h, ok := m.handlers[req.URL.Host+req.URL.Path]; ok {
		status, data = h(req)
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Request: req, Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func TestSearchAndInstall(t *testing.T) {
	tmp := t.TempDir()
	retryDelay = time.Millisecond
	rt := &pathRT{handlers: map[string]func(*http.Request) (int, []byte){}}
	httpClient = &http.Client{Transport: rt}
	H := rt.handlers
	js := func(v any) func(*http.Request) (int, []byte) {
		return func(*http.Request) (int, []byte) { return 200, mustJSON(v) }
	}
	p := newPaths(filepath.Join(tmp, "data"))
	inst, _ := createInstance(p, "Pack", "1.20.1", "forge", "47.3.0")
	mods := filepath.Join(inst.GameDir(), "mods")
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	// ---- Modrinth
	jarA1, jarA2, jarB := []byte("A1"), []byte("A2"), []byte("B")
	aVersion := "1"
	H["api.modrinth.com/v2/search"] = func(req *http.Request) (int, []byte) {
		f := req.URL.Query().Get("facets")
		if !strings.Contains(f, `"versions:1.20.1"`) || !strings.Contains(f, `"categories:forge"`) || !strings.Contains(f, `"project_type:mod"`) || req.URL.Query().Get("query") != "create" {
			return 400, []byte(f)
		}
		return 200, mustJSON(M{"hits": []any{M{"project_id": "AAA", "slug": "create", "title": "Create", "description": "Gears", "author": "simi", "icon_url": "https://cdn/i.png", "downloads": 1234567, "project_type": "mod"}}})
	}
	H["api.modrinth.com/v2/project/AAA"] = js(M{"title": "Create", "description": "Gears", "slug": "create", "icon_url": "https://cdn/i.png", "project_type": "mod"})
	H["api.modrinth.com/v2/project/BBB"] = js(M{"title": "Flywheel", "slug": "flywheel", "project_type": "mod"})
	H["api.modrinth.com/v2/project/AAA/version"] = func(req *http.Request) (int, []byte) {
		if req.URL.Query().Get("game_versions") != `["1.20.1"]` || req.URL.Query().Get("loaders") != `["forge"]` {
			return 400, nil
		}
		data, name := jarA1, "create-1.jar"
		if aVersion == "2" {
			data, name = jarA2, "create-2.jar"
		}
		return 200, mustJSON([]any{
			M{"version_type": "beta", "files": []any{M{"url": "https://cdn.modrinth.com/beta.jar", "filename": "create-beta.jar", "primary": true}}},
			M{"version_type": "release", "files": []any{M{"url": "https://cdn.modrinth.com/extra.jar", "filename": "sources.jar", "primary": false},
				M{"url": "https://cdn.modrinth.com/" + name, "filename": name, "primary": true, "hashes": M{"sha1": sum(data)}}},
				"dependencies": []any{M{"project_id": "BBB", "dependency_type": "required"}, M{"project_id": "CCC", "dependency_type": "optional"}}}})
	}
	H["api.modrinth.com/v2/project/BBB/version"] = js([]any{M{"version_type": "release", "files": []any{M{"url": "https://cdn.modrinth.com/fly.jar", "filename": "flywheel.jar", "primary": true}}}})
	H["cdn.modrinth.com/create-1.jar"] = func(*http.Request) (int, []byte) { return 200, jarA1 }
	H["cdn.modrinth.com/create-2.jar"] = func(*http.Request) (int, []byte) { return 200, jarA2 }
	H["cdn.modrinth.com/fly.jar"] = func(*http.Request) (int, []byte) { return 200, jarB }

	hits, err := searchModrinth(inst, "create", "mod")
	if err != nil || len(hits) != 1 || hits[0].Name != "Create" || hits[0].Installed || hits[0].URL != "https://modrinth.com/mod/create" {
		t.Fatalf("modrinth search: %v %+v", err, hits)
	}
	seen := map[string]bool{}
	if err := installModrinth(inst, "AAA", "mod", logf, seen, 0); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(mods, "create-1.jar")) || !exists(filepath.Join(mods, "flywheel.jar")) || exists(filepath.Join(mods, "create-beta.jar")) || len(seen) != 2 {
		t.Fatalf("modrinth install: release file and required add-on expected; logs %v", logs)
	}
	if hits, _ = searchModrinth(inst, "create", "mod"); !hits[0].Installed {
		t.Fatal("installed project not marked in results")
	}
	list := scanMods(inst)
	if len(list) != 2 || list[0].Name != "Create" || list[0].Logo != "https://cdn/i.png" {
		t.Fatalf("mod list after install: %+v", list)
	}
	aVersion = "2"
	if err := installModrinth(inst, "AAA", "mod", logf, map[string]bool{}, 0); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(mods, "create-2.jar")) || exists(filepath.Join(mods, "create-1.jar")) {
		t.Fatal("installing a newer version should replace the old file")
	}
	vanilla, _ := createInstance(p, "Plain", "1.20.1", "vanilla", "")
	if checkKind(vanilla, "mod") == nil || checkKind(vanilla, "resourcepack") != nil {
		t.Fatal("vanilla packs take resource packs but not mods")
	}

	// ---- CurseForge
	var ae *apiError
	if _, err := searchCurseForge(inst, "jei", "mod", ""); !errors.As(err, &ae) || ae.code != "need_cf_key" {
		t.Fatalf("missing key should be explained: %v", err)
	}
	jei, lib := []byte("JEI"), []byte("LIB")
	H["api.curseforge.com/v1/mods/search"] = func(req *http.Request) (int, []byte) {
		q := req.URL.Query()
		if req.Header.Get("x-api-key") != "KEY" {
			return 403, nil
		}
		if q.Get("gameId") != "432" || q.Get("classId") != "6" || q.Get("gameVersion") != "1.20.1" || q.Get("modLoaderType") != "1" || q.Get("searchFilter") != "jei" {
			return 400, nil
		}
		return 200, mustJSON(M{"data": []any{M{"id": 238222, "name": "Just Enough Items", "summary": "Recipes", "downloadCount": 99, "logo": M{"thumbnailUrl": "https://cdn/j.png"},
			"links": M{"websiteUrl": "https://www.curseforge.com/minecraft/mc-mods/jei"}, "authors": []any{M{"name": "mezz"}}}}})
	}
	H["api.curseforge.com/v1/mods/238222"] = js(M{"data": M{"id": 238222, "name": "Just Enough Items", "classId": 6, "links": M{"websiteUrl": "https://www.curseforge.com/minecraft/mc-mods/jei"}}})
	H["api.curseforge.com/v1/mods/238222/files"] = js(M{"data": []any{
		M{"id": 1, "fileName": "jei-old.jar", "releaseType": 1, "fileDate": "2024-01-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/jei-old.jar"},
		M{"id": 3, "fileName": "jei-alpha.jar", "releaseType": 3, "fileDate": "2025-06-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/jei-alpha.jar"},
		M{"id": 2, "fileName": "jei-new.jar", "releaseType": 1, "fileDate": "2025-01-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/jei-new.jar",
			"hashes": []any{M{"algo": 1, "value": sum(jei)}}, "dependencies": []any{M{"modId": 555, "relationType": 3}, M{"modId": 777, "relationType": 2}}}}})
	H["api.curseforge.com/v1/mods/555"] = js(M{"data": M{"id": 555, "name": "Lib", "classId": 6}})
	H["api.curseforge.com/v1/mods/555/files"] = js(M{"data": []any{M{"id": 9, "fileName": "lib.jar", "releaseType": 1, "fileDate": "2025-01-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/lib.jar"}}})
	H["api.curseforge.com/v1/mods/666"] = js(M{"data": M{"id": 666, "name": "Blocked Mod", "classId": 6, "links": M{"websiteUrl": "https://www.curseforge.com/minecraft/mc-mods/blocked"}}})
	H["api.curseforge.com/v1/mods/666/files"] = js(M{"data": []any{M{"id": 8, "fileName": "blocked.jar", "releaseType": 1, "downloadUrl": nil}}})
	H["edge.forgecdn.net/jei-new.jar"] = func(*http.Request) (int, []byte) { return 200, jei }
	H["edge.forgecdn.net/lib.jar"] = func(*http.Request) (int, []byte) { return 200, lib }

	hits, err = searchCurseForge(inst, "jei", "mod", "KEY")
	if err != nil || len(hits) != 1 || hits[0].ID != "238222" || hits[0].Author != "mezz" {
		t.Fatalf("curseforge search: %v %+v", err, hits)
	}
	seen = map[string]bool{}
	if err := installCurseForge(inst, 238222, "mod", "KEY", logf, seen, 0); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(mods, "jei-new.jar")) || !exists(filepath.Join(mods, "lib.jar")) || exists(filepath.Join(mods, "jei-alpha.jar")) || len(seen) != 2 {
		t.Fatalf("curseforge install: newest release and required add-on expected; logs %v", logs)
	}
	if hits, _ = searchCurseForge(inst, "jei", "mod", "KEY"); !hits[0].Installed {
		t.Fatal("installed CurseForge project not marked")
	}
	err = installCurseForge(inst, 666, "mod", "KEY", logf, map[string]bool{}, 0)
	if !errors.As(err, &ae) || !strings.Contains(ae.msg, "only be downloaded from the CurseForge site") || exists(filepath.Join(mods, "blocked.jar")) {
		t.Fatalf("author-restricted file should be refused with an explanation: %v", err)
	}
	if len(scanMods(inst)) != 4 {
		t.Fatalf("expected 4 mods, got %+v", scanMods(inst))
	}
}

func TestModpackSearchAndInstall(t *testing.T) {
	tmp := t.TempDir()
	retryDelay = time.Millisecond
	prepareAfterImport = false
	defer func() { prepareAfterImport = true }()
	rt := &pathRT{handlers: map[string]func(*http.Request) (int, []byte){}}
	httpClient = &http.Client{Transport: rt}
	H := rt.handlers
	js := func(v any) func(*http.Request) (int, []byte) {
		return func(*http.Request) (int, []byte) { return 200, mustJSON(v) }
	}
	raw := func(b []byte) func(*http.Request) (int, []byte) {
		return func(*http.Request) (int, []byte) { return 200, b }
	}
	p := newPaths(filepath.Join(tmp, "data"))
	cfg := &Config{CurseForgeAPIKey: "KEY", MemoryMB: 4096}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	png := []byte("\x89PNG\r\n\x1a\nicon")

	// ---- Modrinth pack (.mrpack)
	sodium := []byte("SODIUM")
	mrpack := zipBytes(map[string][]byte{
		"modrinth.index.json": mustJSON(M{"formatVersion": 1, "game": "minecraft", "versionId": "5.0", "name": "Fab Pack",
			"dependencies": M{"minecraft": "1.21.1", "fabric-loader": "0.16.5"},
			"files": []any{
				M{"path": "mods/sodium.jar", "hashes": M{"sha1": sum(sodium)}, "downloads": []any{"https://cdn.modrinth.com/data/sodium.jar"}},
				M{"path": "mods/server-only.jar", "env": M{"client": "unsupported"}, "downloads": []any{"https://cdn.modrinth.com/data/server.jar"}},
				M{"path": "../../escape.jar", "downloads": []any{"https://cdn.modrinth.com/data/sodium.jar"}},
				M{"path": "mods/plain-http.jar", "downloads": []any{"http://insecure.example/x.jar"}}}}),
		"overrides/config/a.toml": []byte("a=1"), "client-overrides/options.txt": []byte("fov:90"), "overrides/../../evil.txt": []byte("no")})
	H["api.modrinth.com/v2/search"] = func(req *http.Request) (int, []byte) {
		if !strings.Contains(req.URL.Query().Get("facets"), "project_type:modpack") {
			return 400, nil
		}
		return 200, mustJSON(M{"hits": []any{M{"project_id": "PPP", "slug": "fab-pack", "title": "Fabulous Pack", "description": "Fast", "author": "x", "downloads": 5000}}})
	}
	H["api.modrinth.com/v2/project/PPP"] = js(M{"title": "Fabulous Pack", "icon_url": "https://cdn.modrinth.com/icon.png"})
	H["api.modrinth.com/v2/project/PPP/version"] = js([]any{M{"version_type": "release", "version_number": "5.0",
		"files": []any{M{"url": "https://cdn.modrinth.com/pack.mrpack", "filename": "pack.mrpack", "primary": true, "hashes": M{"sha1": sum(mrpack)}}}}})
	H["cdn.modrinth.com/pack.mrpack"], H["cdn.modrinth.com/data/sodium.jar"], H["cdn.modrinth.com/icon.png"] = raw(mrpack), raw(sodium), raw(png)

	hits, err := searchPacksModrinth("fab")
	if err != nil || len(hits) != 1 || hits[0].URL != "https://modrinth.com/modpack/fab-pack" {
		t.Fatalf("modrinth pack search: %v %+v", err, hits)
	}
	inst, err := installPackModrinth(p, cfg, "PPP", logf, nil)
	if err != nil {
		t.Fatalf("modrinth pack install: %v\n%v", err, logs)
	}
	g := inst.GameDir()
	if inst.Name != "Fabulous Pack" || inst.MCVersion != "1.21.1" || inst.Loader != "fabric" || inst.LoaderVersion != "0.16.5" || inst.Pack.Version != "5.0" {
		t.Fatalf("instance from mrpack: %+v", inst)
	}
	if !exists(filepath.Join(g, "mods", "sodium.jar")) || !exists(filepath.Join(g, "config", "a.toml")) || !exists(filepath.Join(g, "options.txt")) {
		t.Fatal("mrpack files and both override folders should be in place")
	}
	if exists(filepath.Join(g, "mods", "server-only.jar")) || exists(filepath.Join(g, "mods", "plain-http.jar")) || exists(filepath.Join(tmp, "escape.jar")) ||
		exists(filepath.Join(p.Instances, "escape.jar")) || exists(filepath.Join(inst.Dir, "evil.txt")) || exists(filepath.Join(p.Instances, "evil.txt")) {
		t.Fatal("server-only, non-https and path-escaping entries must be skipped")
	}
	if inst.Icon == "" || !exists(filepath.Join(inst.Dir, inst.Icon)) {
		t.Fatal("pack picture should be taken from the site")
	}

	// ---- CurseForge pack
	cfZip := zipBytes(map[string][]byte{"manifest.json": mustJSON(M{"minecraft": M{"version": "1.20.1", "modLoaders": []any{M{"id": "forge-47.3.0", "primary": true}}},
		"manifestType": "minecraftModpack", "name": "All The Monkeys", "version": "1.0", "author": "atm", "overrides": "overrides", "files": []any{}}),
		"overrides/config/b.cfg": []byte("b")})
	H["api.curseforge.com/v1/mods/search"] = func(req *http.Request) (int, []byte) {
		if req.URL.Query().Get("classId") != "4471" || req.Header.Get("x-api-key") != "KEY" {
			return 400, nil
		}
		return 200, mustJSON(M{"data": []any{M{"id": 900, "name": "All The Monkeys", "downloadCount": 7, "links": M{"websiteUrl": "https://www.curseforge.com/minecraft/modpacks/atm"}}}})
	}
	H["api.curseforge.com/v1/mods/900"] = js(M{"data": M{"id": 900, "name": "All The Monkeys", "mainFileId": 12, "logo": M{"thumbnailUrl": "https://media.forgecdn.net/logo.png"}}})
	H["api.curseforge.com/v1/mods/900/files"] = js(M{"data": []any{
		M{"id": 13, "fileName": "server.zip", "isServerPack": true, "releaseType": 1, "fileDate": "2026-01-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/server.zip"},
		M{"id": 12, "displayName": "ATM 1.0", "fileName": "atm.zip", "releaseType": 1, "fileDate": "2025-01-01T00:00:00Z", "downloadUrl": "https://edge.forgecdn.net/atm.zip"}}})
	H["edge.forgecdn.net/atm.zip"], H["media.forgecdn.net/logo.png"] = raw(cfZip), raw(png)
	if hits, err = searchPacksCurseForge("monkeys", "KEY"); err != nil || len(hits) != 1 || hits[0].ID != "900" {
		t.Fatalf("curseforge pack search: %v %+v", err, hits)
	}
	inst2, err := installPackCurseForge(p, cfg, 900, logf, nil)
	if err != nil {
		t.Fatalf("curseforge pack install: %v\n%v", err, logs)
	}
	if inst2.Name != "All The Monkeys" || inst2.Loader != "forge" || !exists(filepath.Join(inst2.GameDir(), "config", "b.cfg")) || inst2.Icon == "" {
		t.Fatalf("instance from CurseForge pack: %+v", inst2)
	}
	if _, err := searchPacksCurseForge("x", ""); err == nil {
		t.Fatal("CurseForge pack search without a key should explain the key")
	}
}

// Packs that were extracted and zipped again (7-Zip, WinRAR, Windows) must still import.
func TestRezippedPacks(t *testing.T) {
	tmp := t.TempDir()
	prepareAfterImport = false
	defer func() { prepareAfterImport = true }()
	p := newPaths(filepath.Join(tmp, "data"))
	cfg := &Config{MemoryMB: 4096}
	logf := func(string, ...any) {}
	manifest := mustJSON(M{"minecraft": M{"version": "1.20.1", "modLoaders": []any{M{"id": "forge-47.3.0", "primary": true}}},
		"manifestType": "minecraftModpack", "name": "Nested", "version": "1", "overrides": "overrides", "files": []any{}})
	cases := map[string]map[string][]byte{
		"folder.zip":    {"My Pack/manifest.json": manifest, "My Pack/modlist.html": []byte(`<a href="https://x">One</a>`), "My Pack/overrides/config/a.cfg": []byte("a"), "My Pack/overrides/mods/manifest.json": []byte("{}")},
		"backslash.zip": {`My Pack\manifest.json`: manifest, `My Pack\overrides\config\a.cfg`: []byte("a")},
		"plain.zip":     {"manifest.json": manifest, "overrides/config/a.cfg": []byte("a")},
	}
	for name, files := range cases {
		zp := filepath.Join(tmp, name)
		writeFileAtomic(zp, zipBytes(files))
		inst, err := importAny(p, cfg, zp, logf, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if inst.Loader != "forge" || !exists(filepath.Join(inst.GameDir(), "config", "a.cfg")) || exists(filepath.Join(inst.GameDir(), "My Pack")) {
			t.Fatalf("%s: files not placed correctly", name)
		}
	}
	mr := filepath.Join(tmp, "nested.mrpack")
	writeFileAtomic(mr, zipBytes(map[string][]byte{"Pack/modrinth.index.json": mustJSON(M{"formatVersion": 1, "game": "minecraft", "name": "MR",
		"dependencies": M{"minecraft": "1.21.1", "fabric-loader": "0.16.5"}, "files": []any{}}), "Pack/overrides/options.txt": []byte("x")}))
	inst, err := importAny(p, cfg, mr, logf, nil)
	if err != nil || inst.Loader != "fabric" || !exists(filepath.Join(inst.GameDir(), "options.txt")) {
		t.Fatalf("nested mrpack: %v", err)
	}
	junk := filepath.Join(tmp, "junk.zip")
	writeFileAtomic(junk, zipBytes(map[string][]byte{"a/b/c/d/manifest.json": manifest, "readme.txt": []byte("x")}))
	if _, err := importAny(p, cfg, junk, logf, nil); err == nil {
		t.Fatal("a zip with no pack near its top level should be refused")
	}
}
