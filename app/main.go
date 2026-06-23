package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// Build-time variables (overridden via -ldflags in the Dockerfile)
var (
	BuildCommit  = "dev"
	BuildTime    = "unknown"
	BuildVersion = "dev"
)

var (
	startTime = time.Now()
	requests  atomic.Uint64
)

func main() {
	port := getEnv("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/api/info", infoHandler)
	mux.HandleFunc("/", rootHandler)

	log.Printf("shipyard listening on :%s (commit=%s build=%s)", port, BuildCommit, BuildTime)
	if err := http.ListenAndServe(":"+port, requestCounter(mux)); err != nil {
		log.Fatal(err)
	}
}

func requestCounter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't count health checks - the ALB hits /health every 15s and would skew the count.
		if r.URL.Path != "/health" {
			requests.Add(1)
		}
		next.ServeHTTP(w, r)
	})
}

// ── handlers ─────────────────────────────────────────────────────────────────

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = healthTmpl.Execute(w, snapshot())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = infoTmpl.Execute(w, snapshot())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot())
}

// wantsHTML returns true when the client (typically a browser) prefers HTML.
// ALB target-group probes send Accept: */* and get JSON.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	for _, t := range []string{"text/html", "application/xhtml+xml"} {
		if len(accept) >= len(t) && containsToken(accept, t) {
			return true
		}
	}
	return false
}

func containsToken(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, snapshot()); err != nil {
		log.Printf("template error: %v", err)
	}
}

// ── data gathering ───────────────────────────────────────────────────────────

type Snapshot struct {
	Status       string
	Uptime       string
	StartedAt    string
	CurrentTime  string
	RequestCount uint64
	AWS          AWSInfo
	Build        BuildInfo
	System       SystemInfo
}

type AWSInfo struct {
	Available     bool
	Region        string
	AvailZone     string
	Cluster       string
	TaskID        string
	ContainerName string
	ImageTag      string
}

type BuildInfo struct {
	Commit  string
	Time    string
	Version string
}

type SystemInfo struct {
	GoVersion  string
	OS         string
	Arch       string
	CPUs       int
	Goroutines int
	MemoryMB   string
}

func snapshot() Snapshot {
	now := time.Now().UTC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	awsInfo := fetchAWS()

	return Snapshot{
		Status:       "ok",
		Uptime:       time.Since(startTime).Round(time.Second).String(),
		StartedAt:    startTime.UTC().Format(time.RFC3339),
		CurrentTime:  now.Format(time.RFC3339),
		RequestCount: requests.Load(),
		AWS:          awsInfo,
		Build: BuildInfo{
			Commit:  BuildCommit,
			Time:    BuildTime,
			Version: BuildVersion,
		},
		System: SystemInfo{
			GoVersion:  runtime.Version(),
			OS:         runtime.GOOS,
			Arch:       runtime.GOARCH,
			CPUs:       runtime.NumCPU(),
			Goroutines: runtime.NumGoroutine(),
			MemoryMB:   fmt.Sprintf("%.1f", float64(m.Alloc)/(1024*1024)),
		},
	}
}

// fetchAWS reads the ECS Task Metadata Endpoint v4 to discover region, AZ,
// task ID, cluster, etc. Falls back gracefully when running locally.
func fetchAWS() AWSInfo {
	info := AWSInfo{
		Region:   getEnv("AWS_REGION", getEnv("AWS_DEFAULT_REGION", "")),
		ImageTag: getEnv("IMAGE_TAG", ""),
	}

	metaURL := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if metaURL == "" {
		// Not running in ECS - probably local docker. Stay graceful.
		return info
	}

	type ecsTask struct {
		Cluster              string `json:"Cluster"`
		TaskARN              string `json:"TaskARN"`
		AvailabilityZone     string `json:"AvailabilityZone"`
		Family               string `json:"Family"`
		Revision             string `json:"Revision"`
		ContainerInstanceARN string `json:"ContainerInstanceARN"`
	}
	type ecsContainer struct {
		Name  string `json:"Name"`
		Image string `json:"Image"`
	}

	client := &http.Client{Timeout: 2 * time.Second}

	if body, err := getURL(client, metaURL); err == nil {
		var c ecsContainer
		if json.Unmarshal(body, &c) == nil {
			info.ContainerName = c.Name
			// Image is like "<account>.dkr.ecr.<region>.amazonaws.com/shipyard:latest"
			if c.Image != "" && info.ImageTag == "" {
				if idx := lastIndex(c.Image, ':'); idx >= 0 {
					info.ImageTag = c.Image[idx+1:]
				}
			}
		}
	}

	if body, err := getURL(client, metaURL+"/task"); err == nil {
		var t ecsTask
		if json.Unmarshal(body, &t) == nil {
			info.Available = true
			info.Cluster = lastSegment(t.Cluster, '/')
			info.AvailZone = t.AvailabilityZone
			if info.Region == "" && len(t.AvailabilityZone) > 1 {
				// AZ "eu-north-1a" -> region "eu-north-1"
				info.Region = t.AvailabilityZone[:len(t.AvailabilityZone)-1]
			}
			info.TaskID = lastSegment(t.TaskARN, '/')
		}
	}

	return info
}

func getURL(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastSegment(s string, sep byte) string {
	if i := lastIndex(s, sep); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ── template ─────────────────────────────────────────────────────────────────

var tmpl = template.Must(template.New("page").Parse(pageHTML))
var healthTmpl = template.Must(template.New("health").Parse(healthHTML))
var infoTmpl = template.Must(template.New("info").Parse(infoHTML))

const commonCSS = `
  *{box-sizing:border-box;margin:0;padding:0}
  :root{--bg:#070a14;--surface:rgba(20,26,42,.65);--surface-hi:rgba(30,38,58,.75);
        --border:rgba(148,163,184,.12);--border-hi:rgba(148,163,184,.22);
        --text:#f1f5f9;--muted:#94a3b8;--dim:#64748b;
        --green:#22d3a4;--orange:#fb923c;--purple:#a78bfa;--pink:#f472b6;--blue:#60a5fa}
  html{background:var(--bg);min-height:100%}
  html,body{font-family:'Inter',-apple-system,system-ui,sans-serif;color:var(--text);
            min-height:100vh;line-height:1.5;-webkit-font-smoothing:antialiased}
  body{background:transparent;padding:3rem 1.25rem;display:flex;flex-direction:column;
       align-items:center;gap:1.75rem;overflow-x:hidden;position:relative}
  body::before,body::after{content:'';position:fixed;border-radius:50%;
                           filter:blur(120px);opacity:.32;z-index:-2;pointer-events:none}
  body::before{width:600px;height:600px;background:radial-gradient(circle,#a78bfa 0%,transparent 70%);
               top:-200px;left:-150px}
  body::after{width:520px;height:520px;background:radial-gradient(circle,#f472b6 0%,transparent 70%);
              top:30%;right:-180px}
  .back{position:absolute;top:1.25rem;left:1.25rem;display:inline-flex;align-items:center;
        gap:.4rem;color:var(--muted);text-decoration:none;font-size:.85rem;font-weight:500;
        padding:.5rem .85rem;border-radius:8px;border:1px solid var(--border);
        background:var(--surface);backdrop-filter:blur(20px);transition:all .2s ease}
  .back:hover{color:var(--text);border-color:var(--border-hi);background:var(--surface-hi)}
  h1{font-size:clamp(2rem,5vw,2.75rem);font-weight:800;letter-spacing:-0.03em;
     background:linear-gradient(180deg,#fff 0%,#cbd5e1 100%);
     -webkit-background-clip:text;background-clip:text;color:transparent;
     text-align:center;line-height:1.05}
  .sub{color:var(--muted);font-size:1rem;text-align:center;max-width:520px}
  @keyframes fadeUp{from{opacity:0;transform:translateY(12px)}to{opacity:1;transform:translateY(0)}}
`

const healthHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Live Status &middot; Shipyard</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;800&family=JetBrains+Mono:wght@500&display=swap" rel="stylesheet">
<style>` + commonCSS + `
  .health-card{background:var(--surface);border:1px solid var(--border);border-radius:20px;
               padding:3rem 2.5rem 2.5rem;max-width:520px;width:100%;text-align:center;
               backdrop-filter:blur(20px);animation:fadeUp .6s ease-out both;
               position:relative;overflow:hidden}
  .health-card::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;
                       background:linear-gradient(90deg,transparent,#22d3a4 50%,transparent)}
  .ring{width:130px;height:130px;margin:0 auto 1.75rem;display:flex;align-items:center;
        justify-content:center;border-radius:50%;background:rgba(34,211,164,.1);
        border:2px solid rgba(34,211,164,.45);position:relative}
  .ring::before,.ring::after{content:'';position:absolute;inset:-2px;border-radius:50%;
                              border:2px solid rgba(34,211,164,.4);animation:ripple 2.5s ease-out infinite}
  .ring::after{animation-delay:1.25s}
  @keyframes ripple{0%{transform:scale(1);opacity:.7}100%{transform:scale(1.45);opacity:0}}
  .ring svg{width:54px;height:54px;stroke:var(--green);stroke-width:2.5;fill:none;
            stroke-linecap:round;stroke-linejoin:round;
            filter:drop-shadow(0 0 12px rgba(34,211,164,.4))}
  .status-label{font-size:2.4rem;font-weight:800;color:var(--green);letter-spacing:-.025em;
                margin-bottom:.4rem;line-height:1.05}
  .status-sub{color:var(--muted);font-size:.95rem;margin-bottom:2rem}
  .vitals{display:grid;grid-template-columns:repeat(3,1fr);gap:.85rem;
          padding-top:1.5rem;border-top:1px solid var(--border)}
  .vital{padding:.5rem .25rem}
  .vital-k{font-size:.62rem;text-transform:uppercase;letter-spacing:.14em;
           color:var(--dim);font-weight:600;margin-bottom:.45rem;
           font-family:'JetBrains Mono',monospace}
  .vital-v{font-family:'JetBrains Mono',monospace;font-size:1rem;
           color:var(--text);font-weight:600;letter-spacing:-.01em}
  .vital-v.time{font-size:.78rem;color:var(--muted);font-weight:500;line-height:1.3}
  /* mini ECG bar in card */
  .pulse-bar{margin-top:1.75rem;height:46px;border-radius:10px;
             background:rgba(34,211,164,.05);border:1px solid rgba(34,211,164,.15);
             overflow:hidden;position:relative;
             mask-image:linear-gradient(90deg,transparent,#000 10%,#000 90%,transparent);
             -webkit-mask-image:linear-gradient(90deg,transparent,#000 10%,#000 90%,transparent)}
  .pulse-bar svg{display:block;width:200%;height:100%;animation:pulseScroll 4s linear infinite}
  .pulse-bar path{fill:none;stroke:#22d3a4;stroke-width:1.5;stroke-linecap:round;
                  filter:drop-shadow(0 0 4px rgba(34,211,164,.8))}
  @keyframes pulseScroll{from{transform:translateX(0)}to{transform:translateX(-50%)}}
</style></head><body>
  <a class="back" href="/">&larr; Dashboard</a>
  <div class="health-card">
    <div class="ring">
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <polyline points="20 6 9 17 4 12"/>
      </svg>
    </div>
    <div class="status-label">All systems go</div>
    <div class="status-sub">The service is alive and responding</div>
    <div class="vitals">
      <div class="vital"><div class="vital-k">Uptime</div><div class="vital-v">{{.Uptime}}</div></div>
      <div class="vital"><div class="vital-k">Requests</div><div class="vital-v">{{.RequestCount}}</div></div>
      <div class="vital"><div class="vital-k">Memory</div><div class="vital-v">{{.System.MemoryMB}} MB</div></div>
    </div>
    <div class="pulse-bar">
      <svg viewBox="0 0 1200 46" preserveAspectRatio="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M0,23 L90,23 L100,23 L108,8 L116,38 L124,18 L132,30 L140,23 L230,23 L240,23 L248,8 L256,38 L264,18 L272,30 L280,23 L370,23 L380,23 L388,8 L396,38 L404,18 L412,30 L420,23 L510,23 L520,23 L528,8 L536,38 L544,18 L552,30 L560,23 L650,23 L660,23 L668,8 L676,38 L684,18 L692,30 L700,23 L790,23 L800,23 L808,8 L816,38 L824,18 L832,30 L840,23 L930,23 L940,23 L948,8 L956,38 L964,18 L972,30 L980,23 L1070,23 L1080,23 L1088,8 L1096,38 L1104,18 L1112,30 L1120,23 L1200,23"/>
      </svg>
    </div>
  </div>
</body></html>`

const infoHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Runtime Snapshot &middot; Shipyard</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>` + commonCSS + `
  .hero{text-align:center;max-width:620px;animation:fadeUp .6s ease-out both}
  /* stat tiles row */
  .tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:1rem;
         width:100%;max-width:1100px;
         animation:fadeUp .8s ease-out .1s both}
  .tile{background:var(--surface);border:1px solid var(--border);border-radius:14px;
        padding:1.25rem 1.4rem;display:flex;flex-direction:column;gap:.35rem;
        backdrop-filter:blur(20px);position:relative;overflow:hidden;
        transition:transform .25s ease,border-color .25s ease,background .25s ease}
  .tile:hover{transform:translateY(-2px);border-color:var(--border-hi);background:var(--surface-hi)}
  .tile::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;
                background:var(--tile-accent,linear-gradient(90deg,transparent,#94a3b8 50%,transparent));
                opacity:.7}
  .tile.t1{--tile-accent:linear-gradient(135deg,#fb923c,#f472b6)}
  .tile.t2{--tile-accent:linear-gradient(135deg,#22d3a4,#60a5fa)}
  .tile.t3{--tile-accent:linear-gradient(135deg,#a78bfa,#60a5fa)}
  .tile.t4{--tile-accent:linear-gradient(135deg,#f472b6,#a78bfa)}
  .tile .label{font-size:.65rem;text-transform:uppercase;letter-spacing:.14em;
               color:var(--dim);font-weight:600;font-family:'JetBrains Mono',monospace}
  .tile .value{font-family:'JetBrains Mono',monospace;font-size:1.75rem;font-weight:600;
               letter-spacing:-.02em;background:var(--tile-accent);
               -webkit-background-clip:text;background-clip:text;color:transparent;
               word-break:break-all}
  .tile .unit{font-size:.95rem;color:var(--muted);font-weight:500;margin-left:.25rem}
  /* detail cards */
  .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:1.25rem;
        width:100%;max-width:1100px;
        animation:fadeUp 1s ease-out .25s both}
  .card{background:var(--surface);border:1px solid var(--border);border-radius:16px;
        padding:1.5rem;backdrop-filter:blur(20px);
        position:relative;overflow:hidden;
        transition:transform .25s ease,border-color .25s ease,background .25s ease}
  .card:hover{transform:translateY(-2px);border-color:var(--border-hi);background:var(--surface-hi)}
  .card::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;
                background:var(--accent,linear-gradient(90deg,transparent,#94a3b8 50%,transparent))}
  .card.cloud{--accent:linear-gradient(135deg,#a78bfa,#60a5fa)}
  .card.build{--accent:linear-gradient(135deg,#f472b6,#a78bfa)}
  .card.system{--accent:linear-gradient(135deg,#22d3a4,#60a5fa)}
  .card-title{display:flex;align-items:center;gap:.6rem;margin-bottom:1rem;
              font-size:.72rem;text-transform:uppercase;letter-spacing:.14em;
              color:var(--muted);font-weight:600;font-family:'JetBrains Mono',monospace}
  .card-title .dot{width:.5rem;height:.5rem;border-radius:50%;
                   box-shadow:0 0 10px currentColor}
  .card.cloud .dot{background:var(--purple);color:var(--purple)}
  .card.build .dot{background:var(--pink);color:var(--pink)}
  .card.system .dot{background:var(--green);color:var(--green)}
  .kv{display:flex;flex-direction:column;gap:.65rem}
  .kv-row{display:flex;justify-content:space-between;align-items:baseline;gap:.75rem;
          padding-bottom:.5rem;border-bottom:1px dashed var(--border)}
  .kv-row:last-child{border-bottom:0;padding-bottom:0}
  .kv-k{color:var(--dim);font-size:.78rem;text-transform:uppercase;letter-spacing:.06em}
  .kv-v{font-family:'JetBrains Mono',monospace;font-size:.85rem;color:var(--text);
        font-weight:500;text-align:right;word-break:break-all}
  .kv-v.muted{color:var(--muted);font-weight:400}
  .kv-v.pill{background:rgba(167,139,250,.12);color:var(--purple);
             border:1px solid rgba(167,139,250,.25);padding:.15rem .55rem;border-radius:6px}
  .pill-ok{background:rgba(34,211,164,.14);color:var(--green);
           border:1px solid rgba(34,211,164,.32);padding:.15rem .55rem;border-radius:6px;
           font-family:'JetBrains Mono',monospace;font-size:.78rem;font-weight:500}
  .pill-no{background:rgba(148,163,184,.12);color:var(--muted);
           border:1px solid var(--border-hi);padding:.15rem .55rem;border-radius:6px;
           font-family:'JetBrains Mono',monospace;font-size:.78rem;font-weight:500}
  /* memory bar */
  .mem-bar{height:5px;background:rgba(148,163,184,.12);border-radius:99px;
           margin-top:.6rem;overflow:hidden}
  .mem-bar>span{display:block;height:100%;background:linear-gradient(90deg,#22d3a4,#60a5fa);
                border-radius:99px;transition:width .6s ease;width:6%}
  .live-badge{display:inline-flex;align-items:center;gap:.4rem;
              background:rgba(34,211,164,.1);color:var(--green);
              border:1px solid rgba(34,211,164,.3);border-radius:99px;
              padding:.32rem .85rem;font-size:.75rem;font-weight:500;margin-top:.5rem}
  .live-badge .dot{width:.4rem;height:.4rem;border-radius:50%;background:var(--green);
                   animation:livePulse 2s ease-out infinite}
  @keyframes livePulse{0%,100%{opacity:1}50%{opacity:.3}}
</style></head><body>
  <a class="back" href="/">&larr; Dashboard</a>
  <section class="hero">
    <h1>Runtime Snapshot</h1>
    <p class="sub">Everything happening inside this container, right now.</p>
    <div class="live-badge"><span class="dot"></span>Auto-refreshing every 2 seconds</div>
  </section>

  <section class="tiles">
    <div class="tile t1">
      <div class="label">Uptime</div>
      <div class="value" data-f="Uptime">{{.Uptime}}</div>
    </div>
    <div class="tile t2">
      <div class="label">Requests served</div>
      <div class="value" data-f="RequestCount">{{.RequestCount}}</div>
    </div>
    <div class="tile t3">
      <div class="label">Memory</div>
      <div class="value"><span data-f="MemoryMB">{{.System.MemoryMB}}</span><span class="unit">MB</span></div>
    </div>
    <div class="tile t4">
      <div class="label">Goroutines</div>
      <div class="value" data-f="Goroutines">{{.System.Goroutines}}</div>
    </div>
  </section>

  <section class="grid">
    <div class="card cloud">
      <div class="card-title"><span class="dot"></span>Cloud environment</div>
      <div class="kv">
        <div class="kv-row"><span class="kv-k">Running on ECS</span><span class="kv-v">{{if .AWS.Available}}<span class="pill-ok">yes</span>{{else}}<span class="pill-no">local</span>{{end}}</span></div>
        <div class="kv-row"><span class="kv-k">Region</span><span class="kv-v">{{if .AWS.Region}}{{.AWS.Region}}{{else}}&mdash;{{end}}</span></div>
        <div class="kv-row"><span class="kv-k">Availability zone</span><span class="kv-v">{{if .AWS.AvailZone}}{{.AWS.AvailZone}}{{else}}&mdash;{{end}}</span></div>
        <div class="kv-row"><span class="kv-k">Cluster</span><span class="kv-v">{{if .AWS.Cluster}}<span class="pill">{{.AWS.Cluster}}</span>{{else}}&mdash;{{end}}</span></div>
        <div class="kv-row"><span class="kv-k">Task ID</span><span class="kv-v muted">{{if .AWS.TaskID}}{{.AWS.TaskID}}{{else}}&mdash;{{end}}</span></div>
        <div class="kv-row"><span class="kv-k">Container</span><span class="kv-v">{{if .AWS.ContainerName}}{{.AWS.ContainerName}}{{else}}&mdash;{{end}}</span></div>
      </div>
    </div>

    <div class="card build">
      <div class="card-title"><span class="dot"></span>Build &amp; Image</div>
      <div class="kv">
        <div class="kv-row"><span class="kv-k">Commit</span><span class="kv-v">{{.Build.Commit}}</span></div>
        <div class="kv-row"><span class="kv-k">Image tag</span><span class="kv-v"><span class="pill">{{if .AWS.ImageTag}}{{.AWS.ImageTag}}{{else}}{{.Build.Version}}{{end}}</span></span></div>
        <div class="kv-row"><span class="kv-k">Built at</span><span class="kv-v muted">{{.Build.Time}}</span></div>
        <div class="kv-row"><span class="kv-k">Started at</span><span class="kv-v muted" data-f="StartedAt">{{.StartedAt}}</span></div>
        <div class="kv-row"><span class="kv-k">Server time</span><span class="kv-v muted" data-f="CurrentTime">{{.CurrentTime}}</span></div>
      </div>
    </div>

    <div class="card system">
      <div class="card-title"><span class="dot"></span>System &amp; Resources</div>
      <div class="kv">
        <div class="kv-row"><span class="kv-k">Go version</span><span class="kv-v">{{.System.GoVersion}}</span></div>
        <div class="kv-row"><span class="kv-k">OS &amp; arch</span><span class="kv-v">{{.System.OS}} &middot; {{.System.Arch}}</span></div>
        <div class="kv-row"><span class="kv-k">CPUs</span><span class="kv-v">{{.System.CPUs}}</span></div>
        <div class="kv-row"><span class="kv-k">Goroutines</span><span class="kv-v" data-f="Goroutines">{{.System.Goroutines}}</span></div>
        <div class="kv-row" style="border-bottom:0"><span class="kv-k">Memory in use</span><span class="kv-v"><span data-f="MemoryMB">{{.System.MemoryMB}}</span> MB</span></div>
      </div>
      <div class="mem-bar"><span id="memBar"></span></div>
    </div>
  </section>

  <script>
    async function refresh(){
      try{
        const r = await fetch('/api/info', {cache:'no-store', headers:{Accept:'application/json'}});
        if(!r.ok) return;
        const d = await r.json();
        const map = {
          Uptime:d.Uptime, RequestCount:d.RequestCount,
          MemoryMB:d.System.MemoryMB, Goroutines:d.System.Goroutines,
          StartedAt:d.StartedAt, CurrentTime:d.CurrentTime,
        };
        for(const [k,v] of Object.entries(map)){
          document.querySelectorAll('[data-f="'+k+'"]').forEach(el => {
            if(v !== undefined && el.textContent !== String(v)) el.textContent = v;
          });
        }
        const mb = parseFloat(d.System.MemoryMB);
        const bar = document.getElementById('memBar');
        if(bar && !isNaN(mb)) bar.style.width = Math.min(100, Math.max(3, (mb / 64) * 100)) + '%';
      }catch(e){}
    }
    setInterval(refresh, 2000);
    refresh();
  </script>
</body></html>`


const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Shipyard — running on AWS ECS Fargate</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
  <style>
    *{box-sizing:border-box;margin:0;padding:0}
    :root{
      --bg:#070a14;
      --surface:rgba(20,26,42,.65);
      --surface-hi:rgba(30,38,58,.75);
      --border:rgba(148,163,184,.12);
      --border-hi:rgba(148,163,184,.22);
      --text:#f1f5f9;--muted:#94a3b8;--dim:#64748b;
      --green:#22d3a4;--orange:#fb923c;--purple:#a78bfa;--pink:#f472b6;--blue:#60a5fa;
      --grad-orange:linear-gradient(135deg,#fb923c,#f472b6);
      --grad-purple:linear-gradient(135deg,#a78bfa,#60a5fa);
      --grad-pink:linear-gradient(135deg,#f472b6,#a78bfa);
      --grad-green:linear-gradient(135deg,#22d3a4,#60a5fa);
    }
    html{background:var(--bg);min-height:100%}
    html,body{font-family:'Inter',-apple-system,BlinkMacSystemFont,system-ui,sans-serif;
              color:var(--text);min-height:100vh;line-height:1.5;
              -webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
    body{background:transparent;padding:2.5rem 1.25rem 3rem;
         display:flex;flex-direction:column;align-items:center;
         gap:2rem;overflow-x:hidden}
    .hero,.grid,.stack,footer{position:relative;z-index:2}
    /* aurora backdrop */
    body::before,body::after{content:'';position:fixed;border-radius:50%;
                             filter:blur(120px);opacity:.32;z-index:-2;pointer-events:none;
                             animation:drift 18s ease-in-out infinite}
    body::before{width:600px;height:600px;background:radial-gradient(circle,#a78bfa 0%,transparent 70%);
                 top:-200px;left:-150px}
    body::after{width:520px;height:520px;background:radial-gradient(circle,#f472b6 0%,transparent 70%);
                top:30%;right:-180px;animation-delay:-9s;animation-direction:reverse}
    @keyframes drift{0%,100%{transform:translate(0,0) scale(1)}50%{transform:translate(40px,30px) scale(1.1)}}
    body::before,body::after{will-change:transform}
    .gridbg{position:fixed;inset:0;z-index:-1;pointer-events:none;
            background-image:linear-gradient(rgba(148,163,184,.04) 1px,transparent 1px),
                             linear-gradient(90deg,rgba(148,163,184,.04) 1px,transparent 1px);
            background-size:48px 48px;mask-image:radial-gradient(ellipse at center,#000 35%,transparent 80%)}

    /* heartbeat ECG line — runs across the viewport horizontally behind content */
    .heartbeat{position:fixed;left:0;right:0;top:55%;transform:translateY(-50%);
               height:140px;z-index:0;pointer-events:none;
               mask-image:linear-gradient(90deg,transparent,#000 10%,#000 90%,transparent);
               -webkit-mask-image:linear-gradient(90deg,transparent,#000 10%,#000 90%,transparent);
               opacity:.85;overflow:hidden}
    .heartbeat svg{display:block;width:200%;height:100%;
                   animation:ecg 5s linear infinite}
    .heartbeat path{fill:none;stroke:#22d3a4;stroke-width:1.8;
                    stroke-linecap:round;stroke-linejoin:round;
                    filter:drop-shadow(0 0 6px rgba(34,211,164,.85))
                           drop-shadow(0 0 16px rgba(34,211,164,.4))}
    @keyframes ecg{from{transform:translateX(0)}to{transform:translateX(-50%)}}

    /* hero */
    .hero{text-align:center;max-width:640px;width:100%;padding-top:1rem;
          animation:fadeUp .8s ease-out both}
    .logo{width:72px;height:72px;margin:0 auto .75rem;display:block;
          filter:drop-shadow(0 8px 28px rgba(167,139,250,.45))}
    h1{font-size:clamp(2.8rem,7vw,4.25rem);font-weight:800;letter-spacing:-0.04em;
       background:linear-gradient(180deg,#fff 0%,#cbd5e1 100%);
       -webkit-background-clip:text;background-clip:text;color:transparent;
       margin-bottom:.9rem;line-height:1}
    .badge{display:inline-flex;align-items:center;gap:.5rem;
           background:rgba(34,211,164,.12);color:var(--green);
           border:1px solid rgba(34,211,164,.35);border-radius:99px;
           padding:.4rem 1rem;font-size:.82rem;font-weight:500;
           backdrop-filter:blur(10px);-webkit-backdrop-filter:blur(10px)}
    .badge .dot{width:.5rem;height:.5rem;border-radius:50%;background:var(--green);
                box-shadow:0 0 0 0 rgba(34,211,164,.7);animation:pulse 2s ease-out infinite}
    @keyframes pulse{0%{box-shadow:0 0 0 0 rgba(34,211,164,.6)}
                     70%{box-shadow:0 0 0 10px rgba(34,211,164,0)}
                     100%{box-shadow:0 0 0 0 rgba(34,211,164,0)}}

    /* grid + cards */
    .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));
          gap:1.25rem;width:100%;max-width:1100px;
          animation:fadeUp 1s ease-out .15s both}
    .card{position:relative;background:var(--surface);
          border:1px solid var(--border);border-radius:16px;
          padding:1.5rem 1.5rem 1.4rem;display:flex;flex-direction:column;gap:.85rem;
          backdrop-filter:blur(20px) saturate(140%);
          -webkit-backdrop-filter:blur(20px) saturate(140%);
          transition:transform .25s ease,border-color .25s ease,background .25s ease;
          overflow:hidden}
    .card::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;
                  background:var(--accent,linear-gradient(90deg,transparent,#94a3b8 50%,transparent));
                  opacity:.7}
    .card:hover{transform:translateY(-2px);border-color:var(--border-hi);background:var(--surface-hi)}
    .card.runtime{--accent:var(--grad-orange)}
    .card.aws{--accent:var(--grad-purple)}
    .card.build{--accent:var(--grad-pink)}
    .card.system{--accent:var(--grad-green)}

    .card-title{display:flex;align-items:center;gap:.55rem;font-size:.7rem;
                text-transform:uppercase;letter-spacing:.12em;color:var(--muted);font-weight:600}
    .card-title .swatch{width:.55rem;height:.55rem;border-radius:50%;
                        box-shadow:0 0 12px currentColor}
    .runtime .swatch{background:var(--orange);color:var(--orange)}
    .aws .swatch{background:var(--purple);color:var(--purple)}
    .build .swatch{background:var(--pink);color:var(--pink)}
    .system .swatch{background:var(--green);color:var(--green)}

    /* headline number per card */
    .headline{font-family:'JetBrains Mono','SF Mono',ui-monospace,Menlo,monospace;
              font-size:1.85rem;font-weight:600;letter-spacing:-.02em;line-height:1.1;
              background:var(--accent);-webkit-background-clip:text;background-clip:text;
              color:transparent;margin:.1rem 0 .25rem;word-break:break-all}
    .headline-sub{color:var(--muted);font-size:.78rem;margin-top:-.15rem;margin-bottom:.4rem;
                  font-family:'JetBrains Mono',ui-monospace,monospace}

    .rows{display:flex;flex-direction:column;gap:.55rem;margin-top:.25rem;
          padding-top:.85rem;border-top:1px solid var(--border)}
    .row{display:flex;justify-content:space-between;align-items:baseline;gap:.75rem;font-size:.85rem}
    .row .k{color:var(--dim);font-size:.78rem;text-transform:uppercase;letter-spacing:.05em}
    .row .v{color:var(--text);font-family:'JetBrains Mono','SF Mono',ui-monospace,Menlo,monospace;
            font-size:.82rem;text-align:right;word-break:break-all;font-weight:500}
    .row .v.muted{color:var(--muted);font-weight:400}
    .pill{display:inline-block;background:rgba(167,139,250,.12);color:var(--purple);
          padding:.15rem .55rem;border-radius:6px;font-family:'JetBrains Mono',monospace;
          font-size:.78rem;border:1px solid rgba(167,139,250,.25)}

    /* memory bar */
    .memory-bar{height:4px;background:rgba(148,163,184,.12);border-radius:99px;
                margin-top:.3rem;overflow:hidden}
    .memory-bar > span{display:block;height:100%;background:var(--grad-green);
                       border-radius:99px;transition:width .6s ease}

    /* tech stack */
    .stack{width:100%;max-width:980px;display:flex;flex-direction:column;
           align-items:center;gap:1.35rem;
           animation:fadeUp 1.1s ease-out .25s both}
    .stack-label{display:flex;align-items:center;gap:.85rem;width:100%;max-width:560px;
                 color:var(--dim);font-size:.7rem;font-weight:600;
                 text-transform:uppercase;letter-spacing:.18em;
                 font-family:'JetBrains Mono',ui-monospace,monospace}
    .stack-label::before,.stack-label::after{content:'';flex:1;height:1px;
                                             background:linear-gradient(90deg,transparent,var(--border-hi),transparent)}
    .tech-row{display:flex;flex-wrap:wrap;justify-content:center;gap:.7rem}
    .tech{display:inline-flex;align-items:center;gap:.65rem;
          background:rgba(255,255,255,.04);border:1px solid var(--border-hi);
          border-radius:999px;padding:.7rem 1.25rem;
          backdrop-filter:blur(20px);-webkit-backdrop-filter:blur(20px);
          font-size:.95rem;font-weight:500;color:var(--text);
          transition:transform .25s ease,border-color .25s ease,background .25s ease}
    .tech:hover{transform:translateY(-2px);border-color:rgba(167,139,250,.4);
                background:rgba(255,255,255,.07)}
    .tech img{width:22px;height:22px;display:block}
    .tech svg.tech-icon{display:block;height:22px;width:auto}
    .aws-services{display:flex;flex-wrap:wrap;justify-content:center;gap:.55rem;
                  max-width:820px;margin-top:.35rem}
    .svc{display:inline-flex;align-items:center;gap:.45rem;
         font-family:'JetBrains Mono',ui-monospace,monospace;font-size:.82rem;
         color:var(--text);background:rgba(255,255,255,.04);
         border:1px solid var(--border-hi);border-radius:8px;
         padding:.42rem .8rem;font-weight:500;
         backdrop-filter:blur(20px);-webkit-backdrop-filter:blur(20px);
         transition:transform .2s ease,border-color .2s ease,background .2s ease}
    .svc:hover{transform:translateY(-1px);border-color:rgba(167,139,250,.45);
               background:rgba(255,255,255,.07)}
    .svc::before{content:'';width:.45rem;height:.45rem;border-radius:50%;
                 background:var(--svc-color,#94a3b8);box-shadow:0 0 8px var(--svc-color,#94a3b8)}
    .svc.compute{--svc-color:#fb923c}
    .svc.network{--svc-color:#a78bfa}
    .svc.security{--svc-color:#f472b6}
    .svc.storage{--svc-color:#22d3a4}
    .svc.observability{--svc-color:#60a5fa}

    /* endpoint cards (replaces plain footer links) */
    .endpoints{display:flex;flex-wrap:wrap;justify-content:center;gap:.85rem;
               margin-top:.25rem;width:100%;max-width:680px;
               animation:fadeUp 1.2s ease-out .4s both}
    .endpoint{display:flex;align-items:center;gap:.85rem;
              text-decoration:none;color:var(--text);
              background:var(--surface);border:1px solid var(--border);
              border-radius:14px;padding:.85rem 1.15rem;flex:1;min-width:220px;
              backdrop-filter:blur(20px);-webkit-backdrop-filter:blur(20px);
              transition:transform .25s ease,border-color .25s ease,background .25s ease}
    .endpoint:hover{transform:translateY(-2px);border-color:var(--border-hi);
                    background:var(--surface-hi)}
    .endpoint-icon{width:38px;height:38px;border-radius:10px;flex-shrink:0;
                   display:flex;align-items:center;justify-content:center}
    .endpoint-icon.health{background:rgba(34,211,164,.14);color:var(--green)}
    .endpoint-icon.info{background:rgba(96,165,250,.14);color:var(--blue)}
    .endpoint-icon svg{width:20px;height:20px;stroke:currentColor;
                       fill:none;stroke-width:2;stroke-linecap:round;stroke-linejoin:round}
    .endpoint{position:relative}
    .endpoint-text{display:flex;flex-direction:column;gap:.1rem;min-width:0}
    .endpoint-path{font-family:'JetBrains Mono',ui-monospace,monospace;font-size:.9rem;
                   font-weight:600;color:var(--text)}
    .endpoint-desc{font-size:.75rem;color:var(--muted)}

    /* hover preview popover — opens BELOW the card to avoid overlapping the AWS pills */
    .preview{position:absolute;top:calc(100% + 12px);left:0;right:0;
             background:#0b1326;border:1px solid var(--border-hi);
             border-radius:14px;padding:1rem 1.1rem;
             opacity:0;visibility:hidden;transform:translateY(-6px);
             transition:opacity .2s ease,transform .2s ease,visibility .2s ease;
             pointer-events:none;z-index:1000;
             box-shadow:0 14px 36px rgba(0,0,0,.5)}
    .preview::before{content:'';position:absolute;bottom:100%;left:28px;
                    border:7px solid transparent;border-bottom-color:var(--border-hi)}
    .preview::after{content:'';position:absolute;bottom:100%;left:28px;
                    border:7px solid transparent;border-bottom-color:#0b1326;
                    margin-bottom:-1px}
    .endpoint:hover .preview{opacity:1;visibility:visible;transform:translateY(0)}
    .preview-head{display:flex;align-items:center;gap:.55rem;
                  padding-bottom:.7rem;margin-bottom:.7rem;
                  border-bottom:1px solid var(--border)}
    .preview-head .dot{width:.5rem;height:.5rem;border-radius:50%;
                       background:var(--green);box-shadow:0 0 8px var(--green);
                       animation:pulse 2s ease-out infinite}
    .preview-head.info .dot{background:var(--blue);box-shadow:0 0 8px var(--blue)}
    .preview-head-text{font-size:.85rem;font-weight:600;color:var(--text)}
    .preview-mini{display:grid;grid-template-columns:repeat(2,1fr);gap:.65rem .9rem}
    .preview-mini.three{grid-template-columns:repeat(3,1fr)}
    .mini-k{font-size:.6rem;text-transform:uppercase;letter-spacing:.12em;
            color:var(--dim);font-weight:600;font-family:'JetBrains Mono',monospace;
            margin-bottom:.2rem}
    .mini-v{font-family:'JetBrains Mono',monospace;font-size:.82rem;
            color:var(--text);font-weight:600;letter-spacing:-.01em}

    @keyframes fadeUp{from{opacity:0;transform:translateY(12px)}
                      to{opacity:1;transform:translateY(0)}}

    @media (max-width:540px){
      body{padding:1.5rem .75rem 2rem;gap:1.5rem}
      .card{padding:1.25rem}
      .headline{font-size:1.55rem}
    }
  </style>
</head>
<body>
  <div class="gridbg"></div>
  <div class="heartbeat" aria-hidden="true">
    <svg viewBox="0 0 2400 120" preserveAspectRatio="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M0,60 L180,60 L195,60 L205,30 L215,90 L225,45 L235,75 L245,60 L420,60 L435,60 L445,30 L455,90 L465,45 L475,75 L485,60 L660,60 L675,60 L685,30 L695,90 L705,45 L715,75 L725,60 L900,60 L915,60 L925,30 L935,90 L945,45 L955,75 L965,60 L1140,60 L1155,60 L1165,30 L1175,90 L1185,45 L1195,75 L1205,60 L1380,60 L1395,60 L1405,30 L1415,90 L1425,45 L1435,75 L1445,60 L1620,60 L1635,60 L1645,30 L1655,90 L1665,45 L1675,75 L1685,60 L1860,60 L1875,60 L1885,30 L1895,90 L1905,45 L1915,75 L1925,60 L2100,60 L2115,60 L2125,30 L2135,90 L2145,45 L2155,75 L2165,60 L2340,60 L2400,60"/>
    </svg>
  </div>

  <section class="hero">
    <svg class="logo" viewBox="0 0 64 64" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
      <defs>
        <linearGradient id="anchorGrad" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stop-color="#a78bfa"/>
          <stop offset="100%" stop-color="#f472b6"/>
        </linearGradient>
      </defs>
      <circle cx="32" cy="14" r="5" stroke="url(#anchorGrad)" stroke-width="2.5"/>
      <path d="M32 19 L32 52" stroke="url(#anchorGrad)" stroke-width="2.5" stroke-linecap="round"/>
      <path d="M22 28 L42 28" stroke="url(#anchorGrad)" stroke-width="2.5" stroke-linecap="round"/>
      <path d="M14 38 Q14 52 32 52 Q50 52 50 38" stroke="url(#anchorGrad)" stroke-width="2.5" stroke-linecap="round" fill="none"/>
      <path d="M10 38 L18 38" stroke="url(#anchorGrad)" stroke-width="2.5" stroke-linecap="round"/>
      <path d="M46 38 L54 38" stroke="url(#anchorGrad)" stroke-width="2.5" stroke-linecap="round"/>
    </svg>
    <h1>Shipyard</h1>
    <div class="badge"><span class="dot"></span>Healthy</div>
  </section>

  <section class="grid">
    <div class="card runtime">
      <div class="card-title"><span class="swatch"></span>Runtime</div>
      <div class="headline" data-field="Uptime">{{.Uptime}}</div>
      <div class="headline-sub">since <span data-field="StartedAt">{{.StartedAt}}</span></div>
      <div class="rows">
        <div class="row"><span class="k">Server time</span><span class="v muted" data-field="CurrentTime">{{.CurrentTime}}</span></div>
        <div class="row"><span class="k">Requests served</span><span class="v" data-field="RequestCount">{{.RequestCount}}</span></div>
      </div>
    </div>

    <div class="card aws">
      <div class="card-title"><span class="swatch"></span>AWS Environment</div>
      {{if .AWS.Available}}
      <div class="headline">{{.AWS.Region}}</div>
      <div class="headline-sub">{{.AWS.AvailZone}} &middot; cluster <span class="pill">{{.AWS.Cluster}}</span></div>
      <div class="rows">
        <div class="row"><span class="k">Task ID</span><span class="v">{{.AWS.TaskID}}</span></div>
        <div class="row"><span class="k">Container</span><span class="v">{{.AWS.ContainerName}}</span></div>
      </div>
      {{else}}
      <div class="headline">local</div>
      <div class="headline-sub">not running on ECS</div>
      <div class="rows">
        <div class="row"><span class="k">Region</span><span class="v muted">{{if .AWS.Region}}{{.AWS.Region}}{{else}}—{{end}}</span></div>
        <div class="row"><span class="k">Metadata</span><span class="v muted">unavailable</span></div>
      </div>
      {{end}}
    </div>

    <div class="card build">
      <div class="card-title"><span class="swatch"></span>Build</div>
      <div class="headline">{{.Build.Commit}}</div>
      <div class="headline-sub">tag <span class="pill">{{if .AWS.ImageTag}}{{.AWS.ImageTag}}{{else}}{{.Build.Version}}{{end}}</span></div>
      <div class="rows">
        <div class="row"><span class="k">Build time</span><span class="v muted">{{.Build.Time}}</span></div>
      </div>
    </div>

    <div class="card system">
      <div class="card-title"><span class="swatch"></span>System</div>
      <div class="headline"><span data-field="MemoryMB">{{.System.MemoryMB}}</span> MB</div>
      <div class="memory-bar"><span id="memBar" style="width:6%"></span></div>
      <div class="rows">
        <div class="row"><span class="k">Go</span><span class="v">{{.System.GoVersion}}</span></div>
        <div class="row"><span class="k">OS / arch</span><span class="v">{{.System.OS}} / {{.System.Arch}}</span></div>
        <div class="row"><span class="k">CPUs</span><span class="v">{{.System.CPUs}}</span></div>
        <div class="row"><span class="k">Goroutines</span><span class="v" data-field="Goroutines">{{.System.Goroutines}}</span></div>
      </div>
    </div>
  </section>

  <section class="stack">
    <div class="stack-label">Built with</div>
    <div class="tech-row">
      <span class="tech">
        <svg class="tech-icon" viewBox="0 0 40 26" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
          <text x="20" y="14" text-anchor="middle" font-family="Helvetica,Arial,sans-serif" font-weight="900" font-size="14" fill="#FF9900" letter-spacing="-0.5">aws</text>
          <path d="M6 21 Q20 26 34 21" stroke="#FF9900" stroke-width="2.2" fill="none" stroke-linecap="round"/>
          <path d="M30 18.5 L34 21 L30 23.5" stroke="#FF9900" stroke-width="2.2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
        AWS
      </span>
      <span class="tech"><img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/docker/docker-original.svg" alt=""/>Docker</span>
      <span class="tech"><img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/go/go-original.svg" alt=""/>Go</span>
      <span class="tech"><img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/terraform/terraform-original.svg" alt=""/>Terraform</span>
      <span class="tech"><img src="https://cdn.jsdelivr.net/gh/devicons/devicon/icons/githubactions/githubactions-original.svg" alt=""/>GitHub Actions</span>
    </div>
    <div class="aws-services">
      <span class="svc compute">ECS Fargate</span>
      <span class="svc network">ALB</span>
      <span class="svc network">Route 53</span>
      <span class="svc network">VPC</span>
      <span class="svc security">ACM</span>
      <span class="svc security">IAM · OIDC</span>
      <span class="svc storage">ECR</span>
      <span class="svc storage">S3</span>
      <span class="svc observability">CloudWatch</span>
    </div>
  </section>

  <section class="endpoints">
    <a class="endpoint" href="/health">
      <span class="endpoint-icon health">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/>
        </svg>
      </span>
      <span class="endpoint-text">
        <span class="endpoint-path">Live Status</span>
        <span class="endpoint-desc">Real-time service health</span>
      </span>
      <div class="preview">
        <div class="preview-head">
          <span class="dot"></span>
          <span class="preview-head-text">All systems go</span>
        </div>
        <div class="preview-mini">
          <div><div class="mini-k">Uptime</div><div class="mini-v" data-pf="Uptime">{{.Uptime}}</div></div>
          <div><div class="mini-k">Requests</div><div class="mini-v" data-pf="RequestCount">{{.RequestCount}}</div></div>
        </div>
      </div>
    </a>
    <a class="endpoint" href="/api/info">
      <span class="endpoint-icon info">
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path d="M3 3h7v7H3z M14 3h7v4h-7z M14 10h7v11h-7z M3 14h7v7H3z"/>
        </svg>
      </span>
      <span class="endpoint-text">
        <span class="endpoint-path">Runtime Snapshot</span>
        <span class="endpoint-desc">Live metrics &amp; environment</span>
      </span>
      <div class="preview">
        <div class="preview-head info">
          <span class="dot"></span>
          <span class="preview-head-text">Live metrics</span>
        </div>
        <div class="preview-mini three">
          <div><div class="mini-k">Region</div><div class="mini-v" data-pf="Region">{{if .AWS.Region}}{{.AWS.Region}}{{else}}local{{end}}</div></div>
          <div><div class="mini-k">Memory</div><div class="mini-v"><span data-pf="MemoryMB">{{.System.MemoryMB}}</span> MB</div></div>
          <div><div class="mini-k">Goroutines</div><div class="mini-v" data-pf="Goroutines">{{.System.Goroutines}}</div></div>
        </div>
      </div>
    </a>
  </section>

  <script>
    // Live-refresh the dashboard every 2s by polling /api/info.
    // Fields are matched by [data-field="<JSON path>"] selectors.
    const fields = {
      'Uptime': d => d.Uptime,
      'StartedAt': d => d.StartedAt,
      'CurrentTime': d => d.CurrentTime,
      'RequestCount': d => d.RequestCount,
      'MemoryMB': d => d.System.MemoryMB,
      'Goroutines': d => d.System.Goroutines,
    };
    async function refresh(){
      try{
        const r = await fetch('/api/info', {cache:'no-store'});
        if(!r.ok) return;
        const d = await r.json();
        for(const [k, get] of Object.entries(fields)){
          document.querySelectorAll('[data-field="'+k+'"]').forEach(el => {
            const v = get(d);
            if(v !== undefined && el.textContent !== String(v)) el.textContent = v;
          });
        }
        const mb = parseFloat(d.System.MemoryMB);
        const bar = document.getElementById('memBar');
        if(bar && !isNaN(mb)) bar.style.width = Math.min(100, Math.max(3, (mb / 64) * 100)) + '%';
        // refresh hover preview tiles
        const pmap = {
          Uptime:d.Uptime, RequestCount:d.RequestCount,
          MemoryMB:d.System.MemoryMB, Goroutines:d.System.Goroutines,
          Region:d.AWS.Region || 'local',
        };
        for(const [k,v] of Object.entries(pmap)){
          document.querySelectorAll('[data-pf="'+k+'"]').forEach(el => {
            if(v !== undefined && el.textContent !== String(v)) el.textContent = v;
          });
        }
      }catch(e){}
    }
    setInterval(refresh, 2000);
    refresh();
  </script>
</body>
</html>`
