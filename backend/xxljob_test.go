package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// mockAdmin is a miniature XXL-JOB admin console: form login with the identity
// cookie, PageHelper-shaped page endpoints, ReturnT mutations, the GLUE page,
// and the trigger chart. It exists so the plugin can be tested without a real
// deployment, including the expired-session path.
type mockAdmin struct {
	mu           sync.Mutex
	contextPath  string
	username     string
	password     string
	sessions     map[string]bool
	loginCalls   int
	expireAfter  int
	authRequests int
	form         map[string]url.Values
	groups       []map[string]any
	jobs         []map[string]any
	logs         []map[string]any
	glue         string
	detailByID   bool
	killCalled   bool
	// logDetailRaw overrides the logDetailCat answer, so the "executor never
	// received the trigger" case can be reproduced exactly as 2.4.1 sends it.
	logDetailRaw string
}

func newMockAdmin(t *testing.T, contextPath string) (*mockAdmin, *httptest.Server) {
	t.Helper()
	admin := &mockAdmin{
		contextPath: contextPath,
		username:    "admin",
		password:    "123456",
		sessions:    map[string]bool{},
		form:        map[string]url.Values{},
		glue:        "public class DemoHandler { }",
		groups: []map[string]any{
			{"id": 1, "appname": "xxl-job-executor-sample", "title": "示例执行器", "addressType": 0,
				"addressList": "", "registryList": []string{"http://127.0.0.1:9999/"}},
			{"id": 2, "appname": "billing-executor", "title": "计费执行器", "addressType": 1,
				"addressList": "http://10.0.0.2:9999/", "registryList": []string{}},
		},
		jobs: []map[string]any{
			{"id": 1, "jobGroup": 1, "jobDesc": "示例任务", "author": "XXL", "scheduleType": "CRON",
				"scheduleConf": "0 0 0 * * ?", "glueType": "BEAN", "executorHandler": "demoJobHandler",
				"executorRouteStrategy": "ROUND", "misfireStrategy": "DO_NOTHING",
				"executorBlockStrategy": "SERIAL_EXECUTION", "triggerStatus": 1, "triggerLastTime": 1700000000000},
			{"id": 2, "jobGroup": 2, "jobDesc": "账单核对", "author": "alice", "scheduleType": "FIX_RATE",
				"scheduleConf": "30", "glueType": "GLUE_SHELL", "executorHandler": "billCheck",
				"executorRouteStrategy": "FIRST", "misfireStrategy": "FIRE_ONCE_NOW",
				"executorBlockStrategy": "DISCARD_LATER", "triggerStatus": 0},
		},
		logs: []map[string]any{
			{"id": 11, "jobGroup": 1, "jobId": 1, "jobDesc": "示例任务", "executorAddress": "http://127.0.0.1:9999/",
				"executorHandler": "demoJobHandler", "triggerTime": 1700000000000, "triggerCode": 200,
				"triggerMsg": "触发成功", "handleCode": 200, "handleMsg": "执行成功"},
			{"id": 12, "jobGroup": 2, "jobId": 2, "jobDesc": "账单核对", "executorAddress": "http://10.0.0.2:9999/",
				"executorHandler": "billCheck", "triggerTime": 1700000100000, "triggerCode": 200,
				"triggerMsg": "触发成功", "handleCode": 500, "handleMsg": "执行失败"},
		},
	}
	mux := http.NewServeMux()
	prefix := contextPath
	mux.HandleFunc(prefix+"/login", admin.handleLogin)
	mux.HandleFunc(prefix+"/toLogin", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(writer, `<html><head><title>XXL-JOB</title></head><body>xxl-job 登录</body></html>`)
	})
	mux.HandleFunc(prefix+"/", admin.handleAPI)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return admin, server
}

func (admin *mockAdmin) setForm(path string, form url.Values) {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	admin.form[path] = form
}

func (admin *mockAdmin) lastForm(path string) url.Values {
	admin.mu.Lock()
	defer admin.mu.Unlock()
	return admin.form[path]
}

func (admin *mockAdmin) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)
		return
	}
	admin.mu.Lock()
	admin.loginCalls++
	admin.form["/login"] = request.PostForm
	valid := request.PostForm.Get("userName") == admin.username && request.PostForm.Get("password") == admin.password
	if valid {
		admin.sessions["token"] = true
	}
	admin.mu.Unlock()
	if !valid {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = io.WriteString(writer, `{"code":500,"msg":"账号或密码错误","content":null}`)
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: loginCookieName, Value: "token", Path: "/", HttpOnly: true})
	writer.Header().Set("Location", admin.contextPath+"/")
	writer.WriteHeader(http.StatusFound)
}

// mockRoutes mirrors the console's mapped endpoints. An unmapped URL is a 404
// in Spring regardless of authentication, which is exactly what a wrong context
// path produces in a real deployment.
var mockRoutes = map[string]bool{
	"/jobgroup/pageList": true, "/jobinfo/pageList": true, "/jobinfo/detailById": true,
	"/jobinfo/add": true, "/jobinfo/update": true, "/jobinfo/start": true, "/jobinfo/stop": true,
	"/jobinfo/remove": true, "/jobinfo/trigger": true, "/jobinfo/nextTriggerTime": true,
	"/joblog/pageList": true, "/joblog/logDetailCat": true, "/joblog/logKill": true,
	"/jobcode": true, "/jobcode/save": true, "/chartInfo": true,
}

func (admin *mockAdmin) handleAPI(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, admin.contextPath)
	if path == "" {
		path = "/"
	}
	if !mockRoutes[path] {
		http.NotFound(writer, request)
		return
	}
	admin.mu.Lock()
	cookie, _ := request.Cookie(loginCookieName)
	authenticated := cookie != nil && admin.sessions[cookie.Value]
	if authenticated {
		admin.authRequests++
		if admin.expireAfter > 0 && admin.authRequests > admin.expireAfter {
			// Expire exactly once so the client's automatic re-login can succeed.
			admin.sessions = map[string]bool{}
			admin.expireAfter = 0
			authenticated = false
		}
	}
	admin.mu.Unlock()
	if !authenticated {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = io.WriteString(writer, `{"code":500,"msg":"登录已过期","content":null}`)
		return
	}
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)
		return
	}
	if request.Method == http.MethodPost {
		admin.setForm(path, request.PostForm)
	} else if len(request.URL.Query()) > 0 {
		admin.setForm(path, request.URL.Query())
	}
	writer.Header().Set("Content-Type", "application/json; charset=UTF-8")

	switch {
	case path == "/jobgroup/pageList":
		admin.mu.Lock()
		groups := append([]map[string]any(nil), admin.groups...)
		admin.mu.Unlock()
		writePage(writer, request, groups)
	case path == "/jobinfo/pageList":
		if !requireIntQuery(writer, request, "jobGroup") {
			return
		}
		admin.mu.Lock()
		jobs := make([]map[string]any, 0, len(admin.jobs))
		for _, job := range admin.jobs {
			if !matchesJob(job, request.URL.Query()) {
				continue
			}
			jobs = append(jobs, job)
		}
		admin.mu.Unlock()
		writePage(writer, request, jobs)
	case path == "/jobinfo/detailById":
		admin.mu.Lock()
		supported := admin.detailByID
		admin.mu.Unlock()
		if !supported {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		id, _ := strconv.Atoi(request.URL.Query().Get("id"))
		admin.mu.Lock()
		job := findJob(admin.jobs, id)
		admin.mu.Unlock()
		writeReturn(writer, job)
	case path == "/jobinfo/add":
		admin.mu.Lock()
		job := map[string]any{"id": len(admin.jobs) + 1}
		for key, values := range request.PostForm {
			job[key] = values[0]
		}
		job["id"] = len(admin.jobs) + 1
		admin.jobs = append(admin.jobs, job)
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/jobinfo/update":
		admin.mu.Lock()
		id, _ := strconv.Atoi(request.PostForm.Get("id"))
		if job := findJob(admin.jobs, id); job != nil {
			for key, values := range request.PostForm {
				if key != "id" {
					job[key] = values[0]
				}
			}
		}
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/jobinfo/start", path == "/jobinfo/stop":
		admin.mu.Lock()
		id, _ := strconv.Atoi(request.PostForm.Get("id"))
		if job := findJob(admin.jobs, id); job != nil {
			if path == "/jobinfo/start" {
				job["triggerStatus"] = 1
			} else {
				job["triggerStatus"] = 0
			}
		}
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/jobinfo/remove":
		admin.mu.Lock()
		id, _ := strconv.Atoi(request.PostForm.Get("id"))
		remaining := admin.jobs[:0]
		for _, job := range admin.jobs {
			if intValue(job["id"]) != id {
				remaining = append(remaining, job)
			}
		}
		admin.jobs = remaining
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/jobinfo/trigger":
		writeReturn(writer, nil)
	case path == "/jobinfo/nextTriggerTime":
		writeReturn(writer, []string{"2026-10-22 00:00:00", "2026-10-23 00:00:00"})
	case path == "/joblog/pageList":
		if !requireIntQuery(writer, request, "jobGroup") {
			return
		}
		admin.mu.Lock()
		logs := make([]map[string]any, 0, len(admin.logs))
		jobID, _ := strconv.Atoi(request.URL.Query().Get("jobId"))
		for _, entry := range admin.logs {
			if jobID > 0 && intValue(entry["jobId"]) != jobID {
				continue
			}
			logs = append(logs, entry)
		}
		admin.mu.Unlock()
		writePage(writer, request, logs)
	case path == "/joblog/logDetailCat":
		admin.mu.Lock()
		raw := admin.logDetailRaw
		admin.mu.Unlock()
		if raw != "" {
			_, _ = io.WriteString(writer, raw)
			return
		}
		fromLine, _ := strconv.Atoi(request.URL.Query().Get("fromLineNum"))
		content := map[string]any{
			"fromLineNum": fromLine,
			"toLineNum":   fromLine + 1,
			"logContent":  fmt.Sprintf("line %d\ncomplete\n", fromLine),
			"end":         true,
		}
		writeReturn(writer, content)
	case path == "/joblog/logKill":
		admin.mu.Lock()
		admin.killCalled = true
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/jobcode":
		admin.mu.Lock()
		source := admin.glue
		admin.mu.Unlock()
		writer.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(writer, `<html><body><textarea id="glueSource" name="glueSource">`+
			strings.ReplaceAll(source, "<", "&lt;")+`</textarea></body></html>`)
	case path == "/jobcode/save":
		admin.mu.Lock()
		admin.glue = request.PostForm.Get("glueSource")
		admin.mu.Unlock()
		writeReturn(writer, nil)
	case path == "/chartInfo":
		writeReturn(writer, map[string]any{
			"triggerDayList":         []string{"2026-10-20", "2026-10-21"},
			"triggerDayCount":        []int{12, 30},
			"triggerDayCountSuc":     []int{10, 28},
			"triggerDayCountFail":    []int{2, 2},
			"triggerDayCountRunning": []int{0, 1},
		})
	default:
		http.NotFound(writer, request)
	}
}

func matchesJob(job map[string]any, query url.Values) bool {
	if group := query.Get("jobGroup"); group != "" && group != "-1" && intValue(group) > 0 {
		if intValue(job["jobGroup"]) != intValue(group) {
			return false
		}
	}
	if status := query.Get("triggerStatus"); status == "1" || status == "0" {
		if intValue(job["triggerStatus"]) != intValue(status) {
			return false
		}
	}
	for _, pair := range [][2]string{{"jobDesc", "jobDesc"}, {"executorHandler", "executorHandler"}, {"author", "author"}} {
		if filter := query.Get(pair[0]); filter != "" {
			if !strings.Contains(strings.ToLower(stringValue(job[pair[1]])), strings.ToLower(filter)) {
				return false
			}
		}
	}
	return true
}

func findJob(jobs []map[string]any, id int) map[string]any {
	for _, job := range jobs {
		if intValue(job["id"]) == id {
			return job
		}
	}
	return nil
}

// requireIntQuery reproduces how xxl-job-admin 2.4.1 binds page filters:
// `jobGroup` is a primitive int, so a request that omits it is answered with
// HTTP 200 and a 500 envelope carrying Spring's binding error. Modelling it
// here keeps the plugin's probes honest instead of silently accepting a
// request a real console rejects.
func requireIntQuery(writer http.ResponseWriter, request *http.Request, name string) bool {
	if _, ok := request.URL.Query()[name]; ok {
		return true
	}
	writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = io.WriteString(writer, `{"code":500,"msg":"java.lang.IllegalStateException: Optional int parameter '`+
		name+`' is present but cannot be translated into a null value due to being declared as a primitive type.`+
		` Consider declaring it as object wrapper for the corresponding primitive type.","content":null}`)
	return false
}

func writePage(writer http.ResponseWriter, request *http.Request, rows []map[string]any) {
	start, _ := strconv.Atoi(request.URL.Query().Get("start"))
	length, _ := strconv.Atoi(request.URL.Query().Get("length"))
	if length <= 0 {
		length = 10
	}
	begin := start
	if begin > len(rows) {
		begin = len(rows)
	}
	end := begin + length
	if end > len(rows) {
		end = len(rows)
	}
	payload := map[string]any{
		"recordsTotal":    len(rows),
		"recordsFiltered": len(rows),
		"data":            rows[begin:end],
	}
	encoded, _ := json.Marshal(payload)
	_, _ = writer.Write(encoded)
}

func writeReturn(writer http.ResponseWriter, content any) {
	encoded, _ := json.Marshal(map[string]any{"code": 200, "msg": nil, "content": content})
	_, _ = writer.Write(encoded)
}

func noopNotify(string, any) {}

func testLifecycle(baseURL string) map[string]any {
	parsed, _ := url.Parse(baseURL)
	host := parsed.Hostname()
	port, _ := strconv.Atoi(parsed.Port())
	return map[string]any{
		"provider": map[string]any{"id": providerID, "databaseType": "xxljob"},
		"connection": map[string]any{
			"id":       "conn-1",
			"name":     "本地 XXL-JOB",
			"host":     host,
			"port":     port,
			"username": "admin",
			"password": "123456",
			"external_config": map[string]any{
				"scheme":               "http",
				"context_path":         parsed.Path,
				"connect_timeout_secs": 10,
			},
		},
		"runtime": map[string]any{"host": host, "port": port},
	}
}

func TestBuildBaseURL(t *testing.T) {
	cases := []struct {
		scheme, host string
		port         int
		contextPath  string
		expected     string
	}{
		{"http", "127.0.0.1", 8080, "/xxl-job-admin", "http://127.0.0.1:8080/xxl-job-admin"},
		{"http", "127.0.0.1", 8080, "xxl-job-admin/", "http://127.0.0.1:8080/xxl-job-admin"},
		{"https", "admin.example.com", 443, "/", "https://admin.example.com"},
		{"http", "admin.example.com", 80, "", "http://admin.example.com"},
		{"", "http://10.0.0.2:9090/custom-admin", 0, "", "http://10.0.0.2:9090/custom-admin"},
		{"http", "localhost", 0, "", "http://localhost:8080"},
	}
	for _, testCase := range cases {
		actual, err := buildBaseURL(testCase.scheme, testCase.host, testCase.port, testCase.contextPath)
		if err != nil {
			t.Fatalf("buildBaseURL(%v) failed: %v", testCase, err.Message)
		}
		if actual != testCase.expected {
			t.Fatalf("buildBaseURL(%v) = %s, want %s", testCase, actual, testCase.expected)
		}
	}
	if _, err := buildBaseURL("http", "", 8080, ""); err == nil {
		t.Fatal("expected an error for an empty host")
	}
	if _, err := buildBaseURL("ftp", "host", 21, ""); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
}

func TestConnectionTestAndGroups(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	instance := &plugin{sessions: map[string]*Session{}}
	emitter := noopNotify

	result, err := instance.dispatch("connection/test", testLifecycle(server.URL), emitter)
	if err != nil {
		t.Fatalf("connection/test failed: %v", err.Message)
	}
	testResult := result.(map[string]any)
	if testResult["success"] != true {
		t.Fatalf("connection/test did not succeed: %v", testResult)
	}
	if !strings.Contains(testResult["message"].(string), "执行器分组 2 个") {
		t.Fatalf("unexpected test message: %v", testResult["message"])
	}
	if login := admin.lastForm("/login"); login.Get("userName") != "admin" || login.Get("password") != "123456" {
		t.Fatalf("login form was not sent as a form post: %v", login)
	}
	if _, err := instance.dispatch("connection/connect", testLifecycle(server.URL), emitter); err != nil {
		t.Fatalf("connection/connect failed: %v", err.Message)
	}
	groups, err := instance.dispatch("xxljob/groups", map[string]any{"connectionId": "conn-1"}, emitter)
	if err != nil {
		t.Fatalf("xxljob/groups failed: %v", err.Message)
	}
	list := groups.(map[string]any)["groups"].([]json.RawMessage)
	if len(list) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(list))
	}
}

// TestLoginProbeUsesTheConsolesRequiredFilters pins the login probe against a
// real 2.4.1 console: pageList without jobGroup answers a 500 envelope, and the
// client would report that as a rejected login instead of a working session.
func TestLoginProbeUsesTheConsolesRequiredFilters(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	session, err := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	if err != nil {
		t.Fatalf("newSession failed: %v", err)
	}
	if loginErr := session.ensureLogin(context.Background()); loginErr != nil {
		t.Fatalf("login probe was rejected by the console: %v（%s）", loginErr.Message, loginErr.Detail)
	}
	probe := admin.lastForm("/jobinfo/pageList")
	for _, name := range []string{"start", "length", "jobGroup", "triggerStatus", "jobDesc", "executorHandler", "author"} {
		if _, ok := probe[name]; !ok {
			t.Fatalf("login probe is missing %q: %v", name, probe)
		}
	}
}

func TestConnectionTestRejectsBadCredentials(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["password"] = "wrong"
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result, got %v", err.Message)
	}
	testResult := result.(map[string]any)
	if testResult["success"] != false {
		t.Fatalf("expected a failure result, got %v", testResult)
	}
	if !strings.Contains(testResult["message"].(string), "账号或密码错误") {
		t.Fatalf("unexpected failure message: %v", testResult["message"])
	}
}

func TestLoginFailureNamesTheWorkingContextPath(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/wrong", "connect_timeout_secs": 10,
	}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result: %v", err.Message)
	}
	message := result.(map[string]any)["message"].(string)
	if !strings.Contains(message, "HTTP 404") {
		t.Fatalf("expected the plain 404 to be reported, got: %s", message)
	}
	if !strings.Contains(message, "已探测到管理端实际位于") || !strings.Contains(message, "/xxl-job-admin") {
		t.Fatalf("expected the diagnosis to name the working console root, got: %s", message)
	}
}

func TestConnectRecoversFromWrongContextPath(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/wrong", "connect_timeout_secs": 10,
	}
	instance := &plugin{sessions: map[string]*Session{}}
	connected, err := instance.dispatch("connection/connect", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connect should recover from a wrong context path: %v", err.Message)
	}
	result := connected.(map[string]any)
	if result["autoDetected"] != true {
		t.Fatalf("expected the recovered root to be reported: %+v", result)
	}
	if !strings.HasSuffix(result["baseUrl"].(string), "/xxl-job-admin") {
		t.Fatalf("unexpected recovered base url: %+v", result["baseUrl"])
	}
	// The recovered session must be usable straight away.
	jobs, err := instance.dispatch("xxljob/jobs", map[string]any{"connectionId": "conn-1", "length": 20}, noopNotify)
	if err != nil {
		t.Fatalf("the recovered session is not usable: %v", err.Message)
	}
	if jobs.(map[string]any)["total"].(int) != 2 {
		t.Fatalf("unexpected job page: %+v", jobs)
	}
}

func TestConnectDoesNotMaskBadCredentials(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["password"] = "wrong"
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/wrong", "connect_timeout_secs": 10,
	}
	instance := &plugin{sessions: map[string]*Session{}}
	if _, err := instance.dispatch("connection/connect", lifecycle, noopNotify); err == nil {
		t.Fatal("connect must fail when the credentials are wrong, even if a console root is found")
	} else if !strings.Contains(err.Message, "账号或密码错误") {
		t.Fatalf("expected the credential error to survive, got: %s", err.Message)
	}
	if _, err := instance.dispatch("xxljob/jobs", map[string]any{"connectionId": "conn-1"}, noopNotify); err == nil {
		t.Fatal("a failed connect must not leave a session behind")
	}
}

func TestLoginFailureDiagnosesRootDeployment(t *testing.T) {
	_, server := newMockAdmin(t, "")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/xxl-job-admin", "connect_timeout_secs": 10,
	}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result: %v", err.Message)
	}
	message := result.(map[string]any)["message"].(string)
	if !strings.Contains(message, "已探测到管理端实际位于") || !strings.Contains(message, "改为 /") {
		t.Fatalf("expected the diagnosis to point at the root context path, got: %s", message)
	}
}

func TestDetectEndpointFindsRootDeployment(t *testing.T) {
	_, server := newMockAdmin(t, "")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/xxl-job-admin",
	}
	lifecycle["action"] = map[string]any{"id": "detect-endpoint"}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	action := result.(map[string]any)
	if action["success"] != true {
		t.Fatalf("endpoint detection failed: %+v", action)
	}
	fields := action["fieldValues"].(map[string]any)
	if fields["context_path"] != "/" {
		t.Fatalf("expected the root context path, got: %+v", fields)
	}
}

func TestDetectEndpointReportsUnreachableHost(t *testing.T) {
	lifecycle := map[string]any{
		"provider": map[string]any{"id": providerID, "databaseType": "xxljob"},
		"connection": map[string]any{
			"id": "conn-offline", "host": "127.0.0.1", "port": 1, "username": "admin", "password": "123456",
			"external_config": map[string]any{"scheme": "http", "context_path": "/xxl-job-admin", "connect_timeout_secs": 3},
		},
		"runtime": map[string]any{"host": "127.0.0.1", "port": 1},
		"action":  map[string]any{"id": "detect-endpoint"},
	}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	message := result.(map[string]any)["message"].(string)
	if !strings.Contains(message, "无法连通") {
		t.Fatalf("expected an unreachable-host message, got: %s", message)
	}
}

func TestInspectEndpointConfirmsClassicConsole(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["action"] = map[string]any{"id": "inspect-endpoint"}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	action := result.(map[string]any)
	if action["success"] != true {
		t.Fatalf("expected the classic console to be confirmed: %+v", action)
	}
	if report := action["message"].(string); !strings.Contains(report, "经典 xxl-job-admin 接口") {
		t.Fatalf("unexpected report: %s", report)
	}
}

// TestInspectEndpointReportsRestyledConsole covers the deployment shape that
// actually breaks classic clients: a SPA that answers every GET with index.html
// and exposes its own JSON API under /api.
func TestInspectEndpointReportsRestyledConsole(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/login" && request.Method == http.MethodPost {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"code":200,"msg":"ok","data":{"token":"abc"}}`)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"code":500,"msg":"未登录"}`)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(writer, `<html><head><title>XXL-JOB</title></head><body><div id="app"></div></body></html>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{
		"scheme": "http", "context_path": "/",
	}
	lifecycle["action"] = map[string]any{"id": "inspect-endpoint"}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	action := result.(map[string]any)
	if action["success"] != false {
		t.Fatalf("an SPA console must not be reported as classic: %+v", action)
	}
	report := action["message"].(string)
	if !strings.Contains(report, "没有找到经典 xxl-job-admin") {
		t.Fatalf("missing the classic-console verdict: %s", report)
	}
	if !strings.Contains(report, "/api/login") || !strings.Contains(report, "JSON code=200") {
		t.Fatalf("the probe did not surface the SPA login endpoint: %s", report)
	}
	if !strings.Contains(report, "疑似 SPA") {
		t.Fatalf("the probe did not flag the SPA shell: %s", report)
	}
}

func TestConnectionTestReportsTransportFailure(t *testing.T) {
	instance := &plugin{sessions: map[string]*Session{}}
	lifecycle := map[string]any{
		"provider": map[string]any{"id": providerID, "databaseType": "xxljob"},
		"connection": map[string]any{
			"id": "conn-offline", "host": "127.0.0.1", "port": 1, "username": "admin", "password": "123456",
			"external_config": map[string]any{"scheme": "http", "context_path": "/xxl-job-admin", "connect_timeout_secs": 3},
		},
		"runtime": map[string]any{"host": "127.0.0.1", "port": 1},
	}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should never fail the RPC for a transport error: %v", err.Message)
	}
	testResult := result.(map[string]any)
	if testResult["success"] != false {
		t.Fatalf("expected a failure result for a closed port, got %+v", testResult)
	}
	if !strings.Contains(testResult["message"].(string), "无法连接") {
		t.Fatalf("unexpected failure message: %v", testResult["message"])
	}
}

func TestSessionExpiryReloginsOnce(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	admin.expireAfter = 1
	session, newErr := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	if newErr != nil {
		t.Fatalf("newSession failed: %v", newErr)
	}
	ctx := context.Background()
	if err := session.ensureLogin(ctx); err != nil {
		t.Fatalf("login failed: %v", err.Message)
	}
	first, err := session.Groups(ctx)
	if err != nil {
		t.Fatalf("first call failed: %v", err.Message)
	}
	if len(first) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(first))
	}
	// The admin dropped the cookie, so the next call must log in again.
	second, err := session.Groups(ctx)
	if err != nil {
		t.Fatalf("call after expiry failed: %v", err.Message)
	}
	if len(second) != 2 {
		t.Fatalf("expected 2 groups after re-login, got %d", len(second))
	}
	if admin.loginCalls < 2 {
		t.Fatalf("expected a second login, got %d", admin.loginCalls)
	}
}

func TestJobsPagingFilterAndDetailFallback(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	ctx := context.Background()

	page, err := session.Jobs(ctx, JobsRequest{GroupID: 1, Status: -1, Start: 0, Length: 20})
	if err != nil {
		t.Fatalf("Jobs failed: %v", err.Message)
	}
	jobs := page["jobs"].([]json.RawMessage)
	if len(jobs) != 1 || page["total"].(int) != 1 {
		t.Fatalf("group filter returned the wrong page: %+v", page)
	}
	search, err := session.Jobs(ctx, JobsRequest{Status: -1, SearchField: "executorHandler", Keyword: "bill", Start: 0, Length: 20})
	if err != nil {
		t.Fatalf("filtered Jobs failed: %v", err.Message)
	}
	if len(search["jobs"].([]json.RawMessage)) != 1 {
		t.Fatalf("handler filter returned the wrong page: %+v", search)
	}
	stopped, err := session.Jobs(ctx, JobsRequest{Status: 0, Start: 0, Length: 20})
	if err != nil {
		t.Fatalf("status filter failed: %v", err.Message)
	}
	if len(stopped["jobs"].([]json.RawMessage)) != 1 {
		t.Fatalf("stopped filter returned the wrong page: %+v", stopped)
	}
	job, err := session.Job(ctx, 2)
	if err != nil {
		t.Fatalf("Job detail fallback failed: %v", err.Message)
	}
	if job["jobDesc"] != "账单核对" {
		t.Fatalf("unexpected job detail: %+v", job)
	}
	if _, err := session.Job(ctx, 99); err == nil || err.Code != errNotFound {
		t.Fatalf("expected a not-found error, got %+v", err)
	}
}

func TestJobMutationsEncodeTheConsoleForm(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	ctx := context.Background()

	if _, err := session.SaveJob(ctx, map[string]any{
		"jobGroup":        1,
		"jobDesc":         "新建任务",
		"executorHandler": "demoJobHandler",
		"scheduleType":    "CRON",
		"scheduleConf":    "0 0/5 * * * ?",
	}); err != nil {
		t.Fatalf("SaveJob(add) failed: %v", err.Message)
	}
	add := admin.lastForm("/jobinfo/add")
	if add.Get("jobDesc") != "新建任务" || add.Get("executorRouteStrategy") != "ROUND" || add.Get("id") != "" {
		t.Fatalf("unexpected add form: %v", add)
	}
	if _, err := session.SaveJob(ctx, map[string]any{"id": 1, "jobGroup": 1, "jobDesc": "改名", "executorHandler": "demoJobHandler", "scheduleType": "NONE"}); err != nil {
		t.Fatalf("SaveJob(update) failed: %v", err.Message)
	}
	if admin.lastForm("/jobinfo/update").Get("id") != "1" {
		t.Fatalf("update did not send the id: %v", admin.lastForm("/jobinfo/update"))
	}
	if _, err := session.StartJob(ctx, 2); err != nil {
		t.Fatalf("StartJob failed: %v", err.Message)
	}
	if admin.lastForm("/jobinfo/start").Get("id") != "2" {
		t.Fatal("start did not send the id")
	}
	if _, err := session.StopJob(ctx, 2); err != nil {
		t.Fatalf("StopJob failed: %v", err.Message)
	}
	trigger, err := session.TriggerJob(ctx, 1, "bizDate=20261021", "")
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err.Message)
	}
	if trigger["addressList"] != "http://127.0.0.1:9999/" {
		t.Fatalf("trigger did not resolve the executor address: %+v", trigger)
	}
	triggerForm := admin.lastForm("/jobinfo/trigger")
	if triggerForm.Get("executorParam") != "bizDate=20261021" || triggerForm.Get("addressList") == "" {
		t.Fatalf("unexpected trigger form: %v", triggerForm)
	}
	if _, err := session.RemoveJob(ctx, 2); err != nil {
		t.Fatalf("RemoveJob failed: %v", err.Message)
	}
	if _, err := session.SaveJob(ctx, map[string]any{"jobGroup": 1, "jobDesc": "缺 handler"}); err == nil {
		t.Fatal("expected validation to reject a BEAN job without a handler")
	}
}

func TestLogsDetailAndKill(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	ctx := context.Background()

	logs, err := session.Logs(ctx, -1, 2, -1, 0, 10, "")
	if err != nil {
		t.Fatalf("Logs failed: %v", err.Message)
	}
	if len(logs["logs"].([]json.RawMessage)) != 1 {
		t.Fatalf("job filter returned the wrong log page: %+v", logs)
	}
	detail, err := session.LogDetail(ctx, 12, 3)
	if err != nil {
		t.Fatalf("LogDetail failed: %v", err.Message)
	}
	if detail["fromLineNum"].(int) != 3 || !strings.Contains(detail["logContent"].(string), "line 3") {
		t.Fatalf("unexpected log detail: %+v", detail)
	}
	if _, err := session.KillLog(ctx, 12); err != nil {
		t.Fatalf("KillLog failed: %v", err.Message)
	}
	if !admin.killCalled {
		t.Fatal("kill endpoint was not called")
	}
}

// TestLogDetailExplainsUnroutableTrigger covers the 2.4.1 answer for a log
// whose trigger never reached an executor: code 500 with a null message, which
// must not surface as a bare "读取调度日志失败" in the workbench.
func TestLogDetailExplainsUnroutableTrigger(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	admin.logDetailRaw = `{"code":500,"msg":null,"content":null}`
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)

	_, err := session.LogDetail(context.Background(), 11, 1)
	if err == nil {
		t.Fatal("expected an error for a log without an executor")
	}
	if !strings.Contains(err.Message, "执行器") {
		t.Fatalf("unexpected message: %v", err.Message)
	}
}

// TestLogDetailExplainsUnreachableExecutor covers the console's forwarded
// remoting error: the scheduling record is intact, but the executor that hosts
// the execution log has gone away.
func TestLogDetailExplainsUnreachableExecutor(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	admin.logDetailRaw = `{"code":500,"msg":"Http Request Error (Connection refused), for url : http://10.18.58.5:9999/log","content":null}`
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)

	_, err := session.LogDetail(context.Background(), 11, 1)
	if err == nil {
		t.Fatal("expected an error for an unreachable executor")
	}
	if !strings.Contains(err.Message, "10.18.58.5:9999") || !strings.Contains(err.Message, "执行器") {
		t.Fatalf("unexpected message: %v", err.Message)
	}
	if !strings.Contains(err.Detail, "Connection refused") {
		t.Fatalf("raw detail should be preserved: %q", err.Detail)
	}
}

func TestGlueSourceRoundTrip(t *testing.T) {
	admin, server := newMockAdmin(t, "/xxl-job-admin")
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	ctx := context.Background()

	source, err := session.GlueSource(ctx, 2)
	if err != nil {
		t.Fatalf("GlueSource failed: %v", err.Message)
	}
	if source["supported"] != true || source["source"] != "public class DemoHandler { }" {
		t.Fatalf("unexpected glue source: %+v", source)
	}
	if _, err := session.SaveGlueSource(ctx, 2, "GLUE_SHELL", "echo hello", ""); err != nil {
		t.Fatalf("SaveGlueSource failed: %v", err.Message)
	}
	if admin.glue != "echo hello" {
		t.Fatalf("glue source was not saved: %q", admin.glue)
	}
	if admin.lastForm("/jobcode/save").Get("glueType") != "GLUE_SHELL" {
		t.Fatal("glue type was not sent")
	}
	if remark := admin.lastForm("/jobcode/save").Get("glueRemark"); remark == "" {
		t.Fatal("glue remark was not sent")
	}
}

func TestDashboardAndNextTriggerTime(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	session, _ := newSession(server.URL+"/xxl-job-admin", "admin", "123456", 5*time.Second, false)
	ctx := context.Background()

	dashboard, err := session.Dashboard(ctx, 7)
	if err != nil {
		t.Fatalf("Dashboard failed: %v", err.Message)
	}
	stats := dashboard["stats"].(map[string]any)
	if stats["groups"].(int) != 2 || stats["jobs"].(int) != 2 || stats["running"].(int) != 1 {
		t.Fatalf("unexpected dashboard stats: %+v", stats)
	}
	if dashboard["chart"] == nil {
		t.Fatal("expected chart data")
	}
	preview, err := session.NextTriggerTime(ctx, "CRON", "0 0 0 * * ?")
	if err != nil {
		t.Fatalf("NextTriggerTime failed: %v", err.Message)
	}
	if preview["supported"] != true || len(preview["times"].([]string)) != 2 {
		t.Fatalf("unexpected next trigger preview: %+v", preview)
	}
}

func TestDetectEndpointAction(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{"scheme": "http", "context_path": "/wrong"}
	lifecycle["action"] = map[string]any{"id": "detect-endpoint"}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	action := result.(map[string]any)
	if action["success"] != true {
		t.Fatalf("endpoint detection failed: %+v", action)
	}
	fields := action["fieldValues"].(map[string]any)
	if fields["context_path"] != "/xxl-job-admin" {
		t.Fatalf("unexpected detected context path: %+v", fields)
	}
}

// TestSidecarProtocolEndToEnd drives the real sidecar over the documented
// JSON-Lines protocol: handshake, test, connect, list, trigger, logs, disconnect.
func TestSidecarProtocolEndToEnd(t *testing.T) {
	_, server := newMockAdmin(t, "/xxl-job-admin")
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	sidecar := dbxpluginsdk.NewServer(
		dbxpluginsdk.Metadata{ID: pluginID, Version: "1.0.0", Capabilities: []string{"connections", "events"}},
		&plugin{sessions: map[string]*Session{}},
	).WithIO(inputReader, outputWriter, os.Stderr)
	go func() { _ = sidecar.Serve() }()

	responses := make(chan map[string]any, 32)
	go func() {
		scanner := bufio.NewScanner(outputReader)
		scanner.Buffer(make([]byte, 64*1024), 8<<20)
		for scanner.Scan() {
			var message map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				continue
			}
			responses <- message
		}
		close(responses)
	}()

	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inputWriter.Write(append(payload, '\n')); err != nil {
			t.Fatalf("write %s failed: %v", method, err)
		}
		deadline := time.After(10 * time.Second)
		for {
			select {
			case message, ok := <-responses:
				if !ok {
					t.Fatalf("%s: sidecar closed its output", method)
				}
				if intValue(message["id"]) != id {
					continue
				}
				return message
			case <-deadline:
				t.Fatalf("%s: timed out waiting for a response", method)
			}
		}
	}

	init := call(1, "plugin/initialize", map[string]any{"host": map[string]any{"protocolVersions": []int{1}}})
	if init["error"] != nil {
		t.Fatalf("initialize failed: %v", init["error"])
	}
	result := init["result"].(map[string]any)
	if result["protocolVersion"].(float64) != 1 {
		t.Fatalf("unexpected protocol version: %+v", result)
	}

	lifecycle := testLifecycle(server.URL)
	testResult := call(2, "connection/test", lifecycle)
	if testResult["error"] != nil {
		t.Fatalf("connection/test failed: %v", testResult["error"])
	}
	if testResult["result"].(map[string]any)["success"] != true {
		t.Fatalf("connection/test returned a failure: %+v", testResult["result"])
	}

	connect := call(3, "connection/connect", lifecycle)
	if connect["error"] != nil {
		t.Fatalf("connection/connect failed: %v", connect["error"])
	}

	jobs := call(4, "xxljob/jobs", map[string]any{"connectionId": "conn-1", "groupId": 0, "length": 20})
	if jobs["error"] != nil {
		t.Fatalf("xxljob/jobs failed: %v", jobs["error"])
	}
	if jobs["result"].(map[string]any)["total"].(float64) != 2 {
		t.Fatalf("unexpected job page: %+v", jobs["result"])
	}

	trigger := call(5, "xxljob/job/trigger", map[string]any{"connectionId": "conn-1", "id": 1, "executorParam": "x=1"})
	if trigger["error"] != nil {
		t.Fatalf("xxljob/job/trigger failed: %v", trigger["error"])
	}

	logs := call(6, "xxljob/logs", map[string]any{"connectionId": "conn-1", "groupId": -1, "jobId": 1, "length": 10})
	if logs["error"] != nil {
		t.Fatalf("xxljob/logs failed: %v", logs["error"])
	}

	tools := call(7, "mcp/tools", map[string]any{})
	if tools["error"] != nil {
		t.Fatalf("mcp/tools failed: %v", tools["error"])
	}
	if len(tools["result"].(map[string]any)["tools"].([]any)) != 4 {
		t.Fatalf("unexpected AI tool list: %+v", tools["result"])
	}

	missing := call(8, "xxljob/jobs", map[string]any{"connectionId": "gone"})
	errorObject, ok := missing["error"].(map[string]any)
	if !ok || intValue(errorObject["code"]) != errNoSession {
		t.Fatalf("expected a no-session error, got %+v", missing)
	}

	disconnect := call(9, "connection/disconnect", map[string]any{"connectionId": "conn-1"})
	if disconnect["error"] != nil {
		t.Fatalf("connection/disconnect failed: %v", disconnect["error"])
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
}
