package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// This file answers one question the connection dialog cannot: when the console
// rejects the classic xxl-job-admin endpoints, what does the server actually
// expose? The report is deliberately short and copyable so a user can paste it
// back into an issue instead of opening DevTools.

const probeTimeout = 4 * time.Second

type probeResult struct {
	Method      string
	URL         string
	Status      int
	ContentType string
	Body        []byte
	Err         error
}

func (result probeResult) ok() bool {
	return result.Err == nil && result.Status > 0 && result.Status < 400
}

// diagnoseConsole probes the classic console surface plus the JSON-API layouts
// newer or restyled consoles use, and reports what each one answered.
func diagnoseConsole(ctx context.Context, root string, insecureTLS bool) (any, *SessionError) {
	origin, ok := consoleOrigin(root)
	if !ok {
		return nil, newSessionError(errInvalidParams, "无法解析管理端地址："+root)
	}
	configured := contextPathLabel(root)
	if configured == "/" {
		configured = ""
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	page := probe(ctx, http.MethodGet, origin+configured+"/", nil, "", insecureTLS)
	results := []probeResult{page}

	// Classic console: page route plus the two endpoints login depends on.
	// The job page is probed with every primitive-bound filter the 2.4.1
	// controller declares; without jobGroup Spring answers a binding error
	// instead of the page envelope the report is looking for.
	for _, path := range []string{"/toLogin", "/login", jobPageProbeClassic} {
		method := http.MethodGet
		var body []byte
		contentType := ""
		if path == "/login" {
			method = http.MethodPost
			body = []byte("ifRemember=on")
			contentType = "application/x-www-form-urlencoded"
		}
		results = append(results, probe(ctx, method, origin+configured+path, body, contentType, insecureTLS))
		if ctx.Err() != nil {
			break
		}
	}

	// REST layouts: the same controller names under /api, plus the login routes
	// admin SPAs commonly use.
	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, path := range []string{"/login", "/auth/login", "/jobinfo/pageList?start=0&length=1", "/job/list?page=0&size=1"} {
			method := http.MethodGet
			var body []byte
			contentType := ""
			if strings.Contains(path, "login") {
				method = http.MethodPost
				body = []byte(`{"username":"","password":""}`)
				contentType = "application/json"
			}
			results = append(results, probe(ctx, method, origin+configured+prefix+path, body, contentType, insecureTLS))
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}

	classic := classicConsoleConfirmed(results)
	sso := ssoProtected(results)
	recon := reconSpa(ctx, origin+configured, insecureTLS)
	report := formatProbeReport(origin+configured, results, classic, sso) + formatRecon(recon)
	if classic {
		return map[string]any{
			"success": true,
			"message": report,
		}, nil
	}
	return map[string]any{
		"success": false,
		"message": report,
	}, nil
}

// ssoProtected reports whether the console's own endpoints answered with an
// XXL-SSO rejection, which means they exist but need an SSO session or token
// instead of the classic form login.
func ssoProtected(results []probeResult) bool {
	for _, result := range results {
		if result.Err != nil || strings.Contains(result.URL, "/api/") {
			continue
		}
		if _, message, ok := returnEnvelope(result.Body); ok {
			lower := strings.ToLower(message)
			if strings.Contains(lower, "sso") || strings.Contains(lower, "access token") {
				return true
			}
		}
	}
	return false
}

// classicConsoleConfirmed reports whether the classic login mapping answered
// with the console's JSON envelope, which is what the plugin actually needs.
func classicConsoleConfirmed(results []probeResult) bool {
	for _, result := range results {
		if result.Method != http.MethodPost || !strings.HasSuffix(result.URL, "/login") || result.Err != nil {
			continue
		}
		if strings.Contains(result.URL, "/api/") {
			continue
		}
		if result.Status == http.StatusOK && hasJSONCode(result.Body) {
			return true
		}
	}
	return false
}

func hasJSONCode(body []byte) bool {
	_, _, ok := returnEnvelope(body)
	return ok
}

func probe(ctx context.Context, method, target string, body []byte, contentType string, insecureTLS bool) probeResult {
	result := probeResult{Method: method, URL: target}
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var reader *strings.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	} else {
		reader = strings.NewReader("")
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target, reader)
	if err != nil {
		result.Err = err
		return result
	}
	request.Header.Set("Accept", "application/json, text/html, */*")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("Referer", target)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS}}, //nolint:gosec // diagnostics only
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		result.Err = err
		return result
	}
	defer response.Body.Close()
	payload := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	for len(payload) < 8192 {
		read, readErr := response.Body.Read(buffer)
		payload = append(payload, buffer[:read]...)
		if readErr != nil {
			break
		}
	}
	result.Status = response.StatusCode
	result.ContentType = response.Header.Get("Content-Type")
	result.Body = payload
	return result
}

func formatProbeReport(root string, results []probeResult, classic bool, sso bool) string {
	sort.SliceStable(results, func(left, right int) bool {
		if results[left].Method != results[right].Method {
			return results[left].Method < results[right].Method
		}
		return results[left].URL < results[right].URL
	})
	lines := make([]string, 0, len(results)+4)
	switch {
	case classic:
		lines = append(lines, "结论：这是经典 xxl-job-admin 接口，插件可直接使用。", "")
	case sso:
		lines = append(lines,
			"结论：经典 xxl-job-admin 的业务接口存在（未登录时会返回 XXL-SSO 异常），但 POST "+root+"/login 不存在。",
			"该部署把表单登录换成了 XXL-SSO：请在连接配置的 access_token 里填入管理端 access token（xxl.job.accessToken 或 SSO token），插件会用它换取会话；填好后重新“测试”即可。",
			"",
		)
	default:
		lines = append(lines,
			"结论：没有找到经典 xxl-job-admin 的登录接口（POST "+root+"/login 未返回管理端的 JSON 信封）。",
			"如果你的管理端是带 #/ 路由的新版或定制前端，它的接口路径与经典版不同，请把下面这份探测结果反馈给插件作者，以便适配。",
			"",
		)
	}
	lines = append(lines, "探测结果：")
	for _, result := range results {
		display := strings.TrimPrefix(result.URL, root)
		if display == "" {
			display = "/"
		}
		if result.Err != nil {
			lines = append(lines, fmt.Sprintf("%-4s %-46s 请求失败：%s", result.Method, display, shortError(result.Err)))
			continue
		}
		contentType := strings.TrimSpace(strings.Split(result.ContentType, ";")[0])
		lines = append(lines,
			fmt.Sprintf("%-4s %-46s %d  %-16s %s", result.Method, display, result.Status, contentType, bodyHint(result)))
	}
	return strings.Join(lines, "\n")
}

func bodyHint(result probeResult) string {
	trimmed := strings.TrimSpace(string(result.Body))
	if trimmed == "" {
		return "（空响应）"
	}
	if code, message, ok := returnEnvelope([]byte(trimmed)); ok {
		if message != "" {
			return fmt.Sprintf("JSON code=%d msg=%s", code, truncate(message, 40))
		}
		return fmt.Sprintf("JSON code=%d", code)
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var probeShape map[string]any
		if json.Unmarshal([]byte(trimmed), &probeShape) == nil {
			keys := make([]string, 0, len(probeShape))
			for key := range probeShape {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return "JSON 字段：" + truncate(strings.Join(keys, ","), 60)
		}
		return "JSON " + truncate(trimmed, 60)
	}
	if location := extractTitle(trimmed); location != "" {
		return "HTML 标题「" + truncate(location, 30) + "」" + spaHint(trimmed)
	}
	return truncate(strings.Join(strings.Fields(trimmed), " "), 60)
}

func spaHint(body string) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, `id="app"`) || strings.Contains(lower, "type=\"module\"") || strings.Contains(lower, "vue") {
		return "，疑似 SPA"
	}
	return ""
}

func extractTitle(body string) string {
	lower := strings.ToLower(body)
	start := strings.Index(lower, "<title>")
	if start < 0 {
		return ""
	}
	end := strings.Index(lower[start:], "</title>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start+len("<title>") : start+end])
}

func shortError(err error) string {
	message := err.Error()
	if urlError, ok := err.(*url.Error); ok {
		message = urlError.Err.Error()
	}
	return truncate(message, 70)
}

func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
