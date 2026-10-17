// kube-emulator is a fake Kubernetes API server. It speaks the Kubernetes REST
// API (so kubectl and client libraries work against it) but never starts a
// container: objects only live in memory or in an SQLite database, and a few
// simulated controllers fill in the status a real cluster would report.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rnapoles/kube-emulator/internal/config"
	"github.com/rnapoles/kube-emulator/internal/controllers"
	"github.com/rnapoles/kube-emulator/internal/events"
	"github.com/rnapoles/kube-emulator/internal/kubesim"
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/openapi"
	"github.com/rnapoles/kube-emulator/internal/server"
	"github.com/rnapoles/kube-emulator/internal/store"
	"github.com/rnapoles/kube-emulator/internal/trace"
	"github.com/rnapoles/kube-emulator/internal/tui"
)

// flagEnvKeys maps each flag to the environment variable / .env key that sets it.
var flagEnvKeys = map[string]string{
	"bind-address":     "KUBE_EMULATOR_BIND_ADDRESS",
	"port":             "KUBE_EMULATOR_PORT",
	"storage":          "KUBE_EMULATOR_STORAGE",
	"sqlite-path":      "KUBE_EMULATOR_SQLITE_PATH",
	"write-kubeconfig": "KUBE_EMULATOR_WRITE_KUBECONFIG",
	"kinds-dir":        "KUBE_EMULATOR_KINDS_DIR",
	"kinds-reload":     "KUBE_EMULATOR_KINDS_RELOAD",
	"openapi-spec":     "KUBE_EMULATOR_OPENAPI_SPEC",
	"nodes":            "KUBE_EMULATOR_NODES",
	"node-cpu":         "KUBE_EMULATOR_NODE_CPU",
	"node-memory":      "KUBE_EMULATOR_NODE_MEMORY",
	"node-pods":        "KUBE_EMULATOR_NODE_PODS",
	"delays":           "KUBE_EMULATOR_DELAYS",
	"seed-dir":         "KUBE_EMULATOR_SEED_DIR",
	"tui":              "KUBE_EMULATOR_TUI",
	"v":                "KUBE_EMULATOR_VERBOSE",
}

func main() {
	var (
		bind     = flag.String("bind-address", "127.0.0.1", "address to listen on (use 0.0.0.0 to expose the emulator on the network)")
		port     = flag.Int("port", 8085, "port to listen on")
		storage  = flag.String("storage", "memory", `where objects are kept: "memory" or "sqlite"`)
		dbPath   = flag.String("sqlite-path", "kube-emulator.db", "SQLite database file (used with --storage=sqlite)")
		kubecfg  = flag.String("write-kubeconfig", "", "also write a kubeconfig that points at this server to the given file")
		verbose  = flag.Bool("v", false, "verbose: dump every request (full headers and body) and log its duration")
		kindsDir = flag.String("kinds-dir", "", "folder with CustomResourceDefinition files (YAML/JSON); the kinds they define are served without code changes")
		kindsInt = flag.Duration("kinds-reload", 2*time.Second, "how often --kinds-dir is re-scanned for changes (0 = load once at start-up)")
		specPath = flag.String("openapi-spec", "", "swagger.json (OpenAPI v2) whose definitions are served so that `kubectl explain` works for built-in kinds")
		nodes    = flag.Int("nodes", 1, "number of fake nodes to create (emulator-node-1..N, spread over three zones)")
		nodeCPU  = flag.String("node-cpu", "8", "CPU capacity of generated nodes and of nodes created without capacity")
		nodeMem  = flag.String("node-memory", "16Gi", "memory capacity of generated nodes and of nodes created without capacity")
		nodePods = flag.Int("node-pods", 110, "maximum number of pods per node")
		delays   = flag.String("delays", "", `pod lifecycle delays, for example "pull=3s,ready=5s" (names: schedule, pull, start, ready, job, terminate, backoff) or "instant"`)
		seedDir  = flag.String("seed-dir", "", "folder with manifests (nodes, namespaces, workloads, ...) created at start-up if they do not exist yet")
		tuiFlag  = flag.Bool("tui", false, "show a terminal UI: live list of requests and responses with filters and a JSON inspector")
		envFile  = flag.String("env-file", ".env", "file with default settings (KUBE_EMULATOR_*=value); command line flags take precedence")
		showHelp = flag.Bool("h", false, "show help")
	)
	flag.Parse()
	if *showHelp {
		flag.Usage()
		return
	}

	// Defaults from the .env file and the environment. Flags given on the
	// command line always win, so they are skipped here.
	cli := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { cli[f.Name] = true })
	dotenv, err := config.LoadFile(*envFile)
	if err != nil {
		if cli["env-file"] || !errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("reading env file: %v", err)
		}
		dotenv = nil // no .env next to us: nothing to load
	}
	applied, err := config.Apply(flag.CommandLine, flagEnvKeys, dotenv, cli)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	for _, a := range applied {
		log.Printf("config: %s", a)
	}

	var persister store.Persister
	switch *storage {
	case "memory":
	case "sqlite":
		sp, err := store.NewSQLitePersister(*dbPath)
		if err != nil {
			log.Fatalf("opening SQLite database %s: %v", *dbPath, err)
		}
		persister = sp
	default:
		log.Fatalf("unknown --storage %q (use memory or sqlite)", *storage)
	}

	st, err := store.NewStore(persister)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ipam := objects.NewIPAM(st.CurrentRV())
	srv := server.New(st, ipam, *verbose)
	if *specPath != "" {
		spec, err := openapi.Load(*specPath)
		if err != nil {
			log.Fatalf("loading --openapi-spec: %v", err)
		}
		srv.SetOpenAPI(spec)
		log.Printf("openapi: loaded %d definitions from %s", spec.NumDefinitions(), *specPath)
	}
	if err := controllers.Bootstrap(st, *port); err != nil {
		log.Fatalf("bootstrapping cluster objects: %v", err)
	}

	nodeOpts := kubesim.NodeOptions{Count: *nodes, CPU: *nodeCPU, Memory: *nodeMem, Pods: *nodePods}
	if err := nodeOpts.Validate(); err != nil {
		log.Fatal(err)
	}
	if err := kubesim.EnsureNodes(st, nodeOpts); err != nil {
		log.Fatalf("creating nodes: %v", err)
	}
	lifecycle, err := kubesim.ParseDelays(*delays, kubesim.DefaultDelays())
	if err != nil {
		log.Fatalf("--delays: %v", err)
	}
	evrec := events.NewRecorder(st)
	sim := kubesim.New(st, evrec, ipam, lifecycle, nodeOpts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sim.Run(ctx)
	go controllers.New(st, evrec).Run(ctx)

	if *kindsDir != "" {
		loader := srv.NewKindsLoader(*kindsDir)
		if err := loader.Sync(); err != nil {
			log.Fatalf("loading kinds from %s: %v", *kindsDir, err)
		}
		if *kindsInt > 0 {
			go loader.Watch(ctx, *kindsInt)
		}
		log.Printf("kinds: loading CustomResourceDefinitions from %s", *kindsDir)
	}
	if *seedDir != "" {
		if err := srv.SeedDir(*seedDir); err != nil {
			log.Fatalf("seeding from %s: %v", *seedDir, err)
		}
	}

	host := *bind
	addr := net.JoinHostPort(host, strconv.Itoa(*port))
	serverURL := "http://" + net.JoinHostPort(displayHost(host), strconv.Itoa(*port))

	if *kubecfg != "" {
		if err := writeKubeconfig(*kubecfg, serverURL); err != nil {
			log.Fatalf("writing kubeconfig: %v", err)
		}
	}

	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			_ = httpSrv.Close() // long-lived watches did not finish in time
		}
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listening on %s: %v", addr, err)
	}

	// With --tui the terminal belongs to the UI: requests are recorded for it
	// and log output is collected instead of printed.
	var rec *trace.Recorder
	var logs *tui.LogBuffer
	if *tuiFlag {
		rec = trace.NewRecorder(2000, 256<<10)
		httpSrv.Handler = rec.Middleware(srv)
		logs = tui.NewLogBuffer(1000)
		log.SetOutput(logs)
	}

	log.Printf("cluster: %d fake node(s), pod lifecycle delays %+v", *nodes, lifecycle)
	if *storage == "sqlite" {
		log.Printf("storage: SQLite (%s)", *dbPath)
	} else {
		log.Printf("storage: memory (state is lost on exit)")
	}
	log.Printf("emulated Kubernetes API listening on %s", serverURL)

	if *tuiFlag {
		go func() {
			if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("server: %v", err)
				stop()
			}
		}()
		if err := tui.Run(ctx, tui.Options{Rec: rec, Logs: logs, URL: serverURL}); err != nil {
			log.SetOutput(os.Stderr)
			log.Printf("terminal UI: %v", err)
		}
		stop() // quitting the UI stops the emulator
		<-shutdownDone
		return
	}

	fmt.Printf("\ntry:  kubectl --server=%s get namespaces\n", serverURL)
	if *kubecfg != "" {
		fmt.Printf("or:   KUBECONFIG=%s kubectl get nodes\n", *kubecfg)
	}
	fmt.Println()

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

func displayHost(bind string) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return "localhost"
	}
	return bind
}

func writeKubeconfig(path, serverURL string) error {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["emulator"] = &clientcmdapi.Cluster{Server: serverURL}
	cfg.AuthInfos["emulator"] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["emulator"] = &clientcmdapi.Context{Cluster: "emulator", AuthInfo: "emulator", Namespace: "default"}
	cfg.CurrentContext = "emulator"
	return clientcmd.WriteToFile(*cfg, path)
}
