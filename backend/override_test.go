package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newGuardedConsole models any console whose auth the plugin does not implement:
// the business endpoints answer an SSO rejection unless the caller already holds
// a credential that `allow` recognises.
func newGuardedConsole(t *testing.T, allow func(*http.Request) bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	handler := func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if !allow(request) {
			_, _ = fmt.Fprint(writer, `{"code":500,"msg":"com.xxl.sso.core.exception.XxlSsoException: not login"}`)
			return
		}
		switch request.URL.Path {
		case "/jobgroup/pageList":
			_, _ = fmt.Fprint(writer, `{"recordsTotal":1,"recordsFiltered":1,"data":[{"id":1,"appname":"exec","title":"执行器","registryList":[]}]}`)
		case "/jobinfo/pageList":
			_, _ = fmt.Fprint(writer, `{"recordsTotal":1,"recordsFiltered":1,"data":[{"id":7,"jobGroup":1,"jobDesc":"任务","executorHandler":"h"}]}`)
		case "/jobinfo/start":
			_, _ = fmt.Fprint(writer, `{"code":200,"msg":null,"content":null}`)
		default:
			http.NotFound(writer, request)
		}
	}
	mux.HandleFunc("/", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func overrideLifecycle(server *httptest.Server, secrets map[string]string) map[string]any {
	lifecycle := testLifecycle(server.URL)
	connection := lifecycle["connection"].(map[string]any)
	connection["external_config"] = map[string]any{"scheme": "http", "context_path": "/", "connect_timeout_secs": 10}
	connection["connection_secrets"] = secrets
	return lifecycle
}

func TestSessionCookieOverrideConnects(t *testing.T) {
	server := newGuardedConsole(t, func(request *http.Request) bool {
		cookie, err := request.Cookie("XXL_SSO_SESSION")
		return err == nil && cookie.Value == "sso"
	})
	lifecycle := overrideLifecycle(server, map[string]string{"session_cookie": "XXL_SSO_SESSION=sso; other=1"})
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test failed: %v", err.Message)
	}
	if result.(map[string]any)["success"] != true {
		t.Fatalf("a replayed browser session did not connect: %+v", result)
	}
	if _, err := instance.dispatch("connection/connect", lifecycle, noopNotify); err != nil {
		t.Fatalf("connection/connect failed: %v", err.Message)
	}
	jobs, err := instance.dispatch("xxljob/jobs", map[string]any{"connectionId": "conn-1"}, noopNotify)
	if err != nil {
		t.Fatalf("the replayed session is not usable: %v", err.Message)
	}
	if jobs.(map[string]any)["total"].(int) != 1 {
		t.Fatalf("unexpected job page: %+v", jobs)
	}
}

func TestSessionCookieOverrideRejected(t *testing.T) {
	server := newGuardedConsole(t, func(request *http.Request) bool {
		cookie, err := request.Cookie("XXL_SSO_SESSION")
		return err == nil && cookie.Value == "sso"
	})
	lifecycle := overrideLifecycle(server, map[string]string{"session_cookie": "XXL_SSO_SESSION=expired"})
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test should report failure as a result: %v", err.Message)
	}
	message := result.(map[string]any)["message"].(string)
	if !strings.Contains(message, "连接凭证") {
		t.Fatalf("expected a credential hint, got: %s", message)
	}
}

func TestAuthHeaderOverrideConnects(t *testing.T) {
	server := newGuardedConsole(t, func(request *http.Request) bool {
		return request.Header.Get("Authorization") == "Bearer abc"
	})
	lifecycle := overrideLifecycle(server, map[string]string{"auth_header": "Authorization: Bearer abc"})
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test failed: %v", err.Message)
	}
	if result.(map[string]any)["success"] != true {
		t.Fatalf("an Authorization header did not connect: %+v", result)
	}
}

func TestAuthHeaderAcceptsBareToken(t *testing.T) {
	server := newGuardedConsole(t, func(request *http.Request) bool {
		return request.Header.Get("Authorization") == "Bearer abc"
	})
	lifecycle := overrideLifecycle(server, map[string]string{"auth_header": "abc"})
	instance := &plugin{sessions: map[string]*Session{}}
	result, err := instance.dispatch("connection/test", lifecycle, noopNotify)
	if err != nil {
		t.Fatalf("connection/test failed: %v", err.Message)
	}
	if result.(map[string]any)["success"] != true {
		t.Fatalf("a bare token was not sent as a Bearer header: %+v", result)
	}
}

func TestParseAuthHeader(t *testing.T) {
	cases := []struct {
		raw, name, value string
	}{
		{"Authorization: Bearer abc", "Authorization", "Bearer abc"},
		{"xxl-sso-token: t-1", "xxl-sso-token", "t-1"},
		{"abc", "Authorization", "Bearer abc"},
		{"", "", ""},
		{"bad header: v", "Authorization", "Bearer bad header: v"},
	}
	for _, testCase := range cases {
		name, value := parseAuthHeader(testCase.raw)
		if name != testCase.name || value != testCase.value {
			t.Fatalf("parseAuthHeader(%q) = (%q, %q), want (%q, %q)", testCase.raw, name, value, testCase.name, testCase.value)
		}
	}
}
