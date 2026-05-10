package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

var startTime = time.Now()

func main() {
	port := getEnv("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", rootHandler)

	log.Printf("shipyard listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(startTime).Round(time.Second)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, page, uptime)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const page = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Shipyard</title>
  <style>
    *{box-sizing:border-box;margin:0;padding:0}
    body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;
         display:flex;align-items:center;justify-content:center;min-height:100vh}
    .card{background:#1e293b;border:1px solid #334155;border-radius:12px;
          padding:2rem 2.5rem;max-width:480px;width:90%%;text-align:center}
    h1{font-size:2rem;margin-bottom:.5rem}
    .anchor{font-size:3rem;margin-bottom:1rem}
    p{color:#94a3b8;margin-top:.5rem}
    .badge{display:inline-block;background:#22c55e22;color:#22c55e;
           border:1px solid #22c55e44;border-radius:99px;
           padding:.25rem .75rem;font-size:.85rem;margin-top:1rem}
  </style>
</head>
<body>
  <div class="card">
    <div class="anchor">&#9875;</div>
    <h1>Shipyard</h1>
    <p>Running on AWS ECS Fargate</p>
    <p>Uptime: %s</p>
    <div class="badge">&#9679; Healthy</div>
  </div>
</body>
</html>`
