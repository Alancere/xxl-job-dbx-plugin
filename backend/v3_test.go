package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newMockAdminV3 models a 3.x console: form login at /auth/doLogin with a
// `xxl_job_login_token` cookie, a `{code,data,msg,success}` envelope, and the
// offset/pagesize pagination plus the insert/delete mutation names.
func newMockAdminV3(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authed := func(request *http.Request) bool {
		cookie, err := request.Cookie(v3LoginCookieName)
		return err == nil && cookie.Value == "v3-token"
	}
	mux.HandleFunc("/xxl-job-admin/auth/doLogin", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if request.PostForm.Get("userName") != "admin" || request.PostForm.Get("password") != "123456" {
			_, _ = writer.Write([]byte(`{"code":500,"data":null,"msg":"账号或密码错误","success":false}`))
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: v3LoginCookieName, Value: "v3-token", Path: "/", HttpOnly: true})
		_, _ = writer.Write([]byte(`{"code":200,"data":null,"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/login", func(writer http.ResponseWriter, _ *http.Request) {
		http.NotFound(writer, nil)
	})
	mux.HandleFunc("/xxl-job-admin/jobinfo/pageList", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if !authed(request) {
			_, _ = writer.Write([]byte(`{"code":500,"msg":"com.xxl.sso.core.exception.XxlSsoException: not login"}`))
			return
		}
		// 3.x requires every text filter to be present; missing one is a 400.
		for _, key := range []string{"jobGroup", "triggerStatus", "jobDesc", "executorHandler", "author", "offset", "pagesize"} {
			if _, ok := request.URL.Query()[key]; !ok {
				http.Error(writer, "missing "+key, http.StatusBadRequest)
				return
			}
		}
		_, _ = writer.Write([]byte(`{"code":200,"data":{"data":[{"id":1,"jobGroup":3,"jobDesc":"示例任务","author":"admin"}],"offset":0,"pagesize":20,"total":1},"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/jobgroup/pageList", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if !authed(request) {
			_, _ = writer.Write([]byte(`{"code":500,"msg":"not login"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"code":200,"data":{"data":[{"id":3,"appname":"exec","title":"执行器","registryList":["http://127.0.0.1:9999"]}],"total":1},"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/jobinfo/insert", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = writer.Write([]byte(`{"code":200,"data":"2","msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/jobinfo/delete", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if request.URL.Query().Get("ids[]") == "" && request.PostForm.Get("ids[]") == "" && request.FormValue("ids[]") == "" {
			http.Error(writer, "missing ids", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`{"code":200,"data":null,"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/jobinfo/nextTriggerTime", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = writer.Write([]byte(`{"code":200,"data":["2026-10-22 01:00:00"],"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/joblog/logDetailCat", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = writer.Write([]byte(`{"code":200,"data":{"fromLineNum":1,"toLineNum":2,"logContent":"line 1\n","end":true},"msg":"Success","success":true}`))
	})
	mux.HandleFunc("/xxl-job-admin/chartInfo", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = writer.Write([]byte(`{"code":200,"data":{"triggerDayList":["2026-10-20"],"triggerDayCountSucList":[1],"triggerDayCountFailList":[0],"triggerDayCountRunningList":[0]},"msg":"Success","success":true}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func v3Lifecycle(server *httptest.Server) map[string]any {
	lifecycle := testLifecycle(server.URL)
	connection := lifecycle["connection"].(map[string]any)
	connection["external_config"] = map[string]any{"scheme": "http", "context_path": "/xxl-job-admin", "connect_timeout_secs": 10}
	return lifecycle
}

func TestV3ConsoleEndToEnd(t *testing.T) {
	server := newMockAdminV3(t)
	instance := &plugin{sessions: map[string]*Session{}}

	// Login must fall back to /auth/doLogin after the classic /login returns 404.
	if _, err := instance.dispatch("connection/connect", v3Lifecycle(server), noopNotify); err != nil {
		t.Fatalf("connect failed: %v", err.Message)
	}

	groups, err := instance.dispatch("xxljob/groups", map[string]any{"connectionId": "conn-1"}, noopNotify)
	if err != nil {
		t.Fatalf("groups failed: %v", err.Message)
	}
	if list := groups.(map[string]any)["groups"].([]json.RawMessage); len(list) != 1 {
		t.Fatalf("expected 1 group, got %d", len(list))
	}

	jobs, err := instance.dispatch("xxljob/jobs", map[string]any{"connectionId": "conn-1", "groupId": 0, "length": 20}, noopNotify)
	if err != nil {
		t.Fatalf("jobs failed: %v", err.Message)
	}
	if jobs.(map[string]any)["total"].(int) != 1 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}

	saved, err := instance.dispatch("xxljob/job/save", map[string]any{"connectionId": "conn-1", "job": map[string]any{
		"jobGroup": 3, "jobDesc": "v3 job", "author": "admin", "scheduleType": "NONE",
		"glueType": "BEAN", "executorHandler": "demoJobHandler",
	}}, noopNotify)
	if err != nil || saved.(map[string]any)["success"] != true {
		t.Fatalf("save failed: %v %+v", err, saved)
	}

	removed, err := instance.dispatch("xxljob/job/remove", map[string]any{"connectionId": "conn-1", "id": 2}, noopNotify)
	if err != nil || removed.(map[string]any)["success"] != true {
		t.Fatalf("remove failed: %v %+v", err, removed)
	}

	preview, err := instance.dispatch("xxljob/job/nextTriggerTime", map[string]any{"connectionId": "conn-1", "scheduleType": "CRON", "scheduleConf": "0 0 1 * * ?"}, noopNotify)
	if err != nil {
		t.Fatalf("nextTriggerTime failed: %v", err.Message)
	}
	if times := preview.(map[string]any)["times"].([]string); len(times) != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}

	dashboard, err := instance.dispatch("xxljob/dashboard", map[string]any{"connectionId": "conn-1", "days": 7}, noopNotify)
	if err != nil {
		t.Fatalf("dashboard failed: %v", err.Message)
	}
	chart := dashboard.(map[string]any)["chart"].(map[string]any)
	if chart["triggerDayCountSuc"] == nil || chart["triggerDayCountRunning"] == nil {
		t.Fatalf("chart keys were not normalized: %+v", chart)
	}
}

// TestV3PageQueryShape verifies the paging parameter names without a server.
func TestV3PageQueryShape(t *testing.T) {
	session := &Session{version: consoleV3}
	query := session.pageQuery(5, 50)
	if query.Get("offset") != "5" || query.Get("pagesize") != "50" || query.Get("start") != "" {
		t.Fatalf("unexpected v3 page query: %v", query)
	}
	session.version = consoleV2
	query = session.pageQuery(5, 50)
	if query.Get("start") != "5" || query.Get("length") != "50" || query.Get("offset") != "" {
		t.Fatalf("unexpected v2 page query: %v", query)
	}
}

// TestReturnTPayloadPrefersData ensures the 3.x `data` field is read when the
// 2.x `content` field is absent.
func TestReturnTPayloadPrefersData(t *testing.T) {
	var ret ReturnT
	if err := json.Unmarshal([]byte(`{"code":200,"data":["a"],"msg":"Success"}`), &ret); err != nil {
		t.Fatal(err)
	}
	if string(ret.payload()) != `["a"]` {
		t.Fatalf("payload should come from data: %s", ret.payload())
	}
}
