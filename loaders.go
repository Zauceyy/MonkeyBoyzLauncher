package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	fabricMeta    = "https://meta.fabricmc.net/v2/versions/loader/%s/%s/profile/json"
	quiltMeta     = "https://meta.quiltmc.org/v3/versions/loader/%s/%s/profile/json"
	forgeMaven    = "https://maven.minecraftforge.net/net/minecraftforge/forge/"
	neoforgeMaven = "https://maven.neoforged.net/releases/net/neoforged/"
)

// installLoader installs the mod loader and returns the version id to launch.
func installLoader(p *Paths, mc, loader, lv, javaOverride string, logf Logger, prog Progress) (string, error) {
	loader = strings.ToLower(loader)
	vanilla, err := installVanillaJSON(p, mc, logf)
	if err != nil {
		return "", err
	}
	switch loader {
	case "", "vanilla":
		return mc, nil
	case "fabric", "quilt":
		return installFabricLike(p, mc, loader, lv, logf)
	case "forge", "neoforge":
		return installForgeLike(p, vanilla, mc, loader, lv, javaOverride, logf, prog)
	}
	return "", fmt.Errorf("unsupported mod loader: %s", loader)
}

// ---------------------------------------------------------------- Fabric / Quilt

func installFabricLike(p *Paths, mc, loader, lv string, logf Logger) (string, error) {
	tmpl := fabricMeta
	if loader == "quilt" {
		tmpl = quiltMeta
	}
	marker := filepath.Join(p.Versions, fmt.Sprintf(".%s-%s-%s.installed", loader, mc, lv))
	if b, err := os.ReadFile(marker); err == nil && exists(versionJSONPath(p, strings.TrimSpace(string(b)))) {
		return strings.TrimSpace(string(b)), nil
	}
	logf("Installing %s loader %s for Minecraft %s...", loader, lv, mc)
	raw, err := getJSON(fmt.Sprintf(tmpl, mc, lv), nil)
	if err != nil {
		return "", fmt.Errorf("%s %s is not available for Minecraft %s (%v)", loader, lv, mc, err)
	}
	profile := asM(raw)
	vid := asS(profile["id"])
	if vid == "" {
		return "", fmt.Errorf("unexpected reply from the %s servers", loader)
	}
	if asS(profile["inheritsFrom"]) == "" {
		profile["inheritsFrom"] = mc
	}
	if err := writeJSON(versionJSONPath(p, vid), profile); err != nil {
		return "", err
	}
	os.WriteFile(marker, []byte(vid), 0o644)
	return vid, nil
}

// ---------------------------------------------------------------- Forge / NeoForge

func installerURLs(mc, loader, lv string) []string {
	lv = strings.TrimPrefix(lv, mc+"-")
	if loader == "neoforge" {
		if mc == "1.20.1" { // NeoForge's first releases kept Forge's naming scheme
			full := mc + "-" + lv
			return []string{neoforgeMaven + "forge/" + full + "/forge-" + full + "-installer.jar"}
		}
		return []string{neoforgeMaven + "neoforge/" + lv + "/neoforge-" + lv + "-installer.jar"}
	}
	var urls []string
	for _, full := range []string{mc + "-" + lv, mc + "-" + lv + "-" + mc, mc + "-" + lv + "-mc" + strings.ReplaceAll(mc, ".", "")} {
		urls = append(urls, forgeMaven+full+"/forge-"+full+"-installer.jar")
	}
	return urls
}

func zipRead(z *zip.Reader, name string) ([]byte, error) {
	for _, f := range z.File {
		if f.Name == name {
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", name)
}

func zipExtract(f *zip.File, dest string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	os.MkdirAll(filepath.Dir(dest), 0o755)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func installForgeLike(p *Paths, vanilla M, mc, loader, lv, javaOverride string, logf Logger, prog Progress) (string, error) {
	marker := filepath.Join(p.Versions, fmt.Sprintf(".%s-%s-%s.installed", loader, mc, lv))
	if b, err := os.ReadFile(marker); err == nil && exists(versionJSONPath(p, strings.TrimSpace(string(b)))) {
		return strings.TrimSpace(string(b)), nil
	}
	logf("Downloading the %s %s installer...", loader, lv)
	installer := filepath.Join(p.Temp, fmt.Sprintf("%s-%s-%s-installer.jar", loader, mc, lv))
	if err := download(installerURLs(mc, loader, lv), installer, "", nil); err != nil {
		return "", fmt.Errorf("could not download %s %s for Minecraft %s: %v", loader, lv, mc, err)
	}
	zr, err := zip.OpenReader(installer)
	if err != nil {
		os.Remove(installer)
		return "", fmt.Errorf("the %s installer download is corrupt, try again", loader)
	}
	defer zr.Close()
	pb, err := zipRead(&zr.Reader, "install_profile.json")
	if err != nil {
		return "", err
	}
	var profile M
	if err := json.Unmarshal(pb, &profile); err != nil {
		return "", err
	}
	var vid string
	if profile["versionInfo"] != nil {
		vid, err = installForgeLegacy(p, &zr.Reader, profile, mc, logf)
	} else {
		vid, err = installForgeModern(p, &zr.Reader, installer, profile, vanilla, mc, javaOverride, logf, prog)
	}
	if err != nil {
		return "", err
	}
	os.WriteFile(marker, []byte(vid), 0o644)
	return vid, nil
}

// Forge for Minecraft 1.12.2 and older (old installer format).
func installForgeLegacy(p *Paths, z *zip.Reader, profile M, mc string, logf Logger) (string, error) {
	logf("Installing Forge (classic format)...")
	install := asM(profile["install"])
	vinfo := asM(profile["versionInfo"])
	if asS(vinfo["inheritsFrom"]) == "" {
		parent := asS(install["minecraft"])
		if parent == "" {
			parent = mc
		}
		vinfo["inheritsFrom"] = parent
	}
	data, err := zipRead(z, asS(install["filePath"]))
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(libPath(p, mavenPath(asS(install["path"]))), data); err != nil {
		return "", err
	}
	vid := asS(vinfo["id"])
	return vid, writeJSON(versionJSONPath(p, vid), vinfo)
}

func extractMaven(p *Paths, z *zip.Reader) {
	for _, f := range z.File {
		if !strings.HasPrefix(f.Name, "maven/") || f.FileInfo().IsDir() || strings.Contains(f.Name, "..") {
			continue
		}
		zipExtract(f, libPath(p, strings.TrimPrefix(f.Name, "maven/")))
	}
}

func libJobs(p *Paths, libraries []any) []dlJob {
	var jobs []dlJob
	for _, raw := range libraries {
		lib := asM(raw)
		art := asM(asM(lib["downloads"])["artifact"])
		rel := asS(art["path"])
		if rel == "" {
			rel = mavenPath(asS(lib["name"]))
		}
		dest := libPath(p, rel)
		if exists(dest) {
			continue
		}
		if u := asS(art["url"]); u != "" {
			jobs = append(jobs, dlJob{[]string{u}, dest, asS(art["sha1"])})
		} else if art == nil {
			jobs = append(jobs, dlJob{mirrorURLs(lib, rel), dest, ""})
		}
	}
	return jobs
}

func jarMainClass(jar string) (string, error) {
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	b, err := zipRead(&zr.Reader, "META-INF/MANIFEST.MF")
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n ", "")
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Main-Class:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Main-Class:")), nil
		}
	}
	return "", fmt.Errorf("no Main-Class in %s", filepath.Base(jar))
}

var tokenRe = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

func resolveProcessorArg(arg string, data map[string]string, p *Paths) string {
	if strings.HasPrefix(arg, "[") && strings.HasSuffix(arg, "]") {
		return libPath(p, mavenPath(arg[1:len(arg)-1]))
	}
	return tokenRe.ReplaceAllStringFunc(arg, func(m string) string {
		if v, ok := data[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

func buildProcessorData(p *Paths, z *zip.Reader, installer string, profile M, mc, work string) map[string]string {
	data := map[string]string{}
	for key, sides := range asM(profile["data"]) {
		val := asS(asM(sides)["client"])
		switch {
		case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
			data[key] = libPath(p, mavenPath(val[1:len(val)-1]))
		case strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") && len(val) >= 2:
			data[key] = val[1 : len(val)-1]
		default: // a file bundled inside the installer
			inner := strings.TrimLeft(val, "/")
			target := filepath.Join(work, filepath.FromSlash(inner))
			for _, f := range z.File {
				if f.Name == inner && !strings.Contains(inner, "..") {
					zipExtract(f, target)
				}
			}
			data[key] = target
		}
	}
	data["SIDE"] = "client"
	data["MINECRAFT_JAR"] = clientJarPath(p, mc)
	data["MINECRAFT_VERSION"] = mc
	data["ROOT"] = p.Root
	data["INSTALLER"] = installer
	data["LIBRARY_DIR"] = p.Libraries
	return data
}

func installForgeModern(p *Paths, z *zip.Reader, installer string, profile, vanilla M, mc, javaOverride string, logf Logger, prog Progress) (string, error) {
	jsonName := strings.TrimLeft(asS(profile["json"]), "/")
	if jsonName == "" {
		jsonName = "version.json"
	}
	vb, err := zipRead(z, jsonName)
	if err != nil {
		return "", err
	}
	var vjson M
	if err := json.Unmarshal(vb, &vjson); err != nil {
		return "", err
	}
	vid := asS(vjson["id"])
	if asS(vjson["inheritsFrom"]) == "" {
		vjson["inheritsFrom"] = mc
	}
	extractMaven(p, z)

	// the vanilla game jar is an input to the installer steps
	client := asM(asM(vanilla["downloads"])["client"])
	if err := download([]string{asS(client["url"])}, clientJarPath(p, mc), asS(client["sha1"]), nil); err != nil {
		return "", fmt.Errorf("could not download Minecraft %s: %v", mc, err)
	}
	jobs := append(libJobs(p, asA(profile["libraries"])), libJobs(p, asA(vjson["libraries"]))...)
	if errs := downloadMany(jobs, "Downloading mod loader", prog, 12); len(errs) > 0 {
		return "", fmt.Errorf("failed to download mod loader files: %s", strings.Join(firstN(errs, 5), "; "))
	}

	var procs []M
	for _, raw := range asA(profile["processors"]) {
		proc := asM(raw)
		sides := asA(proc["sides"])
		ok := sides == nil
		for _, s := range sides {
			if asS(s) == "client" {
				ok = true
			}
		}
		if ok {
			procs = append(procs, proc)
		}
	}
	if len(procs) > 0 {
		java := javaOverride
		if java == "" {
			if java, err = ensureJava(p, vanilla, logf, prog); err != nil {
				return "", err
			}
		}
		work := filepath.Join(p.Temp, "loader-work-"+vid)
		os.RemoveAll(work)
		os.MkdirAll(work, 0o755)
		defer os.RemoveAll(work)
		data := buildProcessorData(p, z, installer, profile, mc, work)
		for i, proc := range procs {
			if prog != nil {
				prog("Setting up the mod loader", i, len(procs))
			}
			outputs := asM(proc["outputs"])
			upToDate := len(outputs) > 0
			for k, v := range outputs {
				file := resolveProcessorArg(k, data, p)
				want := strings.Trim(resolveProcessorArg(asS(v), data, p), "'")
				if !exists(file) || sha1File(file) != want {
					upToDate = false
				}
			}
			if upToDate {
				continue
			}
			jar := libPath(p, mavenPath(asS(proc["jar"])))
			cp := []string{jar}
			for _, c := range asA(proc["classpath"]) {
				cp = append(cp, libPath(p, mavenPath(asS(c))))
			}
			main, err := jarMainClass(jar)
			if err != nil {
				return "", err
			}
			args := []string{"-cp", strings.Join(cp, classpathSep()), main}
			for _, a := range asA(proc["args"]) {
				args = append(args, resolveProcessorArg(asS(a), data, p))
			}
			cmd := hiddenCommand(java, args...)
			cmd.Dir = work
			out, err := cmd.CombinedOutput()
			if err != nil {
				lines := strings.Split(strings.TrimSpace(string(out)), "\n")
				if len(lines) > 20 {
					lines = lines[len(lines)-20:]
				}
				return "", fmt.Errorf("mod loader setup step %d of %d failed (%s):\n%s", i+1, len(procs), asS(proc["jar"]), strings.Join(lines, "\n"))
			}
		}
		if prog != nil {
			prog("Setting up the mod loader", len(procs), len(procs))
		}
	}
	return vid, writeJSON(versionJSONPath(p, vid), vjson)
}
