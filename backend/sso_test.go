package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newSsoConsole models a deployment whose form login was replaced by XXL-SSO:
// `POST /api/login` exchanges an access token for a session cookie, the classic
// `/login` mapping does not exist, and the business endpoints insist on both the
// cookie and the XXL-JOB-ACCESS-TOKEN header.
func newSsoConsole(t *testing.T, accessToken string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var pageCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			AccessToken string `json:"accessToken"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if payload.AccessToken != accessToken {
			_, _ = fmt.Fprint(writer, `{"code":500,"msg":"The access token is wrong."}`)
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: "XXL_SSO_SESSION", Value: "sso", Path: "/"})
		_, _ = fmt.Fprint(writer, `{"code":200,"msg":"ok","data":null}`)
	})
	mux.HandleFunc("/jobinfo/pageList", func(writer http.ResponseWriter, request *http.Request) {
		pageCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		cookie, cookieErr := request.Cookie("XXL_SSO_SESSION")
		if cookieErr != nil || cookie.Value != "sso" || request.Header.Get(accessTokenHeader) != accessToken {
			_, _ = fmt.Fprint(writer, `{"code":500,"msg":"com.xxl.sso.core.exception.XxlSsoException: not login"}`)
			return
		}
		_, _ = fmt.Fprint(writer, `{"recordsTotal":0,"recordsFiltered":0,"data":[]}`)
	})
	mux.HandleFunc("/jobgroup/pageList", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = fmt.Fprint(writer, `{"recordsTotal":1,"recordsFiltered":1,"data":[{"id":1,"appname":"sso-executor","title":"SSO 执行器","addressType":0,"registryList":["http://127.0.0.1:9999/"]}]}`)
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		http.NotFound(writer, request)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &pageCalls
}

func ssoLifecycle(server *httptest.Server, accessToken string) map[string]any {
	lifecycle := testLifecycle(server.URL)
	connection := lifecycle["connection"].(map[string]any)
	connection["external_config"] = map[string]any{"scheme": "http", "context_path": "/", "connect_timeout_secs": 10}
	connection["connection_secrets"] = map[string]string{"access_token": accessToken}
	return lifecycle
}

func TestTokenLoginConnectsSsoConsole(t *testing.T) {
	server, pageCalls := newSsoConsole(t, "s3cret")
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", ssoLifecycle(server, "s3cret"), noopNotify)
	if err != nil {
		t.Fatalf("connection/test failed: %v", err.Message)
	}
	testResult := result.(map[string]any)
	if testResult["success"] != true {
		t.Fatalf("access token login did not connect: %+v", testResult)
	}
	if pageCalls.Load() == 0 {
		t.Fatal("the console was never asked for a job page")
	}

	if _, err := instance.dispatch("connection/connect", ssoLifecycle(server, "s3cret"), noopNotify); err != nil {
		t.Fatalf("connection/connect failed: %v", err.Message)
	}
	jobs, err := instance.dispatch("xxljob/jobs", map[string]any{"connectionId": "conn-1", "length": 20}, noopNotify)
	if err != nil {
		t.Fatalf("the SSO session is not usable: %v", err.Message)
	}
	if jobs.(map[string]any)["total"].(int) != 0 {
		t.Fatalf("unexpected job page: %+v", jobs)
	}
}

func TestTokenLoginRejectsWrongToken(t *testing.T) {
	server, _ := newSsoConsole(t, "s3cret")
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", ssoLifecycle(server, "wrong-token"), noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result: %v", err.Message)
	}
	message := result.(map[string]any)["message"].(string)
	if !strings.Contains(message, "token") {
		t.Fatalf("expected the token rejection to be reported, got: %s", message)
	}
	if strings.Contains(message, "上下文路径") {
		t.Fatalf("a rejected token must not be reported as a path problem: %s", message)
	}
}

func TestTokenLoginIsSkippedWithoutToken(t *testing.T) {
	server, _ := newSsoConsole(t, "s3cret")
	instance := &plugin{sessions: map[string]*Session{}}
	lifecycle := ssoLifecycle(server, "")
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result: %v", err.Message)
	}
	if result.(map[string]any)["success"] != false {
		t.Fatalf("an SSO console must not connect without a token: %+v", result)
	}
}

// TestInspectEndpointExtractsSpaRoutes proves the diagnostic reads the console's
// own bundle when the classic endpoints are absent.
func TestInspectEndpointExtractsSpaRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/assets/app.js":
			writer.Header().Set("Content-Type", "application/javascript")
			_, _ = fmt.Fprint(writer, `
				const login = (token) => fetch("/api/login", {method:"POST", headers:{Authorization:"Bearer "+token}});
				const jobs = () => fetch("/jobinfo/pageList?start=0&length=10");
				const t = localStorage.getItem("accessToken");
			`)
		case strings.HasPrefix(request.URL.Path, "/api/"):
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"code":500,"msg":"The access token is wrong."}`)
		default:
			writer.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = fmt.Fprint(writer, `<html><head><title>XXL-JOB</title><script src="/assets/app.js"></script></head><body><div id="app"></div></body></html>`)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	lifecycle := testLifecycle(server.URL)
	lifecycle["connection"].(map[string]any)["external_config"] = map[string]any{"scheme": "http", "context_path": "/"}
	lifecycle["action"] = map[string]any{"id": "inspect-endpoint"}
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/action", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/action failed: %v", err.Message)
	}
	report := result.(map[string]any)["message"].(string)
	for _, expected := range []string{"前端资源", "/assets/app.js", "/api/login", "/jobinfo/pageList", "认证线索", "accessToken"} {
		if !strings.Contains(report, expected) {
			t.Fatalf("report is missing %q:\n%s", expected, report)
		}
	}
}
