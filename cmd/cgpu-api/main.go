// Command cgpu-api is the CGPU REST API and job manager (issue B2 / #12).
//
// It runs on the application worker (Ubuntu PC 2), stores jobs in MongoDB, asks the CGPU
// scheduler on the control plane for a GPU slice, launches each placed job as a Kubernetes
// Job from deploy/templates/cgpu-workload-template.yaml, follows it to completion, and keeps
// its logs and checkpoints on local disk for download.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/jobs"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("CGPU_API_LISTEN", ":8080"), "HTTP listen address")
	mongoURI := flag.String("mongo-uri", env("CGPU_MONGO_URI", "mongodb://127.0.0.1:27017"), "MongoDB connection URI")
	mongoDB := flag.String("mongo-db", env("CGPU_MONGO_DB", "cgpu"), "MongoDB database name")
	memStore := flag.Bool("memory-store", false, "keep jobs in memory instead of MongoDB (development only)")
	schedURL := flag.String("scheduler-url", env("CGPU_SCHEDULER_URL", "http://10.20.24.31:8082"), "CGPU scheduler base URL")
	kubectl := flag.String("kubectl", env("CGPU_KUBECTL", "kubectl"), "kubectl binary")
	kubeconfig := flag.String("kubeconfig", env("CGPU_KUBECONFIG", "/etc/cgpu/api-kubeconfig"), "kubeconfig for the cgpu-api ServiceAccount")
	namespace := flag.String("namespace", env("CGPU_NAMESPACE", "cgpu-jobs"), "namespace for workload Jobs")
	tmplPath := flag.String("job-template", env("CGPU_JOB_TEMPLATE", "deploy/templates/cgpu-workload-template.yaml"), "shared Kubernetes Job template")
	artifactsDir := flag.String("artifacts-dir", env("CGPU_ARTIFACTS_DIR", "/var/lib/cgpu/artifacts"), "where job logs and checkpoints are stored")
	publicURL := flag.String("public-url", env("CGPU_PUBLIC_URL", "http://10.20.24.32:8080"), "API address reachable from workload pods (for uploads)")
	agentPort := flag.Int("agent-port", 9400, "cgpu-worker-agent port on GPU workers")
	interval := flag.Duration("reconcile-interval", 3*time.Second, "how often to advance active jobs")
	smoke := flag.Bool("enable-smoke-workload", os.Getenv("CGPU_ENABLE_SMOKE") == "1", "add the CPU-only cgpu-pipeline-smoke workload to the catalog")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store jobs.Store
	if *memStore {
		log.Println("[CGPU-API] WARNING: in-memory store; jobs are lost on restart")
		store = jobs.NewMemoryStore()
	} else {
		ms, err := jobs.NewMongoStore(ctx, *mongoURI, *mongoDB)
		if err != nil {
			log.Fatalf("[CGPU-API] MongoDB: %v", err)
		}
		defer ms.Close(context.Background())
		store = ms
		log.Printf("[CGPU-API] MongoDB connected (%s, db %s)", *mongoURI, *mongoDB)
	}

	tmpl, err := jobs.LoadJobTemplate(*tmplPath)
	if err != nil {
		log.Fatalf("[CGPU-API] job template: %v", err)
	}
	if err := os.MkdirAll(*artifactsDir, 0o750); err != nil {
		log.Fatalf("[CGPU-API] artifacts dir: %v", err)
	}

	m := &jobs.Manager{
		Store:     store,
		Kube:      &jobs.Kubectl{Binary: *kubectl, Kubeconfig: *kubeconfig, Namespace: *namespace},
		Sched:     jobs.NewSchedulerClient(*schedURL),
		Agents:    jobs.NewAgentClient(*agentPort),
		Artifacts: &jobs.ArtifactStore{Root: *artifactsDir},
		Catalog:   jobs.DefaultCatalog(*smoke),
		Template:  tmpl,
		Cfg:       jobs.Config{Namespace: *namespace, APIURL: *publicURL, ImagePullGrace: 5 * time.Minute},
	}
	go m.Run(ctx, *interval)

	srv := &http.Server{Addr: *listen, Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("[CGPU-API] listening on %s (scheduler %s, namespace %s, artifacts %s)", *listen, *schedURL, *namespace, *artifactsDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[CGPU-API] server: %v", err)
		}
	}()
	<-ctx.Done()
	log.Println("[CGPU-API] shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}
