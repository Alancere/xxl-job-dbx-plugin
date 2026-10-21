package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JSON-RPC error codes the workbench UI branches on. They stay stable so the
// sandbox UI can tell "reconnect the connection" apart from "the admin said no".
const (
	errInvalidParams = -32602
	errNoSession     = -32010
	errUpstream      = -32020
	errAuth          = -32021
	errNetwork       = -32022
	errNotFound      = -32023
	errUnsupported   = -32024
)

const (
	defaultAPITimeout = 60 * time.Second
	maxResponseBytes  = 16 << 20
	loginCookieName   = "XXL_JOB_LOGIN_IDENTITY"
	// v3LoginCookieName is what 3.x consoles hand back from /auth/doLogin; the
	// classic console still uses XXL_JOB_LOGIN_IDENTITY.
	v3LoginCookieName = "xxl_job_login_token"
	accessTokenHeader = "XXL-JOB-ACCESS-TOKEN"
	// jobPageProbeClassic is the cheapest authenticated read for a 2.x console.
	// Every filter the console binds as a primitive Java type is sent
	// explicitly: Spring answers HTTP 200 with `code:500
	// java.lang.IllegalStateException` when one is missing (2.4.1 does exactly
	// that for jobGroup), which would otherwise be mistaken for a rejected
	// login.
	jobPageProbeClassic = "/jobinfo/pageList?start=0&length=1&jobGroup=-1" +
		"&triggerStatus=-1&jobDesc=&executorHandler=&author="
	defaultPort = 8080
)

// consoleVersion distinguishes the two admin API generations the plugin talks
// to: the classic 2.x console and the 3.x rewrite.
const (
	consoleV2 = 2
	consoleV3 = 3
)

// SessionError is an error with a JSON-RPC code attached, so transport details
// never have to leak into the UI to explain what happened.
type SessionError struct {
	Code    int
	Message string
	Detail  string
}

func (err *SessionError) Error() string { return err.Message }

func newSessionError(code int, message string) *SessionError {
	return &SessionError{Code: code, Message: message}
}

func upstreamError(message string, detail []byte) *SessionError {
	return &SessionError{Code: errUpstream, Message: message, Detail: snippet(detail)}
}

func networkError(err error) *SessionError {
	return &SessionError{Code: errNetwork, Message: "无法连接 XXL-JOB 管理端：" + cleanMessage(err), Detail: ""}
}

// Session is one logged-in XXL-JOB admin console. XXL-JOB has no token API:
// the console authenticates with a form login and keeps a cookie, so every
// session owns its own cookie jar and re-logs in once when the admin expires it.
type Session struct {
	baseURL  string
	username string
	password string
	timeout  time.Duration
	client   *http.Client
	jar      http.CookieJar

	// insecureTLS is kept so console-root probes inherit the same TLS policy as
	// the connection that triggered them.
	insecureTLS bool
	// accessToken is the shared secret consoles behind XXL-SSO or the OpenAPI
	// guard require; it rides every request and is never logged.
	accessToken string
	// sessionCookie and authHeader replay credentials copied from a working
	// browser session. They make consoles with an unpublished or SSO-only auth
	// flow usable without the plugin having to know that flow at all.
	sessionCookie   string
	authHeaderName  string
	authHeaderValue string
	// requestedBaseURL is the console root the connection configuration asked
	// for; it differs from baseURL when connect recovered from a wrong context
	// path, so the workbench can tell the user what to fix.
	requestedBaseURL string
	autoDetected     bool

	// version selects the admin API generation (2.x vs 3.x); it is fixed by the
	// login route that succeeded and never changes for the life of the session.
	version int
	// identityCookie is the cookie that holds the login state; it is cleared on
	// re-login and has a different name per console generation.
	identityCookie string

	mu     sync.Mutex
	logged bool
}

func newSession(baseURL, username, password string, timeout time.Duration, insecureTLS bool) (*Session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultAPITimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecureTLS} //nolint:gosec // opt-in for self-signed admin consoles
	return &Session{
		baseURL:          baseURL,
		requestedBaseURL: baseURL,
		username:         username,
		password:         password,
		timeout:          timeout,
		jar:              jar,
		insecureTLS:      insecureTLS,
		version:          consoleV2,
		identityCookie:   loginCookieName,
		client: &http.Client{
			Jar:       jar,
			Timeout:   defaultAPITimeout,
			Transport: transport,
			// A redirect is how this console answers an unauthenticated request,
			// so redirects are inspected instead of silently followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// BaseURL is the resolved admin console root, e.g. http://host:8080/xxl-job-admin.
func (session *Session) BaseURL() string { return session.baseURL }

func (session *Session) do(ctx context.Context, method, path string, form url.Values) (int, []byte, error) {
	if form == nil {
		return session.doWithBody(ctx, method, path, nil, "")
	}
	return session.doWithBody(ctx, method, path, []byte(form.Encode()), "application/x-www-form-urlencoded; charset=UTF-8")
}

// doJSON posts a JSON body, which is what token-based consoles expect.
func (session *Session) doJSON(ctx context.Context, method, path, payload string) (int, []byte, error) {
	return session.doWithBody(ctx, method, path, []byte(payload), "application/json; charset=UTF-8")
}

func (session *Session) doWithBody(ctx context.Context, method, path string, body []byte, contentType string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, session.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	// The console answers JSON instead of redirecting to the login page only
	// when it believes the caller is an Ajax request.
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("User-Agent", browserUserAgent())
	// Some gateways in front of an admin console reject requests without an
	// Origin/Referer pair; both are cheap to provide and change no semantics.
	request.Header.Set("Referer", session.baseURL+"/")
	if method == http.MethodPost {
		if origin, ok := consoleOrigin(session.baseURL); ok {
			request.Header.Set("Origin", origin)
		}
	}
	// Deployments that protect the admin with an access token expect it on every
	// request; the classic console ignores the header.
	if session.accessToken != "" {
		request.Header.Set(accessTokenHeader, session.accessToken)
	}
	// A replayed browser session wins over everything else: it is exactly the
	// credential the user already proved works.
	if session.sessionCookie != "" {
		request.Header.Set("Cookie", session.sessionCookie)
	}
	if session.authHeaderName != "" && session.authHeaderValue != "" {
		request.Header.Set(session.authHeaderName, session.authHeaderValue)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := session.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if len(data) > maxResponseBytes {
		return response.StatusCode, nil, errors.New("管理端响应超过 16 MiB 限制")
	}
	return response.StatusCode, data, nil
}

// call performs an authenticated admin request and retries exactly once after
// an expired cookie. It never retries business failures.
func (session *Session) call(ctx context.Context, method, path string, form url.Values) ([]byte, *SessionError) {
	return session.callWith(ctx, method, path, form, false)
}

// callRaw is `call` for endpoints whose failure envelope carries no usable
// message and has to be interpreted by the caller (2.4.1 answers a log read
// through an unreachable executor with `{"code":500,"msg":null}`).
func (session *Session) callRaw(ctx context.Context, method, path string, form url.Values) ([]byte, *SessionError) {
	return session.callMode(ctx, method, path, form, false, false)
}

// callWith runs one authenticated request. `allowHTML` is set for console pages
// that legitimately answer with HTML (the GLUE editor); a login redirect still
// arrives as a 3xx, so the expiry path stays intact.
func (session *Session) callWith(ctx context.Context, method, path string, form url.Values, allowHTML bool) ([]byte, *SessionError) {
	return session.callMode(ctx, method, path, form, allowHTML, true)
}

// callMode is the shared request loop; `failOnEnvelope` decides whether a
// non-200 business envelope is turned into an error or handed to the caller.
func (session *Session) callMode(ctx context.Context, method, path string, form url.Values, allowHTML, failOnEnvelope bool) ([]byte, *SessionError) {
	expired := ""
	for attempt := 0; attempt < 2; attempt++ {
		if err := session.ensureLogin(ctx); err != nil {
			return nil, err
		}
		status, body, err := session.do(ctx, method, path, form)
		if err != nil {
			return nil, networkError(err)
		}
		trimmed := bytes.TrimSpace(body)
		switch {
		case status == http.StatusMovedPermanently, status == http.StatusFound, status == http.StatusTemporaryRedirect,
			status == http.StatusUnauthorized, status == http.StatusForbidden:
			expired = fmt.Sprintf("管理端返回 HTTP %d", status)
			session.invalidate()
			continue
		// A 3xx/401/403 is the console's "log in again" answer; any other
		// non-2xx (a 404 from an endpoint this version does not implement, for
		// example) is reported as-is and must not throw the session away.
		case status < 200 || status >= 300:
			return nil, &SessionError{
				Code:    errUpstream,
				Message: fmt.Sprintf("XXL-JOB 管理端返回 HTTP %d", status),
				Detail:  snippet(trimmed),
			}
		case isHTML(trimmed) && !allowHTML:
			expired = "管理端返回了登录页面"
			session.invalidate()
			continue
		}
		if code, message, ok := returnEnvelope(trimmed); ok && code != 200 {
			if isAuthMessage(message) {
				expired = message
				session.invalidate()
				continue
			}
			if !failOnEnvelope {
				return trimmed, nil
			}
			return nil, &SessionError{Code: errUpstream, Message: messageOrDefault(message, "管理端返回业务失败"), Detail: snippet(trimmed)}
		}
		return trimmed, nil
	}
	if expired == "" {
		expired = "登录已过期"
	}
	return nil, &SessionError{Code: errAuth, Message: "XXL-JOB 登录失败或已过期：" + expired}
}

func (session *Session) ensureLogin(ctx context.Context) *SessionError {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.logged {
		return nil
	}
	return session.loginLocked(ctx)
}

func (session *Session) loginLocked(ctx context.Context) *SessionError {
	// A configured browser session is used as-is: the user has already proven it
	// works, so the plugin only has to confirm it still does.
	if session.sessionCookie != "" || session.authHeaderValue != "" {
		if err := session.verifyLoggedIn(ctx); err == nil {
			return nil
		} else {
			return &SessionError{
				Code: errAuth,
				Message: "连接凭证（浏览器会话 / 自定义请求头）未被管理端接受：" + err.Message +
					"。请在浏览器里重新登录管理端，复制最新的 Cookie 或 Authorization 后更新连接配置",
				Detail: err.Detail,
			}
		}
	}
	// Consoles fronted by XXL-SSO reject the classic form and exchange an access
	// token instead, so a configured token is tried first.
	var tokenErr *SessionError
	if session.accessToken != "" {
		if err := session.tokenLoginLocked(ctx); err == nil {
			return nil
		} else {
			tokenErr = err
		}
	}
	if session.username == "" || session.password == "" {
		if tokenErr != nil {
			return tokenErr
		}
		return newSessionError(errAuth, "请在连接配置中填写 XXL-JOB 管理端的用户名和密码")
	}
	formErr := session.formLoginLocked(ctx)
	if formErr == nil {
		return nil
	}
	// A rejected token explains the situation better than "the form endpoint is
	// missing", which is all a token-protected console can answer here.
	if tokenErr != nil {
		return tokenErr
	}
	return formErr
}

// loginRoutes are the form-login routes of the two admin generations. The
// classic route is tried first because it is by far the most common, and the
// 3.x route is a fallback reached only when the classic mapping does not exist.
var loginRoutes = []struct {
	version int
	path    string
	cookie  string
}{
	{consoleV2, "/login", loginCookieName},
	{consoleV3, "/auth/doLogin", v3LoginCookieName},
}

func (session *Session) formLoginLocked(ctx context.Context) *SessionError {
	form := url.Values{}
	form.Set("userName", session.username)
	form.Set("password", session.password)
	form.Set("ifRemember", "on")

	var lastStatus int
	var lastBody []byte
	for _, route := range loginRoutes {
		status, body, err := session.do(ctx, http.MethodPost, route.path, form)
		if err != nil {
			return networkError(err)
		}
		trimmed := bytes.TrimSpace(body)
		// A JSON business answer (bad credentials, wrong role, …) is final for
		// every generation: trying the other login route would only mask it.
		if code, message, ok := returnEnvelope(trimmed); ok && code != 200 {
			return newSessionError(errAuth, "XXL-JOB 登录失败："+messageOrDefault(message, "账号或密码错误"))
		}
		switch status {
		case http.StatusOK, http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			// The console answers 200 with a body or a redirect to "/".
			session.version = route.version
			session.identityCookie = route.cookie
			return session.verifyLoggedIn(ctx)
		default:
			// A 404 on the classic route usually means this is a 3.x console;
			// remember the answer and try the next route before diagnosing.
			lastStatus, lastBody = status, trimmed
		}
	}
	return session.explainLoginStatus(ctx, lastStatus, lastBody)
}

// tokenLoginLocked exchanges a configured access token for a console session.
// XXL-SSO deployments expose `POST /api/login` and answer a bad token with
// `The access token is wrong.` instead of the classic login form.
func (session *Session) tokenLoginLocked(ctx context.Context) *SessionError {
	status, body, err := session.doJSON(ctx, http.MethodPost, "/api/login", fmt.Sprintf(`{"accessToken":%q}`, session.accessToken))
	if err != nil {
		return networkError(err)
	}
	trimmed := bytes.TrimSpace(body)
	if status != http.StatusOK || !hasJSONCode(trimmed) {
		return newSessionError(errAuth, "access token 登录接口无有效响应")
	}
	code, message, _ := returnEnvelope(trimmed)
	if code != 200 {
		return newSessionError(errAuth, "access token 登录失败："+messageOrDefault(message, "token 被拒绝"))
	}
	return session.verifyLoggedIn(ctx)
}

// verifyLoggedIn proves the session works by reading one page of jobs: the
// console answers a page envelope only for an authenticated caller.
func (session *Session) verifyLoggedIn(ctx context.Context) *SessionError {
	probeStatus, probeBody, err := session.do(ctx, http.MethodGet, session.jobPageProbePath(), nil)
	if err != nil {
		return networkError(err)
	}
	probe := bytes.TrimSpace(probeBody)
	if probeStatus == http.StatusFound || probeStatus == http.StatusMovedPermanently || probeStatus == http.StatusUnauthorized {
		return newSessionError(errAuth, "XXL-JOB 登录失败：账号或密码错误")
	}
	if probeStatus != http.StatusOK || isHTML(probe) {
		return &SessionError{Code: errAuth, Message: "XXL-JOB 登录失败：账号或密码错误，或管理端上下文路径不正确", Detail: snippet(probe)}
	}
	if code, message, ok := returnEnvelope(probe); ok {
		if code == 200 {
			session.logged = true
			return nil
		}
		if strings.Contains(strings.ToLower(message), "sso") {
			return newSessionError(errAuth, "XXL-JOB 登录失败：该管理端使用 XXL-SSO 认证（"+
				messageOrDefault(message, "未登录")+"），请在连接配置中填写 access token，或在管理端开启表单登录")
		}
		return newSessionError(errAuth, "XXL-JOB 登录失败："+messageOrDefault(message, "账号或密码错误"))
	}
	session.logged = true
	return nil
}

// invalidate drops the cached login state and the identity cookie so the next
// request performs a clean login instead of replaying an expired identity.
func (session *Session) invalidate() {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.logged = false
	if parsed, err := url.Parse(session.baseURL); err == nil {
		cookie := session.identityCookie
		if cookie == "" {
			cookie = loginCookieName
		}
		session.jar.SetCookies(parsed, []*http.Cookie{{Name: cookie, Value: "", Path: "/", MaxAge: -1}})
	}
}

// --- console root detection --------------------------------------------

// consoleRootCandidates lists the context paths worth probing on one origin.
// The configured value is tried first, then the layout xxl-job ships with and
// the names deployments commonly rename it to.
func consoleRootCandidates(configured string) []string {
	values := []string{configured, "/xxl-job-admin", "", "/xxl-job", "/job-admin", "/admin"}
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized := "/" + strings.Trim(strings.TrimSpace(value), "/")
		if normalized == "/" {
			normalized = ""
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, normalized)
	}
	return result
}

// consoleOrigin keeps only scheme://host[:port] so candidate context paths can
// be appended without rebuilding the connection configuration.
func consoleOrigin(baseURL string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
}

// probeConsoleRoot reports whether `base` answers like an xxl-job admin console.
//
// A POST to /login (2.x) or /auth/doLogin (3.x) that returns the console's
// `{code,...}` envelope is the strongest signal: it proves a login mapping
// exists even with an empty form (`账号或密码不能为空`), and it does not depend
// on page markup. `reachable` is false only when the host itself did not answer,
// which lets callers stop probing a dead origin instead of trying every path.
func probeConsoleRoot(ctx context.Context, base string, insecureTLS bool) (found bool, reachable bool) {
	probe, err := newSession(base, "", "", 5*time.Second, insecureTLS)
	if err != nil {
		return false, false
	}
	for _, route := range loginRoutes {
		status, body, doErr := probe.do(ctx, http.MethodPost, route.path, url.Values{})
		if doErr != nil {
			return false, false
		}
		// Only the console's JSON envelope proves a login mapping exists. An
		// SPA that renders index.html for every path must never be mistaken
		// for the console page.
		if status == http.StatusOK && hasJSONCode(bytes.TrimSpace(body)) {
			return true, true
		}
	}
	return false, true
}

// contextPathLabel renders the form value a user should type for a console root.
func contextPathLabel(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || strings.Trim(parsed.Path, "/") == "" {
		return "/"
	}
	return "/" + strings.Trim(parsed.Path, "/")
}

// diagnoseConsoleRoot probes the other console roots on the same origin and
// returns the first one that answers, so a wrong context path becomes a
// one-click fix instead of a dead end.
func (session *Session) diagnoseConsoleRoot(parent context.Context) string {
	origin, ok := consoleOrigin(session.baseURL)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for _, candidate := range consoleRootCandidates("") {
		base := origin + candidate
		if base == session.baseURL {
			continue
		}
		found, reachable := probeConsoleRoot(ctx, base, session.insecureTLS)
		if found {
			return base
		}
		if !reachable && ctx.Err() != nil {
			return ""
		}
	}
	return ""
}

// explainLoginStatus turns an unexpected login status into instructions the
// user can act on: the exact URL that answered, and where the console actually
// lives when it can be found on the same host.
func (session *Session) explainLoginStatus(ctx context.Context, status int, body []byte) *SessionError {
	message := fmt.Sprintf("XXL-JOB 登录失败：POST %s/login、/auth/doLogin 返回 HTTP %d", session.baseURL, status)
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusInternalServerError:
		if found := session.diagnoseConsoleRoot(ctx); found != "" {
			message += fmt.Sprintf(
				"。已探测到管理端实际位于 %s，请把“管理端上下文路径”改为 %s（端口 %s）后重试",
				found, contextPathLabel(found), portLabel(found),
			)
		} else {
			message += "。请确认管理端的上下文路径与端口（xxl-job-admin 默认端口 8080），可点击“自动探测管理端路径”后重试"
		}
	}
	return &SessionError{Code: errAuth, Message: message, Detail: snippet(body)}
}

func portLabel(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "?"
	}
	if port := parsed.Port(); port != "" {
		return port
	}
	if parsed.Scheme == "https" {
		return "443"
	}
	return "80"
}

// --- response envelopes -------------------------------------------------

// ReturnT is the console's generic response envelope. The 2.x console carries
// the payload in `content`; the 3.x console renamed it to `data`. Both fields
// are parsed so a single envelope handles either generation.
type ReturnT struct {
	Code    int             `json:"code"`
	Msg     json.RawMessage `json:"msg"`
	Content json.RawMessage `json:"content"`
	Data    json.RawMessage `json:"data"`
}

func (ret ReturnT) message() string {
	if len(ret.Msg) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(ret.Msg, &text); err == nil {
		return text
	}
	return string(ret.Msg)
}

// payload returns the envelope body under whichever key this console generation
// uses (`content` for 2.x, `data` for 3.x).
func (ret ReturnT) payload() json.RawMessage {
	if len(ret.Data) > 0 && string(ret.Data) != "null" {
		return ret.Data
	}
	return ret.Content
}

// Page is a normalized page envelope: 2.x answers a bare PageHelper object
// (`recordsTotal/recordsFiltered/data`), 3.x wraps that object under `data`
// and renames the fields (`total/offset/pagesize/data`). Callers only ever see
// RecordsTotal/RecordsFiltered/Data.
type Page struct {
	RecordsTotal    int               `json:"recordsTotal"`
	RecordsFiltered int               `json:"recordsFiltered"`
	Data            []json.RawMessage `json:"data"`
}

// pageQuery returns the paging parameters the console generation expects:
// 2.x uses start/length, 3.x uses offset/pagesize.
func (session *Session) pageQuery(start, length int) url.Values {
	query := url.Values{}
	if session.version == consoleV3 {
		query.Set("offset", strconv.Itoa(start))
		query.Set("pagesize", strconv.Itoa(length))
	} else {
		query.Set("start", strconv.Itoa(start))
		query.Set("length", strconv.Itoa(length))
	}
	return query
}

// jobPageProbePath is the cheapest authenticated read for the session's
// console generation.
func (session *Session) jobPageProbePath() string {
	if session.version != consoleV3 {
		return jobPageProbeClassic
	}
	query := session.pageQuery(0, 1)
	query.Set("jobGroup", "-1")
	query.Set("triggerStatus", "-1")
	query.Set("jobDesc", "")
	query.Set("executorHandler", "")
	query.Set("author", "")
	return "/jobinfo/pageList?" + query.Encode()
}

func (session *Session) getPage(ctx context.Context, path string, query url.Values) (*Page, *SessionError) {
	raw, sessionErr := session.call(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
	if sessionErr != nil {
		return nil, sessionErr
	}
	if session.version == consoleV3 {
		var envelope struct {
			Code int `json:"code"`
			Data struct {
				Data     []json.RawMessage `json:"data"`
				Total    int               `json:"total"`
				Offset   int               `json:"offset"`
				Pagesize int               `json:"pagesize"`
			} `json:"data"`
			Msg json.RawMessage `json:"msg"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, upstreamError("无法解析 XXL-JOB 分页响应（可能上下文路径或版本不匹配）", raw)
		}
		if envelope.Code != 200 {
			return nil, &SessionError{Code: errUpstream, Message: messageOrDefault(envelopeMessage(envelope.Msg), "管理端拒绝了该操作"), Detail: snippet(raw)}
		}
		data := envelope.Data.Data
		if data == nil {
			data = []json.RawMessage{}
		}
		return &Page{RecordsTotal: envelope.Data.Total, RecordsFiltered: envelope.Data.Total, Data: data}, nil
	}
	var page Page
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, upstreamError("无法解析 XXL-JOB 分页响应（可能上下文路径或版本不匹配）", raw)
	}
	if page.Data == nil {
		page.Data = []json.RawMessage{}
	}
	return &page, nil
}

func (session *Session) postReturn(ctx context.Context, path string, form url.Values) (*ReturnT, *SessionError) {
	raw, sessionErr := session.call(ctx, http.MethodPost, path, form)
	if sessionErr != nil {
		return nil, sessionErr
	}
	var ret ReturnT
	if err := json.Unmarshal(raw, &ret); err != nil {
		return nil, upstreamError("无法解析 XXL-JOB 响应（可能上下文路径或版本不匹配）", raw)
	}
	if ret.Code != 200 {
		return nil, &SessionError{Code: errUpstream, Message: messageOrDefault(ret.message(), "管理端拒绝了该操作"), Detail: snippet(raw)}
	}
	return &ret, nil
}

// --- helpers ------------------------------------------------------------

// envelopeMessage decodes a raw `msg` field into a string.
func envelopeMessage(msg json.RawMessage) string {
	if len(msg) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(msg, &text); err == nil {
		return text
	}
	return string(msg)
}

func returnEnvelope(body []byte) (int, string, bool) {
	if len(body) == 0 || body[0] != '{' {
		return 0, "", false
	}
	var probe struct {
		Code *int            `json:"code"`
		Msg  json.RawMessage `json:"msg"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Code == nil {
		return 0, "", false
	}
	var message string
	if len(probe.Msg) > 0 {
		_ = json.Unmarshal(probe.Msg, &message)
	}
	return *probe.Code, message, true
}

func browserUserAgent() string {
	return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
}

func isHTML(body []byte) bool {
	return len(body) > 0 && (body[0] == '<' || bytes.HasPrefix(bytes.ToLower(body), []byte("<!doctype")))
}

func isAuthMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(message, "登录") || strings.Contains(message, "过期") ||
		strings.Contains(lower, "login") || strings.Contains(lower, "expire") || strings.Contains(lower, "unauthor") ||
		strings.Contains(lower, "xxlsso") || strings.Contains(lower, "sso.core") || strings.Contains(lower, "access token")
}

func messageOrDefault(message, fallback string) string {
	message = strings.TrimSpace(message)
	if message == "" || message == "null" {
		return fallback
	}
	return message
}

// snippet keeps a short, single-line excerpt of an unexpected response for the
// error message instead of dumping a whole HTML page into the UI.
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

func cleanMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "网络错误"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "连接超时（" + message + "）"
	}
	return message
}

// buildBaseURL accepts either a bare host or a fully pasted console URL and
// returns a normalized `scheme://host[:port][/context-path]` root.
func buildBaseURL(scheme, host string, port int, contextPath string) (string, *SessionError) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", newSessionError(errInvalidParams, "请填写 XXL-JOB 管理端主机地址")
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil || parsed.Host == "" {
			return "", newSessionError(errInvalidParams, "主机地址无法解析："+host)
		}
		scheme = strings.ToLower(parsed.Scheme)
		if trimmed := strings.Trim(parsed.Path, "/"); trimmed != "" {
			contextPath = "/" + trimmed
		}
		if parsedPort := parsed.Port(); parsedPort != "" {
			if parsedPortValue, convErr := strconv.Atoi(parsedPort); convErr == nil {
				port = parsedPortValue
			}
		}
		host = parsed.Hostname()
	}
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return "", newSessionError(errInvalidParams, "协议只支持 http 或 https，当前为："+scheme)
	}
	if port <= 0 {
		port = defaultPort
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	root := scheme + "://" + host
	includePort := !(scheme == "http" && port == 80) && !(scheme == "https" && port == 443)
	if includePort {
		root += ":" + strconv.Itoa(port)
	}
	path := "/" + strings.Trim(strings.TrimSpace(contextPath), "/")
	if path == "/" {
		return root, nil
	}
	return root + path, nil
}

// --- payload value helpers ---------------------------------------------

// configValue reads one external_config entry, which may arrive as a JSON
// string, number, or boolean depending on the declared field type.
func configValue(config map[string]any, key string) (any, bool) {
	if config == nil {
		return nil, false
	}
	value, ok := config[key]
	if !ok || value == nil {
		return nil, false
	}
	return value, true
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	case bool:
		if typed {
			return 1
		}
	}
	return 0
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true
		}
	case float64:
		return typed != 0
	case int:
		return typed != 0
	}
	return false
}
