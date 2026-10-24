package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/agent"
)

// stopEnginesOptions are the flags of the stop-engines subcommand.
type stopEnginesOptions struct {
	StateDir    string
	Namespace   string
	KubeContext string
	OllamaPort  int
	LogLevel    string
}

const stopEnginesUsage = "llmkube-metal-agent stop-engines [flags]: " +
	"stop every engine this agent started and remove its records\n" +
	"Stop the agent first (launchctl bootout gui/$(id -u)/com.llmkube.metal-agent), then run stop-engines.\n"

// parseStopEnginesArgs parses the subcommand's flags. --kubeconfig is the
// flag controller-runtime registers on the default flag set, so it is added
// here from there and read by loadKubeconfig.
func parseStopEnginesArgs(args []string) (stopEnginesOptions, error) {
	var opts stopEnginesOptions
	fs := flag.NewFlagSet("stop-engines", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(os.Stderr, stopEnginesUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.StateDir, "state-dir", "",
		"Directory for agent state, as for the agent. Default: ~/Library/Application Support/llmkube/metal-agent.")
	fs.StringVar(&opts.Namespace, "namespace", "default",
		"Namespace the agent watched, for log lines only: every recorded engine is stopped")
	fs.StringVar(&opts.KubeContext, "kube-context", "", "kubeconfig context to withdraw endpoints with")
	fs.IntVar(&opts.OllamaPort, "ollama-port", agent.DefaultOllamaPort, "Port of the Ollama daemon")
	fs.StringVar(&opts.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	if kc := flag.Lookup("kubeconfig"); kc != nil {
		fs.Var(kc.Value, "kubeconfig", kc.Usage)
	}
	if err := fs.Parse(args); err != nil {
		return stopEnginesOptions{}, err
	}
	if fs.NArg() > 0 {
		return stopEnginesOptions{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return opts, nil
}

// runStopEnginesCommand runs the stop-engines subcommand and returns the
// process exit code: 0 when every engine is stopped, 1 otherwise, 2 for bad
// flags.
func runStopEnginesCommand(args []string) int {
	opts, err := parseStopEnginesArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	baseLogger, err := newLogger(opts.LogLevel)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		return 1
	}
	defer func() { _ = baseLogger.Sync() }()
	logger := baseLogger.Sugar()

	k8sClient := buildStopEnginesClient(opts.KubeContext)
	if k8sClient == nil {
		logger.Warnw("no Kubernetes client: endpoints are not withdrawn, engines are still stopped")
	}
	err = agent.RunStopEngines(context.Background(), agent.StopEnginesConfig{
		StateDir:   opts.StateDir,
		Namespace:  opts.Namespace,
		K8sClient:  k8sClient,
		OllamaPort: opts.OllamaPort,
		Logger:     logger,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "stop-engines: some engines were not stopped:\n%v\n", err)
		return 1
	}
	_, _ = fmt.Println("stop-engines: every recorded engine is stopped")
	return 0
}

// buildStopEnginesClient returns nil when no client can be built; the caller
// then skips endpoint withdrawal.
func buildStopEnginesClient(kubeContext string) client.Client {
	k8sConfig, err := loadKubeconfig(kubeContext)
	if err != nil {
		return nil
	}
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil
	}
	c, err := client.New(k8sConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil
	}
	return c
}
