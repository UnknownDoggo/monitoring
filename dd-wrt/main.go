package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JulienBalestra/monitoring/cmd/version"
	"github.com/JulienBalestra/monitoring/pkg/collector"
	"github.com/JulienBalestra/monitoring/pkg/collector/catalog"
	datadogCollector "github.com/JulienBalestra/monitoring/pkg/collector/datadog"
	"github.com/JulienBalestra/monitoring/pkg/datadog"
	"github.com/JulienBalestra/monitoring/pkg/forward"
	"github.com/JulienBalestra/monitoring/pkg/tagger"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
)

const (
	datadogAPIKeyFlag         = "datadog-api-key"
	datadogAPPKeyFlag         = "datadog-app-key"
	datadogClientSendInterval = "datadog-client-send-interval"
	hostnameFlag              = "hostname"
	defaultCollectionInterval = time.Second * 30
	defaultPIDFilePath        = "/tmp/monitoring.pid"
)

func notifySignals(ctx context.Context, cancel context.CancelFunc, tag *tagger.Tagger) {
	signals := make(chan os.Signal)
	defer close(signals)
	defer signal.Stop(signals)
	defer signal.Reset()

	signal.Notify(signals, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	for {
		select {
		case <-ctx.Done():
			zap.L().Info("end of system signal handling")
			return

		case sig := <-signals:
			zap.L().Info("signal received", zap.String("signal", sig.String()))
			switch sig {
			case syscall.SIGUSR1:
				tag.Print()
			case syscall.SIGUSR2:
				_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)

			case syscall.SIGHUP:
				// nohup

			default:
				cancel()
			}
		}
	}
}

func setDatadogKeys(key *string, flag, envvar string) error {
	if *key != "" {
		return nil
	}
	*key = os.Getenv(envvar)
	if *key == "" {
		return fmt.Errorf("flag --%s or envvar %s must be set to a datadog key", flag, envvar)
	}
	return nil
}

func main() {
	zapConfig := zap.NewProductionConfig()
	zapConfig.OutputPaths = append(zapConfig.OutputPaths)
	zapLevel := zapConfig.Level.String()

	root := &cobra.Command{
		Short: "monitoring app for dd-wrt routers",
		Long:  "monitoring app for dd-wrt routers, disable any collector with --collector-${collector}=0s",
		Use:   "monitoring",
	}
	root.AddCommand(version.NewCommand())
	fs := &pflag.FlagSet{}

	var hostTagsStrings []string
	pidFilePath := ""
	hostname, _ := os.Hostname()
	timezone := time.Local.String()
	hostname = strings.ToLower(hostname)
	var client *datadog.Client
	datadogClientConfig := &datadog.Config{
		Host:          hostname,
		ClientMetrics: &datadog.ClientMetrics{},
	}

	fs.StringSliceVar(&hostTagsStrings, "datadog-host-tags", nil, "datadog host tags")
	fs.StringVar(&pidFilePath, "pid-file", defaultPIDFilePath, "file to write process id")
	fs.StringVarP(&datadogClientConfig.DatadogAPIKey, datadogAPIKeyFlag, "i", "", "datadog API key")
	fs.StringVarP(&datadogClientConfig.DatadogAPPKey, datadogAPPKeyFlag, "p", "", "datadog APP key")
	fs.StringVar(&hostname, hostnameFlag, hostname, "datadog host tag")
	fs.StringVar(&timezone, "timezone", timezone, "timezone")
	fs.DurationVar(&datadogClientConfig.SendInterval, datadogClientSendInterval, time.Second*35, "datadog client send interval to the API >= "+datadog.MinimalSendInterval.String())
	fs.StringVar(&zapLevel, "log-level", zapLevel, fmt.Sprintf("log level - %s %s %s %s %s %s %s", zap.DebugLevel, zap.InfoLevel, zap.WarnLevel, zap.ErrorLevel, zap.DPanicLevel, zap.PanicLevel, zap.FatalLevel))
	fs.StringSliceVar(&zapConfig.OutputPaths, "log-output", zapConfig.OutputPaths, "log output")

	collectorCatalog := catalog.CollectorCatalog()
	collectorCatalog[datadogCollector.CollectorName] = func(config *collector.Config) collector.Collector {
		d := datadogCollector.NewClient(config)
		d.ClientMetrics = datadogClientConfig.ClientMetrics
		return d
	}
	collectionDuration := make(map[string]*time.Duration, len(collectorCatalog))
	for name := range collectorCatalog {
		var d time.Duration
		collectionDuration[name] = &d
		fs.DurationVar(&d, "collector-"+name, defaultCollectionInterval, "collection interval/backoff for "+name)
	}

	root.Flags().AddFlagSet(fs)
	root.PreRunE = func(cmd *cobra.Command, args []string) error {
		err := setDatadogKeys(&datadogClientConfig.DatadogAPIKey, datadogAPIKeyFlag, "DATADOG_API_KEY")
		if err != nil {
			return err
		}
		err = setDatadogKeys(&datadogClientConfig.DatadogAPPKey, datadogAPPKeyFlag, "DATADOG_APP_KEY")
		if err != nil {
			return err
		}
		if datadogClientConfig.SendInterval <= datadog.MinimalSendInterval {
			return fmt.Errorf("flag --%s must be greater or equal to %s", datadogClientSendInterval, datadog.MinimalSendInterval)
		}
		if hostname == "" {
			return fmt.Errorf("empty hostname, flag --%s to define one", hostnameFlag)
		}
		err = zapConfig.Level.UnmarshalText([]byte(zapLevel))
		if err != nil {
			return err
		}
		client = datadog.NewClient(datadogClientConfig)
		// TODO make it works
		err = zap.RegisterSink(forward.DatadogZapScheme, forward.NewDatadogForwarder(client))
		if err != nil {
			return err
		}
		logger, err := zapConfig.Build()
		if err != nil {
			return err
		}
		zap.ReplaceGlobals(logger)
		zap.RedirectStdLog(logger)

		tz, err := time.LoadLocation(timezone)
		if err != nil {
			return err
		}
		time.Local = tz
		return nil
	}

	root.RunE = func(cmd *cobra.Command, args []string) error {
		// validate host tags
		_, err := tagger.CreateTags(hostTagsStrings...)
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(pidFilePath, []byte(strconv.Itoa(os.Getpid())), 0644)
		if err != nil {
			return err
		}
		zap.L().Info("wrote pid file", zap.Int("pid", os.Getpid()), zap.String("file", pidFilePath))

		ctx, cancel := context.WithCancel(context.TODO())
		waitGroup := &sync.WaitGroup{}

		tag := tagger.NewTagger()
		waitGroup.Add(1)
		go func() {
			notifySignals(ctx, cancel, tag)
			waitGroup.Done()
		}()

		waitGroup.Add(1)
		go func() {
			client.Run(ctx)
			waitGroup.Done()
		}()
		// TODO lifecycle of this chan / create outside ? Wrap ?
		defer close(client.ChanSeries)

		errorsChan := make(chan error, len(collectorCatalog))
		defer close(errorsChan)

		for name, newFn := range collectorCatalog {
			select {
			case <-ctx.Done():
				break
			default:
				d := collectionDuration[name]
				if *d <= 0 {
					zap.L().Info("ignoring collector", zap.String("collector", name))
					continue
				}
				config := &collector.Config{
					SeriesCh:        client.ChanSeries,
					Tagger:          tag,
					Host:            hostname,
					CollectInterval: *d,
				}
				c := newFn(config)
				waitGroup.Add(1)
				go func(coll collector.Collector) {
					errorsChan <- collector.RunCollection(ctx, coll)
					waitGroup.Done()
				}(c)
			}
		}
		tags := append(tag.GetUnstable(hostname),
			"ts:"+strconv.FormatInt(time.Now().Unix(), 10),
			"commit:"+version.Revision[:8],
		)
		client.MetricClientUp(hostname, tags...)
		_ = client.UpdateHostTags(ctx, hostTagsStrings)
		select {
		case <-ctx.Done():
		case err := <-errorsChan:
			zap.L().Error("failed to run collection", zap.Error(err))
			cancel()
		}

		ctxShutdown, cancel := context.WithTimeout(context.Background(), time.Second*5)
		_ = client.MetricClientShutdown(ctxShutdown, hostname, tags...)
		cancel()
		waitGroup.Wait()
		return nil
	}
	exitCode := 0
	err := root.Execute()
	if err != nil {
		exitCode = 1
		zap.L().Error("program exit", zap.Error(err), zap.Int("exitCode", exitCode))
	} else {
		zap.L().Info("program exit", zap.Int("exitCode", exitCode))
	}
	_ = zap.L().Sync()
	os.Exit(exitCode)
}
