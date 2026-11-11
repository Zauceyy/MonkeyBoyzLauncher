package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Modpacks: importing Modrinth packs (.mrpack), and finding/installing whole packs from
// Modrinth and CurseForge.

var prepareAfterImport = true // tests switch this off to skip installing the game itself

type mrIndex struct {
	FormatVersion int    `json:"formatVersion"`
	Game          string `json:"game"`
	VersionID     string `json:"versionId"`
	Name          string `json:"name"`
	Files         []struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		Env       map[string]string `json:"env"`
		Downloads []string          `json:"downloads"`
	} `json:"files"`
	Dependencies map[string]string `json:"dependencies"`
}

// zipReadNorm reads an entry by name, treating Windows-style backslashes in the archive as slashes.
func zipReadNorm(z *zip.Reader, name string) ([]byte, error) {
	for _, f := range z.File {
		if strings.ReplaceAll(f.Name, `\`, "/") == name {
			return zipRead(z, f.Name)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

// packRoot finds where the pack really starts inside the archive. Packs that were extracted and
// zipped again (with 7-Zip, WinRAR or Windows) often end up inside an extra top-level folder.
// kind is "curseforge", "modrinth" or "" when the archive is not a pack.
func packRoot(zipPath string) (root, kind string) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", ""
	}
	defer zr.Close()
	best := 99
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		base := path.Base(name)
		k := map[string]string{"manifest.json": "curseforge", "modrinth.index.json": "modrinth"}[base]
		depth := strings.Count(name, "/")
		if k == "" || depth > 2 || depth >= best || strings.Contains("/"+name, "/overrides/") {
			continue
		}
		best, kind, root = depth, k, strings.TrimSuffix(name, base)
	}
	return root, kind
}

// importAny imports a CurseForge export (.zip) or a Modrinth pack (.mrpack), whichever the file is.
func importAny(p *Paths, cfg *Config, zipPath string, logf Logger, prog Progress) (*Instance, error) {
	if _, kind := packRoot(zipPath); kind == "modrinth" {
		return importMrpack(p, cfg, zipPath, logf, prog)
	}
	return importPack(p, cfg, zipPath, logf, prog)
}

func safeRel(rel string) (string, bool) {
	rel = path.Clean(strings.ReplaceAll(rel, `\`, "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", false
	}
	return rel, true
}

func importMrpack(p *Paths, cfg *Config, zipPath string, logf Logger, prog Progress) (*Instance, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errors.New("that file is not a valid pack archive")
	}
	root, _ := packRoot(zipPath)
	raw, err := zipReadNorm(&zr.Reader, root+"modrinth.index.json")
	zr.Close()
	if err != nil {
		return nil, err
	}
	var idx mrIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("the pack's index could not be read: %v", err)
	}
	mc := idx.Dependencies["minecraft"]
	if mc == "" || (idx.Game != "" && idx.Game != "minecraft") {
		return nil, errors.New("this Modrinth pack is not for Minecraft Java")
	}
	loader, lv := "vanilla", ""
	for key, name := range map[string]string{"forge": "forge", "neoforge": "neoforge", "fabric-loader": "fabric", "quilt-loader": "quilt"} {
		if v := idx.Dependencies[key]; v != "" {
			loader, lv = name, v
		}
	}
	if idx.Name == "" {
		idx.Name = strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	}
	logf("Importing %s %s: Minecraft %s, %s %s, %d files", idx.Name, idx.VersionID, mc, loader, lv, len(idx.Files))
	inst, err := createInstance(p, idx.Name, mc, loader, lv)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Instance, error) {
		os.RemoveAll(inst.Dir)
		return nil, err
	}
	inst.Pack = &PackInfo{idx.Name, idx.VersionID, "", len(idx.Files)}
	n := 0
	for _, prefix := range []string{"overrides", "client-overrides"} {
		c, err := extractPrefix(zipPath, root+prefix, inst.GameDir())
		if err != nil {
			return fail(err)
		}
		n += c
	}
	logf("Copied %d bundled files (configs, scripts and so on).", n)

	var jobs []dlJob
	var metas []ModMeta
	for _, f := range idx.Files {
		if f.Env["client"] == "unsupported" {
			continue
		}
		rel, ok := safeRel(f.Path)
		if !ok {
			continue
		}
		var urls []string
		for _, u := range f.Downloads {
			if strings.HasPrefix(u, "https://") {
				urls = append(urls, u)
			}
		}
		if len(urls) == 0 {
			continue
		}
		jobs = append(jobs, dlJob{urls, filepath.Join(inst.GameDir(), filepath.FromSlash(rel)), f.Hashes["sha1"]})
		if dir := path.Dir(rel); dir == "mods" || dir == "resourcepacks" || dir == "shaderpacks" {
			metas = append(metas, ModMeta{Source: "modrinth", FileName: path.Base(rel), Folder: dir})
		}
	}
	if errs := downloadMany(jobs, "Downloading mods", prog, 8); len(errs) > 0 {
		for _, e := range firstN(errs, 10) {
			logf("  Download failed: %s", e)
		}
		return fail(fmt.Errorf("%d downloads failed. Check your internet connection and try again", len(errs)))
	}
	writeJSON(filepath.Join(inst.Dir, "modlist.json"), metas)
	if err := inst.Save(); err != nil {
		return fail(err)
	}
	if prepareAfterImport {
		if _, _, err := prepareInstance(p, cfg, inst, logf, prog); err != nil {
			return fail(err)
		}
	}
	logf("Import finished: %s", inst.Name)
	return inst, nil
}

// setIconFromURL gives a freshly installed pack the picture it has on the site (best effort).
func setIconFromURL(inst *Instance, u string) {
	if !strings.HasPrefix(u, "https://") {
		return
	}
	data, err := httpBytes("GET", u, nil, nil)
	if err != nil || len(data) > 4<<20 {
		return
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[http.DetectContentType(data)]
	if ext == "" {
		return
	}
	inst.Icon = fmt.Sprintf("icon-%d%s", time.Now().Unix(), ext)
	if os.WriteFile(filepath.Join(inst.Dir, inst.Icon), data, 0o644) == nil {
		inst.Save()
	}
}

// ---------------------------------------------------------------- searching for packs

func searchPacksModrinth(q string) ([]SearchHit, error) {
	u := modrinthAPI + "/search?limit=30&index=" + map[bool]string{true: "downloads", false: "relevance"}[q == ""] +
		"&query=" + url.QueryEscape(q) + "&facets=" + url.QueryEscape(`[["project_type:modpack"]]`)
	raw, err := getJSON(u, nil)
	if err != nil {
		return nil, fmt.Errorf("Modrinth search failed: %v", err)
	}
	hits := []SearchHit{}
	for _, h := range asA(asM(raw)["hits"]) {
		m := asM(h)
		hits = append(hits, SearchHit{"modrinth", asS(m["project_id"]), asS(m["title"]), asS(m["description"]), asS(m["author"]),
			asS(m["icon_url"]), "https://modrinth.com/modpack/" + asS(m["slug"]), asInt(m["downloads"]), false})
	}
	return hits, nil
}

func searchPacksCurseForge(q, key string) ([]SearchHit, error) {
	headers, err := cfHeaders(key)
	if err != nil {
		return nil, err
	}
	raw, err := getJSON(cfAPI+"/mods/search?gameId=432&classId=4471&pageSize=30&sortField=2&sortOrder=desc&searchFilter="+url.QueryEscape(q), headers)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			return nil, errors.New("CurseForge rejected the API key. Check it in Settings")
		}
		return nil, fmt.Errorf("CurseForge search failed: %v", err)
	}
	hits := []SearchHit{}
	for _, x := range asA(asM(raw)["data"]) {
		m := asM(x)
		author := ""
		if a := asA(m["authors"]); len(a) > 0 {
			author = asS(asM(a[0])["name"])
		}
		hits = append(hits, SearchHit{"curseforge", strconv.Itoa(asInt(m["id"])), asS(m["name"]), asS(m["summary"]), author,
			asS(asM(m["logo"])["thumbnailUrl"]), asS(asM(m["links"])["websiteUrl"]), asInt(m["downloadCount"]), false})
	}
	return hits, nil
}

// ---------------------------------------------------------------- installing a pack from a site

func installPackModrinth(p *Paths, cfg *Config, id string, logf Logger, prog Progress) (*Instance, error) {
	for _, c := range id {
		if !(c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return nil, errors.New("bad project id")
		}
	}
	praw, err := getJSON(modrinthAPI+"/project/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("could not look that pack up on Modrinth: %v", err)
	}
	vraw, err := getJSON(modrinthAPI+"/project/"+id+"/version", nil)
	if err != nil {
		return nil, fmt.Errorf("could not list the pack's versions: %v", err)
	}
	versions := asA(vraw)
	if len(versions) == 0 {
		return nil, errors.New("this pack has no downloadable version")
	}
	pick := asM(versions[0])
	for _, v := range versions {
		if asS(asM(v)["version_type"]) == "release" {
			pick = asM(v)
			break
		}
	}
	var file M
	for _, f := range asA(pick["files"]) {
		if file == nil || asM(f)["primary"] == true {
			file = asM(f)
		}
	}
	if file == nil || !strings.HasPrefix(asS(file["url"]), "https://") {
		return nil, errors.New("this pack has no downloadable file")
	}
	logf("Downloading %s %s from Modrinth...", asS(asM(praw)["title"]), asS(pick["version_number"]))
	tmp := filepath.Join(p.Temp, fmt.Sprintf("pack-%d.mrpack", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := download([]string{asS(file["url"])}, tmp, asS(asM(file["hashes"])["sha1"]), nil); err != nil {
		return nil, fmt.Errorf("could not download the pack: %v", err)
	}
	inst, err := importAny(p, cfg, tmp, logf, prog)
	if err != nil {
		return nil, err
	}
	if t := asS(asM(praw)["title"]); t != "" {
		inst.Name = t
		inst.Save()
	}
	setIconFromURL(inst, asS(asM(praw)["icon_url"]))
	return inst, nil
}

func installPackCurseForge(p *Paths, cfg *Config, id int, logf Logger, prog Progress) (*Instance, error) {
	headers, err := cfHeaders(cfg.CurseForgeAPIKey)
	if err != nil {
		return nil, err
	}
	mraw, err := getJSON(fmt.Sprintf("%s/mods/%d", cfAPI, id), headers)
	if err != nil {
		return nil, fmt.Errorf("could not look that pack up on CurseForge: %v", err)
	}
	mod := asM(asM(mraw)["data"])
	fraw, err := getJSON(fmt.Sprintf("%s/mods/%d/files?pageSize=50", cfAPI, id), headers)
	if err != nil {
		return nil, fmt.Errorf("could not list the pack's files: %v", err)
	}
	var files []M
	for _, f := range asA(asM(fraw)["data"]) {
		if fm := asM(f); fm["isServerPack"] != true {
			files = append(files, fm)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("this pack has no downloadable file")
	}
	main := asInt(mod["mainFileId"])
	sort.SliceStable(files, func(a, b int) bool { // the author's featured file, then full releases, then newest
		ma, mb := asInt(files[a]["id"]) == main, asInt(files[b]["id"]) == main
		if ma != mb {
			return ma
		}
		ra, rb := asInt(files[a]["releaseType"]) == 1, asInt(files[b]["releaseType"]) == 1
		if ra != rb {
			return ra
		}
		return asS(files[a]["fileDate"]) > asS(files[b]["fileDate"])
	})
	file := files[0]
	if asS(file["downloadUrl"]) == "" {
		return nil, &apiError{asS(mod["name"]) + " can only be downloaded from the CurseForge site. Download the pack zip there and drop it onto this window.", "blocked"}
	}
	logf("Downloading %s (%s) from CurseForge...", asS(mod["name"]), asS(file["displayName"]))
	tmp := filepath.Join(p.Temp, fmt.Sprintf("pack-%d.zip", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := download([]string{strings.ReplaceAll(asS(file["downloadUrl"]), " ", "%20")}, tmp, "", nil); err != nil {
		return nil, fmt.Errorf("could not download the pack: %v", err)
	}
	inst, err := importAny(p, cfg, tmp, logf, prog)
	if err != nil {
		return nil, err
	}
	setIconFromURL(inst, asS(asM(mod["logo"])["thumbnailUrl"]))
	return inst, nil
}
