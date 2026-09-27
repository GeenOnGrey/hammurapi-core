// Package app assembles dependencies and runs the binary modes.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/config"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/admin"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/attachments"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/domains"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/feedback"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/handoff"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/imports"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/profile"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/rules"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/voice"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/webhooks"
	"github.com/GeenOnGrey/hammurapi-core/internal/jobs/cleaner"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/acp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/kafka"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/storage"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/whisper"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// core holds dependencies shared by all modes.
type core struct {
	cfg      *config.Config
	pool     *pgxpool.Pool
	provider git.Provider
	authRepo *auth.Repository
	authSvc  *auth.Service
	store    *specdata.PG
	s3       storage.Storage
	events   *events.PGPublisher
}

func newCore(ctx context.Context, cfg *config.Config) (*core, error) {
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	var provider git.Provider
	switch cfg.GitProvider {
	case "github":
		provider = git.NewGitHub(cfg.GitBaseURL, cfg.GitOAuthURL, cfg.GitRepo, cfg.GitHubClientID, cfg.GitHubSecret)
	case "gitlab":
		provider = git.NewGitLab(cfg.GitBaseURL, cfg.GitOAuthURL, cfg.GitRepo, cfg.GitLabClientID, cfg.GitLabSecret)
	}
	box, err := crypto.NewBox(cfg.TokenEncryptionKey)
	if err != nil {
		return nil, err
	}
	s3, err := storage.NewS3(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	authRepo := auth.NewRepository(pool)
	return &core{
		cfg: cfg, pool: pool, provider: provider, authRepo: authRepo,
		authSvc: auth.NewService(authRepo, provider, box, cfg.PublicURL, cfg.BootstrapAdmins, cfg.DefaultLanguage),
		store:   specdata.NewPG(pool), s3: s3, events: events.NewPGPublisher(pool),
	}, nil
}

func (c *core) close() { c.pool.Close() }

// serviceServer exposes /healthz, /readyz and /metrics on the service port.
func serviceServer(addr string, ready func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func (c *core) ready(ctx context.Context) error {
	if err := c.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := kafka.Ping(ctx, c.cfg.KafkaBrokers); err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	return nil
}

// serve runs servers until ctx is done, then shuts them down gracefully.
func serve(ctx context.Context, g *errgroup.Group, servers ...*http.Server) {
	for _, s := range servers {
		s := s
		g.Go(func() error {
			slog.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return s.Shutdown(sctx)
		})
	}
}

func principalLoader(repo *auth.Repository) func(context.Context, uuid.UUID) (*domain.Principal, error) {
	return func(ctx context.Context, id uuid.UUID) (*domain.Principal, error) {
		p, err := repo.Principal(ctx, id)
		if err == nil && p == nil {
			return nil, apperr.NotFound("user_not_found", "user not found")
		}
		return p, err
	}
}

// RunAPI runs the HTTP API, SSE, webhooks, the agent pool and the internal MCP endpoint.
func RunAPI(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	if err := kafka.EnsureTopics(ctx, cfg.KafkaBrokers, kafka.TopicGitPush, kafka.TopicImports); err != nil {
		slog.Warn("could not ensure kafka topics (auto-creation will be used)", "err", err)
	}
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	hub := events.NewHub()
	go hub.Listen(ctx, c.pool)

	tokens := c.authSvc
	branch := cfg.GitDefaultBranch
	loadPrincipal := principalLoader(c.authRepo)

	featureSvc := features.NewService(c.store, c.provider, tokens, c.events, branch)
	gateSvc := gates.NewService(c.store, c.provider, tokens, c.events, branch)
	approvalSvc := approvals.NewService(c.store, approvals.NewRepository(c.pool), c.provider, tokens, c.events)
	handoffSvc := handoff.NewService(c.store, c.provider, tokens, c.events)
	domainSvc := domains.NewService(domains.NewRepository(c.pool), c.events)
	profileSvc := profile.NewService(c.pool)
	adminSvc := admin.NewService(c.pool)
	rulesSvc := rules.NewService(c.pool, c.provider, tokens, branch)
	attSvc := attachments.NewService(c.pool, c.s3, cfg.UploadMaxBytes, cfg.UploadAllowedTypes)
	importSvc := imports.NewService(c.pool, c.store, c.s3, producer, c.provider, tokens, c.events, rulesSvc, loadPrincipal, imports.Config{
		MaxBytes: cfg.ImportMaxBytes, DefaultBranch: branch, AllowedAssets: cfg.ImportAllowedAssetTypes,
		Limits: imports.Limits{MaxUncompressed: cfg.ImportMaxUncompressedBytes, MaxFiles: cfg.ImportMaxFiles},
	})

	mcpServer := mcp.NewServer()
	var chatSvc *agent.Service
	pool := acp.NewPool(acp.Config{
		Command: cfg.ACPCommand, Args: cfg.ACPArgs, Env: cfg.ACPEnv, MaxProcs: cfg.ACPMaxProcs, IdleTimeout: cfg.ACPIdleTimeout,
		OnSessionClosed: func(u uuid.UUID) { chatSvc.OnSessionClosed(u) },
	})
	defer pool.Close()
	chatSvc = agent.NewService(agent.NewRepository(c.pool), c.store, pool, mcpServer, "http://"+cfg.MCPAddr+"/mcp", hub, attSvc, loadPrincipal)
	mcpServer.Register(agent.Tools(agent.ToolDeps{Store: c.store, Git: c.provider, Tokens: tokens, Gates: gateSvc, Principal: loadPrincipal, DefaultBranch: branch})...)
	profileSvc.OnAgentChanged = chatSvc.ResetPersona

	authH := auth.NewHandlers(c.authSvc, auth.PublicConfig{
		Provider: cfg.GitProvider, UploadMaxBytes: cfg.UploadMaxBytes, UploadTypes: cfg.UploadAllowedTypes,
		ImportMaxBytes: cfg.ImportMaxBytes, Languages: domain.Languages, DefaultLanguage: cfg.DefaultLanguage, DefaultBranch: branch,
	}, strings.HasPrefix(cfg.PublicURL, "https://"))

	r := chi.NewRouter()
	r.Use(httpx.Observe)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authH.Authenticate)
		authH.Public(r)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireSession)
			authH.Private(r)
			profileSvc.Routes(r)
			feedback.Routes(r, c.provider, tokens)
			domainSvc.PublicRoutes(r)
			features.NewHandlers(featureSvc).Routes(r)
			gates.NewHandlers(gateSvc).Routes(r)
			approvals.NewHandlers(approvalSvc).Routes(r)
			handoffSvc.Routes(r)
			chatSvc.Routes(r)
			voice.Routes(r, whisper.New(cfg.WhisperURL))
			attSvc.Routes(r)
			importSvc.Routes(r)
			r.Get("/events", events.SSEHandler(hub))
		})
	})
	r.Route("/admin/api/v1", func(r chi.Router) {
		r.Use(authH.Authenticate, auth.RequireSession, requireAnyAdmin)
		adminSvc.Routes(r)
		domainSvc.AdminRoutes(r)
		rulesSvc.Routes(r)
	})
	r.Method(http.MethodPost, "/hooks/v1/git", webhooks.NewReceiver(c.provider, cfg.WebhookSecret, producer))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, apperr.NotFound("not_found", "not found"))
	})

	api := &http.Server{Addr: cfg.HTTPAddr, Handler: otelhttp.NewHandler(r, "http"), ReadHeaderTimeout: 15 * time.Second}
	mcpHTTP := &http.Server{Addr: cfg.MCPAddr, Handler: http.StripPrefix("", mcpMux(mcpServer)), ReadHeaderTimeout: 15 * time.Second}
	svc := serviceServer(cfg.ServiceAddr, c.ready) // agent health is a metric, not readiness

	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, api, mcpHTTP, svc)
	return g.Wait()
}

func mcpMux(s *mcp.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	return mux
}

func requireAnyAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpx.PrincipalFrom(r.Context())
		if p == nil || !p.IsAnyAdmin() {
			httpx.Error(w, r, apperr.Forbidden("forbidden", "administrator role required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RunWorker consumes push events and import jobs.
func RunWorker(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	if err := kafka.EnsureTopics(ctx, cfg.KafkaBrokers, kafka.TopicGitPush, kafka.TopicImports); err != nil {
		slog.Warn("could not ensure kafka topics", "err", err)
	}
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()
	tokens := c.authSvc
	rulesSvc := rules.NewService(c.pool, c.provider, tokens, cfg.GitDefaultBranch)
	importSvc := imports.NewService(c.pool, c.store, c.s3, producer, c.provider, tokens, c.events, rulesSvc, principalLoader(c.authRepo), imports.Config{
		MaxBytes: cfg.ImportMaxBytes, DefaultBranch: cfg.GitDefaultBranch, AllowedAssets: cfg.ImportAllowedAssetTypes,
		Limits: imports.Limits{MaxUncompressed: cfg.ImportMaxUncompressedBytes, MaxFiles: cfg.ImportMaxFiles},
	})
	proc := webhooks.NewProcessor(c.store, c.provider, c.events)

	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, serviceServer(cfg.ServiceAddr, c.ready))
	g.Go(func() error {
		return kafka.Consume(gctx, cfg.KafkaBrokers, "hammurapi-worker", kafka.TopicGitPush, proc.Handle)
	})
	g.Go(func() error {
		return kafka.Consume(gctx, cfg.KafkaBrokers, "hammurapi-worker", kafka.TopicImports, importSvc.Handle)
	})
	return g.Wait()
}

// RunCleaner runs one maintenance pass.
func RunCleaner(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	cl := &cleaner.Cleaner{Pool: c.pool, S3: c.s3, Store: c.store, Git: c.provider, Tokens: c.authSvc}
	_, err = cl.Run(ctx)
	return err
}

// RunMigrate applies database migrations.
func RunMigrate(ctx context.Context, cfg *config.Config) error {
	return postgres.Migrate(ctx, cfg.DatabaseURL)
}
