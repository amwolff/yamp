package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/config"
	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/settings"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
	"github.com/amwolff/yamp/internal/transcode"
)

type testServer struct {
	*httptest.Server
	st     *store.Store
	sets   *settings.Settings
	lib    string
	client *http.Client
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o750); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "t.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sets, err := settings.Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if err = transcode.SeedPresets(ctx, st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	noFFmpeg := func(context.Context, string, ...string) ([]byte, error) { return nil, fmt.Errorf("no ffmpeg") }
	caps := transcode.Detect(ctx, noFFmpeg, "ffmpeg", "")
	hub := events.New()
	cfg := config.Config{Libraries: []string{lib}, DataDir: dir, CacheDir: filepath.Join(dir, "cache")}
	queue := transcode.NewQueue(transcode.QueueConfig{Store: st, Caps: caps, CacheDir: cfg.CacheDir, Hub: hub, Log: log, Workers: sets.Workers})
	srv, err := New(Deps{
		Config: cfg, Store: st, Settings: sets, Caps: caps, Hub: hub, Log: log, Queue: queue,
		Auth:    auth.NewManager(st, false, auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1}),
		Scanner: library.New(library.Config{Roots: cfg.Libraries, Store: st, Log: log, Salt: sets.Salt}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &testServer{Server: ts, st: st, sets: sets, lib: lib, client: client}
}

// result is a finished response.
type result struct {
	StatusCode int
	Header     http.Header
}

// do sends a request like a same-origin browser page would.
func (ts *testServer) do(t *testing.T, method, path string, form url.Values, hdr ...string) (result, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Accept", "text/html")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return result{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

func (ts *testServer) setup(t *testing.T) {
	t.Helper()
	resp, _ := ts.do(t, "POST", "/setup", url.Values{
		"username": {"admin"}, "password": {"password123"}, "password2": {"password123"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// addMedia inserts a media row with a real file behind it.
func (ts *testServer) addMedia(t *testing.T, n int, title string) db.Medium {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(ts.lib, "Secret Folder", title+".mp4")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := []byte(fmt.Sprintf("fingerprint-%d", n))
	now := time.Now().Unix() + int64(n)
	id, err := ts.st.InsertMedia(ctx, db.InsertMediaParams{
		Path: path, Root: ts.lib, RelDir: "Secret Folder", Title: title, Size: 1000, MtimeNs: 1,
		Fingerprint: fp, Pid: privacy.PID(ts.sets.Salt(), fp), DurationMs: 120_000, Container: "mp4",
		Width: 1280, Height: 720, VideoCodec: "h264", AudioCodec: "aac", Mime: "video/mp4",
		Codecs: "avc1.64001F, mp4a.40.2", Playability: "direct", ProbeJson: "{}", AddedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ts.st.GetMedia(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var titleRe = regexp.MustCompile(`<title>([^<]*)</title>`)

func TestAuthFlow(t *testing.T) {
	ts := newTestServer(t)
	if resp, _ := ts.do(t, "GET", "/", nil); resp.Header.Get("Location") != "/setup" {
		t.Fatalf("first start should go to setup, got %q", resp.Header.Get("Location"))
	}
	if resp, _ := ts.do(t, "GET", "/grid", nil, "HX-Request", "true"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("htmx without session: %d", resp.StatusCode)
	}
	bad, body := ts.do(t, "POST", "/setup", url.Values{"username": {"a"}, "password": {"password123"}, "password2": {"different"}})
	if bad.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not the same") {
		t.Fatalf("mismatched passwords: %d", bad.StatusCode)
	}
	ts.setup(t)
	if resp, _ := ts.do(t, "GET", "/", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("home after setup: %d", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "GET", "/setup", nil); resp.Header.Get("Location") != "/login" {
		t.Fatal("setup page still open after setup")
	}
	if resp, _ := ts.do(t, "POST", "/logout", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "GET", "/", nil); resp.Header.Get("Location") != "/login" {
		t.Fatalf("after logout: %q", resp.Header.Get("Location"))
	}
	resp, _ := ts.do(t, "POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong-password"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = ts.do(t, "POST", "/login", url.Values{"username": {"admin"}, "password": {"password123"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	m := ts.addMedia(t, 1, "Movie")
	resp, _ := ts.do(t, "POST", "/api/rating/"+m.Pid, url.Values{"stars": {"5"}}, "Sec-Fetch-Site", "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
	resp, _ = ts.do(t, "POST", "/api/rating/"+m.Pid, url.Values{"stars": {"5"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin POST: %d", resp.StatusCode)
	}
}

func TestPrivacy(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	const secret = "Very Private Title"
	m := ts.addMedia(t, 1, secret)

	resp, body := ts.do(t, "GET", "/v/"+m.Pid, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch: %d", resp.StatusCode)
	}
	if got := titleRe.FindStringSubmatch(body); got == nil || got[1] != m.Pid+" - YAMP" {
		t.Fatalf("title = %v", got)
	}
	if !strings.Contains(body, secret) {
		t.Fatal("the page body should show the title")
	}
	for _, page := range []string{"/", "/v/" + m.Pid, "/admin/jobs", "/admin/stats"} {
		_, body := ts.do(t, "GET", page, nil)
		for _, attr := range regexp.MustCompile(`(?:href|src|action|hx-get|hx-post)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			if strings.Contains(attr[1], "Private") || strings.Contains(attr[1], "Secret") {
				t.Errorf("%s: a URL contains a name: %s", page, attr[1])
			}
		}
		if got := titleRe.FindStringSubmatch(body); got != nil && strings.Contains(got[1], "Private") {
			t.Errorf("%s: title leaks the name: %s", page, got[1])
		}
	}
	h := resp.Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("missing privacy headers: %v", h)
	}
}

func TestMediaRange(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	m := ts.addMedia(t, 1, "Movie")
	resp, body := ts.do(t, "GET", "/media/"+m.Pid+"/original", nil, "Range", "bytes=10-19")
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 {
		t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(body))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 10-19/1000" {
		t.Fatalf("Content-Range = %q", cr)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, m.Pid+".mp4") || strings.Contains(cd, "Movie") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if resp, _ := ts.do(t, "GET", "/media/0000000000000000/original", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown pid: %d", resp.StatusCode)
	}
}

func TestGridPagingAndFolders(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	for i := range 60 {
		ts.addMedia(t, i, fmt.Sprintf("Video %02d", i))
	}
	_, body := ts.do(t, "GET", "/", nil)
	if n := strings.Count(body, `class="card"`); n != pageSize {
		t.Fatalf("first page has %d cards", n)
	}
	next := regexp.MustCompile(`hx-get="(/grid\?[^"]*)"`).FindAllStringSubmatch(body, -1)
	var nextURL string
	for _, m := range next {
		if strings.Contains(m[1], "offset=") {
			nextURL = strings.ReplaceAll(m[1], "&amp;", "&")
		}
	}
	if nextURL == "" {
		t.Fatal("no next-page sentinel")
	}
	_, body = ts.do(t, "GET", nextURL, nil, "HX-Request", "true")
	if n := strings.Count(body, `class="card"`); n != 12 || strings.Contains(body, "sentinel") {
		t.Fatalf("second page has %d cards", n)
	}
	_, body = ts.do(t, "GET", "/grid?q=video+07", nil, "HX-Request", "true")
	if n := strings.Count(body, `class="card"`); n != 1 {
		t.Fatalf("search found %d cards", n)
	}
	// The folder chip uses an opaque ID, not the folder name.
	_, body = ts.do(t, "GET", "/", nil)
	if !strings.Contains(body, ">Secret Folder<") {
		t.Fatal("folder chip missing")
	}
	dirID := privacy.DirID(ts.sets.Salt(), ts.lib, "Secret Folder")
	if !strings.Contains(body, "/?d="+dirID) {
		t.Fatal("folder link does not use the opaque ID")
	}
}

func TestPlaybackStats(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	m := ts.addMedia(t, 1, "Movie") // 120 s: a view needs 30 s
	beat := func(key string, pos, watched int) {
		t.Helper()
		resp, _ := ts.do(t, "POST", "/api/playback/"+m.Pid, url.Values{
			"play": {key}, "pos": {fmt.Sprint(pos)}, "watched": {fmt.Sprint(watched)},
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("heartbeat: %d", resp.StatusCode)
		}
	}
	beat("k1", 10_000, 10_000)
	beat("k1", 20_000, 10_000)
	if v, _ := ts.st.CountViews(context.Background(), m.ID); v != 0 {
		t.Fatalf("views after 20 s = %d", v)
	}
	beat("k1", 31_000, 99_000) // capped at 30 s
	if v, _ := ts.st.CountViews(context.Background(), m.ID); v != 1 {
		t.Fatalf("views after 50 s = %d", v)
	}
	beat("k1", 40_000, 10_000) // the same play does not count twice
	if v, _ := ts.st.CountViews(context.Background(), m.ID); v != 1 {
		t.Fatalf("views = %d", v)
	}
	_, body := ts.do(t, "GET", "/v/"+m.Pid, nil)
	if !strings.Contains(body, `data-resume="40000"`) {
		t.Fatal("resume position missing")
	}
	beat("k2", 119_000, 1_000) // near the end: completed, no resume
	_, body = ts.do(t, "GET", "/v/"+m.Pid, nil)
	if !strings.Contains(body, `data-resume="0"`) {
		t.Fatal("completed video should not resume")
	}

	resp, body := ts.do(t, "POST", "/api/rating/"+m.Pid, url.Values{"stars": {"4"}}, "HX-Request", "true")
	if resp.StatusCode != http.StatusOK || strings.Count(body, "star on") != 4 {
		t.Fatalf("rating fragment: %d %q", resp.StatusCode, body)
	}
	if resp, _ := ts.do(t, "POST", "/api/rating/"+m.Pid, url.Values{"stars": {"9"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rating: %d", resp.StatusCode)
	}
}

func TestAdminPagesRender(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	m := ts.addMedia(t, 1, "Movie")
	for _, p := range []string{"/admin/library", "/admin/transcoding", "/admin/jobs", "/admin/stats",
		"/admin/users", "/admin/privacy", "/admin/library/status", "/admin/jobs/table", "/v/" + m.Pid + "/status"} {
		if resp, body := ts.do(t, "GET", p, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	// An invalid profile shows the form again with the error.
	resp, body := ts.do(t, "POST", "/admin/profiles", url.Values{"name": {"x"}, "policy": {"manual"}, "codec": {"h264"},
		"encoder": {"auto"}, "video_mode": {"encode"}, "quality": {"0"}, "audio_bitrate_k": {"128"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "quality must be") {
		t.Fatalf("invalid profile: %d", resp.StatusCode)
	}
	// Static files are cacheable.
	resp, _ = ts.do(t, "GET", "/static/app.css", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
		t.Fatalf("static: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

func TestFolderTree(t *testing.T) {
	dirs := []db.ListDirsRow{{Root: "/a", RelDir: ""}, {Root: "/a", RelDir: "x/y"}, {Root: "/b", RelDir: "z"}}
	tree := folderTree([]string{"/a", "/b"}, dirs, "s")
	top := tree[""]
	if len(top.Children) != 2 {
		t.Fatalf("top children = %v", top.Children)
	}
	y := tree[privacy.DirID("s", "/a", "x/y")]
	if y == nil || y.Name != "y" || tree[y.Parent].Name != "x" {
		t.Fatalf("bad node: %+v", y)
	}
	single := folderTree([]string{"/a"}, dirs[:2], "s")
	if single[""].Root != "/a" || len(single[""].Children) != 1 {
		t.Fatalf("single root: %+v", single[""])
	}
}

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/":                   true,
		"/admin/privacy":      true,
		"/v/abc?x=1":          true,
		"//evil.example":      false,
		"/\\evil.example":     false,
		"https://evil":        false,
		"":                    false,
		"/a\r\nSet-Cookie: x": false,
	} {
		if got := localPath(p); got != want {
			t.Errorf("localPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAddUserErrorKeepsName(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	resp, body := ts.do(t, "POST", "/admin/users", url.Values{"username": {"carol"}, "password": {"short"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `value="carol"`) {
		t.Fatalf("failed add: %d, name kept: %v", resp.StatusCode, strings.Contains(body, `value="carol"`))
	}
	resp, _ = ts.do(t, "POST", "/admin/users", url.Values{"username": {"carol"}, "password": {"password123"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	_, body = ts.do(t, "GET", "/admin/users", nil)
	if !strings.Contains(body, `form="delete-`) || !strings.Contains(body, ">carol<") {
		t.Fatal("new user row missing")
	}
}

func TestUnplayableFormPreselectsH264(t *testing.T) {
	ts := newTestServer(t)
	ts.setup(t)
	m := ts.addMedia(t, 1, "Movie")
	profiles, err := ts.st.ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var compat int64
	for _, p := range profiles {
		if p.Name == "Compatible 1080p H.264" {
			compat = p.ID
		}
	}
	_, body := ts.do(t, "GET", "/v/"+m.Pid, nil)
	unplayable := between(t, body, `id="profile-unplayable"`, "</select>")
	if !strings.Contains(unplayable, fmt.Sprintf(`value="%d" selected`, compat)) {
		t.Fatalf("the H.264 profile is not preselected:\n%s", unplayable)
	}
	if strings.Contains(between(t, body, `id="profile-transcode"`, "</select>"), "selected") {
		t.Fatal("the details form should keep the default order")
	}
}

// between returns the part of s from start up to the next end.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(s, start)
	if !ok {
		t.Fatalf("%q not found", start)
	}
	part, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatalf("%q not found after %q", end, start)
	}
	return part
}
