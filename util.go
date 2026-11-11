package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	appName    = "MonkeyBoyz Launcher"
	appVersion = "2.5.0"
	userAgent  = "MonkeyBoyzLauncher/" + appVersion
)

// ---------------------------------------------------------------- paths

type Paths struct {
	Root, Versions, Libraries, Assets, Runtime, Instances, Natives, Temp, Config string
}

func defaultDataDir() string {
	if env := os.Getenv("MONKEYBOYZ_HOME"); env != "" {
		return env
	}
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = home
	}
	return filepath.Join(base, "MonkeyBoyzLauncher")
}

func newPaths(root string) *Paths {
	if root == "" {
		root = defaultDataDir()
	}
	p := &Paths{Root: root,
		Versions: filepath.Join(root, "versions"), Libraries: filepath.Join(root, "libraries"),
		Assets: filepath.Join(root, "assets"), Runtime: filepath.Join(root, "runtime"),
		Instances: filepath.Join(root, "instances"), Natives: filepath.Join(root, "natives"),
		Temp: filepath.Join(root, "temp"), Config: filepath.Join(root, "config.json")}
	for _, d := range []string{p.Versions, p.Libraries, p.Assets, p.Runtime, p.Instances, p.Natives, p.Temp} {
		os.MkdirAll(d, 0o755)
	}
	return p
}

// ---------------------------------------------------------------- OS

func osName() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "osx"
	}
	return "linux"
}

func archBits() string {
	if runtime.GOARCH == "386" || runtime.GOARCH == "arm" {
		return "32"
	}
	return "64"
}

func classpathSep() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

// ---------------------------------------------------------------- dynamic JSON helpers

type M = map[string]any

func asM(v any) M      { m, _ := v.(map[string]any); return m }
func asA(v any) []any  { a, _ := v.([]any); return a }
func asS(v any) string { s, _ := v.(string); return s }
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func readJSON(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &v); err != nil {
		return nil, err
	}
	return v, nil
}

func readJSONInto(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), out)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, b []byte) error {
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------------------------------------------------------------- maven

// mavenPath turns "group:artifact:version[:classifier][@ext]" into a relative repo path.
func mavenPath(coord string) string {
	ext := "jar"
	if i := strings.Index(coord, "@"); i >= 0 {
		ext = coord[i+1:]
		coord = coord[:i]
	}
	parts := strings.Split(coord, ":")
	if len(parts) < 3 {
		return coord
	}
	name := parts[1] + "-" + parts[2]
	if len(parts) > 3 {
		name += "-" + parts[3]
	}
	segs := append(strings.Split(parts[0], "."), parts[1], parts[2], name+"."+ext)
	return strings.Join(segs, "/")
}

// mavenKey identifies a library ignoring its version.
func mavenKey(coord string) string {
	if i := strings.Index(coord, "@"); i >= 0 {
		coord = coord[:i]
	}
	parts := strings.Split(coord, ":")
	if len(parts) < 2 {
		return coord
	}
	key := parts[0] + ":" + parts[1]
	if len(parts) > 3 {
		key += ":" + parts[3]
	}
	return key
}

func libPath(p *Paths, rel string) string {
	return filepath.Join(p.Libraries, filepath.FromSlash(rel))
}

// ---------------------------------------------------------------- hashing

func sha1File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha1.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- HTTP

var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 60 * time.Second,
	MaxIdleConnsPerHost:   16,
}}

var retryDelay = 1500 * time.Millisecond

type HTTPError struct {
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("HTTP %d for %s %s", e.Status, e.URL, e.Body))
}

func httpDo(method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return httpClient.Do(req)
}

func httpBytes(method, url string, body []byte, headers map[string]string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := httpDo(method, url, body, headers)
		if err != nil {
			last = err
		} else {
			data, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if rerr == nil {
					return data, nil
				}
				last = rerr
			} else {
				snippet := string(data)
				if len(snippet) > 300 {
					snippet = snippet[:300]
				}
				he := &HTTPError{url, resp.StatusCode, snippet}
				if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
					return nil, he
				}
				last = he
			}
		}
		time.Sleep(retryDelay * time.Duration(attempt+1))
	}
	return nil, last
}

func getJSON(url string, headers map[string]string) (any, error) {
	h := map[string]string{"Accept": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	b, err := httpBytes("GET", url, nil, h)
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(b, &v)
}

func postJSON(url string, payload any, headers map[string]string) (any, error) {
	h := map[string]string{"Accept": "application/json", "Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	body, _ := json.Marshal(payload)
	b, err := httpBytes("POST", url, body, h)
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(b, &v)
}

var partCounter int64

// download fetches the first working URL to dest atomically, verifying sha1 when given.
// A file that already exists is trusted (it was verified when first written).
func download(urls []string, dest, sum string, headers map[string]string) error {
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return nil
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := fmt.Sprintf("%s.part%d", dest, atomic.AddInt64(&partCounter, 1))
	defer os.Remove(tmp)
	var last error
	for _, u := range urls {
		if u == "" {
			continue
		}
		for attempt := 0; attempt < 3; attempt++ {
			err := fetchTo(u, tmp, headers)
			if err == nil && sum != "" && !strings.EqualFold(sha1File(tmp), sum) {
				err = fmt.Errorf("checksum mismatch for %s", u)
			}
			if err == nil {
				return os.Rename(tmp, dest)
			}
			last = err
			if he, ok := err.(*HTTPError); ok && he.Status >= 400 && he.Status < 500 && he.Status != 429 {
				break
			}
			time.Sleep(retryDelay * time.Duration(attempt+1))
		}
	}
	if last == nil {
		last = fmt.Errorf("no download URL for %s", filepath.Base(dest))
	}
	return last
}

func fetchTo(url, tmp string, headers map[string]string) error {
	resp, err := httpDo("GET", url, nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{url, resp.StatusCode, ""}
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

type dlJob struct {
	URLs []string
	Dest string
	SHA1 string
}

type Progress func(label string, done, total int)
type Logger func(format string, args ...any)

// downloadMany downloads in parallel and returns error messages (empty when all succeeded).
func downloadMany(jobs []dlJob, label string, prog Progress, workers int) []string {
	seen := map[string]bool{}
	var todo []dlJob
	for _, j := range jobs {
		if !seen[j.Dest] {
			seen[j.Dest] = true
			todo = append(todo, j)
		}
	}
	total := len(todo)
	if prog != nil {
		prog(label, 0, total)
	}
	if total == 0 {
		return nil
	}
	var mu sync.Mutex
	var errs []string
	done := 0
	ch := make(chan dlJob)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				err := download(j.URLs, j.Dest, j.SHA1, nil)
				mu.Lock()
				if err != nil {
					errs = append(errs, filepath.Base(j.Dest)+": "+err.Error())
				}
				done++
				if prog != nil {
					prog(label, done, total)
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range todo {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return errs
}

func safeName(name string) string {
	var b strings.Builder
	for _, c := range name {
		if strings.ContainsRune(`<>:"/\|?*`, c) || c < 32 {
			b.WriteRune('_')
		} else {
			b.WriteRune(c)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		out = "Instance"
	}
	return out
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func firstN(items []string, n int) []string {
	if len(items) > n {
		return items[:n]
	}
	return items
}
