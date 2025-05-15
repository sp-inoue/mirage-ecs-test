package mirageecs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// APIListResponse is a response of /api/list
type APIListResponse struct {
	Result []*APITaskInfo `json:"result"`
}

type APITaskInfo = Information

// APILaunchResponse is a response of /api/launch, and /api/terminate
type APICommonResponse struct {
	Result string `json:"result"`
}

type APILogsResponse struct {
	Result []string `json:"result"`
}

// APIAccessResponse is a response of /api/access
type APIAccessResponse struct {
	Result   string `json:"result"`
	Duration int64  `json:"duration"`
	Sum      int64  `json:"sum"`
}

type APILaunchRequest struct {
	Subdomain  string            `json:"subdomain" form:"subdomain"`
	Branch     string            `json:"branch" form:"branch"`
	Taskdef    []string          `json:"taskdef" form:"taskdef"`
	Parameters map[string]string `json:"parameters" form:"parameters"`
}

func (r *APILaunchRequest) GetParameter(key string) string {
	if key == "branch" {
		return r.Branch
	}
	return r.Parameters[key]
}

func (r *APILaunchRequest) MergeForm(form url.Values) {
	if r.Parameters == nil {
		r.Parameters = make(map[string]string, len(form))
	}
	for key, values := range form {
		if key == "branch" || key == "subdomain" || key == "taskdef" {
			continue
		}
		r.Parameters[key] = values[0]
	}
}

type APIPurgeRequest struct {
	Duration      json.Number `json:"duration" form:"duration" yaml:"duration"`
	Excludes      []string    `json:"excludes" form:"excludes" yaml:"excludes"`
	ExcludeTags   []string    `json:"exclude_tags" form:"exclude_tags" yaml:"exclude_tags"`
	ExcludeRegexp string      `json:"exclude_regexp" form:"exclude_regexp" yaml:"exclude_regexp"`
}

type PurgeParams struct {
	Duration      time.Duration
	Excludes      []string
	ExcludeTags   []string
	ExcludeRegexp *regexp.Regexp

	excludesMap    map[string]struct{}
	excludeTagsMap map[string]string
}

func (r *APIPurgeRequest) Validate() (*PurgeParams, error) {
	excludes := r.Excludes
	excludeTags := r.ExcludeTags
	di, err := r.Duration.Int64()
	if err != nil {
		return nil, fmt.Errorf("invalid duration %s", r.Duration)
	}
	minimum := int64(PurgeMinimumDuration.Seconds())
	if di < minimum {
		return nil, fmt.Errorf("invalid duration %d (at least %d)", di, minimum)
	}

	excludesMap := make(map[string]struct{}, len(excludes))
	for _, exclude := range excludes {
		excludesMap[exclude] = struct{}{}
	}
	excludeTagsMap := make(map[string]string, len(excludeTags))
	for _, excludeTag := range excludeTags {
		p := strings.SplitN(excludeTag, ":", 2)
		if len(p) != 2 {
			return nil, fmt.Errorf("invalid exclude_tags format %s", excludeTag)
		}
		k, v := p[0], p[1]
		excludeTagsMap[k] = v
	}
	var excludeRegexp *regexp.Regexp
	if r.ExcludeRegexp != "" {
		var err error
		excludeRegexp, err = regexp.Compile(r.ExcludeRegexp)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude_regexp %s", r.ExcludeRegexp)
		}
	}
	duration := time.Duration(di) * time.Second

	return &PurgeParams{
		Duration:      duration,
		Excludes:      excludes,
		ExcludeTags:   excludeTags,
		ExcludeRegexp: excludeRegexp,

		excludesMap:    excludesMap,
		excludeTagsMap: excludeTagsMap,
	}, nil
}

type APITerminateRequest struct {
	ID        string `json:"id" form:"id"`
	Subdomain string `json:"subdomain" form:"subdomain"`
}
