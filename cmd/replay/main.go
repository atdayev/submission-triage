package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/atdayev/submission-triage/pkg/postmarkeml"
)

func main() {
	dir := flag.String("dir", "./testdata/eml", "directory of .eml files to replay")
	url := flag.String("url", "http://localhost:8080/webhooks/postmark", "webhook URL")
	secret := flag.String("secret", os.Getenv("POSTMARK_WEBHOOK_SECRET"), "X-Webhook-Secret header; defaults to $POSTMARK_WEBHOOK_SECRET")
	flag.Parse()

	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatalf("read dir: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".eml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	client := &http.Client{Timeout: 30 * time.Second}
	for _, name := range names {
		payload, err := postmarkeml.FromFile(filepath.Join(*dir, name))
		if err != nil {
			log.Printf("%s: parse failed: %v", name, err)
			continue
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			log.Printf("%s: marshal failed: %v", name, err)
			continue
		}
		req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(raw))
		if err != nil {
			log.Printf("%s: new request failed: %v", name, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if *secret != "" {
			req.Header.Set("X-Webhook-Secret", *secret)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("%s: post failed: %v", name, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("[%s] %s -> %d: %s\n", time.Now().Format("15:04:05"), name, resp.StatusCode, string(body))
	}
}
