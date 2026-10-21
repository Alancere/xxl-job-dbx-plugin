package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Restyled consoles publish no API contract, but their own front end does know
// it. reconSpa reads the console's index page, follows its redirects, downloads
// the bundles it loads, and extracts the API paths and auth hints they mention.
// That turns "which endpoint should the plugin call?" into something the user
// can answer by clicking one button.

const (
	reconBodyLimit   = 512 << 10
	reconMaxScripts  = 3
	reconMaxPaths    = 24
	reconMaxHints    = 8
	reconScriptLimit = 512 << 10
)

var (
	reconTitlePattern  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reconScriptPattern = regexp.MustCompile(`(?is)<script[^>]+src\s*=\s*["']([^"']+)["']`)
	reconPathPattern   = regexp.MustCompile(`["'\x60](/(?:api|auth|sso|login|logout|token|user|sys|oauth|jobinfo|joblog|jobgroup|jobcode|chartInfo)[A-Za-z0-9_\-/.{}:?=&]*)["'\x60]`)
	reconHintPattern   = regexp.MustCompile(`(?i)(xxl-job-access-token|xxl-sso-token|access[_-]?token|authorization|localStorage\.(?:get|set)Item\(["'][^"']{1,32}["']\)|bearer)`)
)

type spaRecon struct {
	Hops     []string
	FinalURL string
	Status   int
	Title    string
	Scripts  []string
	APIPaths []string
	Hints    []string
	Err      error
}

func reconSpa(ctx context.Context, pageURL string, insecureTLS bool) spaRecon {
	recon := spaRecon{}
	var hops []string
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS}}, //nolint:gosec // diagnostics only
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			hops = append(hops, fmt.Sprintf("%d → %s", via[len(via)-1].Response.StatusCode, request.URL.String()))
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, pageURL+"/", nil)
	if err != nil {
		recon.Err = err
		return recon
	}
	request.Header.Set("User-Agent", browserUserAgent())
	request.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	response, err := client.Do(request)
	if err != nil {
		recon.Err = err
		return recon
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, reconBodyLimit))
	recon.Hops = hops
	recon.Status = response.StatusCode
	recon.FinalURL = response.Request.URL.String()
	if match := reconTitlePattern.FindSubmatch(body); len(match) == 2 {
		recon.Title = strings.TrimSpace(string(match[1]))
	}

	base, err := url.Parse(recon.FinalURL)
	if err != nil {
		return recon
	}
	corpus := string(body)
	for _, match := range reconScriptPattern.FindAllStringSubmatch(string(body), reconMaxScripts+2) {
		reference, err := url.Parse(strings.TrimSpace(match[1]))
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(reference)
		if resolved.Host != base.Host {
			continue
		}
		recon.Scripts = append(recon.Scripts, resolved.String())
		if len(recon.Scripts) > reconMaxScripts {
			continue
		}
		if script, ok := fetchText(ctx, client, resolved.String()); ok {
			corpus += "\n" + script
		}
	}
	recon.APIPaths = extractAPIPaths(corpus)
	recon.Hints = extractAuthHints(corpus)
	return recon
}

func fetchText(ctx context.Context, client *http.Client, target string) (string, bool) {
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		return "", false
	}
	request.Header.Set("User-Agent", browserUserAgent())
	response, err := client.Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, reconScriptLimit))
	if err != nil {
		return "", false
	}
	return string(body), true
}

func extractAPIPaths(corpus string) []string {
	seen := map[string]bool{}
	paths := make([]string, 0, reconMaxPaths)
	for _, match := range reconPathPattern.FindAllStringSubmatch(corpus, 400) {
		path := match[1]
		if strings.Contains(path, "${") || strings.HasPrefix(path, "/api/v1/v") {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
		if len(paths) >= reconMaxPaths {
			break
		}
	}
	sort.Strings(paths)
	return paths
}

func extractAuthHints(corpus string) []string {
	counts := map[string]int{}
	for _, match := range reconHintPattern.FindAllString(corpus, 2000) {
		counts[strings.TrimSpace(match)]++
	}
	type hint struct {
		text  string
		count int
	}
	hints := make([]hint, 0, len(counts))
	for text, count := range counts {
		hints = append(hints, hint{text: text, count: count})
	}
	sort.Slice(hints, func(left, right int) bool {
		if hints[left].count != hints[right].count {
			return hints[left].count > hints[right].count
		}
		return hints[left].text < hints[right].text
	})
	result := make([]string, 0, reconMaxHints)
	for _, entry := range hints {
		if len(result) >= reconMaxHints {
			break
		}
		result = append(result, fmt.Sprintf("%s ×%d", entry.text, entry.count))
	}
	return result
}

func formatRecon(recon spaRecon) string {
	lines := []string{"", "前端资源："}
	switch {
	case recon.Err != nil:
		lines = append(lines, "访问管理端首页失败："+shortError(recon.Err))
		return strings.Join(lines, "\n")
	case recon.Status == 0:
		return ""
	}
	line := fmt.Sprintf("GET / → %d", recon.Status)
	for _, hop := range recon.Hops {
		line += "，跳转 " + hop
	}
	if recon.FinalURL != "" {
		line += "，最终 " + recon.FinalURL
	}
	if recon.Title != "" {
		line += fmt.Sprintf("，标题「%s」", truncate(recon.Title, 30))
	}
	lines = append(lines, line)
	if len(recon.Scripts) > 0 {
		lines = append(lines, "脚本："+truncate(strings.Join(recon.Scripts, ", "), 160))
	}
	if len(recon.APIPaths) > 0 {
		lines = append(lines, "前端引用的接口路径：")
		for _, path := range recon.APIPaths {
			lines = append(lines, "  "+path)
		}
	} else {
		lines = append(lines, "前端引用的接口路径：未从 HTML/脚本中提取到（可能是压缩混淆或异步加载）")
	}
	if len(recon.Hints) > 0 {
		lines = append(lines, "认证线索："+strings.Join(recon.Hints, "，"))
	}
	return strings.Join(lines, "\n")
}
