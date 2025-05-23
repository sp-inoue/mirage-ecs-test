package mirageecs

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/samber/lo"
)

var DNSNameRegexpWithPattern = regexp.MustCompile(`^[a-zA-Z*?\[\]][a-zA-Z0-9-*?\[\]]{0,61}[a-zA-Z0-9*?\[\]]$`)

const PurgeMinimumDuration = 5 * time.Minute

const APICallTimeout = 30 * time.Second

type WebApi struct {
	*echo.Echo

	cfg    *Config
	runner TaskRunner
	mu     *sync.Mutex
}

type Template struct {
	templates *template.Template
}

func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	if m, ok := data.(map[string]interface{}); ok {
		m["Version"] = Version
		return t.templates.ExecuteTemplate(w, name, m)
	} else {
		return t.templates.ExecuteTemplate(w, name, data)
	}
}

func NewWebApi(cfg *Config, runner TaskRunner) *WebApi {
	app := &WebApi{
		mu:     &sync.Mutex{},
		runner: runner,
	}
	app.cfg = cfg

	e := echo.New()
	e.Use(middleware.Logger())

	web := e.Group("")
	web.Use(cfg.AuthMiddlewareForWeb)
	web.GET("/", app.Top)
	web.GET("/list", app.List)
	web.GET("/launcher", app.Launcher)
	web.GET("/trace/:taskid", app.Trace)
	web.POST("/launch", app.Launch)
	web.POST("/terminate", app.Terminate)

	api := e.Group("/api")
	api.Use(cfg.CompatMiddlewareForAPI)
	api.Use(cfg.AuthMiddlewareForAPI)
	api.GET("/list", app.ApiList)
	api.GET("/access", app.ApiAccess)
	api.GET("/logs", app.ApiLogs)
	api.POST("/launch", app.ApiLaunch)
	api.POST("/terminate", app.ApiTerminate)
	api.POST("/purge", app.ApiPurge)

	e.Renderer = &Template{
		templates: template.Must(template.ParseGlob(cfg.HtmlDir + "/*")),
	}
	app.Echo = e

	return app
}

func (api *WebApi) Top(c echo.Context) error {
	return c.Render(http.StatusOK, "layout.html", map[string]interface{}{})
}

func (api *WebApi) List(c echo.Context) error {
	ctx := c.Request().Context()
	infoRunning, err := api.runner.List(ctx, statusRunning)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	infoStopped, err := api.runner.List(ctx, statusStopped)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	sort.Slice(infoStopped, func(i, j int) bool {
		return infoStopped[i].Created.Before(infoStopped[j].Created)
	})
	// stopped subdomains shows only one
	stoppedSubdomains := make(map[string]struct{}, len(infoStopped))
	infoStopped = lo.Filter(infoStopped, func(info *Information, _ int) bool {
		if _, ok := stoppedSubdomains[info.SubDomain]; ok {
			// already seen
			return false
		}
		stoppedSubdomains[info.SubDomain] = struct{}{}
		return true
	})
	info := append(infoRunning, infoStopped...)
	value := map[string]interface{}{
		"info":  info,
		"error": err,
	}
	return c.Render(http.StatusOK, "list.html", value)
}

func (api *WebApi) Launcher(c echo.Context) error {
	var taskdefs []string
	if api.cfg.Link.DefaultTaskDefinitions != nil {
		taskdefs = api.cfg.Link.DefaultTaskDefinitions
	} else {
		taskdefs = []string{api.cfg.ECS.DefaultTaskDefinition}
	}
	return c.Render(http.StatusOK, "launcher.html", map[string]interface{}{
		"DefaultTaskDefinitions": taskdefs,
		"Parameters":             api.cfg.Parameter,
	})
}

func (api *WebApi) Launch(c echo.Context) error {
	code, err := api.launch(c)
	if err != nil {
		return c.String(code, err.Error())
	}
	if c.Request().Header.Get("Hx-Request") == "true" {
		return c.String(code, "ok")
	}
	return c.Redirect(http.StatusSeeOther, "/")
}

func (api *WebApi) Terminate(c echo.Context) error {
	code, err := api.terminate(c)
	if err != nil {
		c.String(code, err.Error())
	}
	return c.Redirect(http.StatusSeeOther, "/")
}

func (api *WebApi) Trace(c echo.Context) error {
	taskID := c.Param("taskid")
	if taskID == "" {
		return c.String(http.StatusBadRequest, "taskid required")
	}
	trace, err := api.runner.Trace(c.Request().Context(), taskID)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	return c.String(http.StatusOK, trace)
}

func (api *WebApi) ApiList(c echo.Context) error {
	info, err := api.runner.List(c.Request().Context(), statusRunning)
	if err != nil {
		return c.JSON(500, APIListResponse{})
	}
	return c.JSON(200, APIListResponse{Result: info})
}

func (api *WebApi) ApiLaunch(c echo.Context) error {
	code, err := api.launch(c)
	if err != nil {
		return c.JSON(code, APICommonResponse{Result: err.Error()})
	}
	return c.JSON(code, APICommonResponse{Result: "ok"})
}

func (api *WebApi) launch(c echo.Context) (int, error) {
	r := APILaunchRequest{}
	ps, _ := c.FormParams()
	r.MergeForm(ps)
	if err := c.Bind(&r); err != nil {
		return http.StatusBadRequest, err
	}

	subdomain := r.Subdomain
	subdomain = strings.ToLower(subdomain)
	if err := validateSubdomain(subdomain); err != nil {
		slog.Error(f("launch failed: %s", err))
		return http.StatusBadRequest, err
	}
	taskdefs := r.Taskdef
	parameter, err := api.LoadParameter(r.GetParameter)
	if err != nil {
		slog.Error(f("failed to load parameter: %s", err))
		return http.StatusBadRequest, err
	}

	if subdomain == "" || len(taskdefs) == 0 {
		return http.StatusBadRequest, fmt.Errorf("parameter required: subdomain=%s, taskdef=%v", subdomain, taskdefs)
	} else {
		ctx, cancel := context.WithTimeout(c.Request().Context(), APICallTimeout)
		defer cancel()
		err := api.runner.Launch(ctx, subdomain, parameter, taskdefs...)
		if err != nil {
			slog.Error(f("launch failed: %s", err))
			return http.StatusInternalServerError, err
		}
	}
	return http.StatusOK, nil
}

func (api *WebApi) ApiLogs(c echo.Context) error {
	code, logs, err := api.logs(c)
	if err != nil {
		return c.JSON(code, APICommonResponse{Result: err.Error()})
	}
	return c.JSON(code, APILogsResponse{Result: logs})
}

func (api *WebApi) ApiTerminate(c echo.Context) error {
	code, err := api.terminate(c)
	if err != nil {
		return c.JSON(code, APICommonResponse{Result: err.Error()})
	}
	return c.JSON(code, APICommonResponse{Result: "ok"})
}

func (api *WebApi) ApiAccess(c echo.Context) error {
	code, sum, duration, err := api.accessCounter(c)
	if err != nil {
		return c.JSON(code, APICommonResponse{Result: err.Error()})
	}
	return c.JSON(code, APIAccessResponse{Result: "ok", Sum: sum, Duration: duration})
}

func (api *WebApi) ApiPurge(c echo.Context) error {
	r := APIPurgeRequest{}
	if err := c.Bind(&r); err != nil {
		return c.JSON(http.StatusBadRequest, APICommonResponse{Result: err.Error()})
	}

	params, err := r.Validate()
	if err != nil {
		slog.Error(f("purge failed: %s", err))
		return c.JSON(http.StatusBadRequest, APICommonResponse{Result: err.Error()})
	}

	ctx := c.Request().Context()
	if err := api.purge(ctx, params); err != nil {
		return c.JSON(http.StatusInternalServerError, APICommonResponse{Result: err.Error()})
	}
	return c.JSON(http.StatusOK, APICommonResponse{Result: "accepted"})
}

func (api *WebApi) logs(c echo.Context) (int, []string, error) {
	subdomain := c.QueryParam("subdomain")
	since := c.QueryParam("since")
	tail := c.QueryParam("tail")

	if subdomain == "" {
		return http.StatusBadRequest, nil, fmt.Errorf("parameter required: subdomain")
	}

	var sinceTime time.Time
	if since != "" {
		var err error
		sinceTime, err = time.Parse(time.RFC3339, since)
		if err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("cannot parse since: %s", err)
		}
	}
	var tailN int
	if tail != "" {
		if tail == "all" {
			tailN = 0
		} else if n, err := strconv.Atoi(tail); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("cannot parse tail: %s", err)
		} else {
			tailN = n
		}
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), APICallTimeout)
	defer cancel()
	logs, err := api.runner.Logs(ctx, subdomain, sinceTime, tailN)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	return http.StatusOK, logs, nil
}

func (api *WebApi) terminate(c echo.Context) (int, error) {
	r := APITerminateRequest{}
	if err := c.Bind(&r); err != nil {
		return http.StatusBadRequest, err
	}
	id := r.ID
	subdomain := r.Subdomain

	ctx, cancel := context.WithTimeout(c.Request().Context(), APICallTimeout)
	defer cancel()
	if id != "" {
		if err := api.runner.Terminate(ctx, id); err != nil {
			return http.StatusInternalServerError, err
		}
	} else if subdomain != "" {
		if err := api.runner.TerminateBySubdomain(ctx, subdomain); err != nil {
			return http.StatusInternalServerError, err
		}
	} else {
		return http.StatusBadRequest, fmt.Errorf("parameter required: id or subdomain")
	}
	return http.StatusOK, nil
}

func (api *WebApi) accessCounter(c echo.Context) (int, int64, int64, error) {
	subdomain := c.QueryParam("subdomain")
	duration := c.QueryParam("duration")
	durationInt, _ := strconv.ParseInt(duration, 10, 64)
	if durationInt == 0 {
		durationInt = 86400 // 24 hours
	}
	d := time.Duration(durationInt) * time.Second
	sum, err := api.runner.GetAccessCount(c.Request().Context(), subdomain, d)
	if err != nil {
		slog.Error(f("access counter failed: %s", err))
		return http.StatusInternalServerError, 0, durationInt, err
	}
	return http.StatusOK, sum, durationInt, nil
}

func (api *WebApi) LoadParameter(getFunc func(string) string) (TaskParameter, error) {
	parameter := make(TaskParameter)

	for _, v := range api.cfg.Parameter {
		param := getFunc(v.Name)
		if param == "" && v.Default != "" {
			param = v.Default
		}
		if param == "" && v.Required {
			return nil, fmt.Errorf("lack require parameter: %s", v.Name)
		} else if param == "" {
			continue
		}

		if v.Rule != "" {
			if !v.Regexp.MatchString(param) {
				return nil, fmt.Errorf("parameter %s value is rule error", v.Name)
			}
		}
		if utf8.RuneCountInString(param) > 255 {
			return nil, fmt.Errorf("parameter %s value is too long(max 255 unicode characters)", v.Name)
		}
		parameter[v.Name] = param
	}

	return parameter, nil
}

func validateSubdomain(s string) error {
	if s == "" {
		return fmt.Errorf("subdomain is empty")
	}
	if len(s) < 2 {
		return fmt.Errorf("subdomain is too short")
	}
	if len(s) > 63 {
		return fmt.Errorf("subdomain is too long")
	}
	if !DNSNameRegexpWithPattern.MatchString(s) {
		return fmt.Errorf("subdomain %s includes invalid characters", s)
	}
	if _, err := path.Match(s, "x"); err != nil {
		return err
	}
	return nil
}

func (api *WebApi) purge(ctx context.Context, p *PurgeParams) error {
	infos, err := api.runner.List(ctx, statusRunning)
	if err != nil {
		slog.Error(f("list ecs failed: %s", err))
		return fmt.Errorf("list tasks failed: %w", err)
	}
	slog.Info("purge subdomains",
		"duration", p.Duration,
		"excludes", p.Excludes,
		"exclude_tags", p.ExcludeTags,
		"exclude_regexp", p.ExcludeRegexp,
	)
	terminates := []string{}
	for _, info := range infos {
		if info.ShouldBePurged(p) {
			terminates = append(terminates, info.SubDomain)
		}
	}
	terminates = lo.Uniq(terminates)
	if len(terminates) > 0 {
		slog.Info(f("purge %d subdomains", len(terminates)))
		// running in background. Don't cancel by client context.
		go api.purgeSubdomains(context.Background(), terminates, p.Duration)
	}

	slog.Info("no subdomains to purge")
	return nil
}

func (api *WebApi) purgeSubdomains(ctx context.Context, subdomains []string, duration time.Duration) {
	if api.mu.TryLock() {
		defer api.mu.Unlock()
	} else {
		slog.Info("skip purge subdomains, another purge is running")
		return
	}
	slog.Info(f("start purge subdomains %d", len(subdomains)))
	purged := 0
	for _, subdomain := range subdomains {
		sum, err := api.runner.GetAccessCount(ctx, subdomain, duration)
		if err != nil {
			slog.Warn(f("access count failed: %s %s", subdomain, err))
			continue
		}
		if sum > 0 {
			slog.Info(f("skip purge %s %d access", subdomain, sum))
			continue
		}
		if err := api.runner.TerminateBySubdomain(ctx, subdomain); err != nil {
			slog.Warn(f("terminate failed %s %s", subdomain, err))
		} else {
			purged++
			slog.Info(f("purged %s", subdomain))
		}
		time.Sleep(3 * time.Second)
	}
	slog.Info(f("purge %d subdomains completed", purged))
}
