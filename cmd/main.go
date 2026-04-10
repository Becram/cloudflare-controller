package main

import (
	"flag"
	"os"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cfc "github.com/bikramdhoju/rector/internal/cloudflare"
	"github.com/bikramdhoju/rector/internal/config"
	"github.com/bikramdhoju/rector/internal/configmap"
	"github.com/bikramdhoju/rector/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var (
		configFile             string
		cloudflaredCMName      string
		cloudflaredCMNamespace string
		accountID              string
		zoneID                 string
		tunnelID               string
		metricsAddr            string
		probeAddr              string
		enableLeaderElection   bool
		logLevel               string
	)

	flag.StringVar(&configFile, "config", "", "Path to YAML config file. Individual flags override file values.")
	flag.StringVar(&cloudflaredCMName, "cloudflared-configmap-name", "", "Name of the cloudflared ConfigMap.")
	flag.StringVar(&cloudflaredCMNamespace, "cloudflared-configmap-namespace", "", "Namespace of the cloudflared ConfigMap.")
	flag.StringVar(&accountID, "cloudflare-account-id", "", "Cloudflare account ID.")
	flag.StringVar(&zoneID, "cloudflare-zone-id", "", "Cloudflare DNS zone ID.")
	flag.StringVar(&tunnelID, "cloudflare-tunnel-id", "", "Cloudflare Argo Tunnel UUID.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", "", "Address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&logLevel, "log-level", "", "Log verbosity: debug, info, warn, error. Overrides config file.")

	// opts.BindFlags registers --zap-* flags; parse them alongside our own flags.
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Use a bootstrap logger until the real level is known.
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Start from defaults, optionally populated from a config file.
	cfg := &config.Config{
		MetricsBindAddress:     ":8080",
		HealthProbeBindAddress: ":8081",
	}

	if configFile != "" {
		loaded, err := config.Load(configFile)
		if err != nil {
			ctrl.Log.Error(err, "failed to load config file")
			os.Exit(1)
		}
		cfg = loaded
	}

	// Flags take precedence over file values.
	if accountID != "" {
		cfg.Cloudflare.AccountID = accountID
	}
	if zoneID != "" {
		cfg.Cloudflare.ZoneID = zoneID
	}
	if tunnelID != "" {
		cfg.Cloudflare.TunnelID = tunnelID
	}
	if cloudflaredCMName != "" {
		cfg.Cloudflared.ConfigMap.Name = cloudflaredCMName
	}
	if cloudflaredCMNamespace != "" {
		cfg.Cloudflared.ConfigMap.Namespace = cloudflaredCMNamespace
	}
	if metricsAddr != "" {
		cfg.MetricsBindAddress = metricsAddr
	}
	if probeAddr != "" {
		cfg.HealthProbeBindAddress = probeAddr
	}
	if enableLeaderElection {
		cfg.LeaderElect = true
	}
	if logLevel != "" {
		cfg.LogLevel = logLevel
	}

	// Re-initialise the logger now that the final log level is known.
	// --zap-log-level flag takes precedence; config file logLevel is the fallback.
	ctrl.Log.Info("resolved log level", "logLevel", cfg.LogLevel)
	if opts.Level == nil {
		var level zapcore.Level
		if err := level.UnmarshalText([]byte(cfg.LogLevel)); err == nil {
			opts.Level = level
		}
	}
	opts.Development = cfg.LogLevel == "debug"
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := cfg.Validate(); err != nil {
		ctrl.Log.Error(err, "invalid configuration")
		os.Exit(1)
	}

	// CLOUDFLARE_API_TOKEN must be set — mount from a Kubernetes Secret.
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	if apiToken == "" {
		ctrl.Log.Error(nil, "CLOUDFLARE_API_TOKEN environment variable is not set")
		os.Exit(1)
	}

	cfClient, err := cfc.NewClient(apiToken)
	if err != nil {
		ctrl.Log.Error(err, "failed to create Cloudflare client")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsBindAddress,
		},
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		LeaderElection:         cfg.LeaderElect,
		LeaderElectionID:       "rector.apps.rector.io",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorderFor("rector-cloudflare-controller"),
		ConfigMgr:            configmap.New(mgr.GetClient()),
		ConfigMapName:        cfg.Cloudflared.ConfigMap.Name,
		ConfigMapNamespace:   cfg.Cloudflared.ConfigMap.Namespace,
		CFClient:             cfClient,
		AccountID:            cfg.Cloudflare.AccountID,
		ZoneID:               cfg.Cloudflare.ZoneID,
		TunnelID:             cfg.Cloudflare.TunnelID,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "ServiceReconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting rector cloudflare controller", "config", configFile)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
