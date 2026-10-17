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
| `--kinds-dir` | – | folder of CRD files; the kinds they define are served (see below) |
| `--kinds-reload` | `2s` | how often the folder is re-scanned (`0` = only at start-up) |
| `--openapi-spec` | – | `swagger.json` whose definitions power `kubectl explain` (see below) |
| `--env-file` | `.env` | file with default settings (see below) |
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
    ├── config/              # .env parsing and flag/env/.env precedence
    │   └── dotenv.go
    ├── openapi/             # swagger.json definitions -> OpenAPI v2 / v3 documents (kubectl explain)
    │   └── openapi.go
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
`openapi`, `objects` -> `registry` -> `store` (`config` is used by `main` only).

## Loading kinds from a folder (no code changes)

Built-in kinds (Pod, Deployment, ...) are compiled in, but **any other kind can
be added at run time with a CustomResourceDefinition file**:

```bash
./kube-emulator --kinds-dir=./examples/kinds
kubectl api-resources | grep example.com
kubectl apply -f examples/widget.yaml
kubectl get widgets          # or: kubectl get wg
kubectl apply -f examples/widget.yaml   # a Widget with spec.size: 99 is rejected by the CRD schema
```

* Put any number of `*.yaml`, `*.yml` or `*.json` files in the folder
  (sub-folders are searched; multi-document files and `kind: List` work). Good
  sources are `kubectl get crd <name> -o yaml` on a real cluster, or the
  `config/crd/bases` folder of an operator repository.
* The folder is read at start-up and re-scanned every `--kinds-reload`:
  a new or changed file creates/updates the CRD and the kind is served
  immediately; a deleted file (or a CRD removed from a file) deletes the CRD
  and all of its resources. Only CRDs that came from the folder are pruned;
  CRDs created with `kubectl` are left alone.
* Files go through the same validation as a CRD sent by `kubectl`, and
  resources are validated against the CRD's `openAPIV3Schema` (status
  subresource and short names are honoured). Files with errors are logged and
  skipped.
* Other kinds of manifests in the folder are ignored. A folder cannot add new
  *built-in* kinds or older API versions of built-in kinds, because those need
  Go types for typed validation.

## Configuration with a .env file

Every flag has a `KUBE_EMULATOR_*` variable. Put defaults in a `.env` file in
the directory you start the emulator from (or choose one with `--env-file`):

```
KUBE_EMULATOR_PORT=9000
KUBE_EMULATOR_STORAGE=sqlite
KUBE_EMULATOR_SQLITE_PATH=./data/emulator.db
KUBE_EMULATOR_KINDS_DIR=./kinds
KUBE_EMULATOR_OPENAPI_SPEC=./swagger.json
```

**Precedence, highest first:** command line flags, real environment variables,
the `.env` file, built-in defaults. So `./kube-emulator --port=8086` wins over
`KUBE_EMULATOR_PORT=9000` in `.env`. A missing default `.env` is ignored; a
missing file given with `--env-file`, or an invalid value, is an error. At
start-up the emulator logs every setting it took from the environment or `.env`.
The file may contain comments, `export KEY=value`, quoted values and Windows
line endings; see `.env.example` for every key.

| Flag | Variable |
|---|---|
| `--port` | `KUBE_EMULATOR_PORT` |
| `--bind-address` | `KUBE_EMULATOR_BIND_ADDRESS` |
| `--storage` | `KUBE_EMULATOR_STORAGE` |
| `--sqlite-path` | `KUBE_EMULATOR_SQLITE_PATH` |
| `--write-kubeconfig` | `KUBE_EMULATOR_WRITE_KUBECONFIG` |
| `--kinds-dir` | `KUBE_EMULATOR_KINDS_DIR` |
| `--kinds-reload` | `KUBE_EMULATOR_KINDS_RELOAD` |
| `--openapi-spec` | `KUBE_EMULATOR_OPENAPI_SPEC` |
| `-v` | `KUBE_EMULATOR_VERBOSE` |

## kubectl explain

`kubectl explain` needs the schemas of the kinds, which the emulator does not
contain. Give it the `definitions` of an OpenAPI v2 `swagger.json`:

```bash
# from a real cluster ...
kubectl get --raw /openapi/v2 > swagger.json
# ... or from the Kubernetes repository: api/openapi-spec/swagger.json

./kube-emulator --openapi-spec=./swagger.json
kubectl explain deployment.spec.strategy
kubectl explain pod --recursive
```

* The file is served as `/openapi/v2`, and per group/version OpenAPI v3
  documents (the format `kubectl explain` uses by default) are derived from the
  same definitions, so both `kubectl explain` and
  `kubectl explain --output=plaintext-openapi-v2` work.
* Custom kinds loaded with `--kinds-dir` (or created from a CRD) are explained
  from the `openAPIV3Schema` of their CRD; no swagger file is needed for them.
* Use a swagger file from a Kubernetes version close to the emulator's reported
  version (v1.31). With a spec loaded, kubectl also validates manifests
  client-side against these schemas, as it does against a real cluster.

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

Built-in resources: namespaces, nodes, pods, replicationcontrollers, podtemplates, services, configmaps, secrets,
serviceaccounts, endpoints, events, persistentvolumes(+claims), resourcequotas,
limitranges, deployments, replicasets, statefulsets, daemonsets, jobs,
cronjobs, horizontalpodautoscalers, ingresses, networkpolicies, ingressclasses,
roles/rolebindings/clusterroles/clusterrolebindings, storageclasses,
poddisruptionbudgets, priorityclasses, leases, controllerrevisions, endpointslices,
runtimeclasses, csidrivers, validating/mutatingwebhookconfigurations (stored only,
never called), customresourcedefinitions.

## Known limitations

* No authentication, authorization, TLS or admission webhooks (plain HTTP).
* Not the full `k8s.io/kubernetes` validation: only the checks listed above
  are implemented, so some invalid objects that a real cluster rejects are
  accepted here.
* Server-side apply does not track field ownership (`managedFields`).
* CronJobs are stored but never fire; HPAs are stored but never scale.
* `kubectl exec`, `attach`, `port-forward` return 501; `kubectl logs` returns
  a placeholder line; `kubectl explain` only works for built-in kinds when `--openapi-spec` is given.
* One API version per group; no conversion between versions. Manifests that use
  an older apiVersion than the one served (for example `autoscaling/v1`
  HorizontalPodAutoscaler instead of `autoscaling/v2`) are rejected with
  "no matches for kind". Only JSON is
  served (no protobuf).
* Pagination (`limit`/`continue`) is ignored; everything is returned at once.
