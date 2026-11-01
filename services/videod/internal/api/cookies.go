package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cookieJarRequest carries a Cookie header value copied from a browser.
//
// Users get this from DevTools or from a cookie-export extension; pasting
// "SESSDATA=...; bili_jct=...; DedeUserID=..." is far less error-prone than
// asking them to produce Netscape tab-separated format by hand.

// decodeBody decodes one JSON request body with a size cap.
func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
type cookieJarRequest struct {
	Source string `json:"source"`
	Cookie string `json:"cookie"`
}

// jarPathFor maps a source name to the cookie file its provider reads.
func jarPathFor(dataDir, source string) (string, error) {
	switch source {
	case "youtube":
		return filepath.Join(dataDir, "cookies.txt"), nil
	case "bilibili":
		return filepath.Join(dataDir, "bilibili-cookies.txt"), nil
	default:
		return "", fmt.Errorf("unknown source %q", source)
	}
}

// domainFor maps a source name to the cookie domain written into the jar.
func domainFor(source string) string {
	if source == "bilibili" {
		return ".bilibili.com"
	}
	return ".youtube.com"
}

// WriteCookieJar converts a Cookie header value into a Netscape cookie file.
//
// The provider hands this path to yt-dlp via --cookies, so the file must use
// yt-dlp's expected seven tab-separated columns. Every cookie is written with
// the domain's leading dot (so subdomains match) and a one-year expiry.
func WriteCookieJar(path, domain, cookieHeader string) (int, error) {
	pairs := parseCookieHeader(cookieHeader)
	if len(pairs) == 0 {
		return 0, fmt.Errorf("no cookies found in value")
	}
	expiry := time.Now().AddDate(1, 0, 0).Unix()

	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	b.WriteString("# Written by videod; regenerated on each import.\n\n")
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// domain, includeSubdomains, path, secure, expiry, name, value
		fmt.Fprintf(&b, "%s\tTRUE\t/\tTRUE\t%d\t%s\t%s\n",
			domain, expiry, name, pairs[name])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return 0, err
	}
	return len(pairs), nil
}

// parseCookieHeader splits "a=1; b=2" into a map. Values keep their original
// characters: cookie values are opaque and may contain '=' after the first one.
func parseCookieHeader(header string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = strings.TrimSpace(value)
	}
	return out
}

// importCookies writes the jar a provider reads and reports what it found.
func (h *Handler) importCookies(w http.ResponseWriter, r *http.Request) {
	var in cookieJarRequest
	if err := decodeBody(r, &in); err != nil {
		fail(w, 400, "invalid request")
		return
	}
	if in.Source == "" || in.Cookie == "" {
		fail(w, 400, "source and cookie are required")
		return
	}
	path, err := jarPathFor(h.DataDir, in.Source)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	n, err := WriteCookieJar(path, domainFor(in.Source), in.Cookie)
	if err != nil {
		fail(w, 500, "unable to write cookie jar")
		return
	}
	result := map[string]any{"status": "ok", "source": in.Source, "cookies": n, "path": path}
	// Bilibili's remove API needs the CSRF token, so say up front when it is
	// absent instead of failing later at "mark watched".
	if in.Source == "bilibili" {
		pairs := parseCookieHeader(in.Cookie)
		missing := []string{}
		for _, name := range []string{"SESSDATA", "DedeUserID", "bili_jct"} {
			if pairs[name] == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			result["missing"] = missing
		}
	}
	jsonOut(w, result)
}
