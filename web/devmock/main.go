// Command devmock is a stand-in for vml-sync-server, used only while working
// on the admin page in web/admin. It is not part of the real server and must
// never be deployed.
//
// It serves web/admin straight from disk (edit a file, refresh the browser)
// and keeps an in-memory imitation of the admin API from docs/API.md: the
// login cookie, 401s, the failed-login limit, the X-VML-Admin check, the model
// list with search / sort / paging, delete, the blocklist, the full settings
// object (partial updates, every validation error, the storage change with its
// confirmation, a base_path change that moves the whole site), changing the
// admin password, testing storage settings, cleanup and uploads in progress.
// Nothing is stored; a restart resets everything.
//
// Run it from the repository root:
//
//	go run ./web/devmock
//	go run ./web/devmock -base /vml-test -password admin
//
// then open the address it prints. The seed data deliberately contains hostile
// file names, uploader names and notes (HTML, quotes, line breaks, a 200
// character name) so that unsafe rendering shows up immediately.
//
// There is no object storage behind it. Whether a set of S3 settings "works"
// is decided by what they contain, so that every outcome can be reached from
// the page (see probeStorage).
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	cookieName   = "vml_admin"
	sessionTTL   = 12 * time.Hour
	maxFailures  = 5
	cleanupEvery = 10 * time.Minute
	mib          = int64(1) << 20
	gib          = int64(1) << 30
	maxRetention = 87600
	version      = "0.1.0-devmock"
)

// The page must work without inline code and without anything loaded from
// another origin. Sending this policy with the page makes a violation fail
// loudly in the browser console instead of going unnoticed.
const strictCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

var (
	idRe       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	basePathRe = regexp.MustCompile(`^/[A-Za-z0-9\-_.~/]*$`)
	authKeyRe  = regexp.MustCompile(`^[\x21-\x7e]{16,128}$`)
)

type model struct {
	ID             string
	Name           string
	Size           int64
	Uploader       string
	UploadedAt     int64
	LastDownloadAt int64
	DownloadCount  int64
}

type modelJSON struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Uploader       string `json:"uploader"`
	UploadedAt     int64  `json:"uploaded_at"`
	LastDownloadAt int64  `json:"last_download_at"`
	DownloadCount  int64  `json:"download_count"`
	ExpiresAt      int64  `json:"expires_at"`
}

type blockEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	BlockedAt int64  `json:"blocked_at"`
}

type upload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Uploader  string `json:"uploader"`
	Size      int64  `json:"size"`
	Received  int64  `json:"received"`
	Mode      string `json:"mode"`
	StartedAt int64  `json:"started_at"`

	sent int64 // bytes "on the wire"; Received is derived from it
	rate int64 // bytes per second
}

// s3Config and settings hold everything docs/API.md 3.5 lists.
type s3Config struct {
	Endpoint    string
	Region      string
	Bucket      string
	AccessKeyID string
	Secret      string // write-only: never sent back
	Prefix      string
	PathStyle   bool
}

type settings struct {
	RetentionHours   int64
	BasePath         string
	AuthKey          string
	MaxFileMB        int64
	MaxTotalMB       int64
	ChunkMB          int64
	MaxActiveUploads int64
	Transfer         string
	DirectURLTTL     int64
	VerifyDirect     bool
	TrustForwarded   bool
	StorageType      string
	S3               s3Config
}

func (c *settings) toJSON() map[string]any {
	return map[string]any{
		"retention_hours":         c.RetentionHours,
		"base_path":               c.BasePath,
		"auth_key":                c.AuthKey,
		"max_file_mb":             c.MaxFileMB,
		"max_total_mb":            c.MaxTotalMB,
		"chunk_mb":                c.ChunkMB,
		"max_active_uploads":      c.MaxActiveUploads,
		"transfer":                c.Transfer,
		"direct_url_ttl_seconds":  c.DirectURLTTL,
		"verify_direct_uploads":   c.VerifyDirect,
		"trust_forwarded_headers": c.TrustForwarded,
		"storage": map[string]any{
			"type": c.StorageType,
			"s3": map[string]any{
				"endpoint":              c.S3.Endpoint,
				"region":                c.S3.Region,
				"bucket":                c.S3.Bucket,
				"access_key_id":         c.S3.AccessKeyID,
				"secret_access_key_set": c.S3.Secret != "",
				"prefix":                c.S3.Prefix,
				"path_style":            c.S3.PathStyle,
			},
		},
	}
}

type server struct {
	addr       string
	files      http.Handler
	csp        bool
	quiet      bool
	latency    time.Duration
	lockWindow time.Duration
	diskFree   int64

	mu          sync.Mutex
	cfg         settings
	password    string
	initialPass bool
	models      map[string]*model
	blocked     map[string]*blockEntry
	uploads     []*upload
	lastCleanup int64
	nextCleanup int64
	sessions    map[string]time.Time
	failures    map[string][]time.Time
	failNext    int
	rng         *mrand.Rand
	uploadSeq   int
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	base := flag.String("base", "", `initial base_path, e.g. "/vml-test" (empty = root); the page can change it afterwards`)
	password := flag.String("password", "admin", "admin password")
	initial := flag.Bool("initial-password", true, `report "initial_password": true until the password is changed from the page`)
	dir := flag.String("dir", "", "directory holding the admin page (default: look for web/admin near the working directory)")
	retention := flag.Int64("retention", 72, "initial retention_hours")
	maxTotalMB := flag.Int64("max-total-mb", 8192, "initial max_total_mb (0 = no cap)")
	diskFreeGB := flag.Int64("disk-free-gb", 23, `free disk space reported as "disk_free_bytes", in GB (0 = unknown)`)
	uploads := flag.Int("uploads", 2, "number of simulated uploads in progress at start")
	lockWindow := flag.Duration("lock-window", 5*time.Minute, "window of the 5-failed-attempts limit (shorten it to try the 429 path)")
	latency := flag.Duration("latency", 0, "artificial delay added to every admin API response")
	storage := flag.String("storage", "local", "initial storage.type (local | s3); s3 starts with made-up R2 settings")
	transfer := flag.String("transfer", "proxy", "initial transfer (proxy | direct; direct needs -storage s3)")
	csp := flag.Bool("csp", true, "send a strict Content-Security-Policy with the page")
	quiet := flag.Bool("quiet", false, "do not log requests")
	flag.Parse()

	basePath, err := normalizeBase(*base)
	if err != nil {
		log.Fatalf("devmock: %v", err)
	}
	adminDir, err := findAdminDir(*dir)
	if err != nil {
		log.Fatalf("devmock: %v", err)
	}

	cfg := settings{
		RetentionHours:   *retention,
		BasePath:         basePath,
		AuthKey:          newAuthKey(),
		MaxFileMB:        200,
		MaxTotalMB:       *maxTotalMB,
		ChunkMB:          5,
		MaxActiveUploads: 8,
		Transfer:         *transfer,
		DirectURLTTL:     900,
		VerifyDirect:     true,
		StorageType:      *storage,
		S3:               s3Config{Region: "auto", Prefix: "models/", PathStyle: true},
	}
	if cfg.StorageType == "s3" {
		cfg.S3.Endpoint = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"
		cfg.S3.Bucket = "vml-models"
		cfg.S3.AccessKeyID = "mock-access-key-id"
		cfg.S3.Secret = "mock-secret-access-key"
	}
	if fe := validate(&cfg); fe != nil {
		log.Fatalf("devmock: the flags give an invalid setting: %s: %s", fe.field, fe.msg)
	}

	now := time.Now().Unix()
	s := &server{
		addr:        displayHost(*addr),
		files:       http.FileServer(http.Dir(adminDir)),
		csp:         *csp,
		quiet:       *quiet,
		latency:     *latency,
		lockWindow:  *lockWindow,
		diskFree:    *diskFreeGB * gib,
		cfg:         cfg,
		password:    *password,
		initialPass: *initial,
		models:      map[string]*model{},
		blocked:     map[string]*blockEntry{},
		lastCleanup: now - 240,
		nextCleanup: now - 240 + int64(cleanupEvery/time.Second),
		sessions:    map[string]time.Time{},
		failures:    map[string][]time.Time{},
		rng:         mrand.New(mrand.NewSource(20240607)),
	}
	s.seed(now, *uploads)
	go s.run()

	root := "http://" + s.addr + basePath
	fmt.Printf("devmock: admin page   %s/admin/\n", root)
	fmt.Printf("devmock: password     %s\n", *password)
	fmt.Printf("devmock: page files   %s (served from disk: edit and refresh)\n", adminDir)
	fmt.Printf("devmock: test hooks   plain GET, no login needed (they follow base_path when the page changes it)\n")
	fmt.Printf("    %s/__mock/upload?n=1   start n simulated uploads\n", root)
	fmt.Printf("    %s/__mock/fail?n=1     make the next n admin API calls answer 500\n", root)
	fmt.Printf("    %s/__mock/expire       drop every login session (the next call answers 401)\n", root)
	fmt.Printf("    %s/__mock/unlock       forget failed login / password attempts\n", root)
	fmt.Printf("    %s/__mock/reseed       bring the seed models back (after a storage change wiped them)\n", root)
	fmt.Printf("    %s/__mock/initial      report \"initial_password\": true again\n", root)
	fmt.Printf("devmock: S3 settings   nothing is contacted; what they contain decides whether they \"work\":\n")
	fmt.Printf("    endpoint containing \"unreachable\"      -> the endpoint cannot be reached\n")
	fmt.Printf("    access key id containing \"bad\", or the\n")
	fmt.Printf("    secret \"wrong-secret\"                  -> the credentials are rejected\n")
	fmt.Printf("    bucket containing \"missing\"            -> the bucket does not exist\n")
	fmt.Printf("    anything else                          -> works\n")
	fmt.Printf("devmock: password     a new password equal to the current one is refused as weak_password\n")
	fmt.Printf("devmock: password     a new password equal to the current one is refused as weak_password\n")

	srv := &http.Server{Addr: *addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func normalizeBase(b string) (string, error) {
	b = strings.TrimRight(strings.TrimSpace(b), "/")
	if b == "" {
		return "", nil
	}
	// Git Bash on Windows rewrites an argument like "/vml-test" into a Windows
	// path before the program sees it; catch that instead of serving nothing.
	if strings.Contains(b, `\`) || strings.Contains(b, ":") {
		return "", fmt.Errorf("-base %q does not look like a URL path (in Git Bash, write it as //vml-test or use PowerShell)", b)
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return b, nil
}

func findAdminDir(flagDir string) (string, error) {
	candidates := []string{"web/admin", "../admin", "admin"}
	if flagDir != "" {
		candidates = []string{flagDir}
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return filepath.Abs(c)
		}
	}
	return "", fmt.Errorf("cannot find index.html in %s; run from the repository root or pass -dir", strings.Join(candidates, ", "))
}

func displayHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func newAuthKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------- seed data

func seedID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func (s *server) seed(now int64, nUploads int) {
	s.seedModels(now)

	blocked := []blockEntry{
		{ID: seedID("blocked-0"), Name: "Stolen_Avatar_rip.vrca", Note: "版权投诉 / takedown request", BlockedAt: now - 9*86400},
		{ID: seedID("blocked-1"), Name: `<img src=x onerror=alert('name')>`, Note: `<img src=x onerror=alert('note')> "quoted" & 'more'`, BlockedAt: now - 3*3600},
		{ID: seedID("blocked-2"), Name: "", Note: "", BlockedAt: now - 25*60},
	}
	for i := range blocked {
		s.blocked[blocked[i].ID] = &blocked[i]
	}

	for i := 0; i < nUploads; i++ {
		s.startUploadLocked(now - int64(5*(nUploads-i)))
	}
}

func (s *server) seedModels(now int64) {
	avatars := []string{
		"Chiffon", "Manuka", "Selestia", "Kikyo", "Moe", "Shinra", "Rusk", "Lime", "Karin", "Maya",
		"Sio", "Mafuyu", "Rindo", "Grus", "Lasyusha", "Wolferia", "Kuuta", "Ririka", "Mizuki", "Leefa",
		"Imeris", "Velle", "Minase", "Hakka", "Komano", "Eku", "Marycia", "Shiratsume", "Milltina", "Sophina",
		"Chocolat", "Airi", "Nayu", "桔梗", "萌", "真冬", "竜胆", "しなの", "ここあ", "瑞希",
	}
	suffixes := []string{"", "", "", "_v2", "_Quest", "_改", " (水着)", "_PC_final", "-kaihen", "_2024冬"}
	uploaders := []string{"chinosk", "小明", "Alice", "ボブ", "夜猫子", "xX_Sniper_Xx", "Mika", "阿尔法", ""}

	for i := 0; i < 52; i++ {
		uploaded := now - 3600 - s.rng.Int63n(6*86400)
		var last, count int64
		if s.rng.Intn(4) != 0 {
			last = uploaded + s.rng.Int63n(now-uploaded)
			count = 1 + s.rng.Int63n(40)
		}
		s.addModel(&model{
			ID:             seedID(fmt.Sprintf("seed-%d", i)),
			Name:           avatars[i%len(avatars)] + suffixes[s.rng.Intn(len(suffixes))] + ".vrca",
			Size:           (2+s.rng.Int63n(150))*mib + s.rng.Int63n(mib),
			Uploader:       uploaders[s.rng.Intn(len(uploaders))],
			UploadedAt:     uploaded,
			LastDownloadAt: last,
			DownloadCount:  count,
		})
	}

	// Edge cases for the formatting code.
	s.addModel(&model{ID: seedID("edge-max"), Name: "Exactly_200MB.vrca", Size: 200 * mib, Uploader: "chinosk",
		UploadedAt: now - 5*3600, LastDownloadAt: now - 30, DownloadCount: 12345})
	s.addModel(&model{ID: seedID("edge-tiny"), Name: "tiny.vrca", Size: 512, Uploader: "Alice",
		UploadedAt: now - 71*3600 - 3300, LastDownloadAt: 0, DownloadCount: 0}) // expires in a few minutes
	s.addModel(&model{ID: seedID("edge-old"), Name: "(unknown)", Size: 31 * mib, Uploader: "",
		UploadedAt: now - 400*86400, LastDownloadAt: now - 2*3600, DownloadCount: 1})

	// Hostile content. The real server truncates names and strips control
	// characters; the mock does not, so the page is tested against worse input
	// than it should ever receive. These are the newest uploads, so they are
	// on the first page with the default sort.
	long := strings.Repeat("超长名字_VeryLongAvatarName_", 20)
	hostile := []struct{ name, uploader string }{
		{`<img src=x onerror=alert(1)>`, `<img src=x onerror=alert(2)>`},
		{`<script>alert('xss')</script>.vrca`, `<script>alert(3)</script>`},
		{`"><svg/onload=alert(4)>'.vrca`, `<b onmouseover=alert(5)>hover me</b>`},
		{string([]rune(long)[:196]) + ".vrca", strings.Repeat("W", 64)},
		{"line one\nline two\ttab \"double\" 'single' `backtick` \\backslash.vrca", "new\nline \"quoted\""},
		{`&lt;b&gt;not bold&lt;/b&gt; &amp; &#x41; &quot;.vrca`, `&lt;i&gt;`},
		{"‮gnp.exe‬ reversed? .vrca", "‮resu"},
		{``, ``},
		{`javascript:alert(6)`, `data:text/html,<script>alert(7)</script>`},
		{`{{constructor.constructor('alert(8)')()}} ${alert(9)} <%= 1 %>`, `{{7*7}}`},
		{"🦊きつね🦊 모델 модель نموذج.vrca", "🧑‍🚀"},
		{strings.Repeat("spaced long name ", 11) + "end.vrca", "a b c"},
	}
	for i, h := range hostile {
		var last, count int64
		if i%3 == 1 {
			last, count = now-int64(i)*20, int64(i)
		}
		s.addModel(&model{
			ID:             seedID(fmt.Sprintf("hostile-%d", i)),
			Name:           h.name,
			Size:           (5+int64(i)*7)*mib + 12345,
			Uploader:       h.uploader,
			UploadedAt:     now - int64(i+1)*60,
			LastDownloadAt: last,
			DownloadCount:  count,
		})
	}
}

func (s *server) addModel(m *model) { s.models[m.ID] = m }

func (s *server) startUploadLocked(now int64) {
	names := []string{"Manuka_custom_outfit.vrca", "新衣装_テスト.vrca", `<i>italic?</i> "upload" & more.vrca`, "Kikyo_winter.vrca"}
	uploaders := []string{"chinosk", "夜猫子", `<u>mallory</u>`, "Alice"}
	n := s.uploadSeq
	s.uploadSeq++
	mode := "proxy"
	if n%2 == 1 {
		mode = "direct"
	}
	s.uploads = append(s.uploads, &upload{
		ID:        seedID(fmt.Sprintf("upload-%d-%d", n, now)),
		Name:      names[n%len(names)],
		Uploader:  uploaders[n%len(uploaders)],
		Size:      (40+s.rng.Int63n(120))*mib + s.rng.Int63n(mib),
		Mode:      mode,
		StartedAt: now,
		rate:      (2 + s.rng.Int63n(4)) * mib,
	})
}

// ------------------------------------------------------------ background work

func (s *server) run() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for t := range ticker.C {
		now := t.Unix()
		s.mu.Lock()
		s.advanceUploadsLocked(now)
		if now >= s.nextCleanup {
			s.cleanupLocked(now)
		}
		for tok, exp := range s.sessions {
			if t.After(exp) {
				delete(s.sessions, tok)
			}
		}
		s.mu.Unlock()
	}
}

func (s *server) advanceUploadsLocked(now int64) {
	chunk := s.cfg.ChunkMB * mib
	kept := s.uploads[:0]
	for _, u := range s.uploads {
		u.sent += u.rate
		if u.sent >= u.Size {
			if _, isBlocked := s.blocked[u.ID]; !isBlocked {
				s.models[u.ID] = &model{ID: u.ID, Name: u.Name, Size: u.Size, Uploader: u.Uploader, UploadedAt: now}
			}
			continue
		}
		if u.Mode == "direct" {
			// docs/API.md 2.2: in direct mode progress is estimated from the
			// last part that was signed, so it moves in whole chunks.
			u.Received = u.sent / chunk * chunk
		} else {
			u.Received = u.sent
		}
		kept = append(kept, u)
	}
	for i := len(kept); i < len(s.uploads); i++ {
		s.uploads[i] = nil
	}
	s.uploads = kept
}

func (s *server) expiresAt(m *model) int64 {
	if s.cfg.RetentionHours == 0 {
		return 0
	}
	return max(m.UploadedAt, m.LastDownloadAt) + s.cfg.RetentionHours*3600
}

func (s *server) cleanupLocked(now int64) (deleted int, freed int64) {
	if s.cfg.RetentionHours > 0 {
		for id, m := range s.models {
			if now-max(m.UploadedAt, m.LastDownloadAt) > s.cfg.RetentionHours*3600 {
				deleted++
				freed += m.Size
				delete(s.models, id)
			}
		}
	}
	s.lastCleanup = now
	s.nextCleanup = now + int64(cleanupEvery/time.Second)
	return deleted, freed
}

// -------------------------------------------------------------------- routing

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.route(sw, r)
	if !s.quiet {
		log.Printf("%-4s %s -> %d", r.Method, r.URL.RequestURI(), sw.status)
	}
}

func (s *server) basePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.BasePath
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// base_path is a setting: the page can change it, and from the next
	// request on only the new prefix exists (docs/API.md 3.5).
	base := s.basePath()
	p := r.URL.Path
	if base != "" {
		if p != base && !strings.HasPrefix(p, base+"/") {
			plainNotFound(w)
			return
		}
		p = p[len(base):]
	}
	switch {
	case p == "/admin":
		http.Redirect(w, r, base+"/admin/", http.StatusFound)
	case strings.HasPrefix(p, "/admin/"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			plainNotFound(w)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.csp {
			w.Header().Set("Content-Security-Policy", strictCSP)
		}
		http.StripPrefix(base+"/admin", s.files).ServeHTTP(w, r)
	case strings.HasPrefix(p, "/api/v1/admin/"):
		s.admin(w, r, strings.TrimPrefix(p, "/api/v1/admin/"))
	case strings.HasPrefix(p, "/__mock/"):
		s.hook(w, r, strings.TrimPrefix(p, "/__mock/"))
	default:
		plainNotFound(w)
	}
}

func (s *server) admin(w http.ResponseWriter, r *http.Request, route string) {
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
	if s.takeFailure() {
		writeErr(w, http.StatusInternalServerError, "internal", "simulated failure (asked for through /__mock/fail)")
		return
	}

	// The three session endpoints have their own rules (docs/API.md 1.2).
	switch route {
	case "login":
		if allow(w, r, http.MethodPost) {
			s.login(w, r)
		}
		return
	case "session":
		if allow(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": s.authed(r)})
		}
		return
	case "logout":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.logout(w, r)
		}
		return
	}

	if !s.authed(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "not logged in")
		return
	}
	if r.Method != http.MethodGet && !hasAdminHeader(w, r) {
		return
	}

	switch route + " " + r.Method {
	case "overview GET":
		s.overview(w)
	case "models GET":
		s.listModels(w, r)
	case "models/delete POST":
		s.deleteModels(w, r)
	case "blocklist GET":
		s.listBlocked(w)
	case "blocklist POST":
		s.addBlocked(w, r)
	case "blocklist/remove POST":
		s.removeBlocked(w, r)
	case "settings GET":
		s.getSettings(w)
	case "settings PUT":
		s.putSettings(w, r)
	case "password POST":
		s.changePassword(w, r)
	case "storage/test POST":
		s.testStorage(w, r)
	case "cleanup POST":
		s.cleanup(w)
	case "uploads GET":
		s.listUploads(w)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such admin endpoint: "+r.Method+" "+route)
	}
}

func (s *server) hook(w http.ResponseWriter, r *http.Request, what string) {
	n := 1
	if v := r.URL.Query().Get("n"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 && k <= 1000 {
			n = k
		}
	}
	s.mu.Lock()
	switch what {
	case "upload":
		for i := 0; i < n; i++ {
			s.startUploadLocked(time.Now().Unix())
		}
	case "fail":
		s.failNext = n
	case "expire":
		s.sessions = map[string]time.Time{}
	case "unlock":
		s.failures = map[string][]time.Time{}
	case "reseed":
		s.seedModels(time.Now().Unix())
	case "initial":
		s.initialPass = true
	default:
		s.mu.Unlock()
		plainNotFound(w)
		return
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hook": what, "n": n})
}

func (s *server) takeFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return true
	}
	return false
}

// ---------------------------------------------------------------------- auth

func (s *server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	return ok && time.Now().Before(exp)
}

// lockedLocked reports whether this source has used up its failed attempts,
// and for how many more seconds. Failed logins and failed "current password"
// checks share the count (docs/API.md 1.2, 3.7).
func (s *server) lockedLocked(ip string, now time.Time) (int, bool) {
	recent := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if now.Sub(t) < s.lockWindow {
			recent = append(recent, t)
		}
	}
	s.failures[ip] = recent
	if len(recent) < maxFailures {
		return 0, false
	}
	wait := recent[len(recent)-maxFailures].Add(s.lockWindow).Sub(now)
	return int(wait/time.Second) + 1, true
}

func tooManyAttempts(w http.ResponseWriter, secs int) {
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": "too_many_attempts", "message": "too many failed attempts", "retry_after": secs})
}

func (s *server) sessionCookie(r *http.Request, base, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     base + "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password *string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Password == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `missing "password"`)
		return
	}

	ip := clientIP(r)
	now := time.Now()

	s.mu.Lock()
	if secs, locked := s.lockedLocked(ip, now); locked {
		s.mu.Unlock()
		tooManyAttempts(w, secs)
		return
	}
	if subtle.ConstantTimeCompare([]byte(*body.Password), []byte(s.password)) != 1 {
		s.failures[ip] = append(s.failures[ip], now)
		s.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "bad_password", "wrong password")
		return
	}
	delete(s.failures, ip)
	tok := newToken()
	s.sessions[tok] = now.Add(sessionTTL)
	base := s.cfg.BasePath
	s.mu.Unlock()

	http.SetCookie(w, s.sessionCookie(r, base, tok, int(sessionTTL/time.Second)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if c, err := r.Cookie(cookieName); err == nil {
		delete(s.sessions, c.Value)
	}
	base := s.cfg.BasePath
	s.mu.Unlock()
	http.SetCookie(w, s.sessionCookie(r, base, "", -1))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// changePassword is docs/API.md 3.7.
func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current *string `json:"current"`
		New     *string `json:"new"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Current == nil || body.New == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `"current" and "new" are both required`)
		return
	}
	mine := ""
	if c, err := r.Cookie(cookieName); err == nil {
		mine = c.Value
	}
	ip := clientIP(r)
	now := time.Now()

	s.mu.Lock()
	if secs, locked := s.lockedLocked(ip, now); locked {
		s.mu.Unlock()
		tooManyAttempts(w, secs)
		return
	}
	if subtle.ConstantTimeCompare([]byte(*body.Current), []byte(s.password)) != 1 {
		s.failures[ip] = append(s.failures[ip], now)
		s.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "bad_password", "the current password is wrong")
		return
	}
	if n := utf8.RuneCountInString(*body.New); n < 8 || n > 128 {
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "weak_password",
			fmt.Sprintf("the new password must be 8 to 128 characters long (this one has %d)", n))
		return
	}
	// Not a rule from docs/API.md: the page checks the length itself, so without
	// one more reason to refuse, its handling of weak_password could not be tried.
	if *body.New == *body.Current {
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "weak_password", "the new password is the same as the current one")
		return
	}
	// Not a rule from docs/API.md: the page checks the length itself, so without
	// one more reason to refuse, its handling of weak_password could not be tried.
	if *body.New == *body.Current {
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "weak_password", "the new password is the same as the current one")
		return
	}
	s.password = *body.New
	s.initialPass = false
	for tok := range s.sessions {
		if tok != mine {
			delete(s.sessions, tok)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ----------------------------------------------------------------- endpoints

func (s *server) overview(w http.ResponseWriter) {
	s.mu.Lock()
	var total int64
	for _, m := range s.models {
		total += m.Size
	}
	free := s.diskFree
	if free > 0 && s.cfg.StorageType == "local" {
		free = max(free-total, mib)
	}
	out := map[string]any{
		"version":          version,
		"storage":          s.cfg.StorageType,
		"transfer":         s.cfg.Transfer,
		"max_file_bytes":   s.cfg.MaxFileMB * mib,
		"max_total_bytes":  s.cfg.MaxTotalMB * mib,
		"chunk_bytes":      s.cfg.ChunkMB * mib,
		"model_count":      len(s.models),
		"total_bytes":      total,
		"blocked_count":    len(s.blocked),
		"active_uploads":   len(s.uploads),
		"retention_hours":  s.cfg.RetentionHours,
		"last_cleanup_at":  s.lastCleanup,
		"next_cleanup_at":  s.nextCleanup,
		"now":              time.Now().Unix(),
		"initial_password": s.initialPass,
		"disk_free_bytes":  free,
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listModels(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := strings.ToLower(qv.Get("q"))

	sortKey := qv.Get("sort")
	if sortKey == "" {
		sortKey = "uploaded_at"
	}
	switch sortKey {
	case "uploaded_at", "last_download_at", "size", "name", "uploader", "download_count", "expires_at":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown sort: "+sortKey)
		return
	}
	order := qv.Get("order")
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		writeErr(w, http.StatusBadRequest, "bad_request", "order must be asc or desc")
		return
	}
	offset, ok := intParam(qv.Get("offset"), 0)
	if !ok || offset < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad offset")
		return
	}
	limit, ok := intParam(qv.Get("limit"), 50)
	if !ok || limit < 1 || limit > 500 {
		writeErr(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 500")
		return
	}

	s.mu.Lock()
	items := make([]modelJSON, 0, len(s.models))
	for _, m := range s.models {
		if q != "" && !strings.Contains(strings.ToLower(m.Name), q) &&
			!strings.Contains(strings.ToLower(m.Uploader), q) && !strings.Contains(m.ID, q) {
			continue
		}
		items = append(items, modelJSON{
			ID: m.ID, Name: m.Name, Size: m.Size, Uploader: m.Uploader,
			UploadedAt: m.UploadedAt, LastDownloadAt: m.LastDownloadAt,
			DownloadCount: m.DownloadCount, ExpiresAt: s.expiresAt(m),
		})
	}
	s.mu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		c := compareModels(&items[i], &items[j], sortKey)
		if c == 0 {
			return items[i].ID < items[j].ID
		}
		if order == "asc" {
			return c < 0
		}
		return c > 0
	})

	total := len(items)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items[offset:end]})
}

func compareModels(a, b *modelJSON, key string) int {
	switch key {
	case "last_download_at":
		return cmpInt(a.LastDownloadAt, b.LastDownloadAt)
	case "size":
		return cmpInt(a.Size, b.Size)
	case "name":
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	case "uploader":
		return strings.Compare(strings.ToLower(a.Uploader), strings.ToLower(b.Uploader))
	case "download_count":
		return cmpInt(a.DownloadCount, b.DownloadCount)
	case "expires_at":
		return cmpInt(a.ExpiresAt, b.ExpiresAt)
	default:
		return cmpInt(a.UploadedAt, b.UploadedAt)
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *server) deleteModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs   []string `json:"ids"`
		Block bool     `json:"block"`
		Note  string   `json:"note"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", `"ids" must not be empty`)
		return
	}
	for _, id := range body.IDs {
		if !idRe.MatchString(id) {
			writeErr(w, http.StatusBadRequest, "bad_id", "not a model id: "+id)
			return
		}
	}

	now := time.Now().Unix()
	deleted, blocked := 0, 0
	s.mu.Lock()
	for _, id := range body.IDs {
		name := ""
		if m, ok := s.models[id]; ok {
			name = m.Name
			delete(s.models, id)
			deleted++
		}
		if body.Block {
			if _, already := s.blocked[id]; !already {
				s.blocked[id] = &blockEntry{ID: id, Name: name, Note: body.Note, BlockedAt: now}
				blocked++
			}
			s.dropUploadLocked(id)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "blocked": blocked})
}

func (s *server) dropUploadLocked(id string) {
	kept := s.uploads[:0]
	for _, u := range s.uploads {
		if u.ID != id {
			kept = append(kept, u)
		}
	}
	for i := len(kept); i < len(s.uploads); i++ {
		s.uploads[i] = nil
	}
	s.uploads = kept
}

func (s *server) listBlocked(w http.ResponseWriter) {
	s.mu.Lock()
	items := make([]blockEntry, 0, len(s.blocked))
	for _, b := range s.blocked {
		items = append(items, *b)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].BlockedAt != items[j].BlockedAt {
			return items[i].BlockedAt > items[j].BlockedAt
		}
		return items[i].ID < items[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) addBlocked(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !idRe.MatchString(body.ID) {
		writeErr(w, http.StatusBadRequest, "bad_id", "not a model id")
		return
	}
	s.mu.Lock()
	if m, ok := s.models[body.ID]; ok {
		if body.Name == "" {
			body.Name = m.Name
		}
		delete(s.models, body.ID)
	}
	s.dropUploadLocked(body.ID)
	s.blocked[body.ID] = &blockEntry{ID: body.ID, Name: body.Name, Note: body.Note, BlockedAt: time.Now().Unix()}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) removeBlocked(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	for _, id := range body.IDs {
		if !idRe.MatchString(id) {
			writeErr(w, http.StatusBadRequest, "bad_id", "not a model id: "+id)
			return
		}
	}
	removed := 0
	s.mu.Lock()
	for _, id := range body.IDs {
		if _, ok := s.blocked[id]; ok {
			delete(s.blocked, id)
			removed++
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *server) cleanup(w http.ResponseWriter) {
	s.mu.Lock()
	deleted, freed := s.cleanupLocked(time.Now().Unix())
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "freed_bytes": freed})
}

func (s *server) listUploads(w http.ResponseWriter) {
	s.mu.Lock()
	items := make([]upload, 0, len(s.uploads))
	for _, u := range s.uploads {
		items = append(items, *u)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ------------------------------------------------------------------ settings

// fieldErr is one "invalid_settings" answer: the key in the spelling of the
// table in docs/API.md 3.5, and what is wrong with it.
type fieldErr struct{ field, msg string }

func decode(raw json.RawMessage, into any) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return false // Unmarshal would quietly leave the value alone
	}
	return json.Unmarshal(raw, into) == nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// applySettings copies the keys present in raw onto next: a partial update,
// at every level. Keys that are not there keep their value.
func applySettings(next *settings, raw map[string]json.RawMessage) (confirm bool, fe *fieldErr) {
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		ok := true
		switch k {
		case "retention_hours":
			ok = decode(v, &next.RetentionHours)
		case "base_path":
			ok = decode(v, &next.BasePath)
		case "auth_key":
			ok = decode(v, &next.AuthKey)
		case "max_file_mb":
			ok = decode(v, &next.MaxFileMB)
		case "max_total_mb":
			ok = decode(v, &next.MaxTotalMB)
		case "chunk_mb":
			ok = decode(v, &next.ChunkMB)
		case "max_active_uploads":
			ok = decode(v, &next.MaxActiveUploads)
		case "transfer":
			ok = decode(v, &next.Transfer)
		case "direct_url_ttl_seconds":
			ok = decode(v, &next.DirectURLTTL)
		case "verify_direct_uploads":
			ok = decode(v, &next.VerifyDirect)
		case "trust_forwarded_headers":
			ok = decode(v, &next.TrustForwarded)
		case "confirm_storage_change":
			ok = decode(v, &confirm)
		case "storage":
			if fe := applyStorage(next, v); fe != nil {
				return false, fe
			}
		default:
			return false, &fieldErr{k, "there is no such setting"}
		}
		if !ok {
			return false, &fieldErr{k, "the value has the wrong type"}
		}
	}
	return confirm, nil
}

func applyStorage(next *settings, raw json.RawMessage) *fieldErr {
	var st map[string]json.RawMessage
	if !decode(raw, &st) {
		return &fieldErr{"storage", "must be an object"}
	}
	for _, k := range sortedKeys(st) {
		switch k {
		case "type":
			if !decode(st[k], &next.StorageType) {
				return &fieldErr{"storage.type", "the value has the wrong type"}
			}
		case "s3":
			var s3 map[string]json.RawMessage
			if !decode(st[k], &s3) {
				return &fieldErr{"storage.s3", "must be an object"}
			}
			for _, k2 := range sortedKeys(s3) {
				ok := true
				switch k2 {
				case "endpoint":
					ok = decode(s3[k2], &next.S3.Endpoint)
				case "region":
					ok = decode(s3[k2], &next.S3.Region)
				case "bucket":
					ok = decode(s3[k2], &next.S3.Bucket)
				case "access_key_id":
					ok = decode(s3[k2], &next.S3.AccessKeyID)
				case "secret_access_key":
					// Write-only. Absent or "" keeps the saved one.
					var secret string
					if ok = decode(s3[k2], &secret); ok && secret != "" {
						next.S3.Secret = secret
					}
				case "prefix":
					ok = decode(s3[k2], &next.S3.Prefix)
				case "path_style":
					ok = decode(s3[k2], &next.S3.PathStyle)
				case "secret_access_key_set":
					// Read-only; tolerated so that a GET answer can be sent back as it is.
				default:
					return &fieldErr{"storage.s3." + k2, "there is no such setting"}
				}
				if !ok {
					return &fieldErr{"storage.s3." + k2, "the value has the wrong type"}
				}
			}
		default:
			return &fieldErr{"storage." + k, "there is no such setting"}
		}
	}
	return nil
}

func inRange(v, lo, hi int64) bool { return v >= lo && v <= hi }

// validate checks a whole settings object against the table and the rules in
// docs/API.md 3.5 and names the first key that breaks them.
func validate(c *settings) *fieldErr {
	switch {
	case !inRange(c.RetentionHours, 0, maxRetention):
		return &fieldErr{"retention_hours", "must be a whole number from 0 to 87600"}
	case c.BasePath != "" && (len(c.BasePath) > 64 || !basePathRe.MatchString(c.BasePath) || strings.HasSuffix(c.BasePath, "/")):
		return &fieldErr{"base_path", `must be empty, or start with "/", not end with "/", use only letters, digits and - _ . ~ /, and be at most 64 characters`}
	case !authKeyRe.MatchString(c.AuthKey):
		return &fieldErr{"auth_key", "must be 16 to 128 visible ASCII characters without spaces"}
	case !inRange(c.MaxFileMB, 1, 5000):
		return &fieldErr{"max_file_mb", "must be a whole number from 1 to 5000"}
	case c.MaxTotalMB < 0:
		return &fieldErr{"max_total_mb", "must be 0 (no cap) or a positive whole number"}
	case !inRange(c.ChunkMB, 1, 64):
		return &fieldErr{"chunk_mb", "must be a whole number from 1 to 64"}
	case !inRange(c.MaxActiveUploads, 1, 64):
		return &fieldErr{"max_active_uploads", "must be a whole number from 1 to 64"}
	case c.Transfer != "proxy" && c.Transfer != "direct":
		return &fieldErr{"transfer", `must be "proxy" or "direct"`}
	case !inRange(c.DirectURLTTL, 60, 86400):
		return &fieldErr{"direct_url_ttl_seconds", "must be a whole number from 60 to 86400"}
	case c.StorageType != "local" && c.StorageType != "s3":
		return &fieldErr{"storage.type", `must be "local" or "s3"`}
	}
	if c.StorageType == "s3" {
		if fe := validateS3(&c.S3); fe != nil {
			return fe
		}
	}
	switch {
	case c.Transfer == "direct" && c.StorageType != "s3":
		return &fieldErr{"transfer", `"direct" is only available with S3 storage`}
	case c.Transfer == "direct" && c.ChunkMB < 5:
		return &fieldErr{"chunk_mb", `must be at least 5 while transfer is "direct"`}
	}
	return nil
}

func validateS3(c *s3Config) *fieldErr {
	if c.Endpoint == "" {
		return &fieldErr{"storage.s3.endpoint", "is required for S3 storage"}
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return &fieldErr{"storage.s3.endpoint", "must be https://host[:port] and nothing after the host name (no path, no bucket)"}
	}
	switch {
	case c.Bucket == "":
		return &fieldErr{"storage.s3.bucket", "is required for S3 storage"}
	case c.AccessKeyID == "":
		return &fieldErr{"storage.s3.access_key_id", "is required for S3 storage"}
	case c.Secret == "":
		return &fieldErr{"storage.s3.secret_access_key", "is required: no secret access key has been saved yet"}
	}
	return nil
}

// storageMoved is the definition of "changing the storage" from docs/API.md
// 3.5: the models stay behind in the old place.
func storageMoved(old, next *settings) bool {
	if old.StorageType != next.StorageType {
		return true
	}
	if next.StorageType != "s3" {
		return false
	}
	a, b := &old.S3, &next.S3
	return a.Endpoint != b.Endpoint || a.Bucket != b.Bucket || a.Prefix != b.Prefix ||
		a.PathStyle != b.PathStyle || a.Region != b.Region
}

// probeStorage stands in for "write a probe object, read it back, delete it".
// It returns "" when the storage works. Nothing is contacted: the outcome
// follows from the settings themselves, so the page can reach every case.
func probeStorage(c *settings) string {
	if c.StorageType != "s3" {
		return ""
	}
	host := c.S3.Endpoint
	if u, err := url.Parse(c.S3.Endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	switch {
	case strings.Contains(strings.ToLower(c.S3.Endpoint), "unreachable"):
		return "cannot reach the endpoint: dial tcp: lookup " + host + ": no such host"
	case strings.Contains(strings.ToLower(c.S3.AccessKeyID), "bad") || c.S3.Secret == "wrong-secret":
		return "the credentials were rejected: 403 InvalidAccessKeyId (check the access key id and the secret access key)"
	case strings.Contains(strings.ToLower(c.S3.Bucket), "missing"):
		return `the bucket "` + c.S3.Bucket + `" does not exist: 404 NoSuchBucket`
	}
	return ""
}

func (s *server) getSettings(w http.ResponseWriter) {
	s.mu.Lock()
	out := s.cfg.toJSON()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// putSettings is docs/API.md 3.5: everything is checked first, then everything
// takes effect, or nothing does.
func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.cfg
	next := s.cfg
	confirm, fe := applySettings(&next, raw)
	if fe == nil {
		fe = validate(&next)
	}
	if fe != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_settings", "field": fe.field, "message": fe.msg})
		return
	}

	moved := storageMoved(&old, &next)
	if moved && !confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       "storage_change_needs_confirm",
			"model_count": len(s.models),
			"message":     fmt.Sprintf("changing the storage discards the %d models stored now; send confirm_storage_change: true to go ahead", len(s.models)),
		})
		return
	}
	newCredentials := next.StorageType == "s3" && (old.S3.AccessKeyID != next.S3.AccessKeyID || old.S3.Secret != next.S3.Secret)
	if moved || newCredentials {
		if msg := probeStorage(&next); msg != "" {
			writeErr(w, http.StatusBadRequest, "storage_unreachable", msg)
			return
		}
	}

	if moved {
		// The models stay behind in the old storage; the blocklist is kept.
		s.models = map[string]*model{}
		s.uploads = nil
		log.Printf("devmock: storage changed (%s -> %s): models and uploads in progress dropped, blocklist kept", old.StorageType, next.StorageType)
	}
	s.cfg = next

	if old.BasePath != next.BasePath {
		// The session moves with the site: same token, cookie under the new path,
		// and the cookie under the old path is taken away.
		if c, err := r.Cookie(cookieName); err == nil {
			http.SetCookie(w, s.sessionCookie(r, old.BasePath, "", -1))
			http.SetCookie(w, s.sessionCookie(r, next.BasePath, c.Value, int(sessionTTL/time.Second)))
		}
		log.Printf("devmock: base_path changed %q -> %q; the admin page is now http://%s%s/admin/", old.BasePath, next.BasePath, s.addr, next.BasePath)
	}
	writeJSON(w, http.StatusOK, s.cfg.toJSON())
}

// testStorage is docs/API.md 3.8. Nothing is saved.
func (s *server) testStorage(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	st, ok := raw["storage"]
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", `missing "storage"`)
		return
	}

	s.mu.Lock()
	probe := s.cfg // a missing or empty secret_access_key means the saved one
	s.mu.Unlock()

	if fe := applyStorage(&probe, st); fe != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", fe.field+": "+fe.msg)
		return
	}
	time.Sleep(700 * time.Millisecond) // a real probe takes a moment
	var msg string
	switch {
	case probe.StorageType != "local" && probe.StorageType != "s3":
		msg = `storage.type must be "local" or "s3"`
	case probe.StorageType == "s3" && validateS3(&probe.S3) != nil:
		fe := validateS3(&probe.S3)
		msg = fe.field + " " + fe.msg
	default:
		msg = probeStorage(&probe)
	}
	if msg != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------- helpers

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeErr(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
	return false
}

func hasAdminHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-VML-Admin") == "1" {
		return true
	}
	writeErr(w, http.StatusBadRequest, "bad_request", "missing X-VML-Admin: 1")
	return false
}

func intParam(v string, def int) (int, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "request body is not the expected JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Send "<" and "&" as they are, so the page sees hostile names verbatim.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

func plainNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "404 page not found\n")
}
