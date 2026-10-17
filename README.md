# kube-emulator

A fake Kubernetes API server written in Go. It speaks the Kubernetes REST API,
so `kubectl` (and client libraries) work against it, but it **never starts a
container**: objects live in memory or in an SQLite database, and a few
simulated controllers fill in the status a real cluster would report.

Default address: `http://127.0.0.1:8085`.

## Build and run

```bash
go mod tidy                      # resolves indirect dependencies, writes go.sum
go build -o kube-emulator ./cmd/kube-emulator

./kube-emulator                                   # in-memory, port 8085
./kube-emulator --storage=sqlite                  # persisted in ./kube-emulator.db
./kube-emulator --storage=sqlite --sqlite-path=/tmp/k8s.db --port=9000
./kube-emulator --write-kubeconfig=./emulator.kubeconfig
```

| Flag | Default | Meaning |
|---|---|---|
| `--port` | `8085` | listen port |
| `--bind-address` | `127.0.0.1` | listen address (`0.0.0.0` to expose it) |
| `--storage` | `memory` | `memory` or `sqlite` |
| `--sqlite-path` | `kube-emulator.db` | database file for `--storage=sqlite` |
| `--write-kubeconfig` | – | also write a kubeconfig pointing at the server |
| `-v` | off | dump every request (full headers and body) and log its duration |

## Project layout

```
kube-emulator/
├── go.mod
├── cmd/
│   └── kube-emulator/
│       └── main.go          # flags, storage selection, wiring, graceful shutdown
└── internal/
    ├── store/               # in-memory store, resourceVersions, watch, SQLite persistence
    │   ├── store.go
    │   └── sqlite.go
    ├── registry/            # served resources (built-in + CRDs), discovery, OpenAPI stubs
    │   └── registry.go
    ├── objects/             # shared helpers: typed<->unstructured, defaults, validation, pod/IP simulation
    │   ├── convert.go
    │   ├── validate.go
    │   └── sim.go
    ├── controllers/         # simulated controllers and cluster bootstrap
    │   ├── controllers.go   #   Deployment, ReplicaSet, StatefulSet, DaemonSet, Job, Endpoints, GC
    │   ├── bootstrap.go     #   default namespaces, fake node, "kubernetes" service
    │   └── aliases.go
    └── server/              # HTTP layer
        ├── server.go        #   routing, discovery, REST handlers, watch, patch
        ├── admit.go         #   decode, default, validate, create/update pipeline, CRDs
        ├── tables.go        #   server-side Tables for `kubectl get`
        └── aliases.go
```

Dependencies only point downwards: `main` -> `server`, `controllers` ->
`objects` -> `registry` -> `store`.

## Using kubectl

```bash
kubectl --server=http://localhost:8085 get namespaces
# or
export KUBECONFIG=./emulator.kubeconfig
kubectl get nodes
kubectl create namespace demo
kubectl -n demo create deployment web --image=nginx --replicas=3
kubectl -n demo get deploy,rs,pods
kubectl -n demo scale deploy/web --replicas=5
kubectl -n demo rollout status deploy/web
kubectl -n demo expose deploy/web --port=80 --type=NodePort
kubectl -n demo get svc,endpoints
kubectl apply -f my-manifest.yaml
kubectl delete namespace demo          # cascades to everything inside
```

If `kubectl` complains about validation or stale discovery data, try
`--validate=false` and/or `rm -rf ~/.kube/cache`.

## How it works

* **Decoding and validation use real Kubernetes libraries.** Built-in kinds are
  decoded with `k8s.io/apimachinery` serializers on top of `k8s.io/api` types
  (strict mode rejects unknown/duplicate fields, as `fieldValidation=Strict`
  does on a real server). `metadata` is validated with
  `k8s.io/apimachinery/pkg/api/validation` (names, labels, annotations,
  immutability), and the `util/validation` helpers check container names,
  ports, env vars, config keys and so on. Patches use
  `k8s.io/apimachinery/pkg/util/strategicpatch` and JSON/merge patch.
  Custom resources are validated against their CRD's `openAPIV3Schema` with
  the `kube-openapi` validator.
* **Storage.** `store.go` keeps every object in memory with resourceVersions
  and watch support. With `--storage=sqlite` every write is also written to
  SQLite (one JSON document per object) before it is acknowledged, and the
  state is reloaded on start-up.
* **Simulation instead of containers** (`controllers.go`): pods are bound to a
  fake node `emulator-node` and are immediately `Running` with a fake IP;
  Deployments create ReplicaSets (with revisions and rollouts), ReplicaSets
  create pods, StatefulSets, DaemonSets and Jobs are simulated, Services get
  cluster IPs / node ports and Endpoints, and deleting an owner or a namespace
  cascades.
* `kubectl get` receives server-side Tables, so you get the familiar columns.

## Supported

Discovery (`/api`, `/apis`, `/version`), list/get/create/update/patch
(json, merge, strategic, and an approximation of server-side apply)/delete/
deletecollection, watch (with resourceVersion replay), label and field
selectors, `status` and `scale` subresources, `dryRun=All`, finalizers,
`deletionTimestamp`, and CustomResourceDefinitions (create a CRD and its
resources become available immediately).

Built-in resources: namespaces, nodes, pods, services, configmaps, secrets,
serviceaccounts, endpoints, events, persistentvolumes(+claims), resourcequotas,
limitranges, deployments, replicasets, statefulsets, daemonsets, jobs,
cronjobs, horizontalpodautoscalers, ingresses, networkpolicies, ingressclasses,
roles/rolebindings/clusterroles/clusterrolebindings, storageclasses,
poddisruptionbudgets, priorityclasses, leases, customresourcedefinitions.

## Known limitations

* No authentication, authorization, TLS or admission webhooks (plain HTTP).
* Not the full `k8s.io/kubernetes` validation: only the checks listed above
  are implemented, so some invalid objects that a real cluster rejects are
  accepted here.
* Server-side apply does not track field ownership (`managedFields`).
* CronJobs are stored but never fire; HPAs are stored but never scale.
* `kubectl exec`, `attach`, `port-forward` return 501; `kubectl logs` returns
  a placeholder line; `kubectl explain` does not work (no OpenAPI schemas).
* One API version per group; no conversion between versions. Only JSON is
  served (no protobuf).
* Pagination (`limit`/`continue`) is ignored; everything is returned at once.
