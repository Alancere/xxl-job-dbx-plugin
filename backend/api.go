package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// JobsRequest mirrors the filters the admin console's job list accepts.
// Status is the console's triggerStatus filter: -1 for every job, 0 for stopped
// jobs only, 1 for running jobs only.
type JobsRequest struct {
	GroupID     int
	Status      int
	SearchField string
	Keyword     string
	Start       int
	Length      int
}

func (session *Session) groupsPage(ctx context.Context, start, length int) (*Page, *SessionError) {
	query := session.pageQuery(start, length)
	query.Set("appname", "")
	query.Set("title", "")
	return session.getPage(ctx, "/jobgroup/pageList", query)
}

// Groups lists every executor group, including its registry address list.
func (session *Session) Groups(ctx context.Context) ([]json.RawMessage, *SessionError) {
	page, sessionErr := session.groupsPage(ctx, 0, 1000)
	if sessionErr != nil {
		return nil, sessionErr
	}
	return page.Data, nil
}

// Jobs lists one page of jobs with the console's own filters.
func (session *Session) Jobs(ctx context.Context, request JobsRequest) (map[string]any, *SessionError) {
	if request.Length <= 0 {
		request.Length = 20
	}
	if request.Length > 200 {
		request.Length = 200
	}
	if request.Start < 0 {
		request.Start = 0
	}
	groupID := request.GroupID
	if groupID <= 0 {
		groupID = -1
	}
	query := session.pageQuery(request.Start, request.Length)
	status := request.Status
	if status != 0 && status != 1 {
		status = -1
	}
	query.Set("jobGroup", strconv.Itoa(groupID))
	query.Set("triggerStatus", strconv.Itoa(status))
	// 3.x declares every text filter as a required request param, so all three
	// must be present even when only one carries the search keyword.
	keyword := strings.TrimSpace(request.Keyword)
	query.Set("jobDesc", "")
	query.Set("executorHandler", "")
	query.Set("author", "")
	switch request.SearchField {
	case "executorHandler":
		query.Set("executorHandler", keyword)
	case "author":
		query.Set("author", keyword)
	default:
		query.Set("jobDesc", keyword)
	}
	page, sessionErr := session.getPage(ctx, "/jobinfo/pageList", query)
	if sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{
		"total":    page.RecordsTotal,
		"filtered": page.RecordsFiltered,
		"jobs":     page.Data,
	}, nil
}

func (session *Session) jobPage(ctx context.Context, start, length int) (*Page, *SessionError) {
	query := session.pageQuery(start, length)
	query.Set("jobGroup", "-1")
	query.Set("triggerStatus", "-1")
	query.Set("jobDesc", "")
	query.Set("executorHandler", "")
	query.Set("author", "")
	return session.getPage(ctx, "/jobinfo/pageList", query)
}

// Job resolves one job definition. Newer consoles expose `detailById`; older
// ones only ship the paged list, so the lookup falls back to scanning pages.
func (session *Session) Job(ctx context.Context, id int) (map[string]any, *SessionError) {
	if id <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少任务 ID")
	}
	if raw, sessionErr := session.call(ctx, "GET", "/jobinfo/detailById?id="+strconv.Itoa(id), nil); sessionErr == nil {
		var ret ReturnT
		if json.Unmarshal(raw, &ret) == nil && len(ret.Content) > 0 && ret.Content[0] == '{' {
			if job, ok := decodeJob(ret.Content, id); ok {
				return job, nil
			}
		}
		if job, ok := decodeJob(raw, id); ok {
			return job, nil
		}
	}
	const pageSize = 500
	for pageIndex := 0; pageIndex < 40; pageIndex++ {
		page, sessionErr := session.jobPage(ctx, pageIndex*pageSize, pageSize)
		if sessionErr != nil {
			return nil, sessionErr
		}
		for _, rawJob := range page.Data {
			if job, ok := decodeJob(rawJob, id); ok {
				return job, nil
			}
		}
		if len(page.Data) < pageSize {
			break
		}
	}
	return nil, &SessionError{Code: errNotFound, Message: fmt.Sprintf("执行器上找不到 ID 为 %d 的任务", id)}
}

func decodeJob(raw json.RawMessage, id int) (map[string]any, bool) {
	var job map[string]any
	if json.Unmarshal(raw, &job) != nil {
		return nil, false
	}
	if intValue(job["id"]) != id {
		return nil, false
	}
	return job, true
}

// jobFormKeys is the field set `XxlJobInfo` binds on add/update. Sending the
// zero values keeps an edit from silently wiping fields the UI did not send.
var jobFormKeys = []string{
	"jobGroup", "jobDesc", "author", "alarmEmail", "scheduleType", "scheduleConf",
	"glueType", "executorHandler", "executorParam", "executorRouteStrategy",
	"childJobId", "misfireStrategy", "executorBlockStrategy", "executorTimeout",
	"executorFailRetryCount", "glueRemark",
}

// SaveJob creates or updates one job. `id > 0` means update.
func (session *Session) SaveJob(ctx context.Context, job map[string]any) (map[string]any, *SessionError) {
	if job == nil {
		return nil, newSessionError(errInvalidParams, "缺少任务内容")
	}
	id := intValue(job["id"])
	form := url.Values{}
	defaults := map[string]string{
		"author":                 session.username,
		"alarmEmail":             "",
		"scheduleType":           "NONE",
		"scheduleConf":           "",
		"glueType":               "BEAN",
		"executorParam":          "",
		"executorRouteStrategy":  "ROUND",
		"childJobId":             "",
		"misfireStrategy":        "DO_NOTHING",
		"executorBlockStrategy":  "SERIAL_EXECUTION",
		"executorTimeout":        "0",
		"executorFailRetryCount": "0",
		"glueRemark":             "",
	}
	for _, key := range jobFormKeys {
		value := defaults[key]
		if raw, ok := job[key]; ok && raw != nil {
			value = stringValue(raw)
		}
		form.Set(key, value)
	}
	if id > 0 {
		form.Set("id", strconv.Itoa(id))
	} else {
		// 3.x renders the GLUE editor from a non-null glueSource; seeding an
		// empty value keeps a freshly created GLUE job from tripping that page.
		form.Set("glueSource", "")
	}
	if err := validateJobForm(form); err != nil {
		return nil, err
	}
	path := "/jobinfo/add"
	if session.version == consoleV3 {
		path = "/jobinfo/insert"
	}
	message := "任务已创建"
	if id > 0 {
		path = "/jobinfo/update"
		message = "任务已保存"
	}
	if _, sessionErr := session.postReturn(ctx, path, form); sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"success": true, "message": message, "id": id}, nil
}

func validateJobForm(form url.Values) *SessionError {
	if intValue(form.Get("jobGroup")) <= 0 {
		return newSessionError(errInvalidParams, "请选择执行器分组")
	}
	if strings.TrimSpace(form.Get("jobDesc")) == "" {
		return newSessionError(errInvalidParams, "请填写任务描述")
	}
	if form.Get("glueType") == "BEAN" && strings.TrimSpace(form.Get("executorHandler")) == "" {
		return newSessionError(errInvalidParams, "BEAN 运行模式必须填写 JobHandler")
	}
	if form.Get("scheduleType") != "NONE" && strings.TrimSpace(form.Get("scheduleConf")) == "" {
		return newSessionError(errInvalidParams, "请填写调度配置（Cron 表达式或固定频率）")
	}
	return nil
}

func (session *Session) jobAction(ctx context.Context, path string, id int, successMessage string) (map[string]any, *SessionError) {
	if id <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少任务 ID")
	}
	form := url.Values{}
	if session.version == consoleV3 {
		// 3.x binds the id list as `ids[]`; the classic console takes a single `id`.
		form.Add("ids[]", strconv.Itoa(id))
	} else {
		form.Set("id", strconv.Itoa(id))
	}
	if _, sessionErr := session.postReturn(ctx, path, form); sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"success": true, "message": successMessage}, nil
}

func (session *Session) StartJob(ctx context.Context, id int) (map[string]any, *SessionError) {
	return session.jobAction(ctx, "/jobinfo/start", id, "任务已启动")
}

func (session *Session) StopJob(ctx context.Context, id int) (map[string]any, *SessionError) {
	return session.jobAction(ctx, "/jobinfo/stop", id, "任务已停止")
}

func (session *Session) RemoveJob(ctx context.Context, id int) (map[string]any, *SessionError) {
	path := "/jobinfo/remove"
	if session.version == consoleV3 {
		path = "/jobinfo/delete"
	}
	return session.jobAction(ctx, path, id, "任务已删除")
}

// TriggerJob runs one job immediately. An empty address list lets the console
// resolve addresses from the executor group's own registry.
func (session *Session) TriggerJob(ctx context.Context, id int, executorParam, addressList string) (map[string]any, *SessionError) {
	if id <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少任务 ID")
	}
	if strings.TrimSpace(addressList) == "" {
		if resolved, sessionErr := session.groupAddressList(ctx, id); sessionErr == nil {
			addressList = resolved
		}
	}
	form := url.Values{}
	form.Set("id", strconv.Itoa(id))
	form.Set("executorParam", executorParam)
	form.Set("addressList", addressList)
	if _, sessionErr := session.postReturn(ctx, "/jobinfo/trigger", form); sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"success": true, "message": "已触发一次执行", "addressList": addressList}, nil
}

// groupAddressList resolves the executor addresses registered for a job's group.
func (session *Session) groupAddressList(ctx context.Context, jobID int) (string, *SessionError) {
	job, sessionErr := session.Job(ctx, jobID)
	if sessionErr != nil {
		return "", sessionErr
	}
	groupID := intValue(job["jobGroup"])
	page, sessionErr := session.groupsPage(ctx, 0, 1000)
	if sessionErr != nil {
		return "", sessionErr
	}
	for _, rawGroup := range page.Data {
		var group map[string]any
		if json.Unmarshal(rawGroup, &group) != nil || intValue(group["id"]) != groupID {
			continue
		}
		addresses := make([]string, 0, 8)
		if registry, ok := group["registryList"].([]any); ok {
			for _, entry := range registry {
				if address := strings.TrimSpace(stringValue(entry)); address != "" {
					addresses = append(addresses, address)
				}
			}
		}
		if len(addresses) == 0 {
			for _, entry := range strings.Split(stringValue(group["addressList"]), ",") {
				if address := strings.TrimSpace(entry); address != "" {
					addresses = append(addresses, address)
				}
			}
		}
		return strings.Join(addresses, ","), nil
	}
	return "", nil
}

// NextTriggerTime previews the next fire times for a schedule definition.
func (session *Session) NextTriggerTime(ctx context.Context, scheduleType, scheduleConf string) (map[string]any, *SessionError) {
	form := url.Values{}
	form.Set("scheduleType", scheduleType)
	form.Set("scheduleConf", scheduleConf)
	ret, sessionErr := session.postReturn(ctx, "/jobinfo/nextTriggerTime", form)
	if sessionErr != nil {
		// The preview endpoint only exists from 2.2 onward; an older console
		// answers 404/405 and the UI simply hides the preview.
		if sessionErr.Code == errUpstream || sessionErr.Code == errNotFound {
			return map[string]any{"supported": false, "times": []string{}, "message": sessionErr.Message}, nil
		}
		return nil, sessionErr
	}
	times := make([]string, 0, 5)
	var list []string
	if payload := ret.payload(); len(payload) > 0 && json.Unmarshal(payload, &list) == nil {
		times = append(times, list...)
	}
	return map[string]any{"supported": true, "times": times}, nil
}

// Logs lists the scheduling log of a job (or of a whole group).
func (session *Session) Logs(ctx context.Context, groupID, jobID, status, start, length int, filterTime string) (map[string]any, *SessionError) {
	if length <= 0 {
		length = 20
	}
	if length > 200 {
		length = 200
	}
	if start < 0 {
		start = 0
	}
	if status != 1 && status != 2 && status != 3 && status != 0 {
		status = -1
	}
	if groupID <= 0 {
		groupID = -1
	}
	if jobID < 0 {
		jobID = 0
	}
	query := session.pageQuery(start, length)
	query.Set("jobGroup", strconv.Itoa(groupID))
	query.Set("jobId", strconv.Itoa(jobID))
	query.Set("logStatus", strconv.Itoa(status))
	query.Set("filterTime", filterTime)
	page, sessionErr := session.getPage(ctx, "/joblog/pageList", query)
	if sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"total": page.RecordsTotal, "filtered": page.RecordsFiltered, "logs": page.Data}, nil
}

// LogDetail streams one slice of an execution log, starting at a line number.
func (session *Session) LogDetail(ctx context.Context, logID, fromLine int) (map[string]any, *SessionError) {
	if logID <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少调度日志 ID")
	}
	if fromLine <= 0 {
		fromLine = 1
	}
	var raw []byte
	var sessionErr *SessionError
	if session.version == consoleV3 {
		// 3.x reads the rolling log over POST; 2.x answers a GET.
		form := url.Values{}
		form.Set("logId", strconv.Itoa(logID))
		form.Set("fromLineNum", strconv.Itoa(fromLine))
		raw, sessionErr = session.callRaw(ctx, "POST", "/joblog/logDetailCat", form)
	} else {
		query := url.Values{}
		query.Set("logId", strconv.Itoa(logID))
		query.Set("fromLineNum", strconv.Itoa(fromLine))
		raw, sessionErr = session.callRaw(ctx, "GET", "/joblog/logDetailCat?"+query.Encode(), nil)
	}
	if sessionErr != nil {
		return nil, sessionErr
	}
	type logDetailBody struct {
		FromLineNum int    `json:"fromLineNum"`
		ToLineNum   int    `json:"toLineNum"`
		LogContent  string `json:"logContent"`
		End         bool   `json:"end"`
	}
	var ret struct {
		Code    int             `json:"code"`
		Msg     json.RawMessage `json:"msg"`
		Content logDetailBody   `json:"content"`
		Data    logDetailBody   `json:"data"`
	}
	if err := json.Unmarshal(raw, &ret); err != nil {
		return nil, upstreamError("无法解析调度日志响应", raw)
	}
	if ret.Code != 200 {
		var message string
		_ = json.Unmarshal(ret.Msg, &message)
		// A failed trigger (no executor) leaves the console with nothing to
		// read: 2.4.1 answers `{"code":500,"msg":null}`, 3.x answers a
		// connection error to the executor.
		return nil, newSessionError(errUpstream, messageOrDefault(message,
			"管理端无法读取这条执行日志：该次调度没有路由到执行器（执行器地址为空或执行器不可达）"))
	}
	body := ret.Content
	if session.version == consoleV3 {
		body = ret.Data
	}
	return map[string]any{
		"fromLineNum": body.FromLineNum,
		"toLineNum":   body.ToLineNum,
		"logContent":  body.LogContent,
		"end":         body.End,
	}, nil
}

// KillLog terminates a running execution through the console's kill endpoint.
func (session *Session) KillLog(ctx context.Context, logID int) (map[string]any, *SessionError) {
	if logID <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少调度日志 ID")
	}
	form := url.Values{}
	form.Set("id", strconv.Itoa(logID))
	if _, sessionErr := session.postReturn(ctx, "/joblog/logKill", form); sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"success": true, "message": "已发送终止指令"}, nil
}

// glueSourcePattern pulls the GLUE editor's initial source out of the console's
// jobcode page. Older consoles have no JSON endpoint for it. 2.x renders the
// source in `<textarea id="glueSource">`, 3.x seeds a CodeMirror editor from
// `<textarea id="version_now">`.
var (
	glueSourcePattern   = regexp.MustCompile(`(?is)id\s*=\s*"glueSource"[^>]*>(.*?)</textarea>`)
	glueSourcePatternV3 = regexp.MustCompile(`(?is)id\s*=\s*"version_now"[^>]*>(.*?)</textarea>`)
)

// GlueSource reads the GLUE source of a job; `supported` is false when the
// console's page shape is unknown, and the UI then hides the editor.
func (session *Session) GlueSource(ctx context.Context, jobID int) (map[string]any, *SessionError) {
	if jobID <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少任务 ID")
	}
	param := "id"
	if session.version == consoleV3 {
		param = "jobId"
	}
	raw, sessionErr := session.callWith(ctx, "GET", "/jobcode?"+param+"="+strconv.Itoa(jobID), nil, true)
	if sessionErr != nil {
		// Authentication and transport failures must surface; a console whose
		// page shape simply changed only disables the editor.
		if sessionErr.Code == errAuth || sessionErr.Code == errNetwork {
			return nil, sessionErr
		}
		return map[string]any{"supported": false, "source": "", "message": sessionErr.Message}, nil
	}
	pattern := glueSourcePattern
	if session.version == consoleV3 {
		pattern = glueSourcePatternV3
	}
	match := pattern.FindSubmatch(raw)
	if len(match) < 2 {
		return map[string]any{"supported": false, "source": "", "message": "当前管理端版本的 GLUE 页面无法解析，请使用管理端网页编辑"}, nil
	}
	return map[string]any{"supported": true, "source": html.UnescapeString(string(match[1]))}, nil
}

// SaveGlueSource writes the GLUE source back to the job. `glueRemark` is the
// source remark the console requires on every save; it defaults when the
// workbench did not supply one.
func (session *Session) SaveGlueSource(ctx context.Context, jobID int, glueType, source, glueRemark string) (map[string]any, *SessionError) {
	if jobID <= 0 {
		return nil, newSessionError(errInvalidParams, "缺少任务 ID")
	}
	if strings.TrimSpace(glueType) == "" {
		return nil, newSessionError(errInvalidParams, "缺少运行模式")
	}
	if strings.TrimSpace(glueRemark) == "" {
		glueRemark = "DBX更新"
	}
	form := url.Values{}
	form.Set("id", strconv.Itoa(jobID))
	form.Set("glueType", glueType)
	form.Set("glueSource", source)
	form.Set("glueRemark", glueRemark)
	if _, sessionErr := session.postReturn(ctx, "/jobcode/save", form); sessionErr != nil {
		return nil, sessionErr
	}
	return map[string]any{"success": true, "message": "GLUE 源码已保存"}, nil
}

// ChartInfo reads the console's trigger statistics; it exists from 2.1 onward.
func (session *Session) ChartInfo(ctx context.Context, startDate, endDate string) (map[string]any, *SessionError) {
	var raw []byte
	var sessionErr *SessionError
	if session.version == consoleV3 {
		// 3.x answers the chart over POST and expects a full datetime range.
		form := url.Values{}
		form.Set("startDate", startDate+" 00:00:00")
		form.Set("endDate", endDate+" 23:59:59")
		raw, sessionErr = session.call(ctx, "POST", "/chartInfo", form)
	} else {
		query := url.Values{}
		query.Set("startDate", startDate)
		query.Set("endDate", endDate)
		raw, sessionErr = session.call(ctx, "GET", "/chartInfo?"+query.Encode(), nil)
	}
	if sessionErr != nil {
		return nil, sessionErr
	}
	var ret ReturnT
	if err := json.Unmarshal(raw, &ret); err != nil || ret.Code != 200 {
		return nil, upstreamError("无法解析运行报表响应", raw)
	}
	var content map[string]any
	if payload := ret.payload(); len(payload) > 0 && string(payload) != "null" {
		if err := json.Unmarshal(payload, &content); err != nil {
			return nil, upstreamError("无法解析运行报表内容", raw)
		}
	}
	if content == nil {
		content = map[string]any{}
	}
	if session.version == consoleV3 {
		normalizeChartKeys(content)
	}
	return content, nil
}

// normalizeChartKeys maps the 3.x chart field names onto the 2.x names the
// workbench renders, so one UI handles both console generations.
func normalizeChartKeys(chart map[string]any) {
	for _, pair := range [][2]string{
		{"triggerDayCountSucList", "triggerDayCountSuc"},
		{"triggerDayCountFailList", "triggerDayCountFail"},
		{"triggerDayCountRunningList", "triggerDayCountRunning"},
	} {
		if value, ok := chart[pair[0]]; ok {
			chart[pair[1]] = value
		}
	}
}

// Dashboard combines the console's job counters with its trigger chart.
func (session *Session) Dashboard(ctx context.Context, days int) (map[string]any, *SessionError) {
	if days <= 0 || days > 30 {
		days = 7
	}
	groups, sessionErr := session.Groups(ctx)
	if sessionErr != nil {
		return nil, sessionErr
	}
	query := session.pageQuery(0, 1)
	query.Set("jobGroup", "-1")
	query.Set("triggerStatus", "-1")
	query.Set("jobDesc", "")
	query.Set("executorHandler", "")
	query.Set("author", "")
	all, sessionErr := session.getPage(ctx, "/jobinfo/pageList", query)
	if sessionErr != nil {
		return nil, sessionErr
	}
	runningQuery := url.Values{}
	for key, values := range query {
		runningQuery[key] = append([]string(nil), values...)
	}
	runningQuery.Set("triggerStatus", "1")
	running, sessionErr := session.getPage(ctx, "/jobinfo/pageList", runningQuery)
	if sessionErr != nil {
		running = &Page{}
	}
	end := time.Now()
	startTime := end.AddDate(0, 0, -(days - 1))
	chart, chartErr := session.ChartInfo(ctx, startTime.Format("2006-01-02"), end.Format("2006-01-02"))
	dashboard := map[string]any{
		"baseUrl":          session.baseURL,
		"requestedBaseUrl": session.requestedBaseURL,
		"autoDetected":     session.autoDetected,
		"stats": map[string]any{
			"groups":  len(groups),
			"jobs":    all.RecordsTotal,
			"running": running.RecordsTotal,
			"stopped": all.RecordsTotal - running.RecordsTotal,
		},
		"groups": groups,
		"days":   days,
	}
	if chartErr == nil {
		dashboard["chart"] = chart
	} else {
		dashboard["chart"] = nil
		dashboard["chartUnavailable"] = chartErr.Message
	}
	return dashboard, nil
}
