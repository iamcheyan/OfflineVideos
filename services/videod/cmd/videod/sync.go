package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func periodic(interval time.Duration, addr string) {
	client := &http.Client{Timeout: 6 * time.Minute}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if err := syncOnce(client, addr, []string{"youtube", "bilibili"}); err != nil {
		log.Printf("scheduled sync: %v", err)
	}
	for range ticker.C {
		if e := syncOnce(client, addr, []string{"youtube", "bilibili"}); e != nil {
			log.Printf("scheduled sync: %v", e)
		}
	}
}
func syncOnce(client *http.Client, addr string, sources []string) error {
	for _, s := range sources {
		body, _ := json.Marshal(map[string]string{"source": s, "what": "watch_later"})
		req, e := http.NewRequest(http.MethodPost, "http://"+addr+"/api/sync", strings.NewReader(string(body)))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return fmt.Errorf("%s sync: %w", s, e)
		}
		b, re := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if re != nil {
			return re
		}
		fmt.Printf("%s: HTTP %d %s\n", s, resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("%s sync returned HTTP %d", s, resp.StatusCode)
		}
	}
	return nil
}
func syncCommand(args []string) {
	flags := flag.NewFlagSet("sync", flag.ExitOnError)
	source := flags.String("source", "all", "youtube, bilibili, or all")
	interval := flags.Duration("interval", 0, "repeat interval; zero runs once")
	_ = flags.Parse(args)
	if *source != "all" && *source != "youtube" && *source != "bilibili" {
		log.Fatal("source must be youtube, bilibili, or all")
	}
	addr := env("OFFLINEVIDEOS_LISTEN", "127.0.0.1:8732")
	sources := []string{*source}
	if *source == "all" {
		sources = []string{"youtube", "bilibili"}
	}
	client := &http.Client{Timeout: 6 * time.Minute}
	if *interval <= 0 {
		if e := syncOnce(client, addr, sources); e != nil {
			log.Fatal(e)
		}
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if e := syncOnce(client, addr, sources); e != nil {
			log.Printf("sync: %v", e)
		}
		<-ticker.C
	}
}
