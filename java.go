package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
)

// Mojang publishes the Java runtimes the official launcher uses; we install the same ones.
var runtimeIndexURL = "https://launchermeta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"

var majorToComponent = map[int]string{8: "jre-legacy", 16: "java-runtime-alpha", 17: "java-runtime-gamma", 21: "java-runtime-delta"}

func mojangPlatform() string {
	arm := runtime.GOARCH == "arm64"
	switch runtime.GOOS {
	case "windows":
		if arm {
			return "windows-arm64"
		}
		if archBits() == "32" {
			return "windows-x86"
		}
		return "windows-x64"
	case "darwin":
		if arm {
			return "mac-os-arm64"
		}
		return "mac-os"
	}
	if archBits() == "32" {
		return "linux-i386"
	}
	return "linux"
}

func javaRequirement(vjson M) (string, int) {
	jv := asM(vjson["javaVersion"])
	major := asInt(jv["majorVersion"])
	if major == 0 {
		major = 8
	}
	comp := asS(jv["component"])
	if comp == "" {
		comp = majorToComponent[major]
		if comp == "" {
			comp = "jre-legacy"
		}
	}
	return comp, major
}

func javaExe(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "bin", "java.exe")
	}
	mac := filepath.Join(home, "jre.bundle", "Contents", "Home", "bin", "java")
	if exists(mac) {
		return mac
	}
	return filepath.Join(home, "bin", "java")
}

var javaVersionRe = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

func systemJavaMajor(java string) int {
	out, err := hiddenCommand(java, "-version").CombinedOutput()
	if err != nil {
		return 0
	}
	m := javaVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return 0
	}
	major, _ := strconv.Atoi(m[1])
	if major == 1 && m[2] != "" {
		major, _ = strconv.Atoi(m[2])
	}
	return major
}

func findSystemJava(major int) string {
	var candidates []string
	exe := "java"
	if runtime.GOOS == "windows" {
		exe = "java.exe"
	}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, "bin", exe))
	}
	if w, err := exec.LookPath("java"); err == nil {
		candidates = append(candidates, w)
	}
	for _, c := range candidates {
		if exists(c) && systemJavaMajor(c) == major {
			return c
		}
	}
	return ""
}

// ensureJava returns a java executable suitable for this Minecraft version, installing it if needed.
func ensureJava(p *Paths, vjson M, logf Logger, prog Progress) (string, error) {
	comp, major := javaRequirement(vjson)
	home := filepath.Join(p.Runtime, comp)
	marker := filepath.Join(home, ".installed.json")
	if exists(marker) && exists(javaExe(home)) {
		return javaExe(home), nil
	}
	plat := mojangPlatform()
	logf("Installing Java %d (%s)...", major, comp)

	manifest, version, err := fetchRuntimeManifest(plat, comp)
	if err != nil {
		if sys := findSystemJava(major); sys != "" {
			logf("Using the Java already on this computer: %s", sys)
			return sys, nil
		}
		return "", fmt.Errorf("could not install Java %d: %v. Install Java %d yourself and set its path in Settings", major, err, major)
	}

	var jobs []dlJob
	var execs []string
	type link struct{ path, target string }
	var links []link
	for rel, raw := range asM(manifest["files"]) {
		info := asM(raw)
		target := filepath.Join(home, filepath.FromSlash(rel))
		switch asS(info["type"]) {
		case "directory":
			os.MkdirAll(target, 0o755)
		case "file":
			r := asM(asM(info["downloads"])["raw"])
			jobs = append(jobs, dlJob{[]string{asS(r["url"])}, target, asS(r["sha1"])})
			if b, _ := info["executable"].(bool); b {
				execs = append(execs, target)
			}
		case "link":
			links = append(links, link{target, asS(info["target"])})
		}
	}
	if errs := downloadMany(jobs, fmt.Sprintf("Downloading Java %d", major), prog, 12); len(errs) > 0 {
		return "", fmt.Errorf("Java download failed: %v", firstN(errs, 3))
	}
	for _, e := range execs {
		os.Chmod(e, 0o755)
	}
	for _, l := range links {
		if !exists(l.path) && l.target != "" {
			os.MkdirAll(filepath.Dir(l.path), 0o755)
			os.Symlink(l.target, l.path)
		}
	}
	writeJSON(marker, M{"component": comp, "major": major, "version": version})
	return javaExe(home), nil
}

func fetchRuntimeManifest(plat, comp string) (M, string, error) {
	idx, err := getJSON(runtimeIndexURL, nil)
	if err != nil {
		return nil, "", err
	}
	entries := asA(asM(asM(idx)[plat])[comp])
	if len(entries) == 0 && plat == "mac-os-arm64" { // Rosetta fallback, e.g. Java 8
		entries = asA(asM(asM(idx)["mac-os"])[comp])
	}
	if len(entries) == 0 && plat == "windows-arm64" {
		entries = asA(asM(asM(idx)["windows-x64"])[comp])
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("Mojang has no %s runtime for %s", comp, plat)
	}
	e := asM(entries[0])
	m, err := getJSON(asS(asM(e["manifest"])["url"]), nil)
	if err != nil {
		return nil, "", err
	}
	return asM(m), asS(asM(e["version"])["name"]), nil
}
