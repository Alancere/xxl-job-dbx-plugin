// Command dbx-xxljob is the native sidecar of the DBX XXL-JOB plugin.
//
// It owns every authenticated conversation with an XXL-JOB admin console. The
// sandboxed workbench UI never sees the admin URL, the account, or the cookie:
// it calls this process through the DBX host bridge and only ever holds a saved
// DBX connection id.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

const (
	pluginID    = "alancere.xxljob"
	pluginLabel = "XXL-JOB"
	providerID  = "alancere.xxljob.connection"
	workbenchID = "alancere.xxljob.workbench"

	defaultContextPath = "/xxl-job-admin"
	defaultTimeoutSecs = 15
	maxTimeoutSecs     = 120
	apiCallTimeout     = 90 * time.Second
)

type plugin struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// buildVersion is injected with -ldflags at package time; the fallback keeps a
// plain `go run` usable during development.
var buildVersion = "1.1.0"

func main() {
	version := buildVersion
	if override := strings.TrimSpace(os.Getenv("DBX_PLUGIN_VERSION")); override != "" {
		version = override
	}
	// The manifest version and the handshake version must match exactly or the
	// host refuses the session.
	server := dbxpluginsdk.NewServer(
		dbxpluginsdk.Metadata{
			ID:           pluginID,
			Version:      version,
			Capabilities: []string{"connections", "events"},
		},
		&plugin{sessions: map[string]*Session{}},
	)
	if err := server.Serve(); err != nil {
		log.Printf("[%s] sidecar stopped: %v", pluginID, err)
		os.Exit(1)
	}
}

func (p *plugin) Handle(
	_ dbxpluginsdk.RequestContext,
	method string,
	raw json.RawMessage,
	emitter *dbxpluginsdk.Emitter,
) (any, *dbxpluginsdk.PluginError) {
	params := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, toPluginError(newSessionError(errInvalidParams, "无法解析请求参数"))
		}
	}
	notify := func(name string, payload any) { _ = emitter.Event(name, payload) }
	result, sessionErr := p.dispatch(method, params, notify)
	if sessionErr != nil {
		return nil, toPluginError(sessionErr)
	}
	return result, nil
}

func toPluginError(sessionErr *SessionError) *dbxpluginsdk.PluginError {
	message := sessionErr.Message
	if detail := strings.TrimSpace(sessionErr.Detail); detail != "" {
		message = message + "（" + detail + "）"
	}
	return dbxpluginsdk.NewError(sessionErr.Code, message)
}

// dispatch routes one request. `notify` publishes plugin events; tests pass a
// no-op so the routing logic runs without a live host emitter.
func (p *plugin) dispatch(method string, params map[string]any, notify func(string, any)) (any, *SessionError) {
	switch method {
	case "connection/test":
		return p.testConnection(params)
	case "connection/connect":
		return p.connect(params, notify)
	case "connection/disconnect":
		return p.disconnect(params, notify)
	case "connection/action":
		return p.action(params)
	case "mcp/tools":
		return mcpTools(), nil
	case "mcp/call":
		return p.callTool(params)
	}

	session, sessionErr := p.session(paramString(params, "connectionId"))
	if sessionErr != nil {
		return nil, sessionErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()

	switch method {
	case "xxljob/overview":
		// The workbench reuses this call for its dashboard, so honour the day
		// window instead of resetting the report to seven days on every refresh.
		return session.Dashboard(ctx, paramInt(params, "days"))
	case "xxljob/dashboard":
		return session.Dashboard(ctx, paramInt(params, "days"))
	case "xxljob/groups":
		groups, err := session.Groups(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"groups": groups}, nil
	case "xxljob/jobs":
		// `status` is -1 (all), 0 (stopped), or 1 (running). An omitted filter
		// means "all", never "stopped", so callers can leave it out.
		status := -1
		if raw, ok := params["status"]; ok && raw != nil {
			if value := intValue(raw); value == 0 || value == 1 {
				status = value
			}
		}
		return session.Jobs(ctx, JobsRequest{
			GroupID:     paramInt(params, "groupId"),
			Status:      status,
			SearchField: paramString(params, "searchField"),
			Keyword:     paramString(params, "keyword"),
			Start:       paramInt(params, "start"),
			Length:      paramInt(params, "length"),
		})
	case "xxljob/job":
		return session.Job(ctx, paramInt(params, "id"))
	case "xxljob/job/save":
		return session.SaveJob(ctx, paramMap(params, "job"))
	case "xxljob/job/remove":
		return session.RemoveJob(ctx, paramInt(params, "id"))
	case "xxljob/job/start":
		return session.StartJob(ctx, paramInt(params, "id"))
	case "xxljob/job/stop":
		return session.StopJob(ctx, paramInt(params, "id"))
	case "xxljob/job/trigger":
		return session.TriggerJob(ctx, paramInt(params, "id"), paramString(params, "executorParam"), paramString(params, "addressList"))
	case "xxljob/job/nextTriggerTime":
		return session.NextTriggerTime(ctx, paramString(params, "scheduleType"), paramString(params, "scheduleConf"))
	case "xxljob/logs":
		return session.Logs(ctx,
			paramInt(params, "groupId"),
			paramInt(params, "jobId"),
			paramInt(params, "status"),
			paramInt(params, "start"),
			paramInt(params, "length"),
			paramString(params, "filterTime"),
		)
	case "xxljob/log":
		return session.LogDetail(ctx, paramInt(params, "logId"), paramInt(params, "fromLineNum"))
	case "xxljob/log/kill":
		return session.KillLog(ctx, paramInt(params, "logId"))
	case "xxljob/code":
		return session.GlueSource(ctx, paramInt(params, "id"))
	case "xxljob/code/save":
		return session.SaveGlueSource(ctx, paramInt(params, "id"), paramString(params, "glueType"), paramString(params, "source"), paramString(params, "glueRemark"))
	case "xxljob/relogin":
		if err := session.relogin(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "message": "已重新登录 XXL-JOB 管理端"}, nil
	}
	return nil, newSessionError(-32601, "插件不支持该方法："+method)
}

// --- connection lifecycle -----------------------------------------------

type lifecycleParams struct {
	Provider struct {
		ID           string `json:"id"`
		DatabaseType string `json:"databaseType"`
	} `json:"provider"`
	Connection struct {
		ID                string         `json:"id"`
		Name              string         `json:"name"`
		Host              string         `json:"host"`
		Port              int            `json:"port"`
		Username          string         `json:"username"`
		Password          string         `json:"password"`
		ExternalConfig    map[string]any `json:"external_config"`
		ConnectionSecrets map[string]any `json:"connection_secrets"`
	} `json:"connection"`
	Runtime struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"runtime"`
	Action struct {
		ID string `json:"id"`
	} `json:"action"`
}

func lifecycleFrom(params map[string]any) (lifecycleParams, *SessionError) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return lifecycleParams{}, newSessionError(errInvalidParams, "无法解析连接配置")
	}
	var lifecycle lifecycleParams
	if err := json.Unmarshal(encoded, &lifecycle); err != nil {
		return lifecycleParams{}, newSessionError(errInvalidParams, "无法解析连接配置")
	}
	return lifecycle, nil
}

type endpointConfig struct {
	BaseURL       string
	Username      string
	Password      string
	AccessToken   string
	SessionCookie string
	AuthHeader    string
	Timeout       time.Duration
	InsecureTLS   bool
}

// newEndpointSession builds a session that already carries the configured
// access token, which consoles behind XXL-SSO require on every request.
func newEndpointSession(endpoint endpointConfig) (*Session, error) {
	session, err := newSession(endpoint.BaseURL, endpoint.Username, endpoint.Password, endpoint.Timeout, endpoint.InsecureTLS)
	if err != nil {
		return nil, err
	}
	session.accessToken = endpoint.AccessToken
	session.sessionCookie = endpoint.SessionCookie
	session.authHeaderName, session.authHeaderValue = parseAuthHeader(endpoint.AuthHeader)
	return session, nil
}

// parseAuthHeader accepts either a full "Name: Value" pair or a bare value that
// is sent as a Bearer Authorization header.
func parseAuthHeader(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	name, value, found := strings.Cut(raw, ":")
	if !found {
		return "Authorization", "Bearer " + raw
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" || !validHeaderName(name) {
		return "Authorization", "Bearer " + raw
	}
	return name, value
}

func validHeaderName(name string) bool {
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '-', character == '_':
		default:
			return false
		}
	}
	return name != ""
}

// resolveEndpoint prefers the runtime endpoint DBX resolved (which includes
// SSH/proxy transport layers) and falls back to the saved connection fields.
func resolveEndpoint(lifecycle lifecycleParams) (endpointConfig, *SessionError) {
	host := strings.TrimSpace(lifecycle.Runtime.Host)
	if host == "" {
		host = strings.TrimSpace(lifecycle.Connection.Host)
	}
	port := lifecycle.Runtime.Port
	if port <= 0 {
		port = lifecycle.Connection.Port
	}
	config := lifecycle.Connection.ExternalConfig
	scheme := strings.ToLower(strings.TrimSpace(stringValue(nonEmpty(config, "scheme", "http"))))
	contextPath := stringValue(nonEmpty(config, "context_path", defaultContextPath))
	baseURL, sessionErr := buildBaseURL(scheme, host, port, contextPath)
	if sessionErr != nil {
		return endpointConfig{}, sessionErr
	}
	username := strings.TrimSpace(lifecycle.Connection.Username)
	if username == "" {
		username = stringValue(nonEmpty(config, "username", "admin"))
	}
	password := lifecycle.Connection.Password
	if password == "" && lifecycle.Connection.ConnectionSecrets != nil {
		password = stringValue(lifecycle.Connection.ConnectionSecrets["password"])
	}
	secrets := lifecycle.Connection.ConnectionSecrets
	sessionCookie := strings.TrimSpace(stringValue(secrets["session_cookie"]))
	authHeader := strings.TrimSpace(stringValue(secrets["auth_header"]))
	accessToken := strings.TrimSpace(stringValue(secrets["access_token"]))
	if accessToken == "" {
		accessToken = strings.TrimSpace(stringValue(nonEmpty(config, "access_token", "")))
	}
	timeoutSecs := intValue(nonEmpty(config, "connect_timeout_secs", defaultTimeoutSecs))
	if timeoutSecs <= 0 {
		timeoutSecs = defaultTimeoutSecs
	}
	if timeoutSecs > maxTimeoutSecs {
		timeoutSecs = maxTimeoutSecs
	}
	return endpointConfig{
		BaseURL:       baseURL,
		Username:      username,
		Password:      password,
		AccessToken:   accessToken,
		SessionCookie: sessionCookie,
		AuthHeader:    authHeader,
		Timeout:       time.Duration(timeoutSecs) * time.Second,
		InsecureTLS:   boolValue(nonEmpty(config, "skip_tls_verify", false)),
	}, nil
}

func nonEmpty(config map[string]any, key string, fallback any) any {
	if value, ok := configValue(config, key); ok {
		if text := strings.TrimSpace(stringValue(value)); text != "" {
			return value
		}
	}
	return fallback
}

func (p *plugin) testConnection(params map[string]any) (any, *SessionError) {
	lifecycle, sessionErr := lifecycleFrom(params)
	if sessionErr != nil {
		return nil, sessionErr
	}
	endpoint, sessionErr := resolveEndpoint(lifecycle)
	if sessionErr != nil {
		return map[string]any{"success": false, "message": sessionErr.Message}, nil
	}
	session, err := newEndpointSession(endpoint)
	if err != nil {
		return map[string]any{"success": false, "message": "初始化 HTTP 客户端失败：" + err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), endpoint.Timeout)
	defer cancel()
	if loginErr := session.ensureLogin(ctx); loginErr != nil {
		return map[string]any{"success": false, "message": loginErr.Message}, nil
	}
	groups, groupsErr := session.Groups(ctx)
	if groupsErr != nil {
		return map[string]any{"success": false, "message": groupsErr.Message}, nil
	}
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("已连接 %s（执行器分组 %d 个）", endpoint.BaseURL, len(groups)),
	}, nil
}

func (p *plugin) connect(params map[string]any, notify func(string, any)) (any, *SessionError) {
	lifecycle, sessionErr := lifecycleFrom(params)
	if sessionErr != nil {
		return nil, sessionErr
	}
	connectionID := strings.TrimSpace(lifecycle.Connection.ID)
	if connectionID == "" {
		return nil, newSessionError(errInvalidParams, "缺少连接 ID")
	}
	endpoint, sessionErr := resolveEndpoint(lifecycle)
	if sessionErr != nil {
		return nil, sessionErr
	}
	session, err := newEndpointSession(endpoint)
	if err != nil {
		return nil, newSessionError(errUpstream, "初始化 HTTP 客户端失败："+err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), endpoint.Timeout)
	defer cancel()
	if loginErr := session.ensureLogin(ctx); loginErr != nil {
		// A wrong context path is the most common first-run mistake. Instead of
		// refusing to open the connection, look for the console on the same
		// origin once: a recovered root only counts when the same credentials
		// log in, and the workbench then reports the address it really used.
		recovered, recoveryErr := recoverConsoleRoot(ctx, session, endpoint)
		if recovered == nil {
			if recoveryErr != nil {
				// The console was found elsewhere but rejected the credentials;
				// that is the error worth showing.
				return nil, recoveryErr
			}
			return nil, loginErr
		}
		session = recovered
	}
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = map[string]*Session{}
	}
	p.sessions[connectionID] = session
	p.mu.Unlock()
	notify("xxljob/connectionChanged", map[string]any{
		"connectionId": connectionID,
		"state":        "connected",
		"baseUrl":      session.baseURL,
	})
	message := fmt.Sprintf("已连接 %s", session.baseURL)
	if session.autoDetected {
		message = fmt.Sprintf("已连接 %s（自动探测所得，建议把连接配置的上下文路径改为 %s）", session.baseURL, contextPathLabel(session.baseURL))
	}
	return map[string]any{
		"success":      true,
		"message":      message,
		"baseUrl":      session.baseURL,
		"autoDetected": session.autoDetected,
	}, nil
}

// recoverConsoleRoot retries login against a console root discovered on the same
// origin. It returns nil when nothing answers or the credentials do not work
// there either, so a genuine authentication failure is never masked.
func recoverConsoleRoot(ctx context.Context, original *Session, endpoint endpointConfig) (*Session, *SessionError) {
	if ctx.Err() != nil {
		return nil, nil
	}
	found := original.diagnoseConsoleRoot(ctx)
	if found == "" || found == original.baseURL {
		return nil, nil
	}
	retryEndpoint := endpoint
	retryEndpoint.BaseURL = found
	retry, err := newEndpointSession(retryEndpoint)
	if err != nil {
		return nil, nil
	}
	if loginErr := retry.ensureLogin(ctx); loginErr != nil {
		return nil, &SessionError{
			Code: loginErr.Code,
			Message: fmt.Sprintf("%s（已按探测到的 %s 重试，请把“管理端上下文路径”改为 %s）",
				loginErr.Message, found, contextPathLabel(found)),
			Detail: loginErr.Detail,
		}
	}
	retry.requestedBaseURL = original.baseURL
	retry.autoDetected = true
	return retry, nil
}

func (p *plugin) disconnect(params map[string]any, notify func(string, any)) (any, *SessionError) {
	connectionID := paramString(params, "connectionId")
	if connectionID == "" {
		if connection, ok := params["connection"].(map[string]any); ok {
			connectionID = strings.TrimSpace(stringValue(connection["id"]))
		}
	}
	p.mu.Lock()
	delete(p.sessions, connectionID)
	p.mu.Unlock()
	notify("xxljob/connectionChanged", map[string]any{
		"connectionId": connectionID,
		"state":        "disconnected",
	})
	return map[string]any{"success": true}, nil
}

// action implements `connection/detect-endpoint`: it probes the usual XXL-JOB
// context paths so a misconfigured admin root is fixed from the dialog instead
// of by trial and error.
func (p *plugin) action(params map[string]any) (any, *SessionError) {
	lifecycle, sessionErr := lifecycleFrom(params)
	if sessionErr != nil {
		return nil, sessionErr
	}
	switch lifecycle.Action.ID {
	case "detect-endpoint":
		return detectEndpoint(lifecycle)
	case "inspect-endpoint":
		endpoint, endpointErr := resolveEndpoint(lifecycle)
		if endpointErr != nil {
			return map[string]any{"success": false, "message": endpointErr.Message}, nil
		}
		return diagnoseConsole(context.Background(), endpoint.BaseURL, endpoint.InsecureTLS)
	case "":
		return nil, newSessionError(errInvalidParams, "缺少连接动作 ID")
	default:
		return nil, newSessionError(errInvalidParams, "未知的连接动作："+lifecycle.Action.ID)
	}
}

func detectEndpoint(lifecycle lifecycleParams) (any, *SessionError) {
	host := strings.TrimSpace(lifecycle.Runtime.Host)
	if host == "" {
		host = strings.TrimSpace(lifecycle.Connection.Host)
	}
	port := lifecycle.Runtime.Port
	if port <= 0 {
		port = lifecycle.Connection.Port
	}
	if host == "" {
		return map[string]any{"success": false, "message": "请先填写主机地址"}, nil
	}
	config := lifecycle.Connection.ExternalConfig
	configuredScheme := strings.ToLower(strings.TrimSpace(stringValue(nonEmpty(config, "scheme", "http"))))
	configuredContext := stringValue(nonEmpty(config, "context_path", defaultContextPath))
	insecureTLS := boolValue(nonEmpty(config, "skip_tls_verify", false))

	// The action has a 30s deadline in the manifest; stop probing well before
	// that so a slow host still yields a useful answer instead of a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	schemes := uniqueStrings([]string{configuredScheme, "http", "https"})
	contextPaths := consoleRootCandidates(configuredContext)
	tried := make([]string, 0, 8)
	reachableHost := false
	for _, scheme := range schemes {
		if scheme != "http" && scheme != "https" {
			continue
		}
		for _, contextPath := range contextPaths {
			baseURL, err := buildBaseURL(scheme, host, port, contextPath)
			if err != nil {
				continue
			}
			tried = append(tried, baseURL)
			found, reachable := probeConsoleRoot(ctx, baseURL, insecureTLS)
			if found {
				context := contextPathLabel(baseURL)
				return map[string]any{
					"success": true,
					"message": fmt.Sprintf("已找到 XXL-JOB 管理端：%s（协议 %s，上下文路径 %s）", baseURL, scheme, context),
					"fieldValues": map[string]any{
						"scheme":       scheme,
						"context_path": context,
					},
					"baseUrl": baseURL,
				}, nil
			}
			if reachable {
				reachableHost = true
			} else {
				// This scheme cannot reach the host at all; skip its other paths
				// and let the next scheme try instead of waiting on each timeout.
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	message := fmt.Sprintf("在 %s:%d 上没有找到 XXL-JOB 管理端（已尝试 %d 个地址）", host, port, len(tried))
	if reachableHost {
		message += "。主机可以连通，但常见上下文路径都没有应答：请确认端口（xxl-job-admin 默认 8080）以及是否在反向代理后改过路径"
	} else {
		message += "。主机无法连通：请确认地址、端口与协议（http/https）"
	}
	return map[string]any{
		"success": false,
		"message": message,
		"tried":   tried,
	}, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (p *plugin) session(connectionID string) (*Session, *SessionError) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if connectionID != "" {
		if session, ok := p.sessions[connectionID]; ok {
			return session, nil
		}
	}
	return nil, newSessionError(errNoSession, "XXL-JOB 连接会话已失效，请在侧边栏重新打开该连接")
}

func (session *Session) relogin(ctx context.Context) *SessionError {
	session.invalidate()
	return session.ensureLogin(ctx)
}

// --- AI assistant tools -------------------------------------------------

func mcpTools() any {
	return map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "xxljob_executor_groups",
				"description": "List XXL-JOB executor groups (AppName, title, registered executor addresses).",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			map[string]any{
				"name":        "xxljob_list_jobs",
				"description": "List XXL-JOB jobs with schedule config and trigger status.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"groupId":       map[string]any{"type": "number", "description": "Executor group id; omit for all groups"},
						"keyword":       map[string]any{"type": "string", "description": "Match job description (or handler when searchField=executorHandler)"},
						"searchField":   map[string]any{"type": "string", "enum": []string{"jobDesc", "executorHandler", "author"}},
						"triggerStatus": map[string]any{"type": "string", "enum": []string{"all", "running", "stopped"}},
						"limit":         map[string]any{"type": "number", "description": "Default 20, max 100"},
					},
				},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			map[string]any{
				"name":        "xxljob_job_logs",
				"description": "Read recent scheduling logs of one XXL-JOB job, including trigger and handle results.",
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"jobId"},
					"properties": map[string]any{
						"jobId": map[string]any{"type": "number"},
						"limit": map[string]any{"type": "number", "description": "Default 10, max 50"},
					},
				},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			map[string]any{
				"name":        "xxljob_trigger_job",
				"description": "Trigger one immediate execution of an XXL-JOB job. This runs real work on the executor.",
				"inputSchema": map[string]any{
					"type":     "object",
					"required": []string{"jobId"},
					"properties": map[string]any{
						"jobId":         map[string]any{"type": "number"},
						"executorParam": map[string]any{"type": "string", "description": "Task parameter passed to the executor"},
					},
				},
			},
		},
	}
}

func (p *plugin) callTool(params map[string]any) (any, *SessionError) {
	// AI tool calls carry the open connection's lifecycle payload, secrets
	// included, so no sidecar session has to be pre-registered for them.
	lifecycle, sessionErr := lifecycleFromMap(paramMap(params, "lifecycle"))
	if sessionErr != nil {
		return toolResult("Cannot resolve the XXL-JOB connection: "+sessionErr.Message, true), nil
	}
	endpoint, sessionErr := resolveEndpoint(lifecycle)
	if sessionErr != nil {
		return toolResult(sessionErr.Message, true), nil
	}
	session, err := newEndpointSession(endpoint)
	if err != nil {
		return toolResult("Cannot initialise the HTTP client: "+err.Error(), true), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	if loginErr := session.ensureLogin(ctx); loginErr != nil {
		return toolResult(loginErr.Message, true), nil
	}
	arguments := paramMap(params, "arguments")
	tool := paramString(params, "tool")

	switch tool {
	case "xxljob_executor_groups":
		groups, listErr := session.Groups(ctx)
		if listErr != nil {
			return toolResult(listErr.Message, true), nil
		}
		return toolText(groups)
	case "xxljob_list_jobs":
		status := -1
		switch paramString(arguments, "triggerStatus") {
		case "running":
			status = 1
		case "stopped":
			status = 0
		}
		limit := paramInt(arguments, "limit")
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		result, listErr := session.Jobs(ctx, JobsRequest{
			GroupID:     paramInt(arguments, "groupId"),
			Status:      status,
			SearchField: paramString(arguments, "searchField"),
			Keyword:     paramString(arguments, "keyword"),
			Length:      limit,
		})
		if listErr != nil {
			return toolResult(listErr.Message, true), nil
		}
		return toolText(result)
	case "xxljob_job_logs":
		limit := paramInt(arguments, "limit")
		if limit <= 0 || limit > 50 {
			limit = 10
		}
		result, listErr := session.Logs(ctx, -1, paramInt(arguments, "jobId"), -1, 0, limit, "")
		if listErr != nil {
			return toolResult(listErr.Message, true), nil
		}
		return toolText(result)
	case "xxljob_trigger_job":
		result, triggerErr := session.TriggerJob(ctx, paramInt(arguments, "jobId"), paramString(arguments, "executorParam"), "")
		if triggerErr != nil {
			return toolResult(triggerErr.Message, true), nil
		}
		return toolText(result)
	}
	return toolResult("Unknown tool: "+tool, true), nil
}

func lifecycleFromMap(values map[string]any) (lifecycleParams, *SessionError) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return lifecycleParams{}, newSessionError(errInvalidParams, "无法解析连接配置")
	}
	var lifecycle lifecycleParams
	if err := json.Unmarshal(encoded, &lifecycle); err != nil {
		return lifecycleParams{}, newSessionError(errInvalidParams, "无法解析连接配置")
	}
	return lifecycle, nil
}

func toolText(value any) (any, *SessionError) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return toolResult("Cannot encode the result", true), nil
	}
	return toolResult(string(encoded), false), nil
}

func toolResult(text string, isError bool) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

// --- parameter helpers --------------------------------------------------

func paramString(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(params[key]))
}

func paramInt(params map[string]any, key string) int {
	if params == nil {
		return 0
	}
	return intValue(params[key])
}

func paramMap(params map[string]any, key string) map[string]any {
	if params == nil {
		return nil
	}
	if value, ok := params[key].(map[string]any); ok {
		return value
	}
	return nil
}
