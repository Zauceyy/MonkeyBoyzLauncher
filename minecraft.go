package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	versionManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	resourcesURL       = "https://resources.download.minecraft.net/"
	libMirrors         = []string{"https://libraries.minecraft.net/", "https://maven.minecraftforge.net/",
		"https://maven.neoforged.net/releases/", "https://maven.fabricmc.net/", "https://maven.quiltmc.org/repository/release/",
		"https://repo1.maven.org/maven2/"}
)

// ---------------------------------------------------------------- rules

func ruleMatches(rule M, features map[string]bool) bool {
	if osr := asM(rule["os"]); osr != nil {
		if n := asS(osr["name"]); n != "" && n != osName() {
			return false
		}
		if a := asS(osr["arch"]); a != "" && (a == "x86") != (archBits() == "32") {
			return false
		}
	}
	for k, v := range asM(rule["features"]) {
		want, _ := v.(bool)
		if features[k] != want {
			return false
		}
	}
	return true
}

func rulesAllow(rules []any, features map[string]bool) bool {
	if len(rules) == 0 {
		return true
	}
	allowed := false
	for _, r := range rules {
		rm := asM(r)
		if ruleMatches(rm, features) {
			allowed = asS(rm["action"]) == "allow"
		}
	}
	return allowed
}

// ---------------------------------------------------------------- version JSON

func versionJSONPath(p *Paths, id string) string {
	return filepath.Join(p.Versions, id, id+".json")
}

func clientJarPath(p *Paths, id string) string {
	return filepath.Join(p.Versions, id, id+".jar")
}

func mergeVersions(parent, child M) M {
	out := M{}
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range child {
		switch k {
		case "libraries":
			out[k] = append(append([]any{}, asA(v)...), asA(parent["libraries"])...)
		case "arguments":
			pa, ca := asM(parent["arguments"]), asM(v)
			out[k] = M{
				"game": append(append([]any{}, asA(pa["game"])...), asA(ca["game"])...),
				"jvm":  append(append([]any{}, asA(pa["jvm"])...), asA(ca["jvm"])...),
			}
		case "inheritsFrom":
		default:
			out[k] = v
		}
	}
	jar := asS(child["jar"])
	if jar == "" {
		jar = asS(parent["_jar"])
	}
	out["_jar"] = jar
	out["_vanilla"] = parent["_vanilla"]
	return out
}

// loadVersion reads a version JSON and resolves its inheritsFrom chain.
func loadVersion(p *Paths, id string) (M, error) {
	raw, err := readJSON(versionJSONPath(p, id))
	if err != nil {
		return nil, fmt.Errorf("version %s is not installed", id)
	}
	data := asM(raw)
	if parent := asS(data["inheritsFrom"]); parent != "" {
		pv, err := loadVersion(p, parent)
		if err != nil {
			return nil, err
		}
		return mergeVersions(pv, data), nil
	}
	if asS(data["jar"]) != "" {
		data["_jar"] = data["jar"]
	} else {
		data["_jar"] = data["id"]
	}
	data["_vanilla"] = data["id"]
	return data, nil
}

func listMinecraftVersions() ([]any, error) {
	v, err := getJSON(versionManifestURL, nil)
	if err != nil {
		return nil, err
	}
	return asA(asM(v)["versions"]), nil
}

// ---------------------------------------------------------------- libraries

type nativeLib struct {
	Jar      string
	Excludes []string
}

type resolvedLibs struct {
	Classpath []string
	Natives   []nativeLib
	Jobs      []dlJob
}

func mirrorURLs(lib M, rel string) []string {
	var bases []string
	if u := asS(lib["url"]); u != "" {
		bases = append(bases, u)
	}
	bases = append(bases, libMirrors...)
	var urls []string
	for _, b := range bases {
		urls = append(urls, strings.TrimRight(b, "/")+"/"+rel)
	}
	return urls
}

func resolveLibraries(p *Paths, vjson M) *resolvedLibs {
	res := &resolvedLibs{}
	seen := map[string]bool{}
	for _, raw := range asA(vjson["libraries"]) {
		lib := asM(raw)
		if !rulesAllow(asA(lib["rules"]), nil) {
			continue
		}
		if b, ok := lib["clientreq"].(bool); ok && !b { // legacy Forge: server-only library
			continue
		}
		name := asS(lib["name"])
		downloads := asM(lib["downloads"])
		nativesMap := asM(lib["natives"])
		nativeClassifier := ""
		if c := asS(nativesMap[osName()]); c != "" {
			nativeClassifier = strings.ReplaceAll(c, "${arch}", archBits())
		}
		excludes := []string{"META-INF/"}
		if ex := asA(asM(lib["extract"])["exclude"]); ex != nil {
			excludes = nil
			for _, e := range ex {
				excludes = append(excludes, asS(e))
			}
		}

		art := asM(downloads["artifact"])
		if (art != nil || nativesMap == nil) && name != "" {
			key := mavenKey(name)
			if !seen[key] {
				seen[key] = true
				rel := asS(art["path"])
				if rel == "" {
					rel = mavenPath(name)
				}
				dest := libPath(p, rel)
				if art != nil {
					if u := asS(art["url"]); u != "" {
						res.Jobs = append(res.Jobs, dlJob{[]string{u}, dest, asS(art["sha1"])})
					}
				} else if !exists(dest) {
					res.Jobs = append(res.Jobs, dlJob{mirrorURLs(lib, rel), dest, ""})
				}
				res.Classpath = append(res.Classpath, dest)
				parts := strings.Split(strings.SplitN(name, "@", 2)[0], ":")
				if len(parts) > 3 && strings.HasPrefix(parts[3], "natives-") {
					res.Natives = append(res.Natives, nativeLib{dest, []string{"META-INF/"}})
				}
			}
		}

		if nativeClassifier != "" { // pre-1.19 style natives
			rel := mavenPath(name + ":" + nativeClassifier)
			if cl := asM(asM(downloads["classifiers"])[nativeClassifier]); cl != nil {
				if cp := asS(cl["path"]); cp != "" {
					rel = cp
				}
				res.Jobs = append(res.Jobs, dlJob{[]string{asS(cl["url"])}, libPath(p, rel), asS(cl["sha1"])})
			} else if !exists(libPath(p, rel)) {
				res.Jobs = append(res.Jobs, dlJob{mirrorURLs(lib, rel), libPath(p, rel), ""})
			}
			res.Natives = append(res.Natives, nativeLib{libPath(p, rel), excludes})
		}
	}
	return res
}

func extractNatives(libs *resolvedLibs, dest string) {
	os.RemoveAll(dest)
	os.MkdirAll(dest, 0o755)
	for _, n := range libs.Natives {
		z, err := zip.OpenReader(n.Jar)
		if err != nil {
			continue
		}
		for _, f := range z.File {
			name := f.Name
			lower := strings.ToLower(name)
			if f.FileInfo().IsDir() || !(strings.HasSuffix(lower, ".dll") || strings.HasSuffix(lower, ".so") ||
				strings.HasSuffix(lower, ".dylib") || strings.HasSuffix(lower, ".jnilib")) {
				continue
			}
			skip := false
			for _, e := range n.Excludes {
				if strings.HasPrefix(name, e) {
					skip = true
				}
			}
			if skip {
				continue
			}
			if src, err := f.Open(); err == nil {
				if out, err := os.Create(filepath.Join(dest, path.Base(name))); err == nil {
					io.Copy(out, src)
					out.Close()
				}
				src.Close()
			}
		}
		z.Close()
	}
}

// ---------------------------------------------------------------- install

func installVanillaJSON(p *Paths, mc string, logf Logger) (M, error) {
	vp := versionJSONPath(p, mc)
	if !exists(vp) {
		logf("Fetching Minecraft %s metadata...", mc)
		versions, err := listMinecraftVersions()
		if err != nil {
			return nil, fmt.Errorf("could not reach Mojang's servers: %v", err)
		}
		var entry M
		for _, v := range versions {
			if asS(asM(v)["id"]) == mc {
				entry = asM(v)
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("unknown Minecraft version: %s", mc)
		}
		if err := download([]string{asS(entry["url"])}, vp, asS(entry["sha1"]), nil); err != nil {
			return nil, err
		}
	}
	raw, err := readJSON(vp)
	if err != nil {
		os.Remove(vp)
		return nil, fmt.Errorf("Minecraft %s metadata is corrupt, try again: %v", mc, err)
	}
	return asM(raw), nil
}

// installGameFiles makes sure everything needed to launch versionID is on disk and returns the merged JSON.
func installGameFiles(p *Paths, versionID, gameDir string, logf Logger, prog Progress) (M, error) {
	vjson, err := loadVersion(p, versionID)
	if err != nil {
		return nil, err
	}
	vanillaID := asS(vjson["_vanilla"])
	vraw, _ := readJSON(versionJSONPath(p, vanillaID))
	if client := asM(asM(asM(vraw)["downloads"])["client"]); client != nil {
		logf("Checking the Minecraft %s game file...", vanillaID)
		if err := download([]string{asS(client["url"])}, clientJarPath(p, vanillaID), asS(client["sha1"]), nil); err != nil {
			return nil, fmt.Errorf("could not download Minecraft %s: %v", vanillaID, err)
		}
	}

	libs := resolveLibraries(p, vjson)
	errs := downloadMany(libs.Jobs, "Downloading libraries", prog, 12)
	var missing []string
	for _, c := range libs.Classpath {
		if !exists(c) {
			missing = append(missing, filepath.Base(c))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing libraries: %s %s", strings.Join(firstN(missing, 8), ", "), strings.Join(firstN(errs, 3), "; "))
	}

	if ai := asM(vjson["assetIndex"]); ai != nil {
		id := asS(ai["id"])
		idxPath := filepath.Join(p.Assets, "indexes", id+".json")
		if err := download([]string{asS(ai["url"])}, idxPath, asS(ai["sha1"]), nil); err != nil {
			return nil, fmt.Errorf("could not download the asset index: %v", err)
		}
		raw, _ := readJSON(idxPath)
		index := asM(raw)
		objects := asM(index["objects"])
		var jobs []dlJob
		for _, o := range objects {
			h := asS(asM(o)["hash"])
			if len(h) < 2 {
				continue
			}
			jobs = append(jobs, dlJob{[]string{resourcesURL + h[:2] + "/" + h}, filepath.Join(p.Assets, "objects", h[:2], h), h})
		}
		if errs := downloadMany(jobs, "Downloading game assets", prog, 24); len(errs) > 0 {
			logf("Warning: %d assets failed to download (some sounds may be missing).", len(errs))
		}
		virtual, _ := index["virtual"].(bool)
		mapRes, _ := index["map_to_resources"].(bool)
		if virtual || mapRes {
			targets := []string{filepath.Join(p.Assets, "virtual", id)}
			if mapRes && gameDir != "" {
				targets = append(targets, filepath.Join(gameDir, "resources"))
			}
			for name, o := range objects {
				h := asS(asM(o)["hash"])
				src := filepath.Join(p.Assets, "objects", h[:2], h)
				for _, t := range targets {
					d := filepath.Join(t, filepath.FromSlash(name))
					if exists(src) && !exists(d) {
						copyFile(src, d)
					}
				}
			}
		}
	}
	return vjson, nil
}

// ---------------------------------------------------------------- launching

var varRe = regexp.MustCompile(`\$\{([a-zA-Z0-9_]+)\}`)

func subst(s string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := vars[m[2:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

func flattenArgs(args []any, features map[string]bool) []string {
	var out []string
	for _, a := range args {
		switch v := a.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			if rulesAllow(asA(v["rules"]), features) {
				switch val := v["value"].(type) {
				case string:
					out = append(out, val)
				case []any:
					for _, s := range val {
						out = append(out, asS(s))
					}
				}
			}
		}
	}
	return out
}

func buildLaunchCommand(p *Paths, vjson M, java, gameDir, nativesDir string, acct Account, memoryMB int, extraJVM []string) []string {
	libs := resolveLibraries(p, vjson)
	vid := asS(vjson["id"])

	// Forge's ignoreList expects the game jar to be called <version id>.jar.
	srcJar := clientJarPath(p, asS(vjson["_jar"]))
	jar := clientJarPath(p, vid)
	if exists(srcJar) && !exists(jar) {
		copyFile(srcJar, jar)
	}
	if !exists(jar) {
		jar = srcJar
	}

	sep := classpathSep()
	classpath := strings.Join(append(append([]string{}, libs.Classpath...), jar), sep)
	assetsID := asS(asM(vjson["assetIndex"])["id"])
	if assetsID == "" {
		assetsID = asS(vjson["assets"])
	}
	if assetsID == "" {
		assetsID = "legacy"
	}
	gameAssets := p.Assets
	if raw, err := readJSON(filepath.Join(p.Assets, "indexes", assetsID+".json")); err == nil {
		v, _ := asM(raw)["virtual"].(bool)
		m, _ := asM(raw)["map_to_resources"].(bool)
		if v || m {
			gameAssets = filepath.Join(p.Assets, "virtual", assetsID)
		}
	}
	token := acct.AccessToken
	if token == "" {
		token = "0"
	}
	userType := "legacy"
	if acct.Type == "microsoft" {
		userType = "msa"
	}
	vtype := asS(vjson["type"])
	if vtype == "" {
		vtype = "release"
	}
	vars := map[string]string{
		"auth_player_name": acct.Name, "version_name": vid, "game_directory": gameDir,
		"assets_root": p.Assets, "game_assets": gameAssets, "assets_index_name": assetsID,
		"auth_uuid": acct.UUID, "auth_access_token": token,
		"auth_session": "token:" + token + ":" + acct.UUID, "clientid": acct.ClientID, "auth_xuid": "",
		"user_type": userType, "user_properties": "{}", "version_type": vtype,
		"natives_directory": nativesDir, "launcher_name": "MonkeyBoyzLauncher", "launcher_version": appVersion,
		"classpath": classpath, "classpath_separator": sep, "library_directory": p.Libraries,
	}
	features := map[string]bool{}

	args := asM(vjson["arguments"])
	var jvm, game []string
	if len(asA(args["jvm"])) > 0 {
		jvm = flattenArgs(asA(args["jvm"]), features)
	} else {
		jvm = []string{"-Djava.library.path=${natives_directory}", "-cp", "${classpath}"}
		if osName() == "osx" {
			jvm = append([]string{"-XstartOnFirstThread"}, jvm...)
		}
	}
	if legacy := asS(vjson["minecraftArguments"]); legacy != "" {
		game = strings.Fields(legacy)
	}
	game = append(game, flattenArgs(asA(args["game"]), features)...)

	xms := memoryMB
	if xms > 1024 {
		xms = 1024
	}
	cmd := []string{java, fmt.Sprintf("-Xmx%dM", memoryMB), fmt.Sprintf("-Xms%dM", xms), "-Dlog4j2.formatMsgNoLookups=true"}
	cmd = append(cmd, extraJVM...)
	for _, a := range jvm {
		cmd = append(cmd, subst(a, vars))
	}
	cmd = append(cmd, asS(vjson["mainClass"]))
	for _, a := range game {
		cmd = append(cmd, subst(a, vars))
	}
	return cmd
}

func launchGame(p *Paths, vjson M, java, gameDir, instKey string, acct Account, memoryMB int, extraJVM []string) (*exec.Cmd, io.ReadCloser, error) {
	nativesDir := filepath.Join(p.Natives, instKey)
	extractNatives(resolveLibraries(p, vjson), nativesDir)
	os.MkdirAll(gameDir, 0o755)
	args := buildLaunchCommand(p, vjson, java, gameDir, nativesDir, acct, memoryMB, extraJVM)
	cmd := hiddenCommand(args[0], args[1:]...)
	cmd.Dir = gameDir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("could not start Java: %v", err)
	}
	return cmd, out, nil
}
