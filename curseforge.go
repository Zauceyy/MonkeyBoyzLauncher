package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var (
	cfAPI          = "https://api.curseforge.com/v1"
	cfSiteDownload = "https://www.curseforge.com/api/v1/mods/%d/files/%d/download"
	// CurseForge class ids -> folder inside the game directory
	classFolders = map[int]string{6: "mods", 12: "resourcepacks", 6552: "shaderpacks", 6945: "datapacks", 17: "saves"}
)

type cfManifest struct {
	Minecraft struct {
		Version    string `json:"version"`
		ModLoaders []struct {
			ID      string `json:"id"`
			Primary bool   `json:"primary"`
		} `json:"modLoaders"`
		RecommendedRam int `json:"recommendedRam"`
	} `json:"minecraft"`
	ManifestType string `json:"manifestType"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Author       string `json:"author"`
	Overrides    string `json:"overrides"`
	Files        []struct {
		ProjectID int `json:"projectID"`
		FileID    int `json:"fileID"`
	} `json:"files"`

	loader, loaderVersion string
	root                  string // folder inside the zip where the pack starts ("" normally)
}

// ModMeta is what we remember about each pack file so the mod list can show names and icons.
type ModMeta struct {
	Project  int    `json:"project"`
	File     int    `json:"file"`
	Name     string `json:"name"`
	FileName string `json:"file_name"`
	Folder   string `json:"folder"`
	Summary  string `json:"summary,omitempty"`
	Logo     string `json:"logo,omitempty"`
	URL      string `json:"url,omitempty"`
	Author   string `json:"author,omitempty"`
	Source   string `json:"source,omitempty"` // "modrinth", or empty for CurseForge
	Ref      string `json:"ref,omitempty"`    // Modrinth project id
}

func parseLoaderID(id string) (string, string, error) {
	kind, ver, _ := strings.Cut(strings.TrimSpace(id), "-")
	kind = strings.ToLower(kind)
	switch kind {
	case "forge", "neoforge", "fabric", "quilt":
		if ver != "" {
			return kind, ver, nil
		}
	}
	return "", "", fmt.Errorf("this pack uses a mod loader the launcher does not know: %s", id)
}

func readManifest(zipPath string) (*cfManifest, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errors.New("that file is not a zip archive. Pick the .zip you exported from CurseForge")
	}
	defer zr.Close()
	root, kind := packRoot(zipPath)
	b, err := zipReadNorm(&zr.Reader, root+"manifest.json")
	if err != nil || kind != "curseforge" {
		return nil, errors.New("that file is not a modpack. Use a CurseForge export (.zip, from the profile menu > Export Profile) or a Modrinth pack (.mrpack)")
	}
	m := &cfManifest{root: root}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf")), m); err != nil {
		return nil, fmt.Errorf("manifest.json could not be read: %v", err)
	}
	if m.ManifestType != "" && m.ManifestType != "minecraftModpack" {
		return nil, errors.New("this zip is not a Minecraft modpack")
	}
	if m.Minecraft.Version == "" {
		return nil, errors.New("manifest.json has no Minecraft version")
	}
	if m.Name == "" {
		m.Name = strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	}
	m.Overrides = strings.Trim(m.Overrides, "/")
	if m.Overrides == "" {
		m.Overrides = "overrides"
	}
	m.loader = "vanilla"
	if n := len(m.Minecraft.ModLoaders); n > 0 {
		primary := m.Minecraft.ModLoaders[0]
		for _, l := range m.Minecraft.ModLoaders {
			if l.Primary {
				primary = l
				break
			}
		}
		if m.loader, m.loaderVersion, err = parseLoaderID(primary.ID); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// extractOverrides copies the pack's bundled files (configs, scripts, extra mods) into the game folder.
func extractOverrides(zipPath string, m *cfManifest, gameDir string) (int, error) {
	return extractPrefix(zipPath, m.root+m.Overrides, gameDir)
}

func extractPrefix(zipPath, folder, gameDir string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	prefix := folder + "/"
	count := 0
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if !strings.HasPrefix(name, prefix) || f.FileInfo().IsDir() {
			continue
		}
		rel := path.Clean(strings.TrimPrefix(name, prefix))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
			continue // never write outside the instance
		}
		if err := zipExtract(f, filepath.Join(gameDir, filepath.FromSlash(rel))); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

var modlistRe = regexp.MustCompile(`(?i)<a[^>]*href="([^"]*)"[^>]*>([^<]+)</a>`)

// readPackList reads the human-readable mod list CurseForge puts in every export (modlist.html).
func readPackList(zipPath string) []M {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil
	}
	defer zr.Close()
	root, _ := packRoot(zipPath)
	b, err := zipReadNorm(&zr.Reader, root+"modlist.html")
	if err != nil {
		return nil
	}
	var out []M
	for _, m := range modlistRe.FindAllStringSubmatch(string(b), -1) {
		out = append(out, M{"name": html.UnescapeString(strings.TrimSpace(m[2])), "url": html.UnescapeString(m[1])})
	}
	return out
}

func folderFor(filename string, classID int) string {
	if f, ok := classFolders[classID]; ok {
		return f
	}
	if strings.HasSuffix(strings.ToLower(filename), ".jar") {
		return "mods"
	}
	return "resourcepacks"
}

func chunkInts(v []int, n int) [][]int {
	var out [][]int
	for len(v) > n {
		out = append(out, v[:n])
		v = v[n:]
	}
	return append(out, v)
}

func downloadWithAPI(m *cfManifest, gameDir, apiKey string, logf Logger, prog Progress) ([]ManualItem, []ModMeta, error) {
	headers := map[string]string{"x-api-key": apiKey}
	var fileIDs, projectIDs []int
	seenProj := map[int]bool{}
	for _, f := range m.Files {
		fileIDs = append(fileIDs, f.FileID)
		if !seenProj[f.ProjectID] {
			seenProj[f.ProjectID] = true
			projectIDs = append(projectIDs, f.ProjectID)
		}
	}
	logf("Looking up %d files on CurseForge...", len(fileIDs))
	files, mods := map[int]M{}, map[int]M{}
	for _, chunk := range chunkInts(fileIDs, 500) {
		r, err := postJSON(cfAPI+"/mods/files", M{"fileIds": chunk}, headers)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range asA(asM(r)["data"]) {
			files[asInt(asM(f)["id"])] = asM(f)
		}
	}
	for _, chunk := range chunkInts(projectIDs, 500) {
		r, err := postJSON(cfAPI+"/mods", M{"modIds": chunk}, headers)
		if err != nil {
			return nil, nil, err
		}
		for _, x := range asA(asM(r)["data"]) {
			mods[asInt(asM(x)["id"])] = asM(x)
		}
	}

	var jobs []dlJob
	var manual []ManualItem
	var metas []ModMeta
	for _, e := range m.Files {
		f, mod := files[e.FileID], mods[e.ProjectID]
		name := asS(mod["name"])
		if name == "" {
			name = fmt.Sprintf("Project %d", e.ProjectID)
		}
		page := asS(asM(mod["links"])["websiteUrl"])
		if page == "" {
			page = fmt.Sprintf("https://www.curseforge.com/projects/%d", e.ProjectID)
		}
		fileURL := fmt.Sprintf("%s/files/%d", page, e.FileID)
		if f == nil {
			manual = append(manual, ManualItem{e.ProjectID, e.FileID, name, "", "mods", fileURL, "this file was removed from CurseForge"})
			continue
		}
		fileName := filepath.Base(asS(f["fileName"]))
		folder := folderFor(fileName, asInt(mod["classId"]))
		author := ""
		if a := asA(mod["authors"]); len(a) > 0 {
			author = asS(asM(a[0])["name"])
		}
		metas = append(metas, ModMeta{Project: e.ProjectID, File: e.FileID, Name: name, FileName: fileName, Folder: folder,
			Summary: asS(mod["summary"]), Logo: asS(asM(mod["logo"])["thumbnailUrl"]), URL: page, Author: author})
		if u := asS(f["downloadUrl"]); u != "" {
			sum := ""
			for _, h := range asA(f["hashes"]) {
				if asInt(asM(h)["algo"]) == 1 {
					sum = asS(asM(h)["value"])
				}
			}
			jobs = append(jobs, dlJob{[]string{strings.ReplaceAll(u, " ", "%20")}, filepath.Join(gameDir, folder, fileName), sum})
		} else { // the author switched off downloads from other launchers
			manual = append(manual, ManualItem{e.ProjectID, e.FileID, name, fileName, folder, fileURL, "the author only allows downloads from the CurseForge site"})
		}
	}
	if errs := downloadMany(jobs, "Downloading mods", prog, 8); len(errs) > 0 {
		for _, e := range firstN(errs, 10) {
			logf("  Download failed: %s", e)
		}
		return nil, nil, fmt.Errorf("%d mod downloads failed. Check your internet connection and import the pack again", len(errs))
	}
	return manual, metas, nil
}

func siteDownload(project, file int, gameDir string) (string, string, error) {
	resp, err := httpDo("GET", fmt.Sprintf(cfSiteDownload, project, file), nil, nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	name := path.Base(resp.Request.URL.Path)
	if !strings.Contains(name, ".") || strings.ContainsAny(name, `/\`) {
		return "", "", errors.New("could not work out the file name")
	}
	folder := folderFor(name, 0)
	dest := filepath.Join(gameDir, folder, name)
	if exists(dest) {
		return name, folder, nil
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", "", err
	}
	_, err = out.ReadFrom(resp.Body)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return name, folder, os.Rename(tmp, dest)
}

func downloadWithoutAPI(m *cfManifest, gameDir string, logf Logger, prog Progress) ([]ManualItem, []ModMeta) {
	logf("No CurseForge API key in Settings - downloading %d files through the CurseForge website...", len(m.Files))
	var mu sync.Mutex
	var manual []ManualItem
	var metas []ModMeta
	total, done := len(m.Files), 0
	if prog != nil {
		prog("Downloading mods", 0, total)
	}
	type job struct{ project, file int }
	ch := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				name, folder, err := siteDownload(j.project, j.file, gameDir)
				mu.Lock()
				if err != nil {
					manual = append(manual, ManualItem{j.project, j.file, fmt.Sprintf("Project %d", j.project), "", "mods",
						fmt.Sprintf("https://www.curseforge.com/projects/%d", j.project), "automatic download failed: " + err.Error()})
				} else {
					metas = append(metas, ModMeta{Project: j.project, File: j.file, FileName: name, Folder: folder})
				}
				done++
				if prog != nil {
					prog("Downloading mods", done, total)
				}
				mu.Unlock()
			}
		}()
	}
	for _, f := range m.Files {
		ch <- job{f.ProjectID, f.FileID}
	}
	close(ch)
	wg.Wait()
	return manual, metas
}

func downloadMods(m *cfManifest, gameDir, apiKey string, logf Logger, prog Progress) ([]ManualItem, []ModMeta, error) {
	if len(m.Files) == 0 {
		return nil, nil, nil
	}
	if apiKey != "" {
		manual, metas, err := downloadWithAPI(m, gameDir, apiKey, logf, prog)
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			return nil, nil, errors.New("CurseForge rejected the API key. Check it in Settings")
		}
		return manual, metas, err
	}
	manual, metas := downloadWithoutAPI(m, gameDir, logf, prog)
	if len(manual) == len(m.Files) && len(manual) > 3 {
		return nil, nil, errors.New("CurseForge blocked every download. Add a free CurseForge API key in Settings (it takes a minute) and import again")
	}
	return manual, metas, nil
}

// ---------------------------------------------------------------- manual downloads

var downloadsFolder = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

// collectManual moves files the user downloaded by hand from Downloads into the instance and returns what is still missing.
func collectManual(inst *Instance, logf Logger) []ManualItem {
	if len(inst.Manual) == 0 {
		return nil
	}
	var still []ManualItem
	for _, it := range inst.Manual {
		if it.FileName == "" {
			still = append(still, it)
			continue
		}
		target := filepath.Join(inst.GameDir(), it.Folder, it.FileName)
		if exists(target) || exists(target+".disabled") {
			continue
		}
		src := filepath.Join(downloadsFolder(), it.FileName)
		if exists(src) && copyFile(src, target) == nil {
			logf("Picked up %s from your Downloads folder.", it.FileName)
		} else {
			still = append(still, it)
		}
	}
	if len(still) != len(inst.Manual) {
		inst.Manual = still
		inst.Save()
	}
	return still
}

// ---------------------------------------------------------------- import

func importPack(p *Paths, cfg *Config, zipPath string, logf Logger, prog Progress) (*Instance, error) {
	m, err := readManifest(zipPath)
	if err != nil {
		return nil, err
	}
	loaderDesc := m.loader
	if m.loaderVersion != "" {
		loaderDesc += " " + m.loaderVersion
	}
	logf("Importing %s %s: Minecraft %s, %s, %d files", m.Name, m.Version, m.Minecraft.Version, loaderDesc, len(m.Files))
	inst, err := createInstance(p, m.Name, m.Minecraft.Version, m.loader, m.loaderVersion)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Instance, error) {
		os.RemoveAll(inst.Dir)
		return nil, err
	}
	inst.Pack = &PackInfo{m.Name, m.Version, m.Author, len(m.Files)}
	if r := m.Minecraft.RecommendedRam; r >= 2048 {
		for _, c := range ramOptions() {
			if c >= r {
				inst.MemoryMB = c
				break
			}
		}
	}
	n, err := extractOverrides(zipPath, m, inst.GameDir())
	if err != nil {
		return fail(err)
	}
	logf("Copied %d bundled files (configs, scripts and so on).", n)
	if list := readPackList(zipPath); len(list) > 0 {
		writeJSON(filepath.Join(inst.Dir, "packlist.json"), list)
	}
	manual, metas, err := downloadMods(m, inst.GameDir(), strings.TrimSpace(cfg.CurseForgeAPIKey), logf, prog)
	if err != nil {
		return fail(err)
	}
	writeJSON(filepath.Join(inst.Dir, "modlist.json"), metas)
	inst.Manual = manual
	if err := inst.Save(); err != nil {
		return fail(err)
	}
	collectManual(inst, logf)
	if prepareAfterImport {
		if _, _, err := prepareInstance(p, cfg, inst, logf, prog); err != nil {
			return fail(err)
		}
	}
	if len(inst.Manual) > 0 {
		logf("%d file(s) need a manual download - see the Missing files tab.", len(inst.Manual))
	}
	logf("Import finished: %s", inst.Name)
	return inst, nil
}
