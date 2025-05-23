package mirageecs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv2Config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	metadata "github.com/brunoscheufler/aws-ecs-metadata-go"
	config "github.com/kayac/go-config"
	"github.com/labstack/echo/v4"
)

var DefaultParameter = &Parameter{
	Name:     "branch",
	Env:      "GIT_BRANCH",
	Rule:     "",
	Required: true,
	Default:  "",
}

type Config struct {
	Host      Host       `yaml:"host"`
	Listen    Listen     `yaml:"listen"`
	Network   Network    `yaml:"network"`
	HtmlDir   string     `yaml:"htmldir"`
	Parameter Parameters `yaml:"parameters"`
	ECS       ECSCfg     `yaml:"ecs"`
	Link      Link       `yaml:"link"`
	Auth      *Auth      `yaml:"auth"`
	Purge     *Purge     `yaml:"purge"`

	compatV1  bool
	localMode bool
	awscfg    *aws.Config
	cleanups  []func() error
}

type ECSCfg struct {
	Region                   string                   `yaml:"region"`
	Cluster                  string                   `yaml:"cluster"`
	CapacityProviderStrategy CapacityProviderStrategy `yaml:"capacity_provider_strategy"`
	LaunchType               *string                  `yaml:"launch_type"`
	NetworkConfiguration     *NetworkConfiguration    `yaml:"network_configuration"`
	DefaultTaskDefinition    string                   `yaml:"default_task_definition"`
	EnableExecuteCommand     *bool                    `yaml:"enable_execute_command"`

	capacityProviderStrategy []types.CapacityProviderStrategyItem `yaml:"-"`
	networkConfiguration     *types.NetworkConfiguration          `yaml:"-"`
}

func (c ECSCfg) String() string {
	m := map[string]interface{}{
		"region":                     c.Region,
		"cluster":                    c.Cluster,
		"capacity_provider_strategy": c.capacityProviderStrategy,
		"launch_type":                c.LaunchType,
		"network_configuration":      c.networkConfiguration,
		"default_task_definition":    c.DefaultTaskDefinition,
		"enable_execute_command":     c.EnableExecuteCommand,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (c ECSCfg) validate() error {
	if c.Region == "" {
		return fmt.Errorf("region is required")
	}
	if c.Cluster == "" {
		return fmt.Errorf("cluster is required")
	}
	if c.LaunchType == nil && c.capacityProviderStrategy == nil {
		return fmt.Errorf("launch_type or capacity_provider_strategy is required")
	}
	if c.networkConfiguration == nil {
		return fmt.Errorf("network_configuration is required")
	}
	return nil
}

type CapacityProviderStrategy []*CapacityProviderStrategyItem

func (s CapacityProviderStrategy) toSDK() []types.CapacityProviderStrategyItem {
	if len(s) == 0 {
		return nil
	}
	var items []types.CapacityProviderStrategyItem
	for _, item := range s {
		items = append(items, item.toSDK())
	}
	return items
}

type CapacityProviderStrategyItem struct {
	CapacityProvider *string `yaml:"capacity_provider"`
	Weight           int32   `yaml:"weight"`
	Base             int32   `yaml:"base"`
}

func (i CapacityProviderStrategyItem) toSDK() types.CapacityProviderStrategyItem {
	return types.CapacityProviderStrategyItem{
		CapacityProvider: i.CapacityProvider,
		Weight:           i.Weight,
		Base:             i.Base,
	}
}

type NetworkConfiguration struct {
	AwsVpcConfiguration *AwsVpcConfiguration `yaml:"awsvpc_configuration"`
}

func (c *NetworkConfiguration) toSDK() *types.NetworkConfiguration {
	if c == nil {
		return nil
	}
	return &types.NetworkConfiguration{
		AwsvpcConfiguration: c.AwsVpcConfiguration.toSDK(),
	}
}

type AwsVpcConfiguration struct {
	AssignPublicIp string   `yaml:"assign_public_ip"`
	SecurityGroups []string `yaml:"security_groups"`
	Subnets        []string `yaml:"subnets"`
}

func (c *AwsVpcConfiguration) toSDK() *types.AwsVpcConfiguration {
	return &types.AwsVpcConfiguration{
		AssignPublicIp: types.AssignPublicIp(c.AssignPublicIp),
		Subnets:        c.Subnets,
		SecurityGroups: c.SecurityGroups,
	}
}

type Host struct {
	WebApi             string `yaml:"webapi"`
	ReverseProxySuffix string `yaml:"reverse_proxy_suffix"`
}

type Link struct {
	HostedZoneID           string   `yaml:"hosted_zone_id"`
	DefaultTaskDefinitions []string `yaml:"default_task_definitions"`
}

type Listen struct {
	ForeignAddress string    `yaml:"foreign_address,omitempty"`
	HTTP           []PortMap `yaml:"http,omitempty"`
	HTTPS          []PortMap `yaml:"https,omitempty"`
}

type PortMap struct {
	ListenPort        int  `yaml:"listen"`
	TargetPort        int  `yaml:"target"`
	RequireAuthCookie bool `yaml:"require_auth_cookie"`
}

type Parameter struct {
	Name        string            `yaml:"name"`
	Env         string            `yaml:"env"`
	Rule        string            `yaml:"rule"`
	Required    bool              `yaml:"required"`
	Regexp      regexp.Regexp     `yaml:"-"`
	Default     string            `yaml:"default"`
	Description string            `yaml:"description"`
	Options     []ParameterOption `yaml:"options"`
}

type ParameterOption struct {
	Label string `yaml:"label"`
	Value string `yaml:"value"`
}

type Parameters []*Parameter

type ConfigParams struct {
	Path        string
	Domain      string
	LocalMode   bool
	DefaultPort int
	CompatV1    bool
	LogFormat   string
}

type Network struct {
	ProxyTimeout time.Duration `yaml:"proxy_timeout"`
}

const DefaultPort = 80
const DefaultProxyTimeout = 0
const AuthCookieName = "mirage-ecs-auth"
const AuthCookieExpire = 24 * time.Hour

func NewConfig(ctx context.Context, p *ConfigParams) (*Config, error) {
	domain := p.Domain
	if !strings.HasPrefix(domain, ".") {
		domain = "." + domain
	}
	if p.DefaultPort == 0 {
		p.DefaultPort = DefaultPort
	}
	// default config
	cfg := &Config{
		Host: Host{
			WebApi:             "mirage" + domain,
			ReverseProxySuffix: domain,
		},
		Listen: Listen{
			ForeignAddress: "0.0.0.0",
			HTTP: []PortMap{
				{ListenPort: p.DefaultPort, TargetPort: p.DefaultPort},
			},
			HTTPS: nil,
		},
		Network: Network{
			ProxyTimeout: DefaultProxyTimeout,
		},
		HtmlDir: "./html",
		ECS: ECSCfg{
			Region: os.Getenv("AWS_REGION"),
		},
		Auth:  nil,
		Purge: nil,

		localMode: p.LocalMode,
		compatV1:  p.CompatV1,
	}
	opt := &slog.HandlerOptions{
		Level:     LogLevel,
		AddSource: true,
	}

	switch p.LogFormat {
	case "text", "":
		slog.SetDefault(slog.New(NewLogHandler(os.Stderr, opt)))
	case "json":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opt)))
	default:
		return nil, fmt.Errorf("invalid log format (text or json): %s", p.LogFormat)
	}

	if awscfg, err := awsv2Config.LoadDefaultConfig(ctx, awsv2Config.WithRegion(cfg.ECS.Region)); err != nil {
		return nil, err
	} else {
		cfg.awscfg = &awscfg
	}

	if p.Path == "" {
		slog.Info(f("no config file specified, using default config with domain suffix: %s", domain))
	} else {
		var content []byte
		var err error
		if strings.HasPrefix(p.Path, "s3://") {
			content, err = loadFromS3(ctx, cfg.awscfg, p.Path)
		} else {
			content, err = loadFromFile(p.Path)
		}
		if err != nil {
			return nil, fmt.Errorf("cannot load config: %s: %w", p.Path, err)
		}
		slog.Info(f("loading config file: %s", p.Path))
		if err := config.LoadWithEnvBytes(&cfg, content); err != nil {
			return nil, fmt.Errorf("cannot load config: %s: %w", p.Path, err)
		}
	}

	addDefaultParameter := true
	for _, v := range cfg.Parameter {
		if v.Name == DefaultParameter.Name {
			addDefaultParameter = false
			break
		}
	}
	if addDefaultParameter {
		cfg.Parameter = append(cfg.Parameter, DefaultParameter)
	}

	for _, v := range cfg.Parameter {
		if v.Rule != "" {
			paramRegex, err := regexp.Compile(v.Rule)
			if err != nil {
				return nil, fmt.Errorf("invalid parameter rule: %s: %w", v.Rule, err)
			}
			v.Regexp = *paramRegex
		}
	}

	if strings.HasPrefix(cfg.HtmlDir, "s3://") {
		if err := cfg.downloadHTMLFromS3(ctx); err != nil {
			return nil, err
		}
	}

	if cfg.localMode {
		slog.Info("local mode: setting host suffix to .localtest.me")
		cfg.Host.ReverseProxySuffix = ".localtest.me"
		cfg.Host.WebApi = "mirage.localtest.me"
		slog.Info(f("You can access to http://mirage.localtest.me:%d/", cfg.Listen.HTTP[0].ListenPort))
	}

	cfg.ECS.capacityProviderStrategy = cfg.ECS.CapacityProviderStrategy.toSDK()
	cfg.ECS.networkConfiguration = cfg.ECS.NetworkConfiguration.toSDK()

	if err := cfg.fillECSDefaults(ctx); err != nil {
		slog.Warn(f("failed to fill ECS defaults: %s", err))
	}

	if cfg.Purge != nil {
		if err := cfg.Purge.Validate(); err != nil {
			return nil, fmt.Errorf("invalid purge config: %w", err)
		}
	}
	return cfg, nil
}

func (c *Config) Cleanup() {
	for _, fn := range c.cleanups {
		if err := fn(); err != nil {
			slog.Warn(f("failed to cleanup %s", err))
		}
	}
}

func (c *Config) NewTaskRunner() TaskRunner {
	if c.localMode {
		return NewLocalTaskRunner(c)
	} else {
		return NewECSTaskRunner(c)
	}
}

func (c *Config) fillECSDefaults(ctx context.Context) error {
	if c.localMode {
		slog.Info("ECS config is not used in local mode")
		return nil
	}
	defer func() {
		if err := c.ECS.validate(); err != nil {
			slog.Error(f("invalid ECS config: %s", c.ECS))
			slog.Error(f("ECS config is invalid '%s', so you may not be able to launch ECS tasks", err))
		} else {
			slog.Info(f("built ECS config: %s", c.ECS))
		}
	}()
	if c.ECS.Region == "" {
		c.ECS.Region = os.Getenv("AWS_REGION")
		slog.Info(f("AWS_REGION is not set, using region=%s", c.ECS.Region))
	}
	if c.ECS.LaunchType == nil && c.ECS.CapacityProviderStrategy == nil {
		launchType := "FARGATE"
		c.ECS.LaunchType = &launchType
		slog.Info(f("launch_type and capacity_provider_strategy are not set, using launch_type=%s", *c.ECS.LaunchType))
	}
	if c.ECS.EnableExecuteCommand == nil {
		c.ECS.EnableExecuteCommand = aws.Bool(true)
		slog.Info(f("enable_execute_command is not set, using enable_execute_command=%t", *c.ECS.EnableExecuteCommand))
	}

	meta, err := metadata.Get(ctx, &http.Client{})
	if err != nil {
		return err
		/*
			for local debugging
			meta = &metadata.TaskMetadataV4{
				Cluster: "your test cluster",
				TaskARN: "your test task arn running on the cluster",
			}
		*/
	}
	slog.Debug(f("task metadata: %v", meta))
	var cluster, taskArn, service string
	switch m := meta.(type) {
	case *metadata.TaskMetadataV3:
		cluster = m.Cluster
		taskArn = m.TaskARN
	case *metadata.TaskMetadataV4:
		cluster = m.Cluster
		taskArn = m.TaskARN
	}
	if c.ECS.Cluster == "" && cluster != "" {
		slog.Info(f("ECS cluster is set from task metadata: %s", cluster))
		c.ECS.Cluster = cluster
	}

	svc := ecs.NewFromConfig(*c.awscfg)
	if out, err := svc.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(cluster),
		Tasks:   []string{taskArn},
	}); err != nil {
		return err
	} else {
		if len(out.Tasks) == 0 {
			return fmt.Errorf("cannot find task: %s", taskArn)
		}
		group := aws.ToString(out.Tasks[0].Group)
		if strings.HasPrefix(group, "service:") {
			service = group[8:]
		}
	}
	if out, err := svc.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(cluster),
		Services: []string{service},
	}); err != nil {
		return err
	} else {
		if len(out.Services) == 0 {
			return fmt.Errorf("cannot find service: %s", service)
		}
		if c.ECS.networkConfiguration == nil {
			c.ECS.networkConfiguration = out.Services[0].NetworkConfiguration
			slog.Info(f("network_configuration is not set, using network_configuration=%v", c.ECS.networkConfiguration))
		}
	}
	return nil
}

func (cfg *Config) AuthMiddlewareForWeb(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		ok, err := cfg.Auth.Do(req, c.Response(),
			cfg.Auth.ByToken, cfg.Auth.ByAmznOIDC, cfg.Auth.ByBasic,
		)
		if err != nil {
			slog.Error(f("auth error: %s", err))
			return echo.ErrInternalServerError
		}
		if !ok {
			slog.Warn("all auth methods failed")
			return echo.ErrUnauthorized
		}

		// check origin header
		if req.Method == http.MethodPost {
			origin := c.Request().Header.Get("Origin")
			if origin == "" {
				slog.Error("missing origin header")
				return echo.ErrBadRequest
			}
			u, err := url.Parse(origin)
			if err != nil {
				slog.Error(f("invalid origin header: %s", origin))
				return echo.ErrBadRequest
			}
			host, _, err := net.SplitHostPort(u.Host)
			if err != nil {
				host = u.Host // missing port
			}
			if host != cfg.Host.WebApi {
				slog.Error(f("invalid origin host: %s", u.Host))
				return echo.ErrBadRequest
			}
		}

		cookie, err := cfg.Auth.NewAuthCookie(AuthCookieExpire, cfg.Host.ReverseProxySuffix)
		if err != nil {
			slog.Error(f("failed to create auth cookie: %s", err))
			return echo.ErrInternalServerError
		}
		if cookie.Value != "" {
			c.SetCookie(cookie)
		}
		return next(c)
	}
}

func (cfg *Config) AuthMiddlewareForAPI(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// API allows only token auth
		ok, err := cfg.Auth.Do(c.Request(), c.Response(), cfg.Auth.ByToken)
		if err != nil {
			slog.Error(f("auth error: %s", err))
			return echo.ErrInternalServerError
		}
		if !ok {
			slog.Warn(f("all auth methods failed"))
			return echo.ErrUnauthorized
		}
		return next(c)
	}
}

func (cfg *Config) CompatMiddlewareForAPI(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		contentType := req.Header.Get("Content-Type")
		switch cfg.compatV1 {
		case true:
			// allows any content type
		case false:
			if req.Method == http.MethodPost && !strings.HasPrefix(contentType, "application/json") {
				slog.Error(f("invalid content type: %s for %s %s", contentType, req.Method, req.URL.Path))
				return echo.ErrBadRequest
			}
		}
		return next(c)
	}
}

func loadFromFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func loadFromS3(ctx context.Context, awscfg *aws.Config, u string) ([]byte, error) {
	svc := s3.NewFromConfig(*awscfg)
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "s3" {
		return nil, fmt.Errorf("invalid scheme: %s", parsed.Scheme)
	}
	bucket := parsed.Host
	key := strings.TrimPrefix(parsed.Path, "/")
	out, err := svc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (c *Config) downloadHTMLFromS3(ctx context.Context) error {
	slog.Info(f("downloading html files from %s", c.HtmlDir))
	tmpdir, err := os.MkdirTemp("", "mirage-ecs-htmldir-")
	if err != nil {
		return err
	}
	svc := s3.NewFromConfig(*c.awscfg)
	parsed, err := url.Parse(c.HtmlDir)
	if err != nil {
		return err
	}
	if parsed.Scheme != "s3" {
		return fmt.Errorf("invalid scheme: %s", parsed.Scheme)
	}
	bucket := parsed.Host
	keyPrefix := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}
	slog.Debug(f("bucket: %s keyPrefix: %s", bucket, keyPrefix))
	res, err := svc.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(keyPrefix),
		Delimiter: aws.String("/"),
		MaxKeys:   100, // sufficient for html template files
	})
	if err != nil {
		return err
	}
	if len(res.Contents) == 0 {
		return fmt.Errorf("no objects found in %s", c.HtmlDir)
	}
	files := 0
	for _, obj := range res.Contents {
		slog.Info(f("downloading %s", aws.ToString(obj.Key)))
		r, err := svc.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    obj.Key,
		})
		if err != nil {
			return err
		}
		defer r.Body.Close()
		filename := path.Base(aws.ToString(obj.Key))
		file := filepath.Join(tmpdir, filename)
		if size, err := copyToFile(r.Body, file); err != nil {
			return err
		} else {
			files++
			slog.Info(f("downloaded %s (%d bytes)", file, size))
		}
	}
	slog.Info(f("downloaded %d files from %s", files, c.HtmlDir))
	c.HtmlDir = tmpdir
	c.cleanups = append(c.cleanups, func() error {
		slog.Info(f("removing %s", tmpdir))
		return os.RemoveAll(tmpdir)
	})
	slog.Info(f("setting html dir: %s", c.HtmlDir))
	return nil
}

func copyToFile(src io.Reader, dst string) (int64, error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, src)
}

func (cfg *Config) ValidateOriginMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		return next(c)
	}
}

func (cfg *Config) EncodeSubdomain(subdomain string) string {
	if cfg.compatV1 {
		return base64.URLEncoding.EncodeToString([]byte(subdomain))
	} else {
		return subdomain
	}
}
