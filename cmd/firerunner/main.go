package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/ismoilovdevml/firerunner/pkg/config"
	"github.com/ismoilovdevml/firerunner/pkg/firecracker"
	"github.com/ismoilovdevml/firerunner/pkg/gitlab"
	"github.com/ismoilovdevml/firerunner/pkg/scheduler"
)

var (
	configPath = flag.String("config", "config.yaml", "Path to configuration file")
	version    = "dev"
	commit     = "unknown"
	buildDate  = "unknown"
)

func main() {
	flag.Parse()

	logger := setupLogger()

	logger.WithFields(logrus.Fields{
		"version":    version,
		"commit":     commit,
		"build_date": buildDate,
	}).Info("Starting FireRunner")

	cfg, err := loadConfig(*configPath, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to load configuration")
	}

	app, err := initializeApp(cfg, logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to initialize application")
	}

	if err := app.Start(); err != nil {
		logger.WithError(err).Fatal("Failed to start application")
	}

	waitForShutdown(app, logger)
}

type App struct {
	config          *config.Config
	logger          *logrus.Logger
	flintlockClient *firecracker.Client
	vmManager       *firecracker.Manager
	gitlabService   *gitlab.Service
	scheduler       *scheduler.Scheduler
	webhookHandler  *gitlab.WebhookHandler
	httpServer      *http.Server
	metricsServer   *http.Server
}

func initializeApp(cfg *config.Config, logger *logrus.Logger) (*App, error) {
	logger.Info("Initializing application components")

	flintlockClient, err := firecracker.NewClient(&cfg.Flintlock)
	if err != nil {
		return nil, fmt.Errorf("failed to create Flintlock client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := flintlockClient.Health(ctx); err != nil {
		logger.WithError(err).Warn("Flintlock health check failed (will retry)")
	}
	cancel()

	vmManager := firecracker.NewManager(flintlockClient, &cfg.VM, logger)

	gitlabService, err := gitlab.NewService(&cfg.GitLab, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitLab service: %w", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	if err := gitlabService.Health(ctx); err != nil {
		logger.WithError(err).Warn("GitLab health check failed")
	}
	cancel()

	sched := scheduler.NewScheduler(&cfg.Scheduler, vmManager, gitlabService, logger)

	processor := &EventProcessor{
		scheduler: sched,
		logger:    logger,
	}
	webhookHandler := gitlab.NewWebhookHandler(cfg.GitLab.WebhookSecret, logger, processor)

	httpServer := setupHTTPServer(cfg, webhookHandler)

	var metricsServer *http.Server
	if cfg.Metrics.Enabled {
		metricsServer = setupMetricsServer(cfg)
	}

	return &App{
		config:          cfg,
		logger:          logger,
		flintlockClient: flintlockClient,
		vmManager:       vmManager,
		gitlabService:   gitlabService,
		scheduler:       sched,
		webhookHandler:  webhookHandler,
		httpServer:      httpServer,
		metricsServer:   metricsServer,
	}, nil
}

func (app *App) Start() error {
	app.logger.Info("Starting application services")

	app.vmManager.StartCleanup(app.config.Scheduler.CleanupInterval)

	if err := app.scheduler.Start(); err != nil {
		return fmt.Errorf("failed to start scheduler: %w", err)
	}

	if app.metricsServer != nil {
		go func() {
			app.logger.WithField("port", app.config.Metrics.Port).Info("Starting metrics server")
			if err := app.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				app.logger.WithError(err).Error("Metrics server error")
			}
		}()
	}

	go func() {
		app.logger.WithFields(logrus.Fields{
			"host": app.config.Server.Host,
			"port": app.config.Server.Port,
		}).Info("Starting HTTP server")

		var err error
		if app.config.Server.TLSEnabled {
			err = app.httpServer.ListenAndServeTLS(
				app.config.Server.TLSCertPath,
				app.config.Server.TLSKeyPath,
			)
		} else {
			err = app.httpServer.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			app.logger.WithError(err).Fatal("HTTP server error")
		}
	}()

	app.logger.Info("Application started successfully")
	return nil
}

func (app *App) Shutdown(ctx context.Context) error {
	app.logger.Info("Shutting down application")

	if app.httpServer != nil {
		if err := app.httpServer.Shutdown(ctx); err != nil {
			app.logger.WithError(err).Error("Failed to shutdown HTTP server")
		}
	}

	if app.metricsServer != nil {
		if err := app.metricsServer.Shutdown(ctx); err != nil {
			app.logger.WithError(err).Error("Failed to shutdown metrics server")
		}
	}

	if err := app.scheduler.Shutdown(ctx); err != nil {
		app.logger.WithError(err).Error("Failed to shutdown scheduler")
	}

	if err := app.vmManager.Shutdown(ctx); err != nil {
		app.logger.WithError(err).Error("Failed to shutdown VM manager")
	}

	if app.flintlockClient != nil {
		if err := app.flintlockClient.Close(); err != nil {
			app.logger.WithError(err).Error("Failed to close Flintlock client")
		}
	}

	app.logger.Info("Application shutdown completed")
	return nil
}

type EventProcessor struct {
	scheduler *scheduler.Scheduler
	logger    *logrus.Logger
}

func (ep *EventProcessor) ProcessJobEvent(event *gitlab.JobEvent) error {
	return ep.scheduler.ScheduleJob(event)
}

func (ep *EventProcessor) ProcessPipelineEvent(event *gitlab.PipelineEvent) error {
	ep.logger.WithField("pipeline_id", event.ObjectAttributes.ID).Debug("Pipeline event received")
	return nil
}

func setupLogger() *logrus.Logger {
	logger := logrus.New()

	level := os.Getenv("LOG_LEVEL")
	if level == "" {
		level = "info"
	}

	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		logLevel = logrus.InfoLevel
	}

	logger.SetLevel(logLevel)

	format := os.Getenv("LOG_FORMAT")
	if format == "json" {
		logger.SetFormatter(&logrus.JSONFormatter{})
	} else {
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
		})
	}

	return logger
}

func loadConfig(path string, logger *logrus.Logger) (*config.Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		logger.Warn("Config file not found, using defaults")
		return config.Default(), nil
	}

	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}

	logger.Info("Configuration loaded successfully")
	return cfg, nil
}

func setupHTTPServer(cfg *config.Config, webhookHandler *gitlab.WebhookHandler) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("/webhook", webhookHandler)
	mux.HandleFunc("/health", webhookHandler.HealthCheck)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ready"}`))
	})

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	return &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}
}

func setupMetricsServer(cfg *config.Config) *http.Server {
	mux := http.NewServeMux()
	mux.Handle(cfg.Metrics.Path, promhttp.Handler())

	addr := fmt.Sprintf(":%d", cfg.Metrics.Port)

	return &http.Server{
		Addr:    addr,
		Handler: mux,
	}
}

func waitForShutdown(app *App, logger *logrus.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	sig := <-sigCh
	logger.WithField("signal", sig.String()).Info("Received shutdown signal")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := app.Shutdown(ctx); err != nil {
		logger.WithError(err).Error("Shutdown failed")
		os.Exit(1)
	}

	logger.Info("Goodbye!")
}
