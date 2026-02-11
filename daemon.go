package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

var debug bool

func logf(format string, args ...any) {
	if debug {
		log.Printf(format, args...)
	}
}

// csvHeader is the standard header for the stats CSV file.
var csvHeader = []string{"timestamp", "container", "cpu_pct", "mem_usage_mb", "mem_limit_mb", "mem_pct"}

// openCSV opens (or creates) the CSV file and writes the header if the file is new/empty.
// It returns the file handle and a csv.Writer ready for appending rows.
func openCSV(path string) (*os.File, *csv.Writer, error) {
	info, err := os.Stat(path)
	needHeader := os.IsNotExist(err) || (err == nil && info.Size() == 0)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("open csv: %w", err)
	}

	w := csv.NewWriter(f)
	if needHeader {
		if err := w.Write(csvHeader); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("write csv header: %w", err)
		}
		w.Flush()
	}
	return f, w, nil
}

// writeRow writes a single stats row and flushes.
func writeRow(w *csv.Writer, ts time.Time, name string, cpuPct, memUsageMB, memLimitMB, memPct float64) {
	w.Write([]string{
		ts.Format(time.RFC3339),
		name,
		fmt.Sprintf("%.2f", cpuPct),
		fmt.Sprintf("%.2f", memUsageMB),
		fmt.Sprintf("%.2f", memLimitMB),
		fmt.Sprintf("%.2f", memPct),
	})
	w.Flush()
}

// --- Docker daemon ---

type dockerStatsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage float64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage float64 `json:"system_cpu_usage"`
		OnlineCPUs     float64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage float64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage float64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage float64            `json:"usage"`
		Limit float64            `json:"limit"`
		Stats  map[string]float64 `json:"stats"`
	} `json:"memory_stats"`
}

func calcDockerCPU(s *dockerStatsJSON) float64 {
	cpuDelta := s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage
	sysDelta := s.CPUStats.SystemCPUUsage - s.PreCPUStats.SystemCPUUsage
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	numCPUs := s.CPUStats.OnlineCPUs
	if numCPUs == 0 {
		numCPUs = 1
	}
	return (cpuDelta / sysDelta) * numCPUs * 100.0
}

func calcDockerMem(s *dockerStatsJSON) (usageMB, limitMB, pct float64) {
	usage := s.MemoryStats.Usage
	// Subtract cache: cgroup v2 uses inactive_file, v1 uses cache.
	if inactiveFile, ok := s.MemoryStats.Stats["inactive_file"]; ok && inactiveFile > 0 {
		usage -= inactiveFile
	} else if cache, ok := s.MemoryStats.Stats["cache"]; ok && cache > 0 {
		usage -= cache
	}
	if usage < 0 {
		usage = 0
	}
	limit := s.MemoryStats.Limit
	usageMB = usage / (1024 * 1024)
	limitMB = limit / (1024 * 1024)
	if limit > 0 {
		pct = (usage / limit) * 100.0
	}
	return
}

func containerName(names []string) string {
	for _, n := range names {
		return strings.TrimPrefix(n, "/")
	}
	return "unknown"
}

func runDockerDaemon(stopCh <-chan struct{}, interval int, outfile string) error {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	// Verify connectivity.
	if _, err := cli.Ping(context.Background()); err != nil {
		return fmt.Errorf("cannot reach Docker daemon: %w", err)
	}

	f, w, err := openCSV(outfile)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Printf("Collecting Docker stats every %ds -> %s (Ctrl+C to stop)\n", interval, outfile)
	logf("Docker daemon started: interval=%ds, outfile=%s", interval, outfile)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	stopped := func() bool {
		select {
		case <-stopCh:
			return true
		default:
			return false
		}
	}

	collect := func() {
		if stopped() {
			return
		}
		containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
		if err != nil {
			logf("ContainerList error: %v", err)
			return
		}
		ts := time.Now().UTC()

		type result struct {
			name                          string
			cpuPct, memUsage, memLimit, memPct float64
		}

		results := make([]result, len(containers))
		var wg sync.WaitGroup

		for i := range containers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := containers[i]
				name := containerName(c.Names)

				resp, err := cli.ContainerStats(context.Background(), c.ID, false)
				if err != nil {
					logf("ContainerStats(%s) error: %v", name, err)
					return
				}
				var stats dockerStatsJSON
				if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
					resp.Body.Close()
					logf("decode stats(%s) error: %v", name, err)
					return
				}
				resp.Body.Close()

				memUsage, memLimit, memPct := calcDockerMem(&stats)
				results[i] = result{
					name:     name,
					cpuPct:   calcDockerCPU(&stats),
					memUsage: memUsage,
					memLimit: memLimit,
					memPct:   memPct,
				}
			}(i)
		}
		wg.Wait()

		for _, r := range results {
			if r.name == "" {
				continue
			}
			writeRow(w, ts, r.name, r.cpuPct, r.memUsage, r.memLimit, r.memPct)
			logf("  %s  cpu=%.2f%%  mem=%.1f/%.1f MB (%.2f%%)",
				r.name, r.cpuPct, r.memUsage, r.memLimit, r.memPct)
		}
	}

	// Collect immediately, then on ticker.
	collect()
	for {
		select {
		case <-stopCh:
			logf("Docker daemon stopped")
			return nil
		case <-ticker.C:
			collect()
		}
	}
}

// --- Kubernetes daemon ---

func runK8sDaemon(stopCh <-chan struct{}, interval int, outfile, namespace, selector, kubeContext string) error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		configOverrides.CurrentContext = kubeContext
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	metricsClient, err := metricsv.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("metrics client: %w", err)
	}

	f, w, err := openCSV(outfile)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Printf("Collecting Kubernetes stats every %ds -> %s (Ctrl+C to stop)\n", interval, outfile)
	logf("Kubernetes daemon started: interval=%ds, namespace=%s, selector=%q, outfile=%s",
		interval, namespace, selector, outfile)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	collect := func() {
		listOpts := metav1.ListOptions{}
		if selector != "" {
			listOpts.LabelSelector = selector
		}

		pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), listOpts)
		if err != nil {
			logf("Pods.List error: %v", err)
			return
		}

		// Build limits map: namespace/pod/container -> (cpuMillis, memBytes).
		type limits struct {
			cpuMillis int64
			memBytes  int64
		}
		limitsMap := make(map[string]limits)
		for _, pod := range pods.Items {
			for _, c := range pod.Spec.Containers {
				key := pod.Namespace + "/" + pod.Name + "/" + c.Name
				var lim limits
				if cpuLim, ok := c.Resources.Limits["cpu"]; ok {
					lim.cpuMillis = cpuLim.MilliValue()
				}
				if memLim, ok := c.Resources.Limits["memory"]; ok {
					lim.memBytes = memLim.Value()
				}
				limitsMap[key] = lim
			}
		}

		podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(namespace).List(context.Background(), listOpts)
		if err != nil {
			logf("PodMetrics.List error: %v", err)
			return
		}

		ts := time.Now().UTC()
		for _, pm := range podMetrics.Items {
			for _, cm := range pm.Containers {
				key := pm.Namespace + "/" + pm.Name + "/" + cm.Name
				displayName := pm.Namespace + "/" + pm.Name

				cpuUsedMillis := cm.Usage.Cpu().MilliValue()
				memUsedBytes := cm.Usage.Memory().Value()

				memUsageMB := float64(memUsedBytes) / (1024 * 1024)
				var memLimitMB, memPct, cpuPct float64

				if lim, ok := limitsMap[key]; ok {
					if lim.cpuMillis > 0 {
						cpuPct = float64(cpuUsedMillis) / float64(lim.cpuMillis) * 100.0
					}
					if lim.memBytes > 0 {
						memLimitMB = float64(lim.memBytes) / (1024 * 1024)
						memPct = float64(memUsedBytes) / float64(lim.memBytes) * 100.0
					}
				}

				writeRow(w, ts, displayName, cpuPct, memUsageMB, memLimitMB, memPct)
				logf("  %s  cpu=%.2f%%  mem=%.1f/%.1f MB (%.2f%%)",
					displayName, cpuPct, memUsageMB, memLimitMB, memPct)
			}
		}
	}

	// Collect immediately, then on ticker.
	collect()
	for {
		select {
		case <-stopCh:
			logf("Kubernetes daemon stopped")
			return nil
		case <-ticker.C:
			collect()
		}
	}
}

// --- Entrypoint ---

func runDaemon(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, `Usage: cstats daemon <docker|kubernetes> [flags]

Subcommands:
  docker       Collect Docker container stats via Docker Engine API
  kubernetes   Collect Kubernetes pod stats via metrics API

Run "cstats daemon <subcommand> -h" for subcommand-specific flags.
`)
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logf("Received shutdown signal")
		close(stopCh)
	}()

	sub := args[0]
	switch sub {
	case "docker":
		fs := flag.NewFlagSet("daemon docker", flag.ExitOnError)
		interval := fs.Int("interval", 5, "Collection interval in seconds")
		outfile := fs.String("outfile", "docker-stats.csv", "Output CSV file path")
		debugFlag := fs.Bool("debug", false, "Enable debug logging")
		fs.Parse(args[1:])
		debug = *debugFlag

		if err := runDockerDaemon(stopCh, *interval, *outfile); err != nil {
			log.Fatalf("docker daemon: %v", err)
		}

	case "kubernetes", "k8s":
		fs := flag.NewFlagSet("daemon kubernetes", flag.ExitOnError)
		interval := fs.Int("interval", 5, "Collection interval in seconds")
		outfile := fs.String("outfile", "k8s-stats.csv", "Output CSV file path")
		namespace := fs.String("namespace", "", "Kubernetes namespace (empty = all namespaces)")
		selector := fs.String("selector", "", "Label selector (e.g. app=web)")
		kubeContext := fs.String("context", "", "Kubeconfig context to use")
		debugFlag := fs.Bool("debug", false, "Enable debug logging")
		fs.Parse(args[1:])
		debug = *debugFlag

		if err := runK8sDaemon(stopCh, *interval, *outfile, *namespace, *selector, *kubeContext); err != nil {
			log.Fatalf("kubernetes daemon: %v", err)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown daemon subcommand: %s\nUse 'docker' or 'kubernetes'.\n", sub)
		os.Exit(1)
	}
}

// Ensure io is used (it's used in the main file already, but we import it here too for resp.Body).
var _ io.Reader
