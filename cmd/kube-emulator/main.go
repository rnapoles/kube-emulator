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

	"github.com/rnapoles/kube-emulator/internal/controllers"
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/server"
	"github.com/rnapoles/kube-emulator/internal/store"
)

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
		showHelp = flag.Bool("h", false, "show help")
	)
	flag.Parse()
	if *showHelp {
		flag.Usage()
		return
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
	if err := controllers.Bootstrap(st, ipam, *port); err != nil {
		log.Fatalf("bootstrapping cluster objects: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go controllers.New(st, ipam).Run(ctx)

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

	host := *bind
	addr := net.JoinHostPort(host, strconv.Itoa(*port))
	serverURL := "http://" + net.JoinHostPort(displayHost(host), strconv.Itoa(*port))

	if *kubecfg != "" {
		if err := writeKubeconfig(*kubecfg, serverURL); err != nil {
			log.Fatalf("writing kubeconfig: %v", err)
		}
	}

	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			_ = httpSrv.Close() // long-lived watches did not finish in time
		}
	}()

	if *storage == "sqlite" {
		log.Printf("storage: SQLite (%s)", *dbPath)
	} else {
		log.Printf("storage: memory (state is lost on exit)")
	}
	log.Printf("emulated Kubernetes API listening on %s", serverURL)
	fmt.Printf("\ntry:  kubectl --server=%s get namespaces\n", serverURL)
	if *kubecfg != "" {
		fmt.Printf("or:   KUBECONFIG=%s kubectl get nodes\n", *kubecfg)
	}
	fmt.Println()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
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
