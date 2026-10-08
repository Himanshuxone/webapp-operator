# webapp-operator

A Kubernetes operator written in Go with **controller-runtime**. It defines two custom resources and a controller for each:

| Kind | Purpose | What the controller creates |
|---|---|---|
| `WebApp` | Run a web app from an image, replica count and port | A `Deployment` |
| `BatchJob` | HPC-style gang of N workers that run together | An Indexed `Job` plus a headless `Service` |

The project is a learning vehicle for how Kubernetes custom controllers work: types, code generation, CRDs, reconcile loops, owner references, finalizers, status, leader election and tests. Commands are written for **Windows PowerShell**; WSL2 notes are included where Windows is a poor fit.

**Part 1** (sections 1-18) builds and runs the operator locally. **Part 2** (sections 19-30) containerises it and deploys it to AWS with Docker and a GitHub Actions CI/CD pipeline.

> **Note:** the code in this guide was written without being compiled in your environment. Run `go build ./...` after each step and fix any version-specific differences.

---

## Table of contents

1. [Big picture](#1-big-picture)
2. [Prerequisites (Windows)](#2-prerequisites-windows)
3. [Project layout](#3-project-layout)
4. [Step 1: Scaffold the module](#4-step-1-scaffold-the-module)
5. [Step 2: Register the API group](#5-step-2-register-the-api-group)
6. [Step 3: Define the types](#6-step-3-define-the-types)
7. [Step 4: Generate code and the CRD](#7-step-4-generate-code-and-the-crd)
8. [Step 5: Write the reconciler](#8-step-5-write-the-reconciler)
9. [Step 6: Wire the manager](#9-step-6-wire-the-manager)
10. [Step 7: Run it on a local cluster](#10-step-7-run-it-on-a-local-cluster)
11. [Step 8: kubectl walkthrough and what happens in the code](#11-step-8-kubectl-walkthrough-and-what-happens-in-the-code)
12. [Modifying an existing CRD](#12-modifying-an-existing-crd)
13. [Creating a brand-new CRD (checklist)](#13-creating-a-brand-new-crd-checklist)
14. [The BatchJob resource](#14-the-batchjob-resource)
15. [Testing](#15-testing)
16. [Troubleshooting](#16-troubleshooting)
17. [Concepts glossary](#17-concepts-glossary)
18. [Further reading](#18-further-reading)

**Part 2: Deploy to AWS**

19. [Deployment architecture, costs and why not EKS](#19-deployment-architecture-costs-and-why-not-eks)
20. [Prerequisites for Part 2](#20-prerequisites-for-part-2)
21. [Step 9: Make the app container-ready](#21-step-9-make-the-app-container-ready)
22. [Step 10: Kubernetes manifests (kustomize)](#22-step-10-kubernetes-manifests-kustomize)
23. [Step 11: Test the image and manifests in kind first](#23-step-11-test-the-image-and-manifests-in-kind-first)
24. [Step 12: Create the AWS resources (one-time)](#24-step-12-create-the-aws-resources-one-time)
25. [Step 13: Let GitHub Actions assume an AWS role (OIDC)](#25-step-13-let-github-actions-assume-an-aws-role-oidc)
26. [Step 14: Create the GitHub repository and its variables](#26-step-14-create-the-github-repository-and-its-variables)
27. [Step 15: The deploy script and the workflow](#27-step-15-the-deploy-script-and-the-workflow)
28. [Step 16: First deploy and verification](#28-step-16-first-deploy-and-verification)
29. [Troubleshooting the deployment](#29-troubleshooting-the-deployment)
30. [Day-2 operations, rollback, teardown and EKS](#30-day-2-operations-rollback-teardown-and-eks)

---

## 1. Big picture

A **CustomResourceDefinition (CRD)** teaches the Kubernetes API server a new resource type. A **controller** is a program that watches objects of that type and works to make the real world match what each object asks for. The pair (CRD + controller) is called an **operator**.

```
                 kubectl apply -f webapp.yaml
                              │
                              v
   ┌──────────────────────────────────────────────────────────┐
   │ kube-apiserver  (authn → RBAC → defaults → validation)   │
   │        │ persists to etcd, answers "created"             │
   └────────┼─────────────────────────────────────────────────┘
            │  LIST + WATCH (long-lived HTTP stream)
            v
   ┌──────────────────────────────────────────────────────────┐
   │ webapp-operator process (ctrl.Manager)                   │
   │                                                          │
   │  Reflector → DeltaFIFO → Indexer (in-memory cache)       │
   │                  │                                       │
   │          event handlers (For / Owns)                     │
   │                  │ enqueue "namespace/name"              │
   │                  v                                       │
   │             work queue ──> Reconcile(ctx, req)           │
   │                              │ reads from cache          │
   │                              │ writes to API server      │
   └──────────────────────────────┼───────────────────────────┘
                                  v
                    Deployment / Job / Service created
                                  │
                kube-controller-manager + scheduler + kubelet
                       turn those into running pods
```

Key ideas used throughout:

- **Declarative desired state.** The user writes `spec`; the controller writes `status`.
- **Level-triggered reconciliation.** `Reconcile` receives only an object's *name*, never "what changed". It reads current state and converges. It must be safe to run any number of times (idempotent).
- **Eventual consistency.** `kubectl apply` returns as soon as the object is *stored*. Everything after that happens asynchronously.

---

## 2. Prerequisites (Windows)

```powershell
winget install GoLang.Go
winget install Kubernetes.kubectl
winget install Docker.DockerDesktop      # enable the WSL2 backend
winget install Kubernetes.kind
```

Open a new terminal afterwards so `PATH` updates are picked up.

| Tool | Why |
|---|---|
| Go | Build the operator |
| kubectl | Talk to the cluster |
| Docker Desktop | kind runs Kubernetes nodes as containers |
| kind | Local Kubernetes cluster for development |
| controller-gen | Generates DeepCopy methods, the CRD YAML and RBAC (installed in Step 1) |

**Windows-specific notes**

- Use `;` not `&&` to chain commands in Windows PowerShell 5.1.
- `envtest` (the real API-server test harness) is unreliable on native Windows. Run those tests in **WSL2**, with the repo cloned into the Linux filesystem (`~/webapp-operator`), not under `/mnt/c/...`.
- Add a `.gitattributes` so generated files keep LF line endings:

```gitattributes
* text=auto eol=lf
```

---

## 3. Project layout

```
webapp-operator\
├── api\v1alpha1\
│   ├── groupversion_info.go       # API group/version + SchemeBuilder
│   ├── webapp_types.go            # WebApp Go types
│   ├── batchjob_types.go          # BatchJob Go types
│   └── zz_generated.deepcopy.go   # GENERATED, never hand-edit
├── cmd\main.go                    # starts the manager
├── internal\controller\
│   ├── webapp_controller.go
│   ├── batchjob_controller.go
│   └── *_test.go
├── config\
│   ├── crd\bases\                 # GENERATED CRD YAML
│   ├── crd\kustomization.yaml      # (Part 2)
│   ├── rbac\                      # role.yaml GENERATED; ServiceAccount, bindings (Part 2)
│   ├── manager\                   # operator Deployment (Part 2)
│   ├── default\                   # namespace + kustomize entry point (Part 2)
│   └── samples\                   # example custom resources
├── deploy\remote-deploy.sh         # runs on the EC2 instance (Part 2)
├── .github\workflows\ci-cd.yaml   # CI/CD pipeline (Part 2)
├── Dockerfile                      # (Part 2)
├── .dockerignore  .gitignore  .gitattributes
├── go.mod  go.sum
└── README.md
```

---

## 4. Step 1: Scaffold the module

```powershell
mkdir webapp-operator; cd webapp-operator
go mod init github.com/Himanshuxone/webapp-operator
go get sigs.k8s.io/controller-runtime@latest
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

# go install does not edit PATH. Add it for this session:
$env:Path += ";$(go env GOPATH)\bin"
controller-gen --version

New-Item -ItemType Directory -Force api\v1alpha1, cmd, internal\controller, config\samples
```

**What happens**

- `go mod init` creates `go.mod` and sets the module path. Every internal import uses this prefix, so it must match everywhere (this README uses `github.com/Himanshuxone/webapp-operator`).
- `go get` records `controller-runtime` (and, transitively, `client-go` and `apimachinery`) as dependencies.
- `go install` compiles `controller-gen.exe` into `%USERPROFILE%\go\bin`.

---

## 5. Step 2: Register the API group

`api/v1alpha1/groupversion_info.go`

```go
// +kubebuilder:object:generate=true
// +groupName=apps.example.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "apps.example.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
```

**What happens**

- The `// +...` lines are **markers**: comments that `controller-gen` reads. `object:generate=true` asks for DeepCopy methods for every type in the package; `groupName` sets the API group.
- `GroupVersion` becomes the `apiVersion:` you write in YAML (`apps.example.io/v1alpha1`).
- A **Scheme** is a registry mapping Go types to Group/Version/Kind. `SchemeBuilder` collects your types, and `AddToScheme` is called from `main.go` so the client can encode and decode them. Without registration you get `no kind is registered for the type ...`.

---

## 6. Step 3: Define the types

`api/v1alpha1/webapp_types.go` (condensed; see the source file for the full version)

```go
type WebAppSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`
}

type WebAppStatus struct {
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type WebApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebAppSpec   `json:"spec,omitempty"`
	Status            WebAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebApp `json:"items"`
}

func init() { SchemeBuilder.Register(&WebApp{}, &WebAppList{}) }
```

**What happens and why**

| Element | Effect |
|---|---|
| `Spec` / `Status` split | Users own `spec` (desired); the controller owns `status` (observed) |
| `json:"image"` without `omitempty` | The field is **required** in the generated schema |
| `*int32` for `Replicas` | A pointer distinguishes "not set" from an explicit `0` (scale to zero) |
| `+kubebuilder:validation:*` | Becomes OpenAPI v3 validation in the CRD, so the **API server rejects bad input** before the controller sees it |
| `+kubebuilder:default=...` | The API server fills in the default |
| `+kubebuilder:subresource:status` | Enables the `/status` endpoint so `spec` and `status` have separate write paths and RBAC |
| `printcolumn` | Customises `kubectl get webapps` output |
| `metav1.TypeMeta` (inline) | Supplies `apiVersion` and `kind` |
| `metav1.ObjectMeta` (embedded, **no field name**) | Supplies name, namespace, labels, finalizers, ownerReferences, and the `GetName()` / `GetAnnotations()` methods that make the type a `client.Object` |
| `WebAppList` | Required; informers and `kubectl get` need a list type |

> Embedding matters. Writing `ObjectMeta metav1.ObjectMeta` (a named field) instead of `metav1.ObjectMeta` breaks the promoted methods and produces errors like `missing method GetAnnotations`.

---

## 7. Step 4: Generate code and the CRD

Run the generators **before** building, then build. Point at the package directory, which avoids the `no Go files` error that the `./api/...` pattern can trigger when `api\` itself has no `.go` files.

```powershell
# 1. DeepCopy methods -> api\v1alpha1\zz_generated.deepcopy.go
controller-gen object paths=./api/v1alpha1

# 2. CRD YAML -> config\crd\bases\apps.example.io_webapps.yaml
controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases

# 3. RBAC ClusterRole from +kubebuilder:rbac markers -> config\rbac\role.yaml
controller-gen rbac:roleName=webapp-manager-role paths=./internal/controller output:rbac:artifacts:config=config/rbac

# 4. Tidy dependencies and compile
go mod tidy
go build ./...
```

**What happens**

- `object` writes `DeepCopy`, `DeepCopyInto` and `DeepCopyObject` for each type. Kubernetes caches hand out shared pointers, so code must copy before mutating; `DeepCopyObject` is also what makes a type satisfy `runtime.Object`.
- `crd` converts Go structs plus markers into an installable `CustomResourceDefinition` with an OpenAPI v3 schema.
- `rbac` turns `+kubebuilder:rbac` comments into a `ClusterRole`.
- **Silent success is normal.** Check that the output files exist.

**Rule:** after *any* edit under `api/` (field, marker, type name), re-run `object` and `crd`. Both outputs are snapshots of your Go types. If a stale `zz_generated.deepcopy.go` refers to types that no longer exist, delete it and regenerate.

---

## 8. Step 5: Write the reconciler

`internal/controller/webapp_controller.go` (key parts)

```go
func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	webapp := &appsv1alpha1.WebApp{}
	if err := r.Get(ctx, req.NamespacedName, webapp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)      // deleted: nothing to do
	}
	if !webapp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, webapp)                  // finalizer path
	}
	if !controllerutil.ContainsFinalizer(webapp, finalizerName) {
		controllerutil.AddFinalizer(webapp, finalizerName)
		if err := r.Update(ctx, webapp); err != nil { return ctrl.Result{}, err }
	}
	deploy, err := r.reconcileDeployment(ctx, webapp)          // create or update child
	if err != nil { return ctrl.Result{}, err }
	return ctrl.Result{}, r.updateStatus(ctx, webapp, deploy)  // report what we observed
}
```

### What each part does

| Piece | What happens | Why |
|---|---|---|
| `r.Get` | Reads from the **informer cache** (memory, no network) | Fast, but can be slightly stale |
| `IgnoreNotFound` | A deleted object is not an error | Retrying a deleted key would loop forever |
| Finalizer added first | `AddFinalizer` + `Update` before creating children | If we crashed after creating children but before adding it, a delete could skip cleanup |
| `controllerutil.CreateOrUpdate` | GET the child; if missing run the mutate function then CREATE; if present run mutate and UPDATE **only if something changed** | Idempotency |
| Mutate-in-place | Edit `Containers[0]` instead of replacing the slice | Server-applied defaults (`terminationMessagePath`, `imagePullPolicy`...) are preserved, so a steady state shows no diff and no Update. Replacing the slice causes an endless update loop |
| `SetControllerReference` | Writes `ownerReferences` with `controller: true`, `blockOwnerDeletion: true` | Enables garbage collection **and** lets `Owns()` map Deployment events back to the WebApp |
| Selector guard | Set `spec.selector` only when `CreationTimestamp.IsZero()` | A Deployment's selector is immutable after creation |
| `updateStatus` | Copies `ReadyReplicas` and sets an `Available` condition | `meta.SetStatusCondition` only changes `lastTransitionTime` when the status flips |
| `equality.Semantic.DeepEqual` guard | Skip the write when nothing changed | Prevents wasted calls and event churn |
| `r.Status().Update` | Writes to the `/status` subresource | Cannot modify `spec`; separate RBAC |
| Return `err` | Work queue retries with exponential backoff | Never swallow real errors |

### Registering what to watch

```go
func (r *WebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.WebApp{}).     // every WebApp event enqueues that WebApp's key
		Owns(&appsv1.Deployment{}).      // Deployment events enqueue the OWNER WebApp's key
		Complete(r)
}
```

This is **registration only**; no network call happens here. The actual watch connection is opened later by the informer's Reflector when `mgr.Start` runs.

### How an owner reference works

After the first reconcile the Deployment carries:

```yaml
metadata:
  ownerReferences:
  - apiVersion: apps.example.io/v1alpha1
    kind: WebApp
    name: demo
    uid: 5b3c1e0a-...          # distinguishes a recreated "demo" from the old one
    controller: true           # THE managing controller (only one allowed)
    blockOwnerDeletion: true
```

It does two jobs: **garbage collection** (deleting the WebApp deletes the Deployment, its ReplicaSet and its pods) and **event routing** (`Owns` reads it to find which WebApp to wake). Owner and child must be in the same namespace.

---

## 9. Step 6: Wire the manager

`cmd/main.go` (key parts)

```go
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))   // built-in types: Deployment, Job, Service...
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))     // WebApp, BatchJob
}

func main() {
	mgr, _ := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "webapp-operator.apps.example.io",
	})
	_ = (&controller.WebAppReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)
	_ = (&controller.BatchJobReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)
	_ = mgr.Start(ctrl.SetupSignalHandler())
}
```

(The real file checks every error and exits on failure.)

**What happens**

- `clientgoscheme` (`k8s.io/client-go/kubernetes/scheme`) registers all built-in Kubernetes types. A Scheme maps Go types to Group/Version/Kind, which is needed to decode API responses, build request URLs and set owner references.
- `ctrl.GetConfigOrDie()` finds credentials: `--kubeconfig`, `$KUBECONFIG`, `%USERPROFILE%\.kube\config`, or the in-cluster service account. **No network traffic yet.**
- `NewManager` creates the shared cache, client, metrics, health probes and leader election. Several controllers share one cache, so there is one watch per kind, not one per controller.
- `mgr.Start` is where the network traffic begins: caches start (LIST then WATCH for every kind you registered), the manager waits for the caches to sync, then workers begin calling `Reconcile`.
- **Leader election** uses a `Lease` object so only one replica reconciles at a time. Caches stay warm on standby replicas for fast failover.

---

## 10. Step 7: Run it on a local cluster

```powershell
kind create cluster --name webapp-dev          # skip if it already exists
kubectl config current-context                  # should be kind-webapp-dev
kubectl apply -f config/crd/bases               # install the CRDs
kubectl get crd | Select-String "example.io"    # confirm webapps + batchjobs are registered

go run ./cmd/main.go                            # leave running in terminal 1
```

Running with `go run` uses your kubeconfig's admin rights, so RBAC isn't enforced locally. The generated `config\rbac` role matters when you deploy the operator inside a cluster.

---

## 11. Step 8: kubectl walkthrough and what happens in the code

Use three terminals, all from the project root:

| Terminal | Command |
|---|---|
| 1 | `go run ./cmd/main.go` (operator logs) |
| 2 | `kubectl get webapp,deploy,rs,pods -w` (live view) |
| 3 | The commands below, one at a time |

`-v=6` prints each HTTP call kubectl makes; use `-v=8` to include bodies.

Sample resource, `config/samples/webapp.yaml`:

```yaml
apiVersion: apps.example.io/v1alpha1
kind: WebApp
metadata:
  name: demo
spec:
  image: nginx:1.27
  replicas: 2
  port: 80
```

### 11.1 Create

```powershell
kubectl apply -f config/samples/webapp.yaml -v=6
```

| # | Who | What happens |
|---|---|---|
| 1 | kubectl | Reads the YAML, discovers the URL for `WebApp`, sends `POST .../namespaces/default/webapps` (or `PATCH` if it exists) |
| 2 | kube-apiserver | Authenticates, checks RBAC, applies defaults (`replicas`, `port`), validates against the CRD schema, writes to etcd, sets `generation` and `resourceVersion`, returns `201 Created` |
| 3 | kubectl | Prints `webapp.apps.example.io/demo created` and **exits**. Nothing is running yet |
| 4 | API server | Sends an `ADDED` event down the operator's already-open watch stream |
| 5 | Operator | The `For` handler enqueues `default/demo`; a worker calls `Reconcile` |
| 6 | `Reconcile` | Adds the finalizer (`Update`), then `CreateOrUpdate` creates the Deployment with an ownerReference |
| 7 | kube-controller-manager | Its Deployment controller creates a ReplicaSet, then pods |
| 8 | scheduler, kubelet | Place pods and start containers; pods report Ready |
| 9 | Operator | The `Owns` handler wakes the owner WebApp; `updateStatus` writes `readyReplicas` and the `Available` condition via `/status` |
| 10 | Operator | One more reconcile finds nothing to change (`DeepEqual` guard) and stops |

### 11.2 Inspect

```powershell
kubectl get webapps                                  # Image and Ready columns (printcolumn markers)
kubectl get deploy demo                              # created by the controller
kubectl get webapp demo -o yaml                      # finalizer + status.conditions
kubectl get deploy demo -o jsonpath='{.metadata.ownerReferences}'
kubectl explain webapp.spec                          # schema generated from your Go markers
```

In `kubectl get webapp demo -o yaml` look for:

- `metadata.finalizers` containing `apps.example.io/finalizer`
- `metadata.generation` and `status.observedGeneration` (they match when the controller has caught up)
- `status.conditions` with `type: Available`

### 11.3 Modify the spec (scale)

Quoting JSON in PowerShell is fragile, so use a file:

```powershell
Set-Content -Path patch.json -Value '{"spec":{"replicas":3}}'
kubectl patch webapp demo --type merge --patch-file patch.json -v=6
```

| Who | What happens |
|---|---|
| API server | Validates (`replicas >= 0`), stores, **increments `metadata.generation`** (spec changed), returns the object |
| Operator | `MODIFIED` event → `Reconcile` → `mutateDeployment` sets `Spec.Replicas = 3` → `CreateOrUpdate` sees a diff and calls `Update` |
| Built-in controllers | The ReplicaSet scales up and a third pod starts |
| Operator | Deployment status change wakes the WebApp; `readyReplicas` returns to match, `observedGeneration` catches up |

### 11.4 Watch the stream your operator relies on

```powershell
kubectl get webapp demo -w -v=6
```

The verbose log shows a `GET ...webapps?...watch=true...` request that never returns. It is the same kind of long-lived connection the Reflector holds. The API server never calls controllers; controllers keep a connection open and the server streams events down it.

### 11.5 Self-healing

```powershell
kubectl delete deploy demo
kubectl get deploy -w
```

The `Owns` watch fires on the deletion, `Reconcile` runs, and the Deployment is recreated within about a second.

### 11.6 Delete

```powershell
kubectl delete webapp demo -v=6
kubectl get deploy
```

| Who | What happens |
|---|---|
| API server | Sees a finalizer present, so it only sets `deletionTimestamp` (object stays in `Terminating`) |
| Operator | `Reconcile` takes the delete path: runs `cleanupExternalResources`, removes the finalizer, calls `Update` |
| API server | No finalizers left, so it removes the object from etcd |
| Garbage collector | Sees the owner is gone and deletes the Deployment, then its ReplicaSet and pods |

`kubectl delete` waits for the finalizer to be removed. If it hangs, the finalizer isn't being released, so check the operator logs.

### 11.7 Add log lines to see the flow

Add these to `webapp_controller.go` for a clear picture of each reconcile:

```go
logger.Info("Reconcile triggered", "webapp", req.NamespacedName,
	"generation", webapp.Generation, "finalizers", webapp.Finalizers)   // top of Reconcile
logger.Info("finalizer added")                                           // after adding the finalizer
logf.FromContext(ctx).Info("status unchanged, skipping write")         // DeepEqual guard in updateStatus
logf.FromContext(ctx).Info("updating status", "ready", ready, "desired", desired)
logf.FromContext(ctx).Info("finalizer removed, deletion can complete")  // reconcileDelete
```

More than one `Reconcile triggered` per command is normal: your own writes (finalizer, status) generate events for the same object, and the extra runs are idempotent no-ops.

---

## 12. Modifying an existing CRD

Example: add an optional `env` setting to `WebApp`.

### 12.1 Edit the Go type

```go
type WebAppSpec struct {
	// ...existing fields...

	// Env is extra environment variables for the container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}
```

### 12.2 Use it in the controller

In `mutateDeployment`, next to `c.Image = ...`:

```go
c.Env = webapp.Spec.Env
```

### 12.3 Regenerate, install, restart

```powershell
controller-gen object paths=./api/v1alpha1
controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
go build ./...

kubectl apply -f config/crd/bases        # updates the CRD schema in the cluster
# stop the operator (Ctrl+C) and start it again so the new Go code runs
go run ./cmd/main.go
```

### 12.4 What happens

| Step | Effect |
|---|---|
| `controller-gen object` | DeepCopy functions are regenerated; the new slice field needs a deep copy loop |
| `controller-gen crd` | The OpenAPI schema in the CRD YAML gains `env` |
| `kubectl apply` on the CRD | The API server starts accepting and validating the new field. **Existing objects are not rewritten** |
| Defaults | `+kubebuilder:default` values are applied on create/update and also when objects are read back from etcd (without being persisted) |
| Operator restart | The compiled controller still has the old logic until you rebuild; the CRD and the Go code are separate deployments |
| Next reconcile | `mutateDeployment` writes `env` onto the Deployment; `CreateOrUpdate` sees a diff and updates it |

Check the result:

```powershell
kubectl explain webapp.spec.env
kubectl get crd webapps.apps.example.io -o yaml
```

### 12.5 Safe vs risky changes

| Change | Safe? | Notes |
|---|---|---|
| Add an **optional** field | Yes | Old objects remain valid |
| Add a **required** field | Risky | Existing objects become invalid on their next update; give it a default instead |
| Tighten validation (smaller max, new pattern) | Risky | Existing objects violating the rule fail on update |
| Rename or remove a field | Breaking | Data in etcd is dropped or ignored. Add a new API **version** (`v1beta1`/`v1`) with a conversion strategy instead |
| Change a field's type | Breaking | Same: new version |
| Make a field immutable | Use CEL | `// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="immutable"` |

For large CRDs (for example ones embedding a full `PodTemplateSpec`), plain `kubectl apply` can fail because the `last-applied-configuration` annotation exceeds its size limit. Use server-side apply:

```powershell
kubectl apply --server-side -f config/crd/bases
```

---

## 13. Creating a brand-new CRD (checklist)

1. **Types file** `api/v1alpha1/<kind>_types.go`: `Spec`, `Status`, the root type with `+kubebuilder:object:root=true` and `+kubebuilder:subresource:status`, a `<Kind>List` type, and an `init()` calling `SchemeBuilder.Register(&Kind{}, &KindList{})`.
2. **Markers** for validation, defaults, print columns, short names (`+kubebuilder:resource:shortName=bj`).
3. **Generate**: `controller-gen object ...` then `controller-gen crd ...`.
4. **Controller file** `internal/controller/<kind>_controller.go`: a reconciler struct embedding `client.Client` with a `Scheme`, a `Reconcile` method, RBAC markers, and `SetupWithManager` with `For` and `Owns`.
5. **Register in `main.go`**: one more `(&controller.<Kind>Reconciler{...}).SetupWithManager(mgr)` block. The scheme already includes new types through `appsv1alpha1.AddToScheme`.
6. **RBAC**: regenerate `config/rbac` and make sure the role covers the new resource, its `/status`, and every child kind you create.
7. **Install**: `kubectl apply -f config/crd/bases`.
8. **Sample**: add a YAML under `config/samples/`, apply it, and watch with `kubectl get <shortname> -w`.
9. **Test**: a unit test for pure logic and an envtest for the full loop.

Design questions to ask before writing the reconciler:

- Which fields can safely change after creation? Is the child object's spec immutable (like a Job template)?
- Does anything exist outside the cluster that needs cleanup? If yes, add a finalizer. If not, owner references are enough.
- What exactly means "ready" or "done", and how will it appear in `status.conditions`?

---

## 14. The BatchJob resource

`BatchJob` models an HPC-style gang of workers.

```yaml
apiVersion: apps.example.io/v1alpha1
kind: BatchJob
metadata:
  name: pi-sim
spec:
  image: busybox:1.36
  workers: 3
  walltimeSeconds: 120
  maxRetries: 1
  command: ["sh", "-c"]
  args:
    - |
      echo "rank ${JOB_COMPLETION_INDEX} of 3 on ${HOSTNAME}"
      sleep 30
```

| Spec field | Becomes | HPC analogy |
|---|---|---|
| `workers` | Job `completions` = `parallelism`, Indexed mode | MPI ranks |
| headless Service + `subdomain` | Per-pod DNS `<job>-<rank>.<job>` | Hostfile / peer discovery |
| `walltimeSeconds` | `activeDeadlineSeconds` | Slurm `--time` |
| `resources` (for example `nvidia.com/gpu: 1`) | Container resources | GPU allocation |
| `queueName` | Kueue label `kueue.x-k8s.io/queue-name` | Slurm partition / queue |

Design points worth remembering:

- A Job's pod template is **immutable**, so the controller creates the Job once and never updates it. The headless Service is mutable and uses `CreateOrUpdate`.
- Status has a `phase` plus three conditions (`Queued`, `Running`, `Completed`). Exactly one is true at a time. "Running" means the **whole gang** is ready, not just some workers.
- The phase decision lives in a pure function (`computePhase`) that is unit-tested without a cluster.
- There is no finalizer: everything the controller creates is cluster-internal, so owner references are sufficient.

```powershell
kubectl apply -f config/samples/batchjob.yaml
kubectl get bj -w                                   # Queued -> Running -> Succeeded
kubectl logs -l apps.example.io/batchjob=pi-sim --prefix
kubectl get svc pi-sim                              # headless: CLUSTER-IP None
```

Known limits: the default scheduler does not gang-schedule (use Kueue or Volcano for all-or-nothing admission), a restarted worker moves the phase back to `Queued`, and retries share one budget. These are good starting points for the next iteration.

---

## 15. Testing

### Unit tests (native Windows)

```powershell
go test ./internal/controller -run TestComputePhase -v
```

### Integration tests with envtest (use WSL2)

envtest starts a real `kube-apiserver` and `etcd` but **no** scheduler, kube-controller-manager or garbage collector. Pods never start and ownerReference-based deletion doesn't run, so tests fake Deployment status themselves and verify the ownerReference instead of waiting for GC.

```bash
# inside WSL2, repo cloned to ~/webapp-operator
go get github.com/onsi/gomega
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path)"
go test ./internal/controller/... -v
```

Use a direct (uncached) client for assertions, so tests don't read stale cache data.

---

## 16. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `controller-gen` not recognised | `go install` does not edit `PATH`. Run `$env:Path += ";$(go env GOPATH)\bin"` |
| `no Go files in ...\api` (or `...\internal`) | The `...` pattern visits a folder with no `.go` files. Use `paths=./api/v1alpha1` and `paths=./internal/controller`; also confirm the files exist and aren't empty (`Get-ChildItem api -Recurse \| Select-Object FullName, Length`) |
| `undefined: WebAppSec`, `in.ListMeta undefined` | A stale `zz_generated.deepcopy.go`. Fix typos in the types file, delete the generated file and regenerate |
| `missing method GetAnnotations` | `ObjectMeta` isn't embedded in the type. Use `metav1.ObjectMeta` with no field name |
| `no kind is registered for the type v1.Deployment` | `clientgoscheme.AddToScheme` missing in `main.go` |
| `Operation cannot be fulfilled ... the object has been modified` | Optimistic concurrency conflict (`resourceVersion`). Normal and self-healing on retry |
| `spec.selector: field is immutable` | The Deployment selector was changed. Set it only on creation |
| `deployments.apps is forbidden` | In-cluster RBAC gap. Regenerate `config/rbac` and apply it |
| No `Reconcile` logs | CRD not installed or wrong context: `kubectl get crd`, `kubectl config current-context` |
| `kubectl delete webapp` hangs | Finalizer isn't being removed; check operator logs |
| Status never updates | `Owns(&appsv1.Deployment{})` missing, or no ownerReference on the child |

---

## 17. Concepts glossary

| Term | Meaning |
|---|---|
| **CRD** | Object that teaches the API server a new resource type and its schema |
| **Custom resource (CR)** | An instance of a CRD, such as the `demo` WebApp |
| **Controller** | Loop that observes state and acts to reach the desired state |
| **Operator** | A CRD plus the controller that manages it |
| **Reconcile** | Compare desired state with actual state and fix the difference |
| **Level-triggered** | Acts on current state, not on individual events |
| **Informer** | Watch plus an in-memory cache of one resource type |
| **Reflector** | The informer part that does LIST and WATCH against the API server |
| **DeltaFIFO / Indexer / Lister** | Event buffer / local store with indexes / read-only view of the store |
| **Work queue** | De-duplicating, rate-limited queue of `namespace/name` keys |
| **Scheme** | Registry mapping Go types to Group/Version/Kind |
| **Owner reference** | Metadata marking a child as belonging to a parent (garbage collection and `Owns` routing) |
| **Finalizer** | Marker that blocks deletion until cleanup finishes |
| **Status subresource** | Separate `/status` endpoint so `spec` and `status` have separate write paths |
| **Conditions** | Standard list of typed true/false/unknown states in `status` |
| **`generation` / `observedGeneration`** | Spec version counter / the spec version the status describes |
| **`resourceVersion`** | Per-object version used for optimistic concurrency and watch resume |
| **Leader election** | Lease-based choice of one active replica |
| **Marker** | A `// +kubebuilder:...` comment read by `controller-gen` |

---

## 18. Further reading

Links were collected from memory of well-known documentation locations. Pages can move, so search for the title if one is broken.

### Kubernetes concepts

- [Custom Resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/)
- [Extend the Kubernetes API with CustomResourceDefinitions](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/) (schemas, defaulting, CEL validation rules)
- [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
- [Owners and dependents](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)
- [Finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/) (LIST, WATCH, `resourceVersion`)
- [kubectl quick reference](https://kubernetes.io/docs/reference/kubectl/quick-reference/)
- [Jobs, including Indexed completion mode](https://kubernetes.io/docs/concepts/workloads/controllers/job/)

### Building controllers

- [The Kubebuilder Book](https://book.kubebuilder.io/) (best end-to-end tutorial for this style of project)
- [Kubebuilder marker reference](https://book.kubebuilder.io/reference/markers.html)
- [Kubebuilder: writing tests with envtest](https://book.kubebuilder.io/reference/envtest.html)
- [controller-runtime on GitHub](https://github.com/kubernetes-sigs/controller-runtime) and [API docs on pkg.go.dev](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [controller-tools (controller-gen)](https://github.com/kubernetes-sigs/controller-tools)
- [client-go](https://github.com/kubernetes/client-go) and [`tools/cache` (informers) docs](https://pkg.go.dev/k8s.io/client-go/tools/cache)
- [sample-controller](https://github.com/kubernetes/sample-controller) (the plain client-go informer and workqueue reference)
- [Writing Controllers (Kubernetes contributor guide)](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-api-machinery/controllers.md)
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) (spec/status, conditions, naming)

### Local tooling

- [kind](https://kind.sigs.k8s.io/)

### HPC and batch on Kubernetes

- [Kueue](https://kueue.sigs.k8s.io/) (job queueing and quota)
- [JobSet](https://github.com/kubernetes-sigs/jobset) (multi-pod job groups)
- [Kubeflow Training Operator](https://github.com/kubeflow/training-operator) and [MPI Operator](https://github.com/kubeflow/mpi-operator)
- [Volcano](https://volcano.sh/) (gang scheduling)
- [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator)

### Suggested reading order

1. Operator pattern and Controllers (concepts).
2. The Kubebuilder Book's CronJob tutorial (the same flow as this project, in the standard layout).
3. `sample-controller` to see what controller-runtime hides.
4. Read the source of Kueue or the MPI Operator's controller and compare its reconcile loop to `BatchJob`.

---

---

# Part 2: Deploy to AWS with Docker and GitHub Actions

Everything above runs the operator on your laptop with `go run`. Part 2 packages it as a container image, builds and tests it on every push with **GitHub Actions**, stores the image in **Amazon ECR**, and rolls it out to a small Kubernetes cluster on an **EC2** instance, with no long-lived AWS keys anywhere.

> **You type the code.** Each step below says which file to create or edit and shows exactly what goes in it. Your existing code under `api/`, `cmd/` and `internal/` is **not changed** by Part 2 (the manifests and pipeline sit around it). Commands are PowerShell.
>
> **Not compiled or run by me.** I wrote these steps without access to your AWS account, so treat each command as something to check against your own output. Where a step has a known failure mode, it is listed next to the step and again in [section 29](#29-troubleshooting-the-deployment).

---

## 19. Deployment architecture, costs and why not EKS

```
 Developer                GitHub                              AWS (one region)
 ─────────                ──────                              ────────────────
 git push main ──────▶ GitHub Actions
                         │ 1. test job: build, vet, generated-code check, unit tests
                         │ 2. docker build (linux/amd64)
                         │ 3. assume IAM role via OIDC (no stored keys)
                         ├────────────────────────────────▶ ECR  (image :<git sha>)
                         ├────────────────────────────────▶ S3   (rendered manifests + deploy script)
                         └─ 4. SSM Run Command ───────────▶ EC2 instance (Amazon Linux 2023)
                                                              │  k3s (single-node Kubernetes)
                                                              │  1. fetch manifests from S3
                                                              │  2. pull image from ECR into k3s
                                                              │  3. kubectl apply (CRDs, RBAC, Deployment)
                                                              ▼
                                                        webapp-operator pod  ──▶  watches WebApp/BatchJob
```

| Piece | Role | Why this choice |
|---|---|---|
| **GitHub Actions** | CI/CD runner | Free minutes for small projects; first-class OIDC support for AWS |
| **OIDC federation** | Lets a workflow assume an IAM role for ~1 hour | No access keys stored in GitHub |
| **ECR** | Private image registry | Native to AWS; the instance pulls with its own IAM role |
| **S3** | Hand-off of rendered manifests | Works for private repos; the instance never needs GitHub credentials |
| **SSM Run Command** | Runs the deploy script on the instance | **No open SSH port, no inbound security-group rules** |
| **EC2 + k3s** | A real, single-node Kubernetes cluster | Keeps cost near zero (see below) |

### Why not EKS?

Amazon EKS bills **$0.10 per cluster-hour (about $73/month)** for the control plane alone, before any worker node. That is not free tier territory, and on a credits-based plan it would burn credits quickly. A single EC2 instance running **k3s** (a lightweight, certified Kubernetes distribution) gives you a genuine API server, CRDs, RBAC and controllers for the price of one small instance. The pipeline is the same one you would use with EKS: only the last step differs ([section 30](#306-moving-to-eks-later)).

### What "free tier" means now

AWS changed the rules for accounts created **on or after 15 July 2025**. From the sources I checked:

- New accounts choose a **Free plan** or a **Paid plan**, starting with credits ($100, growing to $200 by completing onboarding activities).
- On the Free plan, the account is limited to roughly **6 months or until the credits run out**, after which it closes automatically; the Paid plan keeps billing at normal rates once credits are used.
- For those newer accounts the free-tier-eligible EC2 types listed in AWS's documentation are `t3.micro`, `t3.small`, `t4g.micro`, `t4g.small`, `c7i-flex.large` and `m7i-flex.large`. Older accounts list `t2.micro` and `t3.micro`.

**Check which applies to you** before creating anything: open the AWS Billing console and look at your plan and credit balance, and list your eligible instance types from the CLI (step 12.6). Rules and prices change, so trust the console over this README.

### What this setup costs

| Resource | Cost note |
|---|---|
| EC2 instance (`t3.small` or `t3.micro`) | Covered by free-tier eligibility or credits while eligible. **Stopped instances still bill for their disk.** Terminate when finished |
| EBS volume, 20 GiB gp3 | Within the free allowance (up to 30 GB) |
| Public IPv4 address | AWS bills public IPv4 addresses hourly; whether your plan covers it depends on the offer. Check the Billing console |
| ECR | Small private repository; a lifecycle rule (step 12.2) keeps only the last 5 images |
| S3 | A few KB per deploy; a lifecycle rule deletes bundles after 7 days |
| SSM Run Command, IAM, OIDC | I believe there is no additional charge, but confirm on the Systems Manager pricing page |
| **NAT Gateway, load balancers, EKS** | **Not used here, and none have a free tier.** Avoid them in this project |

Before you start, create a **zero-spend or low-threshold budget alert** (Billing console → Budgets), and enable Free Tier usage alerts in Billing preferences.

### Honest limitations

- One node, no high availability. This is a learning and portfolio environment, not production.
- A 1 GiB instance is tight for k3s plus an operator. The user-data script adds a 2 GiB swap file, but `t3.small` (2 GiB) is noticeably more comfortable if your account lists it as eligible.
- `t4g` instances are ARM (Graviton). This guide builds `linux/amd64` images, so use a `t3` type unless you also change the build platform.

---

## 20. Prerequisites for Part 2

```powershell
winget install Amazon.AWSCLI
winget install GitHub.cli
winget install Git.Git
```

Open a new terminal, then check:

```powershell
aws --version
gh --version
git --version
docker version        # Docker Desktop must be running
```

### AWS credentials for the one-time setup

1. Sign in to the AWS console as the **root user only to create an IAM user** (then stop using root).
2. Create an IAM user (for example `admin-cli`) with the `AdministratorAccess` policy, and create an access key for the CLI.
3. Configure the CLI:

```powershell
aws configure                      # paste the key, secret, region, and "json" as output format
aws sts get-caller-identity        # confirms who you are
```

You use these admin credentials **only for the setup steps in this README**. The pipeline never uses them: it uses OIDC. When you finish setup, delete the access key (IAM console → your user → Security credentials) or deactivate it.

### Two PowerShell helpers

Windows PowerShell writes CRLF line endings by default, which breaks Linux shell scripts (`\r` ends up in commands). This helper always writes LF. Paste it into your terminal once (and again in any new terminal):

```powershell
New-Item -ItemType Directory -Force .aws-setup | Out-Null     # scratch folder, git-ignored

function Write-Lf([string]$Path, [string]$Text) {
  $full = Join-Path (Get-Location).Path $Path
  [System.IO.File]::WriteAllText($full, ($Text -replace "`r`n", "`n"))
}

$env:AWS_PAGER = ""       # stop the AWS CLI from opening a pager
```

---

## 21. Step 9: Make the app container-ready

### 9.1 `Dockerfile` (project root)

```dockerfile
# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# Set GO_VERSION to the "go" line in your go.mod (check with: go version).
ARG GO_VERSION=1.24
FROM golang:${GO_VERSION} AS builder

# Supplied automatically by buildx so one Dockerfile can target amd64 or arm64.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Copy dependency manifests first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy only the source folders the binary needs (keeps the build context small).
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# Static binary: CGO off means no libc dependency, so it runs in a "static" base image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd/main.go

# ---- runtime stage ---------------------------------------------------------
# distroless/static: no shell, no package manager, tiny attack surface.
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/manager .

# 65532 is the built-in "nonroot" user. Kubernetes' runAsNonRoot check needs a numeric user.
USER 65532:65532

ENTRYPOINT ["/manager"]
```

**What each part does**

| Line | Why |
|---|---|
| `ARG GO_VERSION` before `FROM` | Lets you change the Go version without editing the image line. If it is older than your `go.mod`, Go may download the right toolchain automatically (needs network), but matching it avoids surprises |
| `COPY go.mod go.sum` then `go mod download` | Docker caches each layer. Dependencies change rarely, so rebuilds after a code edit skip the slow download |
| `COPY api/ cmd/ internal/` | Only source goes in. The CRD YAML, docs and tests are not needed to compile |
| `CGO_ENABLED=0` | Produces a fully static binary that runs without any C library |
| `-trimpath -ldflags="-s -w"` | Smaller binary, no local file paths embedded |
| `distroless/static:nonroot` | Final image is roughly binary size plus a few MB. No shell means nothing for an attacker to run |
| `USER 65532:65532` | The pod's `runAsNonRoot: true` setting can only verify a numeric UID |
| `ENTRYPOINT ["/manager"]` | Arguments in the Deployment (`--leader-elect` and so on) are appended to this |

Your `go.sum` must exist and be committed: run `go mod tidy` if `dir go.sum` shows nothing.

### 9.2 `.dockerignore` (project root)

```
.git
.github
.aws-setup
bin
out
*.exe
*.md
config
deploy
```

It keeps the build context small and stops stray files from invalidating the layer cache.

### 9.3 `.gitignore` (project root)

```
bin/
out/
*.exe
.aws-setup/
patch.json
```

Do **not** ignore `zz_generated.deepcopy.go`, `config/crd/bases/*.yaml` or `config/rbac/role.yaml`. They are generated, but CI and the Docker build need them in the repository.

### 9.4 `.gitattributes` (project root, create **before your first commit**)

```gitattributes
# Force LF so shell scripts and generated files are identical on Windows and Linux.
* text=auto eol=lf
```

### 9.5 Commit the generated files and verify the build

```powershell
controller-gen object paths=./api/v1alpha1
controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
controller-gen rbac:roleName=webapp-manager-role paths=./internal/controller output:rbac:artifacts:config=config/rbac
go mod tidy
go build ./...
docker build -t webapp-operator:dev .
docker images webapp-operator:dev
```

The image should be tens of MB, not hundreds. If `docker build` fails with `go: go.mod requires go >= X`, set `GO_VERSION` to that version.

---

## 22. Step 10: Kubernetes manifests (kustomize)

Locally you ran the operator as a process. In a cluster it runs as a **Deployment** under a **ServiceAccount** with **RBAC** that grants exactly what the controllers need. These manifests are plain YAML organised with **kustomize** (built into `kubectl`).

Target layout (new files marked ★; `role.yaml` and the CRDs already exist because they are generated):

```
config\
├── crd\
│   ├── bases\...                          (generated)
│   └── kustomization.yaml                 ★
├── rbac\
│   ├── role.yaml                          (generated)
│   ├── service_account.yaml               ★
│   ├── role_binding.yaml                  ★
│   ├── leader_election_role.yaml          ★
│   ├── leader_election_role_binding.yaml  ★
│   └── kustomization.yaml                 ★
├── manager\
│   ├── manager.yaml                       ★
│   └── kustomization.yaml                 ★
└── default\
    ├── namespace.yaml                     ★
    └── kustomization.yaml                 ★
```

### 10.1 `config/crd/kustomization.yaml`

```yaml
resources:
  - bases/apps.example.io_webapps.yaml
  - bases/apps.example.io_batchjobs.yaml
```

The file names are `<group>_<plural>.yaml`. Check with `dir config\crd\bases`.

### 10.2 `config/rbac/service_account.yaml`

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: controller-manager
  namespace: system        # placeholder; the "namespace:" in config/default replaces it
```

### 10.3 `config/rbac/role_binding.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: webapp-manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: webapp-manager-role      # the name given to controller-gen with rbac:roleName=
subjects:
  - kind: ServiceAccount
    name: controller-manager
    namespace: system
```

### 10.4 `config/rbac/leader_election_role.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: leader-election-role
  namespace: system
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

### 10.5 `config/rbac/leader_election_role_binding.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: leader-election-rolebinding
  namespace: system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: leader-election-role
subjects:
  - kind: ServiceAccount
    name: controller-manager
    namespace: system
```

### 10.6 `config/rbac/kustomization.yaml`

```yaml
resources:
  - service_account.yaml
  - role.yaml
  - role_binding.yaml
  - leader_election_role.yaml
  - leader_election_role_binding.yaml
```

### 10.7 `config/manager/manager.yaml`

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: system
  labels:
    control-plane: controller-manager
spec:
  replicas: 1
  selector:
    matchLabels:
      control-plane: controller-manager
  template:
    metadata:
      labels:
        control-plane: controller-manager
    spec:
      serviceAccountName: controller-manager
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: manager
          # Placeholder. CI replaces this exact string with the ECR image for the commit.
          image: controller:latest
          # The image is pulled onto the node by the deploy script, so never contact a registry.
          imagePullPolicy: IfNotPresent
          args:
            - --leader-elect
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
          ports:
            - name: metrics
              containerPort: 8080
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
            initialDelaySeconds: 15
            periodSeconds: 20
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 250m
              memory: 192Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
```

**Why the arguments matter.** Your `main.go` defaults the metrics and probe addresses to `127.0.0.1` (to avoid the Windows firewall prompt). Inside a pod, the kubelet probes the **pod IP**, not loopback, so `--health-probe-bind-address=:8081` makes the probe reachable. `--leader-elect` turns on the Lease-based election you already coded.

`imagePullPolicy: IfNotPresent` pairs with the deploy script, which pulls the image into the node's container runtime itself. Each commit gets a new tag (the git SHA), so a stale cached image is never reused.

### 10.8 `config/manager/kustomization.yaml`

```yaml
resources:
  - manager.yaml
```

### 10.9 `config/default/namespace.yaml`

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: webapp-operator-system
```

### 10.10 `config/default/kustomization.yaml`

```yaml
# Puts every namespaced resource (and the ServiceAccount subjects) in this namespace.
namespace: webapp-operator-system

resources:
  - namespace.yaml
  - ../crd
  - ../rbac
  - ../manager
```

Render it to check the result (this does not contact any cluster):

```powershell
kubectl kustomize config/default | Select-String "kind:|namespace:|image:"
```

You should see the CRDs, ServiceAccount, ClusterRole, bindings and Deployment, all with `namespace: webapp-operator-system`, and `image: controller:latest`.

---

## 23. Step 11: Test the image and manifests in kind first

Prove the container and RBAC work **before** involving AWS. This catches most mistakes cheaply.

```powershell
# Stop any local "go run ./cmd/main.go" first. Two copies reconciling at once is confusing.

docker build -t webapp-operator:dev .
kind load docker-image webapp-operator:dev --name webapp-dev          # copy the image into the kind node

(kubectl kustomize config/default) -replace 'image: controller:latest', 'image: webapp-operator:dev' | kubectl apply --server-side -f -

kubectl -n webapp-operator-system rollout status deployment/controller-manager --timeout=120s
kubectl -n webapp-operator-system get pods
kubectl -n webapp-operator-system logs deploy/controller-manager --tail=30
```

Expected: one `Running` pod, and logs that include `starting manager`, `Starting workers` and a leader election message such as `successfully acquired lease`.

Now prove the RBAC is enough (this is what local `go run` could not test, because it used your admin rights):

```powershell
kubectl apply -f config/samples/webapp.yaml
kubectl get webapp,deploy,pods
kubectl apply -f config/samples/batchjob.yaml
kubectl get bj
kubectl -n webapp-operator-system logs deploy/controller-manager --tail=20
```

Any line containing `forbidden` in the logs is an RBAC gap: add the missing `+kubebuilder:rbac` marker, re-run `controller-gen rbac...`, rebuild and re-test. Clean up when done:

```powershell
kubectl delete -f config/samples/webapp.yaml
kubectl delete -f config/samples/batchjob.yaml
```

---

## 24. Step 12: Create the AWS resources (one-time)

Run these in one PowerShell session so the variables persist. Replace the owner and repo with yours.

### 12.1 Variables

```powershell
$REGION    = "eu-west-2"                 # pick a region near you; any region works
$ACCOUNT   = aws sts get-caller-identity --query Account --output text
$GH_OWNER  = "Himanshuxone"              # your GitHub username or organisation
$GH_REPO   = "webapp-operator"           # the GitHub repository name you will create
$REPO_NAME = "webapp-operator"           # the ECR repository name
$BUCKET    = "webapp-operator-artifacts-${ACCOUNT}-${REGION}"
$env:AWS_DEFAULT_REGION = $REGION
```

> In PowerShell, write `${ACCOUNT}:` with braces. `$ACCOUNT:` is parsed as a drive-qualified variable and silently produces an empty string.

### 12.2 ECR repository with cleanup

```powershell
aws ecr create-repository --repository-name $REPO_NAME --image-scanning-configuration scanOnPush=true

Write-Lf ".aws-setup/ecr-lifecycle.json" @'
{"rules":[{"rulePriority":1,"description":"Keep the last 5 images","selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":5},"action":{"type":"expire"}}]}
'@
aws ecr put-lifecycle-policy --repository-name $REPO_NAME --lifecycle-policy-text file://.aws-setup/ecr-lifecycle.json
```

### 12.3 S3 bucket for deploy bundles

```powershell
# For us-east-1 omit --create-bucket-configuration entirely.
aws s3api create-bucket --bucket $BUCKET --create-bucket-configuration LocationConstraint=$REGION

aws s3api put-public-access-block --bucket $BUCKET `
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

Write-Lf ".aws-setup/s3-lifecycle.json" @'
{"Rules":[{"ID":"expire-old-deploys","Status":"Enabled","Filter":{"Prefix":"deploy/"},"Expiration":{"Days":7}}]}
'@
aws s3api put-bucket-lifecycle-configuration --bucket $BUCKET --lifecycle-configuration file://.aws-setup/s3-lifecycle.json
```

### 12.4 IAM role for the EC2 instance

The instance needs three things: be managed by SSM, read the manifests from S3, and pull from ECR.

```powershell
Write-Lf ".aws-setup/trust-ec2.json" @'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}
'@

aws iam create-role --role-name webapp-operator-ec2 --assume-role-policy-document file://.aws-setup/trust-ec2.json
aws iam attach-role-policy --role-name webapp-operator-ec2 --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore

Write-Lf ".aws-setup/ec2-access.json" @"
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*"},
    {"Effect": "Allow",
     "Action": ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"],
     "Resource": "arn:aws:ecr:${REGION}:${ACCOUNT}:repository/${REPO_NAME}"},
    {"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::${BUCKET}/deploy/*"}
  ]
}
"@
aws iam put-role-policy --role-name webapp-operator-ec2 --policy-name ec2-access --policy-document file://.aws-setup/ec2-access.json

aws iam create-instance-profile --instance-profile-name webapp-operator-ec2
aws iam add-role-to-instance-profile --instance-profile-name webapp-operator-ec2 --role-name webapp-operator-ec2
Start-Sleep -Seconds 15      # IAM is eventually consistent; wait before using the profile
```

### 12.5 Security group with no inbound rules

```powershell
$VPC = aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query "Vpcs[0].VpcId" --output text
$SG  = aws ec2 create-security-group --group-name webapp-operator-sg `
         --description "No inbound rules. Managed through SSM." --vpc-id $VPC --query GroupId --output text
```

A new security group allows all outbound traffic and no inbound traffic. The instance reaches out to AWS (SSM, ECR, S3) and GitHub-hosted runners never connect in. If `$VPC` prints `None`, you have no default VPC; create one in the VPC console or use a public subnet of your own.

### 12.6 Launch the instance with k3s

First see which instance types your account may use for free:

```powershell
aws ec2 describe-instance-types --filters Name=free-tier-eligible,Values=true `
  --query "InstanceTypes[*].InstanceType" --output text
```

Choose `t3.small` if listed (2 GiB RAM), otherwise `t3.micro`. Avoid `t4g.*` (ARM) for this guide.

```powershell
$INSTANCE_TYPE = "t3.small"

# Boot script. Runs once as root. Single-quoted here-string: nothing is expanded by PowerShell.
Write-Lf ".aws-setup/userdata.sh" @'
#!/bin/bash
set -euxo pipefail

# 2 GiB swap file: small instances run out of memory without it.
dd if=/dev/zero of=/swapfile bs=1M count=2048
chmod 600 /swapfile
mkswap /swapfile
swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab

# Single-node k3s. Skip the ingress controller and metrics-server to save memory.
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --disable traefik --disable metrics-server" sh -
'@

$AMI = aws ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 `
         --query Parameter.Value --output text

$INSTANCE_ID = aws ec2 run-instances `
  --image-id $AMI `
  --instance-type $INSTANCE_TYPE `
  --iam-instance-profile Name=webapp-operator-ec2 `
  --security-group-ids $SG `
  --metadata-options "HttpTokens=required,HttpEndpoint=enabled" `
  --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=20,VolumeType=gp3,DeleteOnTermination=true}" `
  --user-data file://.aws-setup/userdata.sh `
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=webapp-operator-k3s}]" `
  --query "Instances[0].InstanceId" --output text

$INSTANCE_ID
aws ec2 wait instance-running --instance-ids $INSTANCE_ID
```

What the options do: `HttpTokens=required` enforces IMDSv2 (a safer instance metadata service); `DeleteOnTermination=true` removes the disk with the instance so nothing keeps billing; the user-data script installs swap and k3s on first boot.

### 12.7 A helper to run commands on the instance (SSM)

```powershell
function Invoke-Remote([string]$Command) {
  Write-Lf ".aws-setup/ssm-params.json" (@{ commands = @($Command) } | ConvertTo-Json -Compress)
  $id = aws ssm send-command --instance-ids $INSTANCE_ID --document-name AWS-RunShellScript `
          --parameters file://.aws-setup/ssm-params.json --query "Command.CommandId" --output text
  do {
    Start-Sleep -Seconds 3
    $status = aws ssm get-command-invocation --command-id $id --instance-id $INSTANCE_ID `
                --query Status --output text 2>$null
  } while (-not $status -or $status -in "Pending", "InProgress", "Delayed")
  "--- status: $status ---"
  aws ssm get-command-invocation --command-id $id --instance-id $INSTANCE_ID --query StandardOutputContent --output text
  aws ssm get-command-invocation --command-id $id --instance-id $INSTANCE_ID --query StandardErrorContent --output text
}

# Prefix for kubectl commands on the instance (k3s lives in /usr/local/bin).
$K = 'export PATH=$PATH:/usr/local/bin KUBECONFIG=/etc/rancher/k3s/k3s.yaml; '
```

### 12.8 Wait for the instance, then verify k3s

The SSM agent registers a minute or two after boot, and k3s installs a few minutes after that.

```powershell
# Repeat until your instance appears with PingStatus "Online"
aws ssm describe-instance-information --query "InstanceInformationList[*].[InstanceId,PingStatus]" --output text

Invoke-Remote ($K + 'kubectl get nodes -o wide')
```

You want one node with status `Ready`. If k3s is not installed yet, read the boot log:

```powershell
Invoke-Remote 'tail -n 30 /var/log/cloud-init-output.log'
```

Check memory headroom too:

```powershell
Invoke-Remote 'free -m'
```

---

## 25. Step 13: Let GitHub Actions assume an AWS role (OIDC)

**What OIDC does.** GitHub can mint a short-lived signed token for each workflow run that states *which repository and branch* is running. AWS verifies that token against GitHub's public keys and, if the claims match your trust policy, hands the workflow temporary credentials. There is no secret to leak or rotate.

```powershell
# 13.1 Register GitHub as an identity provider (once per AWS account)
aws iam create-open-id-connect-provider --url https://token.actions.githubusercontent.com --client-id-list sts.amazonaws.com
```

If your CLI insists on a thumbprint, add `--thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1`. AWS validates GitHub's certificates itself and ignores the value, but older CLI versions require one. If the provider already exists in your account, you will get an `EntityAlreadyExists` error, which is fine.

```powershell
# 13.2 Trust policy: only THIS repo, only the main branch
Write-Lf ".aws-setup/trust-github.json" @"
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::${ACCOUNT}:oidc-provider/token.actions.githubusercontent.com"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:${GH_OWNER}/${GH_REPO}:ref:refs/heads/main"
      }
    }
  }]
}
"@
aws iam create-role --role-name webapp-operator-github --assume-role-policy-document file://.aws-setup/trust-github.json
```

The `sub` condition is the security boundary. A pull request, another branch, or another repository produces a different `sub` and **cannot** assume the role.

```powershell
# 13.3 Permissions: push to ONE ECR repo, write to ONE S3 prefix, run commands on ONE instance
Write-Lf ".aws-setup/permissions-github.json" @"
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "EcrLogin", "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*"},
    {"Sid": "EcrPush", "Effect": "Allow",
     "Action": ["ecr:BatchCheckLayerAvailability", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart",
                "ecr:CompleteLayerUpload", "ecr:PutImage", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"],
     "Resource": "arn:aws:ecr:${REGION}:${ACCOUNT}:repository/${REPO_NAME}"},
    {"Sid": "UploadBundle", "Effect": "Allow", "Action": "s3:PutObject",
     "Resource": "arn:aws:s3:::${BUCKET}/deploy/*"},
    {"Sid": "RunOnInstance", "Effect": "Allow", "Action": "ssm:SendCommand",
     "Resource": ["arn:aws:ec2:${REGION}:${ACCOUNT}:instance/${INSTANCE_ID}",
                  "arn:aws:ssm:${REGION}::document/AWS-RunShellScript"]},
    {"Sid": "ReadResult", "Effect": "Allow",
     "Action": ["ssm:GetCommandInvocation", "ssm:ListCommandInvocations"], "Resource": "*"}
  ]
}
"@
aws iam put-role-policy --role-name webapp-operator-github --policy-name deploy --policy-document file://.aws-setup/permissions-github.json

$ROLE_ARN = aws iam get-role --role-name webapp-operator-github --query Role.Arn --output text
$ROLE_ARN
```

---

## 26. Step 14: Create the GitHub repository and its variables

The workflow reads five non-secret settings from repository **variables**. Role ARNs and bucket names are identifiers, not credentials.

```powershell
gh auth login                                  # follow the prompts once

# Make sure git is ready, and that .gitattributes and .gitignore exist BEFORE the first commit.
git init -b main
git config user.name                           # if empty: git config user.name "Your Name"
git config user.email                          # if empty: git config user.email "you@example.com"

# Create the repo, but do NOT push yet (the pipeline would run before its variables exist).
gh repo create "$GH_OWNER/$GH_REPO" --private --source . --remote origin

gh variable set AWS_ROLE_ARN      --body $ROLE_ARN
gh variable set AWS_REGION        --body $REGION
gh variable set ECR_REPOSITORY    --body $REPO_NAME
gh variable set ARTIFACT_BUCKET   --body $BUCKET
gh variable set EC2_INSTANCE_ID   --body $INSTANCE_ID

gh variable list
```

If you already created the repository, skip `gh repo create` and run `git remote add origin https://github.com/$GH_OWNER/$GH_REPO.git` instead (make sure the name matches the one in the role's trust policy).

---

## 27. Step 15: The deploy script and the workflow

### 15.1 `deploy/remote-deploy.sh`

This script runs **on the EC2 instance** (as root, through SSM). Create the file in your editor:

```bash
#!/usr/bin/env bash
# Runs ON the EC2 instance. Arguments: BUCKET SHA IMAGE REGION
set -euo pipefail

BUCKET="$1"
SHA="$2"
IMAGE="$3"
REGION="$4"

# SSM runs commands with a minimal PATH. k3s installs its tools in /usr/local/bin.
export PATH="$PATH:/usr/local/bin"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
NAMESPACE=webapp-operator-system

echo "==> Downloading manifests for ${SHA}"
aws s3 cp "s3://${BUCKET}/deploy/${SHA}/rendered.yaml" /tmp/rendered.yaml --region "${REGION}"

echo "==> Pulling ${IMAGE} into k3s containerd"
TOKEN="$(aws ecr get-login-password --region "${REGION}")"
# "-n k8s.io" is the containerd namespace Kubernetes uses; images in other namespaces are invisible to pods.
k3s ctr -n k8s.io images pull --user "AWS:${TOKEN}" "${IMAGE}"

echo "==> Applying manifests"
kubectl apply --server-side --force-conflicts -f /tmp/rendered.yaml

echo "==> Waiting for the rollout"
kubectl -n "${NAMESPACE}" rollout status deployment/controller-manager --timeout=180s
kubectl -n "${NAMESPACE}" get pods -o wide
```

**What happens**

| Command | Why |
|---|---|
| `set -euo pipefail` | Stop at the first error, on unset variables, and on failed pipes. A half-applied deploy must fail loudly |
| `aws s3 cp` | The instance role (not GitHub) fetches the manifests the pipeline rendered |
| `aws ecr get-login-password` | Short-lived (12 hour) registry token from the instance role |
| `k3s ctr ... images pull` | Pulls the image straight into the node's container runtime. This avoids creating and refreshing an `imagePullSecret` |
| `kubectl apply --server-side` | The API server merges the fields you own. It also avoids the annotation size limit that large CRDs hit with plain `apply` |
| `rollout status` | Blocks until the new pod is Ready, or fails. This is what turns "applied" into "deployed" |

### 15.2 `.github/workflows/ci-cd.yaml`

```yaml
name: ci-cd

on:
  pull_request:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read

env:
  AWS_REGION: ${{ vars.AWS_REGION }}
  ECR_REPOSITORY: ${{ vars.ECR_REPOSITORY }}
  # REPLACE with the version printed by "controller-gen --version" on your machine,
  # so the generated-code check below compares like with like.
  CONTROLLER_GEN_VERSION: v0.18.0

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  # ---------------------------------------------------------------- CI
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Install controller-gen
        run: |
          go install sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: Generated files are committed and up to date
        run: |
          controller-gen object paths=./api/v1alpha1
          controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
          controller-gen rbac:roleName=webapp-manager-role paths=./internal/controller output:rbac:artifacts:config=config/rbac
          if [ -n "$(git status --porcelain)" ]; then
            echo "Generated files differ from what is committed. Run controller-gen locally and commit:"
            git status --porcelain
            git diff
            exit 1
          fi

      - name: Build
        run: go build ./...

      - name: Vet
        run: go vet ./...

      - name: Unit tests
        run: go test ./internal/controller -run TestComputePhase -v

      - name: Docker build smoke test
        run: docker build -t webapp-operator:ci .

  # envtest runs a real kube-apiserver + etcd. Linux runners support it (Windows does not).
  # Non-blocking at first; remove "continue-on-error" once it passes reliably.
  integration:
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: Run envtest suite
        run: |
          go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
          export PATH="$PATH:$(go env GOPATH)/bin"
          export KUBEBUILDER_ASSETS="$(setup-envtest use -p path)"
          go test ./internal/controller/... -v

  # ---------------------------------------------------------------- CD
  build-push:
    needs: test
    if: github.event_name != 'pull_request' && github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    permissions:
      id-token: write        # allow the job to request a GitHub OIDC token
      contents: read
    outputs:
      image: ${{ steps.meta.outputs.image }}
    steps:
      - uses: actions/checkout@v4

      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ vars.AWS_ROLE_ARN }}
          aws-region: ${{ env.AWS_REGION }}

      - id: ecr
        uses: aws-actions/amazon-ecr-login@v2

      - id: meta
        run: echo "image=${{ steps.ecr.outputs.registry }}/${{ env.ECR_REPOSITORY }}:${{ github.sha }}" >> "$GITHUB_OUTPUT"

      - uses: docker/setup-buildx-action@v3

      - uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/amd64
          push: true
          provenance: false
          tags: ${{ steps.meta.outputs.image }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

  deploy:
    needs: build-push
    runs-on: ubuntu-latest
    concurrency: deploy-prod          # never run two deploys at once
    permissions:
      id-token: write
      contents: read
    env:
      IMAGE: ${{ needs.build-push.outputs.image }}
      BUCKET: ${{ vars.ARTIFACT_BUCKET }}
      INSTANCE_ID: ${{ vars.EC2_INSTANCE_ID }}
      SHA: ${{ github.sha }}
    steps:
      - uses: actions/checkout@v4

      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ vars.AWS_ROLE_ARN }}
          aws-region: ${{ env.AWS_REGION }}

      - name: Render manifests with the new image
        run: |
          mkdir -p out
          kubectl kustomize config/default | sed "s|image: controller:latest|image: ${IMAGE}|" > out/rendered.yaml
          grep -q "image: ${IMAGE}" out/rendered.yaml || { echo "image placeholder was not replaced"; exit 1; }

      - name: Upload deploy bundle to S3
        run: |
          aws s3 cp out/rendered.yaml "s3://${BUCKET}/deploy/${SHA}/rendered.yaml"
          aws s3 cp deploy/remote-deploy.sh "s3://${BUCKET}/deploy/${SHA}/remote-deploy.sh"

      - name: Roll out on the instance through SSM
        run: |
          set -euo pipefail
          PREFIX="s3://${BUCKET}/deploy/${SHA}"
          PARAMS=$(jq -n \
            --arg a "aws s3 cp ${PREFIX}/remote-deploy.sh /tmp/remote-deploy.sh --region ${AWS_REGION}" \
            --arg b "bash /tmp/remote-deploy.sh ${BUCKET} ${SHA} ${IMAGE} ${AWS_REGION}" \
            '{commands: [$a, $b]}')

          CMD_ID=$(aws ssm send-command \
            --instance-ids "${INSTANCE_ID}" \
            --document-name AWS-RunShellScript \
            --comment "deploy ${SHA}" \
            --parameters "${PARAMS}" \
            --query Command.CommandId --output text)
          echo "SSM command id: ${CMD_ID}"

          STATUS=Pending
          for _ in $(seq 1 90); do                       # up to 7.5 minutes
            sleep 5
            STATUS=$(aws ssm get-command-invocation --command-id "${CMD_ID}" --instance-id "${INSTANCE_ID}" \
                       --query Status --output text 2>/dev/null || echo Pending)
            case "${STATUS}" in Success|Failed|Cancelled|TimedOut) break ;; esac
          done

          echo "::group::remote stdout"
          aws ssm get-command-invocation --command-id "${CMD_ID}" --instance-id "${INSTANCE_ID}" --query StandardOutputContent --output text
          echo "::endgroup::"
          echo "::group::remote stderr"
          aws ssm get-command-invocation --command-id "${CMD_ID}" --instance-id "${INSTANCE_ID}" --query StandardErrorContent --output text
          echo "::endgroup::"

          echo "Final status: ${STATUS}"
          [ "${STATUS}" = "Success" ]
```

### What each job does and why

| Job / step | What happens | Why |
|---|---|---|
| `on:` triggers | PRs run CI only; pushes to `main` run CI then CD; `workflow_dispatch` lets you re-run by hand | Nothing reaches AWS from a pull request |
| `permissions: contents: read` | Default token is read-only | Least privilege |
| `go-version-file: go.mod` | CI uses the Go version your module declares | Local and CI builds agree |
| Pinned `CONTROLLER_GEN_VERSION` | Same generator version as your machine | Different versions write different header comments, which would fail the diff check |
| Generated-files check | Regenerates, then fails if `git status` shows any change or untracked file | Catches "I edited the types but forgot to regenerate/commit" |
| `go build`, `go vet`, unit test | Fast checks | Cheap early failure |
| Docker smoke test | Builds the image without pushing | Catches Dockerfile errors on PRs |
| `integration` job | Runs envtest on Linux | The test you could not run natively on Windows; non-blocking until it proves stable |
| `build-push` `if:` | Only on `main` | PRs cannot push images (and the OIDC trust policy would refuse them anyway) |
| `id-token: write` | Allows requesting the OIDC token | Required for `configure-aws-credentials` |
| Image tag = `github.sha` | One immutable tag per commit | You always know exactly what is running, and can roll back by SHA |
| `provenance: false` | Skip the extra attestation manifest | Keeps a plain single-platform image that `ctr` pulls cleanly |
| `cache-from/to: gha` | Reuse Docker layers between runs | Faster builds |
| `deploy` `concurrency` | Serialises deploys | Two overlapping rollouts could fight |
| Render + `grep -q` | Replaces the placeholder and verifies it worked | A failed `sed` would otherwise deploy `controller:latest` |
| S3 upload then SSM | The instance pulls its work from S3 | No inbound ports, no SSH keys |
| `jq -n --arg` | Builds the SSM parameters safely | Avoids shell-quoting bugs |
| Polling loop and final `[ ... ]` | Waits for the remote script and fails the job if it failed | The pipeline's red/green status reflects the real result |

---

## 28. Step 16: First deploy and verification

### 16.1 Push

```powershell
git add .
git status                                   # check .aws-setup/ and *.exe are NOT listed
git ls-files --eol deploy/remote-deploy.sh   # should show "i/lf" (LF endings)
git commit -m "Add Dockerfile, manifests and CI/CD pipeline"
git push -u origin main
```

Watch it:

```powershell
gh run list --limit 3
gh run watch
```

On success, the `deploy` job log shows the remote script's output ending with a pod in `Running`.

### 16.2 Verify on the instance

```powershell
Invoke-Remote ($K + 'kubectl -n webapp-operator-system get pods -o wide')
Invoke-Remote ($K + 'kubectl -n webapp-operator-system logs deploy/controller-manager --tail=30')
Invoke-Remote ($K + 'kubectl get crd | grep example.io')
```

You want a `Running` pod, `Starting workers` in the logs, and both CRDs present.

### 16.3 Create a WebApp on the cloud cluster

```powershell
Invoke-Remote ($K + @'
kubectl apply -f - <<'YAML'
apiVersion: apps.example.io/v1alpha1
kind: WebApp
metadata:
  name: demo
spec:
  image: nginx:1.27
  replicas: 1
  port: 80
YAML
sleep 20
kubectl get webapp,deploy,pods
'@)
```

Seeing the `demo` Deployment and pod created by your operator, running in AWS, is the end-to-end proof. (Replicas are 1 to respect small-instance memory.) Delete it when done:

```powershell
Invoke-Remote ($K + 'kubectl delete webapp demo')
```

---

## 29. Troubleshooting the deployment

| Symptom | Likely cause and fix |
|---|---|
| `Not authorized to perform sts:AssumeRoleWithWebIdentity` | The trust policy's `sub` does not match. It must be exactly `repo:OWNER/REPO:ref:refs/heads/main`, with the same owner, repo name and branch. Also check the job has `permissions: id-token: write`, and that the OIDC provider exists |
| `Credentials could not be loaded` / empty role ARN | The `AWS_ROLE_ARN` variable is unset. Run `gh variable list` |
| Generated-files check fails | Regenerate locally with the **same controller-gen version** as `CONTROLLER_GEN_VERSION`, and commit all changed files |
| `go: go.mod requires go >= X` in `docker build` | Set `GO_VERSION` in the Dockerfile |
| `COPY go.sum` fails | `go.sum` missing or ignored. Run `go mod tidy` and commit it |
| ECR push `no basic auth credentials` / denied | The role lacks `ecr:GetAuthorizationToken`, or the repository name in the policy differs from `ECR_REPOSITORY` |
| SSM `InvalidInstanceId` / instance not found | The SSM agent has not registered. Check `aws ssm describe-instance-information`. The instance needs the SSM managed policy and outbound internet access (a public IP in a public subnet, or VPC endpoints) |
| `AccessDeniedException ... ssm:SendCommand` | The instance ID or region in the role's policy is stale (for example after recreating the instance). Re-run step 13.3 |
| Remote script: `k3s: command not found` | k3s did not finish installing. Check `/var/log/cloud-init-output.log`; also confirm `PATH` includes `/usr/local/bin` |
| Remote script: `aws: command not found` | Not Amazon Linux 2023 (AWS CLI is preinstalled there). Install it, or use the right AMI |
| `ErrImagePull` / `ImagePullBackOff` | The image was pulled into the wrong containerd namespace (needs `-n k8s.io`), or the tag differs from the one in the manifest. Compare `kubectl get deploy ... -o jsonpath='{..image}'` with `k3s ctr -n k8s.io images ls` |
| Pod `CrashLoopBackOff` with `exec format error` | Architecture mismatch: ARM instance with an amd64 image. Use a `t3` instance |
| Pod `OOMKilled` or node `NotReady` | Out of memory. Use `t3.small`, confirm swap is active (`free -m`), or raise the memory limit |
| Operator logs show `forbidden` | RBAC gap. Add the marker, regenerate `config/rbac/role.yaml`, commit, redeploy |
| Readiness probe fails | The container is not listening on `:8081`. Check the `--health-probe-bind-address=:8081` argument |
| `Warning: ... last-applied-configuration` | Harmless if you mixed client-side and server-side apply. Use `--server-side` consistently |
| Deploy hangs on `rollout status` | Another pod is stuck; run `kubectl -n webapp-operator-system describe pod` through `Invoke-Remote` |
| Pipeline fails after instance replacement | Update the `EC2_INSTANCE_ID` variable (`gh variable set EC2_INSTANCE_ID --body $INSTANCE_ID`) and the role policy |

---

## 30. Day-2 operations, rollback, teardown and EKS

### 30.1 The normal change loop

```powershell
git switch -c feature/add-env-field
# ...edit api/ or internal/ ...
controller-gen object paths=./api/v1alpha1
controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
controller-gen rbac:roleName=webapp-manager-role paths=./internal/controller output:rbac:artifacts:config=config/rbac
go build ./...
go test ./internal/controller -run TestComputePhase
git add . ; git commit -m "Add env to WebApp spec"
git push -u origin feature/add-env-field        # CI runs on the pull request, no deploy
gh pr create --fill
gh pr merge --squash                             # merging to main triggers build, push and deploy
```

### 30.2 What happens when you modify or add a CRD

| Stage | Where | Effect |
|---|---|---|
| You edit `api/v1alpha1/*_types.go` | Laptop | New Go fields/markers |
| `controller-gen object` and `crd` | Laptop, **and verified again in CI** | Regenerated DeepCopy and CRD YAML, which you commit |
| Pull request | GitHub Actions | Build, vet, tests, generated-files check, Docker build. No AWS access |
| Merge to `main` | GitHub Actions | New image tagged with the commit SHA is pushed to ECR |
| `kubectl kustomize config/default` | Runner | CRDs, RBAC and Deployment rendered into one file |
| `kubectl apply --server-side` | Instance, via SSM | **The updated CRD schema is applied first (kustomize orders CRDs early), then the new operator rolls out.** Existing objects are not rewritten |
| New pod starts | Cluster | Old pod is replaced; leader election hands over; the new controller reconciles every existing WebApp once |

The same safe/risky rules from [section 12.5](#125-safe-vs-risky-changes) apply in production. Add optional fields freely; for renames, removals or type changes, introduce a new API version with a conversion path. Note that removing a CRD file from `config/crd` does **not** delete it from the cluster, because `apply` does not prune.

### 30.3 Roll back

```powershell
# Fastest: undo the last Deployment revision (the previous image is still on the node)
Invoke-Remote ($K + 'kubectl -n webapp-operator-system rollout undo deployment/controller-manager')

# Proper: revert the commit so Git matches what is running, then let the pipeline redeploy
git revert HEAD ; git push
```

`rollout undo` only changes the Deployment, not the CRDs. After a schema-changing release, prefer `git revert`.

### 30.4 Look at logs and status

```powershell
Invoke-Remote ($K + 'kubectl -n webapp-operator-system logs deploy/controller-manager --tail=100')
Invoke-Remote ($K + 'kubectl get webapp,batchjob -A')
Invoke-Remote ($K + 'kubectl -n webapp-operator-system get lease')     # who holds leader election
```

### 30.5 Teardown (do this when finished to stop all charges)

```powershell
aws ec2 terminate-instances --instance-ids $INSTANCE_ID
aws ec2 wait instance-terminated --instance-ids $INSTANCE_ID
aws ec2 delete-security-group --group-id $SG

aws ecr delete-repository --repository-name $REPO_NAME --force
aws s3 rb "s3://$BUCKET" --force

aws iam remove-role-from-instance-profile --instance-profile-name webapp-operator-ec2 --role-name webapp-operator-ec2
aws iam delete-instance-profile --instance-profile-name webapp-operator-ec2
aws iam delete-role-policy --role-name webapp-operator-ec2 --policy-name ec2-access
aws iam detach-role-policy --role-name webapp-operator-ec2 --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam delete-role --role-name webapp-operator-ec2

aws iam delete-role-policy --role-name webapp-operator-github --policy-name deploy
aws iam delete-role --role-name webapp-operator-github
# Only if no other project uses GitHub OIDC in this account:
aws iam delete-open-id-connect-provider --open-id-connect-provider-arn "arn:aws:iam::${ACCOUNT}:oidc-provider/token.actions.githubusercontent.com"
```

Then confirm in the console that **EC2 instances, EBS volumes, Elastic/public IPs, ECR and S3** are empty, and check the Billing dashboard the next day. Delete your CLI access key too.

### 30.6 Moving to EKS later

When you want a managed control plane (and accept the cost), the pipeline is nearly identical:

- Keep the `test` and `build-push` jobs as they are.
- Give the GitHub role access to the cluster (an EKS access entry for the role).
- Replace the S3 and SSM steps in `deploy` with: `aws eks update-kubeconfig --name <cluster>`, then `kubectl apply --server-side -f out/rendered.yaml` and `kubectl rollout status`.
- Delete the `ctr images pull` step: EKS nodes pull from ECR using the node role (`AmazonEC2ContainerRegistryReadOnly`), so `imagePullPolicy` can go back to `IfNotPresent` or `Always` without any manual pull.
- Mind the control-plane fee and the extended-support price jump for old Kubernetes versions.

### 30.7 Security notes

- **No stored AWS keys.** OIDC gives per-run credentials, restricted to one repo and branch.
- **No inbound ports.** Access is through SSM only; there is no SSH key to lose.
- **Least privilege on both roles.** Each action is scoped to one repository, one bucket prefix, one instance.
- **Short-lived registry tokens.** The ECR token is minted per deploy.
- **IMDSv2 required** on the instance, and a non-root, read-only container with all capabilities dropped.
- **Review third-party actions.** The workflow uses `actions/*`, `aws-actions/*` and `docker/*`. For stricter supply-chain control, pin them to full commit SHAs.
- **Protect `main`.** Require pull requests and passing checks in the repository settings, since a merge to `main` deploys.
- **Rotate and delete setup keys.** The admin access key from step 20 should not outlive the setup.

### 30.8 Further reading for Part 2

Links were written from memory of well-known documentation locations and may have moved; search for the title if one is broken.

- [AWS Free Tier](https://aws.amazon.com/free/) and [Tracking EC2 free tier usage](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-free-tier-usage.html)
- [Amazon EKS pricing](https://aws.amazon.com/eks/pricing/)
- [k3s documentation](https://docs.k3s.io/)
- [AWS Systems Manager Run Command](https://docs.aws.amazon.com/systems-manager/latest/userguide/run-command.html)
- [Amazon ECR user guide](https://docs.aws.amazon.com/AmazonECR/latest/userguide/what-is-ecr.html)
- [aws-actions/configure-aws-credentials](https://github.com/aws-actions/configure-aws-credentials) (README covers OIDC setup)
- [aws-actions/amazon-ecr-login](https://github.com/aws-actions/amazon-ecr-login)
- [docker/build-push-action](https://github.com/docker/build-push-action)
- [GitHub Actions documentation](https://docs.github.com/en/actions) (search "OpenID Connect in AWS")
- [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/)
- [Distroless images](https://github.com/GoogleContainerTools/distroless)
- [Declarative management with Kustomize](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/)
- [Server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)

---

## Next steps

- Add a `Degraded` condition and an all-or-nothing restart policy to `BatchJob`.
- Install Kueue in the cluster and make `queueName` do real admission.
- Add a Prometheus `ServiceMonitor` and alerts for reconcile errors (the metrics endpoint is already on `:8080`).
- Package the manifests as a Helm chart or add an `ArgoCD` Application for pull-based deployment.
- Hand-write a small client-go informer controller next to this one and compare them line by line.
