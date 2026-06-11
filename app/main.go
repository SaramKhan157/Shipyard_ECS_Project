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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snapshot())
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

const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Shipyard — running on AWS ECS Fargate</title>
  <style>
    *{box-sizing:border-box;margin:0;padding:0}
    :root{
      --bg:#0f172a;--surface:#1e293b;--surface-2:#0b1322;
      --border:#334155;--text:#e2e8f0;--muted:#94a3b8;--dim:#64748b;
      --green:#22c55e;--orange:#fb923c;--purple:#a78bfa;--pink:#f472b6;
    }
    html,body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;
              background:var(--bg);color:var(--text);min-height:100vh;line-height:1.5}
    body{padding:2rem 1rem;display:flex;flex-direction:column;align-items:center;gap:1.5rem}
    .hero{text-align:center;max-width:560px;width:100%;padding-top:2rem}
    .anchor{font-size:3rem;margin-bottom:.5rem}
    h1{font-size:2.5rem;letter-spacing:-0.02em;margin-bottom:.25rem}
    .tagline{color:var(--muted);font-size:1rem;margin-bottom:1rem}
    .badge{display:inline-flex;align-items:center;gap:.4rem;background:#22c55e22;color:var(--green);
           border:1px solid #22c55e55;border-radius:99px;padding:.3rem .9rem;font-size:.85rem;font-weight:500}
    .badge .dot{width:.5rem;height:.5rem;border-radius:50%;background:var(--green);
                box-shadow:0 0 8px var(--green)}
    .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));
          gap:1rem;width:100%;max-width:960px}
    .card{background:var(--surface);border:1px solid var(--border);border-radius:12px;
          padding:1.25rem 1.5rem;display:flex;flex-direction:column;gap:.65rem}
    .card-title{display:flex;align-items:center;gap:.5rem;font-size:.75rem;
                text-transform:uppercase;letter-spacing:.08em;color:var(--muted);font-weight:600}
    .card-title .swatch{width:.6rem;height:.6rem;border-radius:50%}
    .swatch.compute{background:var(--orange)}
    .swatch.aws{background:var(--purple)}
    .swatch.build{background:var(--pink)}
    .swatch.system{background:var(--green)}
    .row{display:flex;justify-content:space-between;align-items:baseline;gap:.5rem;font-size:.9rem}
    .row .k{color:var(--dim)}
    .row .v{color:var(--text);font-family:'SF Mono',ui-monospace,Menlo,monospace;
            font-size:.85rem;text-align:right;word-break:break-all}
    .row .v.muted{color:var(--muted)}
    footer{color:var(--dim);font-size:.8rem;text-align:center;margin-top:1rem;
           font-family:'SF Mono',ui-monospace,Menlo,monospace}
    footer a{color:var(--muted);text-decoration:none;border-bottom:1px dotted var(--dim)}
    footer a:hover{color:var(--text)}
  </style>
</head>
<body>
  <section class="hero">
    <div class="anchor">&#9875;</div>
    <h1>Shipyard</h1>
    <p class="tagline">A containerised Go service deployed end-to-end on AWS</p>
    <div class="badge"><span class="dot"></span>Healthy</div>
  </section>

  <section class="grid">
    <div class="card">
      <div class="card-title"><span class="swatch compute"></span>Runtime</div>
      <div class="row"><span class="k">Uptime</span><span class="v">{{.Uptime}}</span></div>
      <div class="row"><span class="k">Started</span><span class="v muted">{{.StartedAt}}</span></div>
      <div class="row"><span class="k">Server time</span><span class="v muted">{{.CurrentTime}}</span></div>
      <div class="row"><span class="k">Requests served</span><span class="v">{{.RequestCount}}</span></div>
    </div>

    <div class="card">
      <div class="card-title"><span class="swatch aws"></span>AWS Environment</div>
      {{if .AWS.Available}}
      <div class="row"><span class="k">Region</span><span class="v">{{.AWS.Region}}</span></div>
      <div class="row"><span class="k">Availability zone</span><span class="v">{{.AWS.AvailZone}}</span></div>
      <div class="row"><span class="k">ECS cluster</span><span class="v">{{.AWS.Cluster}}</span></div>
      <div class="row"><span class="k">Task ID</span><span class="v">{{.AWS.TaskID}}</span></div>
      <div class="row"><span class="k">Container</span><span class="v">{{.AWS.ContainerName}}</span></div>
      {{else}}
      <div class="row"><span class="k">Region</span><span class="v muted">{{if .AWS.Region}}{{.AWS.Region}}{{else}}local{{end}}</span></div>
      <div class="row"><span class="k">Environment</span><span class="v muted">not running on ECS</span></div>
      {{end}}
    </div>

    <div class="card">
      <div class="card-title"><span class="swatch build"></span>Build</div>
      <div class="row"><span class="k">Commit</span><span class="v">{{.Build.Commit}}</span></div>
      <div class="row"><span class="k">Build time</span><span class="v muted">{{.Build.Time}}</span></div>
      <div class="row"><span class="k">Image tag</span><span class="v">{{if .AWS.ImageTag}}{{.AWS.ImageTag}}{{else}}{{.Build.Version}}{{end}}</span></div>
    </div>

    <div class="card">
      <div class="card-title"><span class="swatch system"></span>System</div>
      <div class="row"><span class="k">Go</span><span class="v">{{.System.GoVersion}}</span></div>
      <div class="row"><span class="k">OS / arch</span><span class="v">{{.System.OS}} / {{.System.Arch}}</span></div>
      <div class="row"><span class="k">CPUs</span><span class="v">{{.System.CPUs}}</span></div>
      <div class="row"><span class="k">Goroutines</span><span class="v">{{.System.Goroutines}}</span></div>
      <div class="row"><span class="k">Memory</span><span class="v">{{.System.MemoryMB}} MB</span></div>
    </div>
  </section>

  <footer>
    <a href="/health">/health</a> &middot; <a href="/api/info">/api/info</a>
  </footer>
</body>
</html>`
