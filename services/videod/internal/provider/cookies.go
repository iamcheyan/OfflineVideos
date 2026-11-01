package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// NetscapeJar holds the cookies of a Netscape-format jar, keyed by domain and
// name. It is a deliberately small reader: providers only need to look up a
// handful of credentials that yt-dlp already wrote to disk.
type NetscapeJar struct {
	byDomain map[string]map[string]string
}

// ReadNetscapeCookies parses a Netscape cookie file.
//
// Blank lines and lines starting with '#' are skipped, with one exception:
// the "#HttpOnly_" prefix marks a cookie that browsers refuse to expose to
// JavaScript, and yt-dlp keeps that prefix in the jar. Such a line is a real
// cookie, so the prefix is stripped rather than treated as a comment.
func ReadNetscapeCookies(path string) (NetscapeJar, error) {
	jar := NetscapeJar{byDomain: make(map[string]map[string]string)}
	if path == "" {
		return jar, fmt.Errorf("cookie path is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return jar, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}
		// domain, includeSubdomains, path, secure, expires, name, value
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		domain, name, value := fields[0], fields[5], fields[6]
		if jar.byDomain[domain] == nil {
			jar.byDomain[domain] = make(map[string]string)
		}
		jar.byDomain[domain][name] = value
	}
	if err := scanner.Err(); err != nil {
		return jar, err
	}
	return jar, nil
}

// Value returns a cookie for domain, falling back to the most specific parent
// domain recorded in the jar.
//
// The fallback must prefer specificity rather than map order: a browser can
// hold the same cookie name for both ".bilibili.cn" and ".bilibili.com" with
// different, separately expired values, and picking the wrong one produces a
// confusing "not signed in" from a jar that is actually valid.
func (j NetscapeJar) Value(domain, name string) string {
	if m, ok := j.byDomain[domain]; ok {
		if v, ok := m[name]; ok {
			return v
		}
	}
	bestDomain := ""
	bestValue := ""
	for d, m := range j.byDomain {
		trimmed := strings.TrimPrefix(d, ".")
		if !strings.HasSuffix(domain, trimmed) {
			continue
		}
		v, ok := m[name]
		if !ok {
			continue
		}
		if len(trimmed) > len(bestDomain) {
			bestDomain = trimmed
			bestValue = v
		}
	}
	return bestValue
}

// decodeJSONBody decodes one JSON document from r.
func decodeJSONBody(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	return dec.Decode(v)
}
