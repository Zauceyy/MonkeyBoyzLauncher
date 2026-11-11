package main

// End-to-end test with a mocked internet and a stand-in "Minecraft" (needs java and javac on PATH).
// It builds a fake vanilla version, a fake Forge-style installer with a real processor step and a
// CurseForge export zip, imports the zip, launches the result and checks what the game process saw.

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type route func(req *http.Request, body []byte) (int, []byte, string)

type mockRT struct{ routes map[string]route }

func (m *mockRT) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	status, data, loc := 404, []byte("not mocked: "+req.URL.String()), ""
	if h, ok := m.routes[req.URL.String()]; ok {
		status, data, loc = h(req, body)
	}
	hdr := http.Header{}
	if loc != "" {
		hdr.Set("Location", loc)
	}
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: hdr, Request: req,
		Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}, nil
}

func static(data []byte) route {
	return func(*http.Request, []byte) (int, []byte, string) { return 200, data, "" }
}

func sum(b []byte) string { h := sha1.Sum(b); return hex.EncodeToString(h[:]) }

func zipBytes(files map[string][]byte) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, data := range files {
		w, _ := z.Create(name)
		w.Write(data)
	}
	z.Close()
	return buf.Bytes()
}

// javacJar compiles Java sources and returns a jar containing the classes.
func javacJar(t *testing.T, tmp, name string, sources map[string]string, mainClass string, cp []string, extra map[string][]byte) []byte {
	t.Helper()
	src, out := filepath.Join(tmp, "src", name), filepath.Join(tmp, "out", name)
	os.MkdirAll(out, 0o755)
	args := []string{"-nowarn", "-d", out}
	if len(cp) > 0 {
		args = append(args, "-cp", strings.Join(cp, string(os.PathListSeparator)))
	}
	for rel, code := range sources {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(code), 0o644)
		args = append(args, p)
	}
	if b, err := exec.Command("javac", args...).CombinedOutput(); err != nil {
		t.Fatalf("javac %s: %v\n%s", name, err, b)
	}
	files := map[string][]byte{}
	if mainClass != "" {
		files["META-INF/MANIFEST.MF"] = []byte("Manifest-Version: 1.0\r\nMain-Class: " + mainClass + "\r\n\r\n")
	}
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".class") {
			rel, _ := filepath.Rel(out, p)
			files[filepath.ToSlash(rel)], _ = os.ReadFile(p)
		}
		return nil
	})
	for k, v := range extra {
		files[k] = v
	}
	jar := zipBytes(files)
	os.WriteFile(filepath.Join(tmp, "out", name+".jar"), jar, 0o644)
	return jar
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestUnits(t *testing.T) {
	eq := func(got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	eq(mavenPath("a.b:c:1.0"), "a/b/c/1.0/c-1.0.jar")
	eq(mavenPath("de.x:mcp:1.2:mappings@txt"), "de/x/mcp/1.2/mcp-1.2-mappings.txt")
	eq(offlineUUID("Notch"), "b50ad385829d3141a2167e7d7539ba7f")
	k, v, err := parseLoaderID("neoforge-21.1.77")
	if err != nil || k != "neoforge" || v != "21.1.77" {
		t.Fatal("loader id", k, v, err)
	}
	if _, _, err := parseLoaderID("rift-1.0"); err == nil {
		t.Fatal("unknown loader accepted")
	}
	eq(installerURLs("1.21.1", "neoforge", "21.1.77")[0], neoforgeMaven+"neoforge/21.1.77/neoforge-21.1.77-installer.jar")
	eq(installerURLs("1.20.1", "neoforge", "47.1.106")[0], neoforgeMaven+"forge/1.20.1-47.1.106/forge-1.20.1-47.1.106-installer.jar")
	eq(installerURLs("1.20.1", "forge", "47.2.0")[0], forgeMaven+"1.20.1-47.2.0/forge-1.20.1-47.2.0-installer.jar")
	if _, err := offlineAccount("no spaces"); err == nil {
		t.Fatal("bad name accepted")
	}
	if len(ramOptions()) == 0 || defaultMemoryMB() < 2048 {
		t.Fatal("ram options")
	}
}

func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not available")
	}
	tmp := t.TempDir()
	retryDelay, pollUnit = time.Millisecond, time.Millisecond
	rt := &mockRT{routes: map[string]route{}}
	httpClient = &http.Client{Transport: rt}
	R := rt.routes
	const B = "https://mock/"
	const MC, FV = "1.99", "9.9"
	out := func(n string) string { return filepath.Join(tmp, "out", n+".jar") }

	helper := javacJar(t, tmp, "helper", map[string]string{"com/example/Helper.java": `package com.example; public class Helper { public static String hi(){return "HELPER_OK";} }`}, "", nil, nil)
	patched := javacJar(t, tmp, "patched", map[string]string{"fake/Patched.java": `package fake; public class Patched { public static String hi(){return "PATCHED_OK";} }`}, "", nil, nil)
	client := javacJar(t, tmp, "client", map[string]string{"net/fake/Main.java": `package net.fake;
public class Main { public static void main(String[] a){ System.out.println("VANILLA_MAIN " + String.join(" ", a) + " " + com.example.Helper.hi()); } }`}, "", []string{out("helper")}, nil)
	forge := javacJar(t, tmp, "forge", map[string]string{"net/fake/ForgeMain.java": `package net.fake;
public class ForgeMain { public static void main(String[] a){
  System.out.println("FORGE_BOOT " + String.join(" ", a));
  System.out.println("LIBS " + com.example.Helper.hi() + " " + fake.Patched.hi());
  System.out.println("IGNORE " + System.getProperty("fake.ignore"));
  System.out.println("MEM " + (Runtime.getRuntime().maxMemory() / 1048576 > 400));
  System.out.println("NATIVES " + java.util.Arrays.toString(new java.io.File(System.getProperty("java.library.path")).list()));
  System.out.println("CWD " + System.getProperty("user.dir")); } }`}, "", []string{out("helper"), out("patched")}, nil)
	proc := javacJar(t, tmp, "proc", map[string]string{"proc/Proc.java": `package proc;
import java.nio.file.*;
public class Proc { public static void main(String[] a) throws Exception {
  String in=null,out=null,mc=null,lit=null;
  for(int i=0;i<a.length;i+=2){ if(a[i].equals("--in"))in=a[i+1]; if(a[i].equals("--out"))out=a[i+1];
    if(a[i].equals("--mc"))mc=a[i+1]; if(a[i].equals("--lit"))lit=a[i+1]; }
  if(!Files.exists(Paths.get(mc))) throw new RuntimeException("no minecraft jar " + mc);
  if(!"hello".equals(lit)) throw new RuntimeException("bad literal " + lit);
  Files.createDirectories(Paths.get(out).getParent());
  Files.copy(Paths.get(in), Paths.get(out), StandardCopyOption.REPLACE_EXISTING); } }`}, "proc.Proc", nil, nil)
	natives := zipBytes(map[string][]byte{"libfake.so": []byte("\x7fELF"), "fake.dll": []byte("MZ"), "libfake.dylib": []byte("x"), "META-INF/x": []byte("no")})
	asset := []byte("sound-bytes")

	otherOS := "windows"
	if osName() == "windows" {
		otherOS = "linux"
	}
	nativeCls := map[string]string{"windows": "natives-windows", "osx": "natives-macos", "linux": "natives-linux"}[osName()]
	vanilla := M{"id": MC, "type": "release", "mainClass": "net.fake.Main",
		"javaVersion": M{"component": "java-runtime-gamma", "majorVersion": 17},
		"assetIndex":  M{"id": "99", "url": B + "assets/99.json"},
		"downloads":   M{"client": M{"url": B + "client.jar", "sha1": sum(client)}},
		"arguments": M{
			"game": []any{"--username", "${auth_player_name}", "--uuid", "${auth_uuid}", "--gameDir", "${game_directory}",
				"--assetIndex", "${assets_index_name}", "--userType", "${user_type}",
				M{"rules": []any{M{"action": "allow", "features": M{"is_demo_user": true}}}, "value": "--demo"},
				M{"rules": []any{M{"action": "allow", "features": M{"has_custom_resolution": true}}}, "value": []any{"--width", "${resolution_width}"}}},
			"jvm": []any{M{"rules": []any{M{"action": "allow", "os": M{"name": otherOS}}}, "value": "-Dwrong.os=1"},
				"-Djava.library.path=${natives_directory}", "-cp", "${classpath}"}},
		"libraries": []any{
			M{"name": "com.example:helper:1.0", "downloads": M{"artifact": M{"path": "com/example/helper/1.0/helper-1.0.jar", "url": B + "helper.jar", "sha1": sum(helper)}}},
			M{"name": "com.example:wrongos:1.0", "rules": []any{M{"action": "allow", "os": M{"name": otherOS}}}, "downloads": M{"artifact": M{"path": "x/wrongos.jar", "url": B + "missing.jar"}}},
			M{"name": "org.fake:natives:1.0:" + nativeCls, "downloads": M{"artifact": M{"path": "org/fake/natives/1.0/natives-1.0-" + nativeCls + ".jar", "url": B + "natives.jar", "sha1": sum(natives)}}},
		}}
	forgeID := MC + "-fakeforge-" + FV
	forgeVersion := M{"id": forgeID, "inheritsFrom": MC, "mainClass": "net.fake.ForgeMain", "type": "release",
		"arguments": M{"game": []any{"--launchTarget", "forgeclient"}, "jvm": []any{"-Dfake.ignore=${version_name}.jar", "-Dlibdir=${library_directory}"}},
		"libraries": []any{
			M{"name": "net.fakeforge:forge:" + MC + "-" + FV, "downloads": M{"artifact": M{"path": "net/fakeforge/forge/" + MC + "-" + FV + "/forge-" + MC + "-" + FV + ".jar", "url": ""}}},
			M{"name": "net.fakeforge:patched:1.0:client", "downloads": M{"artifact": M{"path": "net/fakeforge/patched/1.0/patched-1.0-client.jar", "url": ""}}},
		}}
	profile := M{"spec": 1, "minecraft": MC, "json": "/version.json",
		"data": M{"PATCH": M{"client": "/data/client.jar", "server": "/data/server.jar"}, "OUT": M{"client": "[net.fakeforge:patched:1.0:client]"}, "LIT": M{"client": "'hello'"}},
		"processors": []any{
			M{"sides": []any{"server"}, "jar": "net.fakeforge:doesnotexist:1.0", "args": []any{}},
			M{"jar": "net.fakeforge:proc:1.0", "classpath": []any{"com.example:helper:1.0"},
				"args": []any{"--in", "{PATCH}", "--out", "{OUT}", "--mc", "{MINECRAFT_JAR}", "--lit", "{LIT}"}}},
		"libraries": []any{
			M{"name": "net.fakeforge:proc:1.0", "downloads": M{"artifact": M{"path": "net/fakeforge/proc/1.0/proc-1.0.jar", "url": ""}}},
			M{"name": "com.example:helper:1.0", "downloads": M{"artifact": M{"path": "com/example/helper/1.0/helper-1.0.jar", "url": B + "helper.jar", "sha1": sum(helper)}}}}}
	installer := zipBytes(map[string][]byte{
		"install_profile.json": mustJSON(profile), "version.json": mustJSON(forgeVersion), "data/client.jar": patched,
		"maven/net/fakeforge/proc/1.0/proc-1.0.jar":                                       proc,
		"maven/net/fakeforge/forge/" + MC + "-" + FV + "/forge-" + MC + "-" + FV + ".jar": forge})

	vb := mustJSON(vanilla)
	R[versionManifestURL] = static(mustJSON(M{"versions": []any{M{"id": MC, "type": "release", "url": B + "1.99.json", "sha1": sum(vb)}}}))
	R[B+"1.99.json"], R[B+"client.jar"], R[B+"helper.jar"], R[B+"natives.jar"] = static(vb), static(client), static(helper), static(natives)
	R[B+"assets/99.json"] = static(mustJSON(M{"objects": M{"minecraft/sounds/a.ogg": M{"hash": sum(asset), "size": len(asset)}}}))
	R[resourcesURL+sum(asset)[:2]+"/"+sum(asset)] = static(asset)
	R[forgeMaven+MC+"-"+FV+"/forge-"+MC+"-"+FV+"-installer.jar"] = static(installer)
	R[fmt.Sprintf(fabricMeta, MC, "0.1")] = static(mustJSON(M{"id": "fabric-loader-0.1-" + MC, "inheritsFrom": MC, "mainClass": "net.fake.Main",
		"arguments": M{"game": []any{}, "jvm": []any{"-DFabricMcEmu=x"}}, "libraries": []any{M{"name": "com.example:fab:2.0", "url": B + "maven/"}}}))
	R[B+"maven/com/example/fab/2.0/fab-2.0.jar"] = static(helper)

	// ---- CurseForge
	png := []byte("\x89PNG\r\n\x1a\n0000")
	modA := zipBytes(map[string][]byte{"META-INF/mods.toml": []byte("modLoader=\"javafml\"\n[[mods]]\nmodId=\"moda\"\nversion=\"1.2.3\"\ndisplayName=\"Jar Name A\"\nlogoFile=\"logo.png\"\n"), "logo.png": png})
	bundled := zipBytes(map[string][]byte{"fabric.mod.json": []byte(`{"id":"b","name":"Bundled Mod","version":"0.5","icon":"assets/b/icon.png"}`), "assets/b/icon.png": png})
	packB, modC := []byte("PK-respack"), zipBytes(map[string][]byte{"x": []byte("blocked")})
	cfFiles := map[int]M{
		1001: {"id": 1001, "modId": 11, "fileName": "Mod A+1.0.jar", "downloadUrl": B + "cdn/Mod A+1.0.jar", "hashes": []any{M{"value": sum(modA), "algo": 1}}},
		1002: {"id": 1002, "modId": 12, "fileName": "pack.zip", "downloadUrl": B + "cdn/pack.zip", "hashes": []any{}},
		1003: {"id": 1003, "modId": 13, "fileName": "blocked.jar", "downloadUrl": nil, "hashes": []any{}}}
	cfMods := map[int]M{
		11: {"id": 11, "name": "Mod A", "summary": "Does A things", "classId": 6, "links": M{"websiteUrl": "https://cf/mc-mods/a"}, "logo": M{"thumbnailUrl": "https://cdn/a.png"}, "authors": []any{M{"name": "alice"}}},
		12: {"id": 12, "name": "Pack", "classId": 12, "links": M{"websiteUrl": "https://cf/texture-packs/p"}},
		13: {"id": 13, "name": "Blocked", "classId": 6, "links": M{"websiteUrl": "https://cf/mc-mods/blocked"}}}
	R[cfAPI+"/mods/files"] = func(req *http.Request, body []byte) (int, []byte, string) {
		if req.Header.Get("x-api-key") != "KEY" {
			return 403, nil, ""
		}
		var in struct{ FileIds []int }
		json.Unmarshal(body, &in)
		var data []any
		for _, id := range in.FileIds {
			data = append(data, cfFiles[id])
		}
		return 200, mustJSON(M{"data": data}), ""
	}
	R[cfAPI+"/mods"] = func(req *http.Request, body []byte) (int, []byte, string) {
		var in struct{ ModIds []int }
		json.Unmarshal(body, &in)
		var data []any
		for _, id := range in.ModIds {
			data = append(data, cfMods[id])
		}
		return 200, mustJSON(M{"data": data}), ""
	}
	R[B+"cdn/Mod%20A+1.0.jar"], R[B+"cdn/pack.zip"] = static(modA), static(packB)
	R[fmt.Sprintf(cfSiteDownload, 11, 1001)] = func(*http.Request, []byte) (int, []byte, string) { return 302, nil, B + "files/1/1/Mod%20A+1.0.jar" }
	R[B+"files/1/1/Mod%20A+1.0.jar"] = static(modA)
	R[fmt.Sprintf(cfSiteDownload, 12, 1002)] = func(*http.Request, []byte) (int, []byte, string) { return 302, nil, B + "files/1/2/pack.zip" }
	R[B+"files/1/2/pack.zip"] = static(packB)

	makePack := func(name, loaderID string) string {
		manifest := M{"minecraft": M{"version": MC, "modLoaders": []any{M{"id": loaderID, "primary": true}}, "recommendedRam": 3000},
			"manifestType": "minecraftModpack", "manifestVersion": 1, "name": "Test: Pack?", "version": "2.3", "author": "me", "overrides": "overrides",
			"files": []any{M{"projectID": 11, "fileID": 1001, "required": true}, M{"projectID": 12, "fileID": 1002, "required": true}, M{"projectID": 13, "fileID": 1003, "required": true}}}
		p := filepath.Join(tmp, name)
		os.WriteFile(p, zipBytes(map[string][]byte{"manifest.json": mustJSON(manifest),
			"modlist.html":           []byte(`<ul><li><a href="https://cf/mc-mods/a">Mod A (by alice)</a></li><li><a href="https://cf/x">B &amp; C</a></li></ul>`),
			"overrides/config/a.cfg": []byte("x=1"), "overrides/mods/bundled.jar": bundled, "overrides/../../evil.txt": []byte("nope")}), 0o644)
		return p
	}

	minecraftDir = func() string { return filepath.Join(tmp, "no-minecraft-yet") }
	p := newPaths(filepath.Join(tmp, "data"))
	javaPath, _ := exec.LookPath("java")
	cfg := &Config{JavaPath: javaPath, CurseForgeAPIKey: "KEY", MemoryMB: 512}
	saveConfig(p, cfg)
	fakeDL := filepath.Join(tmp, "Downloads")
	os.MkdirAll(fakeDL, 0o755)
	downloadsFolder = func() string { return fakeDL }
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	check := func(ok bool, what string) {
		t.Helper()
		if !ok {
			t.Fatalf("FAIL: %s\nlogs:\n%s", what, strings.Join(logs, "\n"))
		}
		t.Log("ok  ", what)
	}

	// ---- Forge-style pack through the API
	inst, err := importPack(p, cfg, makePack("pack.zip", "forge-"+FV), logf, nil)
	check(err == nil, fmt.Sprint("import succeeds: ", err))
	g := inst.GameDir()
	check(inst.Loader == "forge" && inst.VersionID == forgeID, "instance records loader and version id")
	check(inst.Name == "Test: Pack?" && inst.ID == "Test_ Pack_", "name kept, folder name made safe")
	check(inst.MemoryMB == 3072, "pack's recommended memory mapped to a dropdown value")
	b, _ := os.ReadFile(filepath.Join(g, "config", "a.cfg"))
	check(string(b) == "x=1" && exists(filepath.Join(g, "mods", "bundled.jar")), "overrides extracted")
	check(!exists(filepath.Join(tmp, "evil.txt")) && !exists(filepath.Join(p.Instances, "evil.txt")) && !exists(filepath.Join(inst.Dir, "evil.txt")), "zip-slip entry ignored")
	check(exists(filepath.Join(g, "mods", "Mod A+1.0.jar")) && exists(filepath.Join(g, "resourcepacks", "pack.zip")), "mod and resource pack downloaded to the right folders")
	check(len(inst.Manual) == 1 && inst.Manual[0].FileName == "blocked.jar", "blocked mod listed for manual download")
	check(exists(libPath(p, "net/fakeforge/patched/1.0/patched-1.0-client.jar")), "installer processor produced its output")
	check(exists(filepath.Join(p.Assets, "objects", sum(asset)[:2], sum(asset))), "assets downloaded")

	// ---- the interface's API, including the mod list
	app := newApp(p, "tok")
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	call := func(method, path string, body any) (int, M) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(mustJSON(body))
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("X-Token", "tok")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out M
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	resp, _ := http.Get(srv.URL + "/api/state")
	check(resp.StatusCode == 403, "API refuses requests without the session token")
	_, st := call("GET", "/api/state", nil)
	iv := asM(asA(st["instances"])[0])
	check(asInt(iv["mods"]) == 2 && asInt(iv["manual_count"]) == 1 && len(asA(st["ram_options"])) > 0, "state lists the instance with mod and missing-file counts")
	_, ml := call("GET", "/api/mods?id="+url.QueryEscape(inst.ID), nil)
	byFile := map[string]M{}
	for _, m := range asA(ml["mods"]) {
		byFile[asS(asM(m)["file"])] = asM(m)
	}
	a := byFile["Mod A+1.0.jar"]
	check(asS(a["name"]) == "Mod A" && asS(a["version"]) == "1.2.3" && asS(a["logo"]) == "https://cdn/a.png" && asS(a["author"]) == "alice", "mod list merges CurseForge details with jar metadata")
	bm := byFile["bundled.jar"]
	check(asS(bm["name"]) == "Bundled Mod" && bm["has_icon"] == true, "bundled mod named from its own fabric.mod.json")
	check(len(asA(ml["pack_list"])) == 2 && asS(asM(asA(ml["pack_list"])[1])["name"]) == "B & C", "modlist.html parsed")
	req, _ := http.NewRequest("GET", srv.URL+"/api/modicon?id="+url.QueryEscape(inst.ID)+"&file=bundled.jar&t=tok", nil)
	ir, _ := http.DefaultClient.Do(req)
	ib, _ := io.ReadAll(ir.Body)
	check(ir.StatusCode == 200 && bytes.Equal(ib, png), "mod icon served from inside the jar")
	call("POST", "/api/mods/toggle", M{"id": inst.ID, "file": "bundled.jar", "enabled": false})
	check(exists(filepath.Join(g, "mods", "bundled.jar.disabled")) && countMods(inst) == 1, "mod switched off")
	code, _ := call("POST", "/api/mods/toggle", M{"id": inst.ID, "file": "../../instance.json", "enabled": false})
	check(code == 400, "toggle rejects paths outside the mods folder")
	call("POST", "/api/mods/toggle", M{"id": inst.ID, "all": true, "enabled": true})
	check(exists(filepath.Join(g, "mods", "bundled.jar")), "enable all switches it back on")
	call("POST", "/api/instance/update", M{"id": inst.ID, "memory_mb": 2048, "name": "Renamed"})
	check(findInstance(p, inst.ID).MemoryMB == 2048 && findInstance(p, inst.ID).Name == "Renamed", "memory and name saved per instance")
	code, pr := call("POST", "/api/play", M{"id": inst.ID})
	check(code == 400 && asS(pr["code"]) == "no_account", "play without an account asks for one")
	code, mr := call("POST", "/api/account/ms/start", M{})
	check(code == 400 && asS(mr["code"]) == "need_client_id", "Microsoft sign-in without a client ID explains the setup")

	// ---- Microsoft sign-in with mocked Microsoft/Xbox/Minecraft services
	polls := 0
	R[msDeviceURL] = static(mustJSON(M{"device_code": "DEV", "user_code": "ABCD1234", "verification_uri": "https://mock/link", "interval": 1, "expires_in": 60}))
	R[msTokenURL] = func(req *http.Request, body []byte) (int, []byte, string) {
		if strings.Contains(string(body), "refresh_token") {
			return 200, mustJSON(M{"access_token": "MS2", "refresh_token": "R2"}), ""
		}
		if polls++; polls < 3 {
			return 400, mustJSON(M{"error": "authorization_pending"}), ""
		}
		return 200, mustJSON(M{"access_token": "MS1", "refresh_token": "R1"}), ""
	}
	R[xblURL] = static(mustJSON(M{"Token": "XBL"}))
	R[xstsURL] = static(mustJSON(M{"Token": "XSTS", "DisplayClaims": M{"xui": []any{M{"uhs": "UHS"}}}}))
	R[mcLoginURL] = func(req *http.Request, body []byte) (int, []byte, string) {
		if !strings.Contains(string(body), "XBL3.0 x=UHS;XSTS") {
			return 400, nil, ""
		}
		return 200, mustJSON(M{"access_token": "MCTOKEN", "expires_in": 86400}), ""
	}
	R[mcProfile] = func(req *http.Request, _ []byte) (int, []byte, string) {
		if req.Header.Get("Authorization") != "Bearer MCTOKEN" {
			return 401, nil, ""
		}
		return 200, mustJSON(M{"id": "0123456789abcdef0123456789abcdef", "name": "MonkeyKing"}), ""
	}
	d, err := startDeviceLogin("CLIENT")
	check(err == nil && d.UserCode == "ABCD1234", "device sign-in starts and returns a code")
	msAcct, err := finishDeviceLogin(d, make(chan struct{}))
	check(err == nil && msAcct.Name == "MonkeyKing" && msAcct.AccessToken == "MCTOKEN" && msAcct.RefreshToken == "R1" && polls == 3, fmt.Sprint("sign-in completes after polling: ", err))
	msAcct.ExpiresAt = 1
	fresh, err := refreshAccount(msAcct)
	check(err == nil && fresh.RefreshToken == "R2" && fresh.ExpiresAt > time.Now().Unix(), "expired sign-in refreshes silently")
	R[mcLoginURL] = func(*http.Request, []byte) (int, []byte, string) { return 403, nil, "" }
	_, err = refreshAccount(msAcct)
	check(err != nil && strings.Contains(err.Error(), "approved by Mojang"), "unapproved client ID gives a clear message")

	// ---- launch through the API with an offline player
	os.WriteFile(filepath.Join(fakeDL, "blocked.jar"), modC, 0o644)
	_, cr := call("POST", "/api/manual/check", M{"id": inst.ID})
	check(asInt(cr["found"]) == 1 && asInt(cr["missing"]) == 0 && exists(filepath.Join(g, "mods", "blocked.jar")), "manual download picked up from Downloads")
	call("POST", "/api/account/offline", M{"name": "Steve_01"})
	code, _ = call("POST", "/api/play", M{"id": inst.ID})
	check(code == 200, "play accepted")
	var text string
	for i := 0; i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
		_, st = call("GET", "/api/state", nil)
		var sb strings.Builder
		for _, l := range asA(st["logs"]) {
			sb.WriteString(asS(asM(l)["text"]) + "\n")
		}
		text = sb.String()
		if st["busy"] == false && asS(st["running"]) == "" && strings.Contains(text, "The game closed") {
			break
		}
	}
	t.Log(text)
	uuid := offlineUUID("Steve_01")
	check(strings.Contains(text, "The game closed normally"), "game process exited cleanly")
	check(strings.Contains(text, "FORGE_BOOT --username Steve_01 --uuid "+uuid), "loader main class, vanilla arguments and account")
	check(strings.Contains(text, "--launchTarget forgeclient") && !strings.Contains(text, "--demo") && !strings.Contains(text, "--width") && !strings.Contains(text, "wrong.os"), "loader arguments appended, rules respected")
	check(strings.Contains(text, "LIBS HELPER_OK PATCHED_OK"), "classpath has vanilla, loader and processor-generated libraries")
	check(strings.Contains(text, "IGNORE "+forgeID+".jar") && exists(clientJarPath(p, forgeID)), "game jar named after the launched version")
	check(strings.Contains(text, "MEM true") && strings.Contains(text, "with 2048 MB"), "per-instance memory used")
	check(strings.Contains(text, "libfake.so") || strings.Contains(text, "fake.dll") || strings.Contains(text, "libfake.dylib"), "natives extracted")
	check(strings.Contains(text, "CWD "+g), "game runs inside the instance folder")
	check(findInstance(p, inst.ID).LastPlayed > 0, "last played recorded")

	// ---- Fabric-style pack without an API key, through the upload endpoint
	app.mu.Lock()
	app.cfg.CurseForgeAPIKey = ""
	app.mu.Unlock()
	zb, _ := os.ReadFile(makePack("pack2.zip", "fabric-0.1"))
	ureq, _ := http.NewRequest("POST", srv.URL+"/api/import", bytes.NewReader(zb))
	ureq.Header.Set("X-Token", "tok")
	ureq.Header.Set("X-Filename", "pack2.zip")
	ur, _ := http.DefaultClient.Do(ureq)
	check(ur.StatusCode == 200, "upload accepted")
	for i := 0; i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
		if _, st = call("GET", "/api/state", nil); st["busy"] == false {
			break
		}
	}
	notice := asM(st["notice"])
	check(asS(notice["kind"]) == "ok" && strings.Contains(asS(notice["text"]), "1 file(s) need a manual download"), fmt.Sprint("import reports the one failed file: ", notice["text"]))
	inst2 := findInstance(p, asS(notice["instance"]))
	check(inst2 != nil && inst2.ID != inst.ID && inst2.Loader == "fabric", "second import gets its own folder")
	check(exists(filepath.Join(inst2.GameDir(), "mods", "Mod A+1.0.jar")) && exists(filepath.Join(inst2.GameDir(), "resourcepacks", "pack.zip")), "website fallback downloads")
	vjson, java, err := prepareInstance(p, cfg, inst2, logf, nil)
	check(err == nil, fmt.Sprint("fabric-style prepare: ", err))
	acct, _ := offlineAccount("Steve_01")
	cmd, pipe, err := launchGame(p, vjson, java, inst2.GameDir(), inst2.ID, acct, 512, nil)
	check(err == nil, "fabric-style launch starts")
	ob, _ := io.ReadAll(pipe)
	check(cmd.Wait() == nil && strings.Contains(string(ob), "VANILLA_MAIN --username Steve_01") && strings.Contains(string(ob), "HELPER_OK"), "fabric-style profile launches")

	// ---- play through the official Minecraft Launcher
	mcDir := filepath.Join(tmp, "dotminecraft")
	minecraftDir = func() string { return mcDir }
	opened := 0
	launcherOpen := true
	officialLauncherRunning = func() bool { return launcherOpen }
	closeOfficialLauncher = func() {}
	launcherCloseWait = time.Millisecond
	openOfficialLauncher = func() bool { opened++; return true }
	waitIdle := func() M {
		for i := 0; i < 300; i++ {
			time.Sleep(30 * time.Millisecond)
			if _, st = call("GET", "/api/state", nil); st["busy"] == false {
				break
			}
		}
		return asM(st["notice"])
	}
	check(asS(asM(st["settings"])["play_via"]) == "builtin", "without the Minecraft Launcher, Play starts the game directly")
	call("POST", "/api/play", M{"id": inst.ID, "via": "official"})
	n := waitIdle()
	check(asS(n["kind"]) == "error" && strings.Contains(asS(n["text"]), "open it once"), "missing Minecraft Launcher explained")
	os.MkdirAll(mcDir, 0o755)
	os.WriteFile(filepath.Join(mcDir, "launcher_profiles.json"), []byte(`{"profiles":{"abc":{"name":"","type":"latest-release","lastVersionId":"latest-release"}},"settings":{"keepLauncherOpen":true,"big":12345678901234567890},"version":3}`), 0o644)
	os.WriteFile(filepath.Join(mcDir, "launcher_profiles_microsoft_store.json"), []byte(`{"profiles":{},"version":3}`), 0o644)
	_, st = call("GET", "/api/state", nil)
	check(asS(asM(st["settings"])["play_via"]) == "official", "with the Minecraft Launcher present, Play uses it by default")
	call("POST", "/api/play", M{"id": inst.ID})
	n = waitIdle()
	check(asS(n["kind"]) == "error" && strings.Contains(asS(n["text"]), "still open") && opened == 0, "stops if the Minecraft Launcher will not close")
	closeOfficialLauncher = func() { launcherOpen = false }
	call("POST", "/api/play", M{"id": inst.ID})
	n = waitIdle()
	check(asS(n["kind"]) == "ok" && opened == 1 && !launcherOpen, fmt.Sprint("Play closes an open Minecraft Launcher, adds the pack and opens it again: ", n["text"]))
	call("POST", "/api/play", M{"id": inst.ID})
	n = waitIdle()
	check(asS(n["kind"]) == "ok" && opened == 2, "Play opens the Minecraft Launcher when it was not running")

	// ---- the player signed in to the Minecraft Launcher
	os.WriteFile(filepath.Join(mcDir, "launcher_accounts.json"), []byte(`{"accounts":{"aaa":{"minecraftProfile":{"id":"11111111111111111111111111111111","name":"Other"},"accessToken":"SECRET"},"bbb":{"minecraftProfile":{"id":"0123456789abcdef0123456789abcdef","name":"MonkeyKing"},"accessToken":"SECRET"}},"activeAccountLocalId":"bbb"}`), 0o644)
	_, st = call("GET", "/api/state", nil)
	check(asS(asM(st["settings"])["official_name"]) == "MonkeyKing" && asS(asM(st["settings"])["official_uuid"]) == "0123456789abcdef0123456789abcdef" && !strings.Contains(fmt.Sprint(st), "SECRET"), "active Minecraft Launcher player shown, tokens not exposed")
	skin := []byte("\x89PNG\r\n\x1a\nskin-pixels")
	tex := base64.StdEncoding.EncodeToString(mustJSON(M{"textures": M{"SKIN": M{"url": "http://textures.minecraft.net/texture/abc"}}}))
	R[sessionProfileURL+"0123456789abcdef0123456789abcdef"] = static(mustJSON(M{"id": "x", "name": "MonkeyKing", "properties": []any{M{"name": "textures", "value": tex}}}))
	R["https://textures.minecraft.net/texture/abc"] = static(skin)
	sreq, _ := http.NewRequest("GET", srv.URL+"/api/official/skin?uuid=0123456789abcdef0123456789abcdef&t=tok", nil)
	sr, _ := http.DefaultClient.Do(sreq)
	sbody, _ := io.ReadAll(sr.Body)
	check(sr.StatusCode == 200 && bytes.Equal(sbody, skin), "player skin fetched from Mojang for the face picture")
	sreq, _ = http.NewRequest("GET", srv.URL+"/api/official/skin?uuid=../../x&t=tok", nil)
	sr, _ = http.DefaultClient.Do(sreq)
	check(sr.StatusCode == 404, "skin endpoint rejects anything that is not a player id")
	check(exists(filepath.Join(mcDir, "versions", forgeID, forgeID+".json")) && exists(filepath.Join(mcDir, "versions", MC, MC+".json")), "mod loader version installed into the Minecraft Launcher folder")
	check(exists(filepath.Join(mcDir, "libraries", "net", "fakeforge", "patched", "1.0", "patched-1.0-client.jar")) && exists(filepath.Join(mcDir, "libraries", "net", "fakeforge", "forge", MC+"-"+FV, "forge-"+MC+"-"+FV+".jar")), "mod loader libraries installed there, including the generated one")
	for _, f := range profileFiles {
		var doc M
		raw, _ := os.ReadFile(filepath.Join(mcDir, f))
		json.Unmarshal(raw, &doc)
		pr := asM(asM(doc["profiles"])["monkeyboyz-"+inst.ID])
		check(asS(pr["name"]) == "Renamed" && asS(pr["lastVersionId"]) == forgeID && asS(pr["gameDir"]) == g && strings.HasPrefix(asS(pr["javaArgs"]), "-Xmx2048M ") && asS(pr["type"]) == "custom", "installation entry written to "+f)
		if f == "launcher_profiles.json" {
			check(asM(doc["profiles"])["abc"] != nil && strings.Contains(string(raw), "12345678901234567890") && asM(doc["settings"])["keepLauncherOpen"] == true && exists(filepath.Join(mcDir, f+".monkeyboyz-backup")), "existing installations and settings preserved, backup kept")
		}
	}
	call("POST", "/api/settings", M{"play_via": "builtin"})
	_, st = call("GET", "/api/state", nil)
	check(asS(asM(st["settings"])["play_via"]) == "builtin", "play mode can be switched back in Settings")

	bad := filepath.Join(tmp, "bad.zip")
	os.WriteFile(bad, zipBytes(map[string][]byte{"readme.txt": []byte("hi")}), 0o644)
	_, err = importPack(p, cfg, bad, logf, nil)
	check(err != nil && strings.Contains(err.Error(), "Export Profile"), "a zip that is not a CurseForge export is refused with a clear message")
	call("POST", "/api/instance/delete", M{"id": inst2.ID})
	check(findInstance(p, inst2.ID) == nil, "instance deleted")
	call("POST", "/api/instance/delete", M{"id": inst.ID})
	raw, _ := os.ReadFile(filepath.Join(mcDir, "launcher_profiles.json"))
	check(!strings.Contains(string(raw), "monkeyboyz-") && strings.Contains(string(raw), "abc"), "deleting a pack removes its Minecraft Launcher entry only")
}
