package telemetry

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/argoproj/argo-workflows/v3/util/logging"

	promgo "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/exporters/prometheus"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/env"

	tlsutils "github.com/argoproj/argo-workflows/v3/util/tls"
)

const (
	DefaultPrometheusServerPort = 9090
	DefaultPrometheusServerPath = "/metrics"
)

func (config *Config) prometheusMetricsExporter(namespace string) (*prometheus.Exporter, error) {
	// Use an exporter that mimics the legacy prometheus exporter
	// We cannot namespace here, because custom metrics are not namespaced
	// in the legacy version, so they cannot be here
	return prometheus.New(
		prometheus.WithNamespace(namespace),
		prometheus.WithoutCounterSuffixes(),
		prometheus.WithoutUnits(),
		prometheus.WithoutScopeInfo(),
		prometheus.WithoutTargetInfo(),
	)
}

func (config *Config) path() string {
	if config.Path == "" {
		return DefaultPrometheusServerPath
	}
	return config.Path
}

func (config *Config) port() int {
	if config.Port == 0 {
		return DefaultPrometheusServerPort
	}
	return config.Port
}

// RunPrometheusServer starts a prometheus metrics server
// If 'isDummy' is set to true, the dummy metrics server will be started. If it's false, the prometheus metrics server will be started
func (m *Metrics) RunPrometheusServer(ctx context.Context, isDummy bool) {
	if !m.config.Enabled {
		return
	}
	defer runtimeutil.HandleCrashWithContext(ctx, runtimeutil.PanicHandlers...)

	logger := logging.RequireLoggerFromContext(ctx)

	name := ""
	mux := http.NewServeMux()
	if isDummy {
		// dummy metrics server responds to all requests with a 200 status, but without providing any metrics data
		name = "dummy metrics server"
		mux.HandleFunc(m.config.path(), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	} else {
		var handlerOpts promhttp.HandlerOpts
		if m.config.IgnoreErrors {
			handlerOpts.ErrorHandling = promhttp.ContinueOnError
		}
		name = "prometheus metrics server"
		mux.Handle(m.config.path(), promhttp.HandlerFor(promgo.DefaultGatherer, handlerOpts))
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%v", m.config.port()), Handler: mux}

	if m.config.Secure {
		tlsMinVersion, err := env.GetInt("TLS_MIN_VERSION", tls.VersionTLS12)
		if err != nil {
			panic(err)
		}
		logger.Info(ctx, "Generating Self Signed TLS Certificates for Telemetry Servers")
		tlsConfig, err := tlsutils.GenerateX509KeyPairTLSConfig(uint16(tlsMinVersion))
		if err != nil {
			panic(err)
		}
		srv.TLSConfig = tlsConfig
		go func() {
			logger.WithFields(logging.Fields{"name": name, "port": m.config.port(), "path": m.config.path()}).Info(ctx, "Starting prometheus server")
			if err := srv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
				panic(err)
			}
		}()
	} else {
		go func() {
			logger.WithFields(logging.Fields{"name": name, "port": m.config.port(), "path": m.config.path()}).Info(ctx, "Starting prometheus server")
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				panic(err)
			}
		}()
	}

	// Waiting for stop signal
	<-ctx.Done()

	// Shutdown the server gracefully with a 1 second timeout
	shutdownCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.WithFields(logging.Fields{"name": name, "port": m.config.port(), "path": m.config.path()}).Info(ctx, "Unable to shutdown prometheus server")
	} else {
		logger.WithFields(logging.Fields{"name": name, "port": m.config.port(), "path": m.config.path()}).Info(ctx, "Successfully shutdown prometheus server")
	}
}
