package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Built-in search: find mods, resource packs and shaders on Modrinth and CurseForge that fit a
// pack's Minecraft version and mod loader, and install them (with required dependencies).

var modrinthAPI = "https://api.modrinth.com/v2"

type SearchHit struct {
	Source    string `json:"source"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
	Author    string `json:"author"`
	Icon      string `json:"icon"`
	URL       string `json:"url"`
	Downloads int    `json:"downloads"`
	Installed bool   `json:"installed"`
}

var kindFolders = map[string]string{"mod": "mods", "resourcepack": "resourcepacks", "shader": "shaderpacks"}

// loaderChoices lists the loaders whose mods run on this pack, best match first.
func loaderChoices(inst *Instance) []string {
	switch inst.Loader {
	case "quilt":
		return []string{"quilt", "fabric"}
	case "neoforge":
		if inst.MCVersion == "1.20.1" {
			return []string{"neoforge", "forge"}
		}
		return []string{"neoforge"}
	case "forge", "fabric":
		return []string{inst.Loader}
	}
	return nil
}

func checkKind(inst *Instance, kind string) error {
	if kindFolders[kind] == "" {
		return errors.New("unknown content type")
	}
	if kind == "mod" && len(loaderChoices(inst)) == 0 {
		return errors.New("this pack is plain Minecraft with no mod loader, so mods cannot be added. Resource packs and shaders still work")
	}
	return nil
}

func jsonList(items []string) string {
	return `["` + strings.Join(items, `","`) + `"]`
}

// installedRefs returns "source:id" for every project already in the pack whose file is still there.
func installedRefs(inst *Instance) map[string]bool {
	var metas []ModMeta
	readJSONInto(filepath.Join(inst.Dir, "modlist.json"), &metas)
	out := map[string]bool{}
	for _, m := range metas {
		f := filepath.Join(inst.GameDir(), m.Folder, m.FileName)
		if m.FileName == "" || !(exists(f) || exists(f+".disabled")) {
			continue
		}
		if m.Source == "modrinth" {
			out["modrinth:"+m.Ref] = true
		} else if m.Project != 0 {
			out["curseforge:"+strconv.Itoa(m.Project)] = true
		}
	}
	return out
}

// recordMod remembers an installed file in the pack's index and removes an older file of the same project.
func recordMod(inst *Instance, meta ModMeta) {
	path := filepath.Join(inst.Dir, "modlist.json")
	var metas []ModMeta
	readJSONInto(path, &metas)
	kept := metas[:0]
	for _, m := range metas {
		same := (meta.Source == "modrinth" && m.Source == "modrinth" && m.Ref == meta.Ref) ||
			(meta.Source != "modrinth" && m.Source != "modrinth" && m.Project != 0 && m.Project == meta.Project)
		if same && m.FileName != meta.FileName && validFileName(m.FileName) {
			old := filepath.Join(inst.GameDir(), m.Folder, m.FileName)
			os.Remove(old)
			os.Remove(old + ".disabled")
		}
		if !same && m.FileName != meta.FileName {
			kept = append(kept, m)
		}
	}
	writeJSON(path, append(kept, meta))
}

// ---------------------------------------------------------------- Modrinth

func searchModrinth(inst *Instance, q, kind string) ([]SearchHit, error) {
	facets := []string{`["project_type:` + kind + `"]`, `["versions:` + inst.MCVersion + `"]`}
	if kind == "mod" {
		var cats []string
		for _, l := range loaderChoices(inst) {
			cats = append(cats, "categories:"+l)
		}
		facets = append(facets, jsonList(cats))
	}
	u := modrinthAPI + "/search?limit=30&index=" + map[bool]string{true: "downloads", false: "relevance"}[q == ""] +
		"&query=" + url.QueryEscape(q) + "&facets=" + url.QueryEscape("["+strings.Join(facets, ",")+"]")
	raw, err := getJSON(u, nil)
	if err != nil {
		return nil, fmt.Errorf("Modrinth search failed: %v", err)
	}
	have := installedRefs(inst)
	hits := []SearchHit{}
	for _, h := range asA(asM(raw)["hits"]) {
		m := asM(h)
		id := asS(m["project_id"])
		hits = append(hits, SearchHit{"modrinth", id, asS(m["title"]), asS(m["description"]), asS(m["author"]), asS(m["icon_url"]),
			"https://modrinth.com/" + asS(m["project_type"]) + "/" + asS(m["slug"]), asInt(m["downloads"]), have["modrinth:"+id]})
	}
	return hits, nil
}

func installModrinth(inst *Instance, id, kind string, logf Logger, seen map[string]bool, depth int) error {
	if seen["modrinth:"+id] || depth > 4 {
		return nil
	}
	seen["modrinth:"+id] = true
	for _, c := range id {
		if !(c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return errors.New("bad project id")
		}
	}
	praw, err := getJSON(modrinthAPI+"/project/"+id, nil)
	if err != nil {
		return fmt.Errorf("could not look that project up on Modrinth: %v", err)
	}
	proj := asM(praw)
	if depth > 0 { // a dependency: work out what kind of thing it is
		kind = asS(proj["project_type"])
		if kindFolders[kind] == "" {
			return nil
		}
	}
	u := modrinthAPI + "/project/" + id + "/version?game_versions=" + url.QueryEscape(jsonList([]string{inst.MCVersion}))
	if kind == "mod" {
		u += "&loaders=" + url.QueryEscape(jsonList(loaderChoices(inst)))
	}
	vraw, err := getJSON(u, nil)
	if err != nil {
		return fmt.Errorf("could not list versions of %s: %v", asS(proj["title"]), err)
	}
	versions := asA(vraw)
	if len(versions) == 0 {
		return fmt.Errorf("%s has no version for Minecraft %s on %s", asS(proj["title"]), inst.MCVersion, inst.Loader)
	}
	pick := asM(versions[0])
	for _, v := range versions { // prefer a full release, and the pack's own loader
		vm := asM(v)
		if asS(vm["version_type"]) == "release" {
			pick = vm
			break
		}
	}
	var file M
	for _, f := range asA(pick["files"]) {
		if file == nil || asM(f)["primary"] == true {
			file = asM(f)
		}
	}
	name := filepath.Base(asS(file["filename"]))
	if file == nil || !validFileName(name) {
		return fmt.Errorf("%s has no downloadable file", asS(proj["title"]))
	}
	folder := kindFolders[kind]
	dest := filepath.Join(inst.GameDir(), folder, name)
	if !exists(dest) && !exists(dest+".disabled") {
		if err := download([]string{asS(file["url"])}, dest, asS(asM(file["hashes"])["sha1"]), nil); err != nil {
			return fmt.Errorf("could not download %s: %v", name, err)
		}
	}
	recordMod(inst, ModMeta{Source: "modrinth", Ref: id, Name: asS(proj["title"]), FileName: name, Folder: folder,
		Summary: asS(proj["description"]), Logo: asS(proj["icon_url"]), URL: "https://modrinth.com/" + kind + "/" + asS(proj["slug"])})
	logf("Installed %s (%s).", asS(proj["title"]), name)
	for _, d := range asA(pick["dependencies"]) {
		dm := asM(d)
		if dep := asS(dm["project_id"]); dep != "" && asS(dm["dependency_type"]) == "required" {
			if err := installModrinth(inst, dep, kind, logf, seen, depth+1); err != nil {
				logf("  A required add-on could not be installed: %v", err)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- CurseForge

var cfLoaderTypes = map[string]int{"forge": 1, "fabric": 4, "quilt": 5, "neoforge": 6}
var cfKindClass = map[string]int{"mod": 6, "resourcepack": 12, "shader": 6552}

func cfHeaders(key string) (map[string]string, error) {
	if strings.TrimSpace(key) == "" {
		return nil, &apiError{"CurseForge search needs the free API key from console.curseforge.com. Paste it in Settings.", "need_cf_key"}
	}
	return map[string]string{"x-api-key": strings.TrimSpace(key)}, nil
}

func searchCurseForge(inst *Instance, q, kind, key string) ([]SearchHit, error) {
	headers, err := cfHeaders(key)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/mods/search?gameId=432&classId=%d&pageSize=30&sortField=2&sortOrder=desc&gameVersion=%s&searchFilter=%s",
		cfAPI, cfKindClass[kind], url.QueryEscape(inst.MCVersion), url.QueryEscape(q))
	if kind == "mod" {
		u += fmt.Sprintf("&modLoaderType=%d", cfLoaderTypes[loaderChoices(inst)[0]])
	}
	raw, err := getJSON(u, headers)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			return nil, errors.New("CurseForge rejected the API key. Check it in Settings")
		}
		return nil, fmt.Errorf("CurseForge search failed: %v", err)
	}
	have := installedRefs(inst)
	hits := []SearchHit{}
	for _, x := range asA(asM(raw)["data"]) {
		m := asM(x)
		id := strconv.Itoa(asInt(m["id"]))
		author := ""
		if a := asA(m["authors"]); len(a) > 0 {
			author = asS(asM(a[0])["name"])
		}
		hits = append(hits, SearchHit{"curseforge", id, asS(m["name"]), asS(m["summary"]), author, asS(asM(m["logo"])["thumbnailUrl"]),
			asS(asM(m["links"])["websiteUrl"]), asInt(m["downloadCount"]), have["curseforge:"+id]})
	}
	return hits, nil
}

func installCurseForge(inst *Instance, id int, kind, key string, logf Logger, seen map[string]bool, depth int) error {
	ref := "curseforge:" + strconv.Itoa(id)
	if seen[ref] || depth > 4 || id <= 0 {
		return nil
	}
	seen[ref] = true
	headers, err := cfHeaders(key)
	if err != nil {
		return err
	}
	mraw, err := getJSON(fmt.Sprintf("%s/mods/%d", cfAPI, id), headers)
	if err != nil {
		return fmt.Errorf("could not look that project up on CurseForge: %v", err)
	}
	mod := asM(asM(mraw)["data"])
	name := asS(mod["name"])
	folder := kindFolders[kind]
	if depth > 0 {
		folder = classFolders[asInt(mod["classId"])]
		if folder == "" {
			return nil
		}
		kind = map[string]string{"mods": "mod", "resourcepacks": "resourcepack", "shaderpacks": "shader"}[folder]
		if kind == "" {
			return nil
		}
	}
	var files []M
	loaders := []string{""}
	if kind == "mod" {
		loaders = loaderChoices(inst)
	}
	for _, l := range loaders {
		u := fmt.Sprintf("%s/mods/%d/files?pageSize=30&gameVersion=%s", cfAPI, id, url.QueryEscape(inst.MCVersion))
		if l != "" {
			u += fmt.Sprintf("&modLoaderType=%d", cfLoaderTypes[l])
		}
		fraw, err := getJSON(u, headers)
		if err != nil {
			return fmt.Errorf("could not list files of %s: %v", name, err)
		}
		for _, f := range asA(asM(fraw)["data"]) {
			files = append(files, asM(f))
		}
		if len(files) > 0 {
			break
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("%s has no file for Minecraft %s on %s", name, inst.MCVersion, inst.Loader)
	}
	sort.SliceStable(files, func(a, b int) bool { // full releases first, then newest
		ra, rb := asInt(files[a]["releaseType"]) == 1, asInt(files[b]["releaseType"]) == 1
		if ra != rb {
			return ra
		}
		return asS(files[a]["fileDate"]) > asS(files[b]["fileDate"])
	})
	file := files[0]
	fileName := filepath.Base(asS(file["fileName"]))
	page := asS(asM(mod["links"])["websiteUrl"])
	if asS(file["downloadUrl"]) == "" || !validFileName(fileName) {
		return &apiError{name + " can only be downloaded from the CurseForge site (its author's choice). Download it there and drop it into the pack's " + folder + " folder.", "blocked:" + page}
	}
	sum := ""
	for _, h := range asA(file["hashes"]) {
		if asInt(asM(h)["algo"]) == 1 {
			sum = asS(asM(h)["value"])
		}
	}
	dest := filepath.Join(inst.GameDir(), folder, fileName)
	if !exists(dest) && !exists(dest+".disabled") {
		if err := download([]string{strings.ReplaceAll(asS(file["downloadUrl"]), " ", "%20")}, dest, sum, nil); err != nil {
			return fmt.Errorf("could not download %s: %v", fileName, err)
		}
	}
	author := ""
	if a := asA(mod["authors"]); len(a) > 0 {
		author = asS(asM(a[0])["name"])
	}
	recordMod(inst, ModMeta{Project: id, File: asInt(file["id"]), Name: name, FileName: fileName, Folder: folder,
		Summary: asS(mod["summary"]), Logo: asS(asM(mod["logo"])["thumbnailUrl"]), URL: page, Author: author})
	logf("Installed %s (%s).", name, fileName)
	for _, d := range asA(file["dependencies"]) {
		if dm := asM(d); asInt(dm["relationType"]) == 3 {
			if err := installCurseForge(inst, asInt(dm["modId"]), kind, key, logf, seen, depth+1); err != nil {
				logf("  A required add-on could not be installed: %v", err)
			}
		}
	}
	return nil
}
