# GitOps for the WebApp operator: Argo CD + GitHub Actions

You already have an operator, CRDs, a Dockerfile and kustomize manifests in `config/`. This adds the missing loop: **push to Git, and the cluster follows**. Everything is ready to copy; type it yourself if you want the practice, and read the "why" notes as you go.

Verified here: all four kustomizations render with a real `kustomize build` against a stand-in for your `config/` tree, and all YAML lints. **Not run here** (no Docker/cluster in my environment): the GitHub Actions workflow and the Argo CD sync. Expect to fix small things on your first run; the troubleshooting table in section 9 covers the likely ones.

---

## 1. The big picture

```
                    CI (GitHub Actions)                          CD (Argo CD, inside your cluster)
 you ──push──▶ GitHub ──▶ test ─▶ manifests ─▶ e2e (kind)
                              │
                     main only▼
                      build image ─▶ ghcr.io/himanshuxone/webapp-operator:sha-abc1234
                              │
                              ▼
              commit "newTag: sha-abc1234" into gitops/overlays/dev   ◀── this commit IS the deployment
                                                │
                       Argo CD polls Git (30s) ─┘
                                │
          ┌─────────────────────┴──────────────────────┐
          ▼ wave 0                                      ▼ wave 1 (waits for wave 0 Healthy)
   Application "webapp-operator"                 Application "webapps-dev"
   gitops/overlays/dev                           gitops/workloads/dev
   CRDs + RBAC + controller Deployment           WebApp "demo", WebApp "hello"
                                                          │
                                                          ▼  your operator reconciles
                                                  Deployments / pods
```

**Two different "reconcilers" are stacked.** Argo CD reconciles *Git → WebApp custom resources*. Your operator reconciles *WebApp → Deployment*. Argo never touches the Deployment; the operator never reads Git. That clean split is the whole point, and it is a good thing to say out loud in an interview.

### The key ideas

| Idea | What it means here |
|---|---|
| **GitOps** | Git holds the *desired* state. A controller in the cluster continuously makes reality match it (OpenGitOps: declarative, versioned, pulled automatically, continuously reconciled) |
| **Pull, not push** | CI never has cluster credentials. Argo CD runs inside the cluster and pulls from Git. This is why local kind works: nothing outside needs to reach in |
| **CI vs CD split** | GitHub Actions answers "is this change good, and can I build it?" Argo CD answers "does the cluster match Git?" |
| **Image tag in Git** | CI does not deploy. It writes `newTag: sha-xxxxxxx` into a file and commits. Rolling back is `git revert` |
| **App of apps** | One hand-applied `root` Application points at a folder of Applications. Adding an environment is a file in Git, not a command |
| **Sync waves** | Wave 0 (operator, CRDs) must be Healthy before wave 1 (WebApps) is applied. Otherwise the CRs arrive before their CRD exists |
| **selfHeal / prune** | Hand edits in the cluster are reverted; things deleted from Git are deleted from the cluster |
| **Server-side apply** | CRDs exceed the 256KB annotation limit of client-side apply; SSA avoids it |

---

## 2. Prerequisites (Windows / PowerShell)

- Docker Desktop (running), `kind`, `kubectl`, `git`, and the repo from earlier parts pushed to `github.com/Himanshuxone/webapp-operator`.
- The repo should be **public** for the simplest setup (Argo reads it with no credentials, and the cluster pulls the image from GHCR with no pull secret). Private repo notes are in section 10.
- Check that these already exist and are committed (CI and Argo need them): `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`, `Dockerfile`.
- Prove your existing manifests render before adding anything:

```powershell
kubectl kustomize config/default | Select-String "kind:|image:"
```

You should see `image: controller:latest`. The overlays below replace exactly that name.

---

## 3. What you will add

```
.github/workflows/gitops-ci.yaml        CI: test, validate, e2e, build, promote
gitops/
├── base/kustomization.yaml             reuses config/default, protects CRDs from deletion
├── overlays/
│   ├── dev/kustomization.yaml          image pin that CI rewrites (Argo watches this)
│   └── e2e/kustomization.yaml          image used only inside the CI test cluster
├── workloads/dev/                      the WebApps. THE folder you edit to test GitOps
│   ├── kustomization.yaml  namespace.yaml  webapp-demo.yaml  webapp-hello.yaml
├── argocd/                             what the root app watches
│   ├── kustomization.yaml  project.yaml  operator-app.yaml  workloads-app.yaml
└── bootstrap/
    ├── root-app.yaml                   the one Application applied by hand
    └── argocd-cm-patch.yaml            30s polling + WebApp health check
scripts/bootstrap.ps1  scripts/refresh.ps1
```

Create the folders:

```powershell
New-Item -ItemType Directory -Force .github\workflows, gitops\base, gitops\overlays\dev, gitops\overlays\e2e, gitops\workloads\dev, gitops\argocd, gitops\bootstrap, scripts | Out-Null
```

> **Line endings.** Your `.gitattributes` forces LF. If you create files in Notepad, save as UTF-8 **without BOM**; VS Code is safer.

---

## 4. The files

### 4.1 `gitops/base/kustomization.yaml`

```yaml
# gitops/base/kustomization.yaml
#
# The "base" is everything that is the same in every environment: your existing
# kustomize entry point (config/default = namespace + CRDs + RBAC + manager Deployment).
# Overlays (dev, e2e) only change the container image.
#
# We REUSE config/default instead of copying it, so there is one source of truth.
# Kustomize allows referencing a directory outside this folder as long as it is a
# directory with its own kustomization.yaml (only individual files outside are blocked).

resources:
  - ../../config/default

# Safety net for Argo CD: never delete the CRDs.
#   Prune=false  -> if a CRD file disappears from Git, Argo will NOT delete it
#   Delete=false -> if the Argo Application is deleted, the CRDs stay
# Deleting a CRD deletes EVERY WebApp and BatchJob in the cluster, so we require a human to do it.
patches:
  - target:
      kind: CustomResourceDefinition
    patch: |-
      apiVersion: apiextensions.k8s.io/v1
      kind: CustomResourceDefinition
      metadata:
        name: not-used-when-target-is-set
        annotations:
          argocd.argoproj.io/sync-options: Prune=false,Delete=false
```

**Why:** one source of truth. Your `config/default` already produces namespace + CRDs + RBAC + Deployment; the base just reuses it. The patch adds an Argo annotation so a mistake in Git can never delete the CRDs (which would wipe every WebApp). The `name:` inside the patch is ignored because `target:` selects the objects; I verified your `controller-gen` annotation survives the merge.

### 4.2 `gitops/overlays/dev/kustomization.yaml`

```yaml
# gitops/overlays/dev/kustomization.yaml
#
# This is the file Argo CD watches for the operator ("webapp-operator" Application).
# GitHub Actions rewrites ONLY the newTag line after each successful build, then commits.
# That commit is the "deployment": Git is the record of what runs in the cluster.
#
# Do not hand-edit newTag unless you are rolling back (see the README).

resources:
  - ../../base

images:
  # "controller" is the placeholder image name in config/manager/manager.yaml
  - name: controller
    newName: ghcr.io/himanshuxone/webapp-operator   # MUST be lowercase (GHCR requirement)
    newTag: sha-0000000                              # CI replaces this line; keep the key "newTag:"
```

**Why:** this is the *only* file CI edits. `name: controller` matches the placeholder in `config/manager/manager.yaml`. The tag is a commit SHA, never `latest`: with `latest`, Git would not say which build is running, and a rollback would be impossible.

### 4.3 `gitops/overlays/e2e/kustomization.yaml`

```yaml
# gitops/overlays/e2e/kustomization.yaml
# Used ONLY by the GitHub Actions end-to-end job. The image is built in the job and loaded
# straight into a throw-away kind cluster, so it never touches a registry.

resources:
  - ../../base

images:
  - name: controller
    newName: webapp-operator
    newTag: e2e
```

**Why:** CI tests the same manifests Argo will apply, but with an image loaded straight into a throw-away kind cluster.

### 4.4 `gitops/workloads/dev/*`

`kustomization.yaml`

```yaml
# gitops/workloads/dev/kustomization.yaml
# The WebApp custom resources for the dev environment. THIS is the folder you edit
# to test GitOps: change a replica count, push, and watch the cluster follow.

namespace: webapps-dev

resources:
  - namespace.yaml
  - webapp-demo.yaml
  - webapp-hello.yaml
```

`namespace.yaml`

```yaml
# gitops/workloads/dev/namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: webapps-dev
```

`webapp-demo.yaml`

```yaml
# gitops/workloads/dev/webapp-demo.yaml
# A WebApp custom resource. The operator turns this into a Deployment named "demo".
# Fields come from api/v1alpha1/webapp_types.go: image (required), replicas, port.
apiVersion: apps.example.io/v1alpha1
kind: WebApp
metadata:
  name: demo
spec:
  image: nginx:1.27
  replicas: 2
  port: 80
```

`webapp-hello.yaml`

```yaml
# gitops/workloads/dev/webapp-hello.yaml
apiVersion: apps.example.io/v1alpha1
kind: WebApp
metadata:
  name: hello
spec:
  image: nginxdemos/hello:plain-text
  replicas: 1
  port: 80
```

**Why:** these use only the fields in your `WebAppSpec` (`image`, `replicas`, `port`). If you have added fields since (such as `network`), add them here too and CI will validate them against your real CRD.

### 4.5 `gitops/argocd/project.yaml`

```yaml
# gitops/argocd/project.yaml
# AppProject = a guardrail. It limits WHERE these apps may pull from and WHAT they may touch.
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: webapp-platform
  namespace: argocd
spec:
  description: WebApp operator and the WebApps it manages

  sourceRepos:
    - https://github.com/Himanshuxone/webapp-operator.git

  destinations:
    - server: https://kubernetes.default.svc
      namespace: webapp-operator-system
    - server: https://kubernetes.default.svc
      namespace: webapps-dev

  # Cluster-scoped kinds these apps may create. Anything else is rejected.
  clusterResourceWhitelist:
    - group: ""
      kind: Namespace
    - group: apiextensions.k8s.io
      kind: CustomResourceDefinition
    - group: rbac.authorization.k8s.io
      kind: ClusterRole
    - group: rbac.authorization.k8s.io
      kind: ClusterRoleBinding
```

**Why:** an AppProject is a guardrail. These Applications can only read this repo, only deploy to two namespaces, and only create four kinds of cluster-wide objects. A staff-level answer to "how do you stop a GitOps repo becoming a way to take over the cluster?" starts here.

### 4.6 `gitops/argocd/operator-app.yaml`

```yaml
# gitops/argocd/operator-app.yaml
# Application 1: the operator itself (CRDs + RBAC + controller Deployment).
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: webapp-operator
  namespace: argocd
  annotations:
    # Sync wave 0 = first. The workloads app (wave 1) waits until this app is Healthy,
    # i.e. until the CRDs exist and the controller Deployment is Available.
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: webapp-platform
  source:
    repoURL: https://github.com/Himanshuxone/webapp-operator.git
    targetRevision: main
    path: gitops/overlays/dev
  destination:
    server: https://kubernetes.default.svc
    namespace: webapp-operator-system
  syncPolicy:
    automated:
      prune: true      # resource removed from Git -> removed from the cluster
      selfHeal: true   # someone edits the cluster by hand -> Argo puts it back
    syncOptions:
      # CRDs are large; client-side apply stores a copy in an annotation and fails past 256KB.
      # Server-side apply avoids that and is the right default for CRDs.
      - ServerSideApply=true
```

### 4.7 `gitops/argocd/workloads-app.yaml`

```yaml
# gitops/argocd/workloads-app.yaml
# Application 2: the WebApp custom resources (what users would normally ask for).
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: webapps-dev
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  project: webapp-platform
  source:
    repoURL: https://github.com/Himanshuxone/webapp-operator.git
    targetRevision: main
    path: gitops/workloads/dev
  destination:
    server: https://kubernetes.default.svc
    namespace: webapps-dev
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      # Safety net: if the WebApp CRD is not registered yet, don't fail the dry run; retry.
      - SkipDryRunOnMissingResource=true
    retry:
      limit: 5
      backoff:
        duration: 10s
        factor: 2
        maxDuration: 2m
```

**Why `SkipDryRunOnMissingResource` and `retry`:** belt and braces. Waves should already order things, but a WebApp applied before its CRD exists would otherwise fail permanently.

### 4.8 `gitops/argocd/kustomization.yaml`

```yaml
# gitops/argocd/kustomization.yaml
# The folder the ROOT Application watches ("app of apps").
# Adding a new environment later = adding another Application file here.
resources:
  - project.yaml
  - operator-app.yaml
  - workloads-app.yaml
```

### 4.9 `gitops/bootstrap/root-app.yaml`

```yaml
# gitops/bootstrap/root-app.yaml
# The ONE thing applied by hand (by scripts/bootstrap.ps1). Everything else is pulled from Git.
# It watches gitops/argocd/, which contains the AppProject and the two child Applications.
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: root
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/Himanshuxone/webapp-operator.git
    targetRevision: main
    path: gitops/argocd
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

**Why:** this is the only object you apply by hand. Note its `project: default`: it has to create the AppProject itself, so it cannot live inside that project.

### 4.10 `gitops/bootstrap/argocd-cm-patch.yaml`

```yaml
# gitops/bootstrap/argocd-cm-patch.yaml
# Merged into Argo CD's own ConfigMap by scripts/bootstrap.ps1.
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-cm
  namespace: argocd
data:
  # Poll Git every 30s instead of the default (~2-3 min) so demos feel quick.
  # Production would use a GitHub webhook instead of polling.
  timeout.reconciliation: 30s

  # Teach Argo how to judge a WebApp's health. Without this, Argo shows every custom
  # resource as Healthy the moment it exists. Key format: resource.customizations.health.<group>_<Kind>
  resource.customizations.health.apps.example.io_WebApp: |
    hs = {}
    if obj.status ~= nil and obj.status.conditions ~= nil then
      for _, c in ipairs(obj.status.conditions) do
        if c.type == "Available" then
          if c.status == "True" then
            hs.status = "Healthy"
            hs.message = "Available"
          else
            hs.status = "Progressing"
            hs.message = c.message or "Not available yet"
          end
          return hs
        end
      end
    end
    hs.status = "Progressing"
    hs.message = "Waiting for the operator to report status"
    return hs
```

**Why the Lua health check:** Argo cannot know what "healthy" means for a custom resource, so by default every WebApp is Healthy instantly. This script reads your `Available` status condition, so Argo shows **Progressing** until the operator reports the app is up. It also means wave 1 genuinely waits for readiness. (A WebApp with `replicas: 0` will stay Progressing, because it is never Available. Adjust the script if you use scale-to-zero.)

### 4.11 `.github/workflows/gitops-ci.yaml`

```yaml
# .github/workflows/gitops-ci.yaml
#
# CI half of the GitOps loop. It NEVER touches the cluster.
#
#   pull request  ->  test + validate manifests + end-to-end test in a throw-away kind cluster
#   push to main  ->  all of the above, then build + push the image to GHCR,
#                     then commit the new image tag into gitops/overlays/dev
#
# The CD half is Argo CD, running in your cluster: it sees that commit and syncs.
#
# NOTE: if you still have .github/workflows/ci-cd.yaml (the AWS/EC2 deploy from Part 2),
# change its trigger to "workflow_dispatch" so both do not run on every push.

name: gitops-ci

on:
  pull_request:
  push:
    branches: [main]
    # Skip runs caused by edits to the GitOps folder or docs (also stops the tag-bump commit
    # from re-triggering CI; commits made with GITHUB_TOKEN do not trigger workflows anyway).
    paths-ignore:
      - "gitops/**"
      - "**.md"
  workflow_dispatch:

permissions:
  contents: read

env:
  # Run "controller-gen --version" locally and put the same version here.
  CONTROLLER_GEN_VERSION: v0.18.0

concurrency:
  group: gitops-ci-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  # ------------------------------------------------------------------ 1. unit-level checks
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

      # If you changed api/v1alpha1/*.go but forgot to regenerate the CRD, Argo would deploy a
      # stale CRD. This step fails the build instead.
      - name: Generated files are committed and up to date
        run: |
          controller-gen object paths=./api/v1alpha1
          controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
          controller-gen rbac:roleName=webapp-manager-role paths=./internal/controller output:rbac:artifacts:config=config/rbac
          if [ -n "$(git status --porcelain)" ]; then
            echo "::error::Generated files differ from what is committed. Run controller-gen locally and commit."
            git status --porcelain
            git diff
            exit 1
          fi

      - run: go build ./...
      - run: go vet ./...
      - run: go test ./internal/controller -run TestComputePhase -v

  # ------------------------------------------------------------------ 2. do the manifests render?
  manifests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Render every kustomization
        run: |
          set -e
          for d in gitops/overlays/dev gitops/overlays/e2e gitops/workloads/dev gitops/argocd; do
            echo "== kubectl kustomize $d"
            kubectl kustomize "$d" > /dev/null
          done
      - name: Dev overlay uses the GHCR image and a pinned tag
        run: |
          expected="ghcr.io/${GITHUB_REPOSITORY,,}"
          grep -q "newName: ${expected}" gitops/overlays/dev/kustomization.yaml \
            || { echo "::error::newName in gitops/overlays/dev must be ${expected}"; exit 1; }
          if grep -q "newTag: latest" gitops/overlays/dev/kustomization.yaml; then
            echo "::error::Do not deploy :latest. Git must say exactly which build runs."; exit 1
          fi

  # ------------------------------------------------------------------ 3. real cluster test
  # Builds the image, loads it into a disposable kind cluster, installs the operator exactly the
  # way Argo would (same kustomize files), applies the SAME WebApp YAML that Argo will apply,
  # and checks that the operator really creates and scales the Deployment.
  e2e:
    needs: [test, manifests]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Read Go version from go.mod
        id: gov
        run: echo "v=$(grep -E '^go [0-9.]+' go.mod | awk '{print $2}')" >> "$GITHUB_OUTPUT"

      - name: Build operator image
        run: docker build --build-arg GO_VERSION=${{ steps.gov.outputs.v }} -t webapp-operator:e2e .

      - uses: helm/kind-action@v1
        with:
          cluster_name: e2e

      - name: Load image into kind
        run: kind load docker-image webapp-operator:e2e --name e2e

      - name: Install the operator (same manifests Argo uses)
        run: |
          kubectl apply --server-side -k gitops/overlays/e2e
          kubectl -n webapp-operator-system rollout status deploy/controller-manager --timeout=120s

      # Catches a bad edit in Git before Argo ever sees it, for example replicas: -1
      # (rejected by the CRD schema's Minimum=0) or a misspelt field.
      - name: Server-side validate the WebApps against the real CRD
        run: |
          kubectl create namespace webapps-dev
          kubectl apply --server-side --dry-run=server -k gitops/workloads/dev

      - name: Apply the WebApps and wait for the operator to reconcile them
        run: |
          kubectl apply --server-side -k gitops/workloads/dev
          kubectl -n webapps-dev wait --for=condition=Available deploy/demo --timeout=180s
          kubectl -n webapps-dev wait --for=condition=Available deploy/hello --timeout=180s

      - name: Scale through the CRD and check the operator follows
        run: |
          kubectl -n webapps-dev patch webapp demo --type merge -p '{"spec":{"replicas":3}}'
          kubectl -n webapps-dev rollout status deploy/demo --timeout=120s
          test "$(kubectl -n webapps-dev get deploy demo -o jsonpath='{.status.readyReplicas}')" = "3"

      - name: Delete a WebApp and check the finalizer lets it go
        run: |
          kubectl -n webapps-dev delete webapp hello --timeout=60s
          # The Deployment has an ownerReference to the WebApp, so Kubernetes garbage-collects it.
          kubectl -n webapps-dev wait --for=delete deploy/hello --timeout=60s

      - name: Debug output on failure
        if: failure()
        run: |
          kubectl get all,webapps -A || true
          kubectl -n webapp-operator-system logs deploy/controller-manager --tail=200 || true

  # ------------------------------------------------------------------ 4. publish + promote (main only)
  # "Promote" = write the new image tag into Git. Argo CD does the rest.
  publish:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    needs: [e2e]
    runs-on: ubuntu-latest
    permissions:
      contents: write    # to commit the new tag
      packages: write    # to push to ghcr.io
    # Never run two promotions at once: they would race on the same file.
    concurrency:
      group: promote-dev
      cancel-in-progress: false
    steps:
      - uses: actions/checkout@v4

      - name: Compute image name and tag
        id: meta
        run: |
          echo "image=ghcr.io/${GITHUB_REPOSITORY,,}" >> "$GITHUB_OUTPUT"
          echo "tag=sha-${GITHUB_SHA::7}" >> "$GITHUB_OUTPUT"
          echo "gov=$(grep -E '^go [0-9.]+' go.mod | awk '{print $2}')" >> "$GITHUB_OUTPUT"

      - uses: docker/setup-buildx-action@v3

      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ${{ steps.meta.outputs.image }}:${{ steps.meta.outputs.tag }}
          build-args: GO_VERSION=${{ steps.meta.outputs.gov }}
          # Links the package to this repo so it inherits the repo's visibility.
          labels: org.opencontainers.image.source=https://github.com/${{ github.repository }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

      - name: Commit the new tag to the GitOps overlay
        run: |
          set -e
          f=gitops/overlays/dev/kustomization.yaml
          sed -i "s|^\(\s*newTag:\).*|\1 ${{ steps.meta.outputs.tag }}|" "$f"
          git diff --stat
          if git diff --quiet; then echo "Tag unchanged, nothing to commit"; exit 0; fi
          git config user.name  "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add "$f"
          git commit -m "deploy: webapp-operator ${{ steps.meta.outputs.tag }} [skip ci]"
          # Another merge may have landed while we were building.
          git pull --rebase origin main
          git push origin HEAD:main
```

**What each job proves**

| Job | Catches |
|---|---|
| `test` | Compile errors, vet issues, and **forgetting to regenerate the CRD** after editing `webapp_types.go` (the check fails if `controller-gen` output differs from what is committed) |
| `manifests` | A kustomization that no longer renders; an accidental `newTag: latest`; a wrong image name |
| `e2e` | The real thing: operator starts, **server-side dry-run rejects a bad WebApp** (for example `replicas: -1`), operator creates Deployments, scaling through the CRD works, deletion works (finalizer) |
| `publish` | Builds and pushes `ghcr.io/himanshuxone/webapp-operator:sha-<7 chars>`, then commits that tag. Runs only on `main`, only after everything passed |

**Design notes**

- `paths-ignore: gitops/**` on push means editing a WebApp YAML does not rebuild an image. Pull requests are still validated in full.
- The promote commit uses `GITHUB_TOKEN`, so GitHub will not re-trigger workflows from it, and the message carries `[skip ci]` as a second guard. This is how you avoid the classic infinite loop.
- `concurrency: promote-dev` plus `git pull --rebase` handles two merges landing close together.
- If you later protect `main` (pull requests required), this direct push will be rejected. The usual fix is a separate config repo, or opening a PR for the tag bump (section 10).

### 4.12 `scripts/bootstrap.ps1`

```powershell
<#
 scripts/bootstrap.ps1  (run from the repository root, in PowerShell)

 Creates a local kind cluster, installs Argo CD, teaches it about WebApp health, and applies the
 ONE root Application. After this, Argo pulls everything else from GitHub.

 Prerequisites: Docker Desktop running, plus kind and kubectl on PATH.
 Usage:   .\scripts\bootstrap.ps1
 Remove:  kind delete cluster --name gitops
#>
$ErrorActionPreference = "Stop"
$Cluster = "gitops"

foreach ($tool in "docker", "kind", "kubectl") {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool not found on PATH" }
}
docker info *> $null
if ($LASTEXITCODE -ne 0) { throw "Docker is not running. Start Docker Desktop and retry." }

# 1. Cluster ---------------------------------------------------------------
if (-not (kind get clusters | Select-String -SimpleMatch $Cluster)) {
    kind create cluster --name $Cluster
} else {
    Write-Host "kind cluster '$Cluster' already exists, reusing it"
}
kubectl config use-context "kind-$Cluster" | Out-Null

# 2. Argo CD ---------------------------------------------------------------
# "stable" tracks the latest release. For repeatable setups, replace it with a version tag.
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
# --server-side: Argo's own CRDs are too big for client-side apply.
kubectl apply -n argocd --server-side --force-conflicts `
    -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
if ($LASTEXITCODE -ne 0) { throw "Argo CD install failed" }

Write-Host "Waiting for Argo CD to be ready (1-3 minutes)..."
kubectl -n argocd rollout status deploy/argocd-server --timeout=300s
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s

# 3. Faster polling + WebApp health check ----------------------------------
kubectl apply -n argocd --server-side --force-conflicts -f gitops/bootstrap/argocd-cm-patch.yaml
kubectl -n argocd rollout restart statefulset/argocd-application-controller
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=120s

# 4. The root Application ---------------------------------------------------
kubectl apply -f gitops/bootstrap/root-app.yaml

# 5. Login details ----------------------------------------------------------
$b64 = kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}"
$pw  = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b64))

Write-Host ""
Write-Host "Argo CD is installed. Open a SECOND terminal and run:" -ForegroundColor Green
Write-Host "    kubectl -n argocd port-forward svc/argocd-server 8080:443"
Write-Host "Then browse to https://localhost:8080  (accept the certificate warning)"
Write-Host "    user:     admin"
Write-Host "    password: $pw"
Write-Host ""
Write-Host "Watch it converge:  kubectl get applications -n argocd -w"
```

### 4.13 `scripts/refresh.ps1`

```powershell
# Ask Argo to re-read Git right now instead of waiting for the next poll.
param([string]$App = "root")
kubectl -n argocd annotate application $App argocd.argoproj.io/refresh=hard --overwrite
foreach ($a in "webapp-operator", "webapps-dev") {
    kubectl -n argocd annotate application $a argocd.argoproj.io/refresh=hard --overwrite 2>$null
}
kubectl get applications -n argocd
```

---

## 5. Commit, push, and watch CI

Disable the old EC2 pipeline's push trigger first (edit `.github/workflows/ci-cd.yaml`, set `on:` to just `workflow_dispatch:`), otherwise both pipelines run.

```powershell
git add .github gitops scripts
git commit -m "Add GitOps: Argo CD apps, kustomize overlays and CI"
git push origin main
```

Open the **Actions** tab. The first run builds the image, so `publish` takes a few minutes. When it finishes you will see a new commit by `github-actions[bot]`:

```powershell
git pull
git log --oneline -3
Select-String newTag gitops\overlays\dev\kustomization.yaml     # newTag: sha-xxxxxxx
```

**Make the image pullable.** On GitHub: your profile → **Packages** → `webapp-operator` → **Package settings** → set visibility to **Public** if it is not already (the `source` label normally makes it inherit the repo's visibility). Without this the pod sticks in `ImagePullBackOff`.

---

## 6. Bootstrap the cluster

```powershell
.\scripts\bootstrap.ps1
```

When it finishes, in a **second terminal**:

```powershell
kubectl -n argocd port-forward svc/argocd-server 8080:443
```

Browse to `https://localhost:8080`, user `admin`, and the password printed by the script. Then watch:

```powershell
kubectl get applications -n argocd -w
```

Expected sequence: `root` Synced → `webapp-operator` Syncing → Healthy → `webapps-dev` Syncing → Healthy.

```powershell
kubectl get pods -n webapp-operator-system
kubectl get webapp,deploy,pods -n webapps-dev
```

In the Argo UI, open `webapps-dev` and look at the tree: **WebApp → Deployment → ReplicaSet → Pod**. Argo shows the Deployment even though it does not own it, because your operator set an ownerReference.

---

## 7. Test it: the experiments

Each one is a small change you make, push, and observe. Use `.\scripts\refresh.ps1` to skip the 30-second poll.

### Experiment A: change a WebApp in Git (the core GitOps loop)

Edit `gitops/workloads/dev/webapp-demo.yaml`: `replicas: 2` → `replicas: 4`.

```powershell
git commit -am "demo: scale to 4"; git push
.\scripts\refresh.ps1
kubectl get webapp,deploy -n webapps-dev
```

*Chain of events:* Git → Argo patches the `WebApp` → your operator's watch fires → `Reconcile` updates the Deployment → pods start. Two controllers, no `kubectl apply` from you. Try changing `image: nginx:1.27` to `nginx:1.28` too and watch the rollout.

### Experiment B: self-heal (prove Git is the boss)

```powershell
'{"spec":{"replicas":9}}' | Set-Content -NoNewline patch.json
kubectl -n webapps-dev patch webapp demo --type merge --patch-file patch.json
kubectl get webapp demo -n webapps-dev -w
```

Within seconds Argo marks it **OutOfSync** and reverts it to the value in Git. Also try `kubectl delete webapp demo -n webapps-dev`; Argo recreates it, and the operator recreates the Deployment.

*Subtle point to remember:* scale the **WebApp**, not the Deployment. Argo manages the WebApp; the operator owns the Deployment and would fight you on it regardless.

### Experiment C: ship a new operator version

Make a visible change in your controller, for example add a log line in `Reconcile`, then:

```powershell
git commit -am "log reconcile start"; git push
```

Watch Actions: test → manifests → e2e → publish → bot commit. Then:

```powershell
git pull
kubectl get deploy controller-manager -n webapp-operator-system -o jsonpath="{.spec.template.spec.containers[0].image}"
kubectl logs -n webapp-operator-system deploy/controller-manager --tail=20
```

The image tag equals your commit SHA's first 7 characters.

### Experiment D: change the CRD itself

Add a field to `WebAppSpec` (for example a `Message string` with the json tag `message,omitempty`), then regenerate and commit **everything**:

```powershell
controller-gen object paths=./api/v1alpha1
controller-gen crd paths=./api/v1alpha1 output:crd:artifacts:config=config/crd/bases
go build ./...
git add -A; git commit -m "WebApp: add message"; git push
```

If you forget the `controller-gen` lines, CI's `test` job fails with "Generated files differ". After merge, Argo applies the new CRD (server-side apply) **and** the new operator together.

### Experiment E: CI blocks a bad change before Argo sees it

On a branch, set `replicas: -1` in `webapp-demo.yaml` and open a pull request:

```powershell
git switch -c bad-replicas
# edit the file, then:
git commit -am "bad replicas"; git push -u origin bad-replicas
```

The `e2e` job fails at "Server-side validate the WebApps against the real CRD", because your CRD says `Minimum=0`. The point: **validation lives in the CRD schema, and CI uses the real CRD**, so broken YAML never reaches `main`, so never reaches Argo.

### Experiment F: rollback

```powershell
git revert HEAD
git push
```

Argo converges the cluster to the previous state. For an operator rollback, revert the bot's `deploy:` commit (or set `newTag` back to an earlier SHA).

### Experiment G: prune

Delete `webapp-hello.yaml` and its line in `workloads/dev/kustomization.yaml`, push. The `hello` WebApp disappears, your finalizer runs, and the Deployment is garbage-collected.

---

## 8. What to look at in Argo CD

| Where | What it shows |
|---|---|
| Applications list | Sync status (Git vs cluster) and Health (is it working) are separate columns. Synced but Progressing means "applied, not up yet" |
| App → **Details → Diff** | Exactly what differs after a manual edit |
| App → **History and rollback** | Every synced commit |
| Wave ordering | `kubectl get applications -n argocd -w` shows `webapps-dev` only starting after `webapp-operator` is Healthy |

---

## 9. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `test` fails: "Generated files differ" | You changed Go types without running `controller-gen`. Run the three commands in section 7D and commit the changes. Also check `CONTROLLER_GEN_VERSION` in the workflow equals `controller-gen --version` locally |
| `e2e`: `deploy/demo` not found | The operator names the Deployment after the WebApp. If yours differs, adjust the `wait` lines in the workflow |
| `e2e`: image pull errors in kind | `kind load` must run after `docker build` and before the install, and the overlay must use `webapp-operator:e2e` (it does) |
| `publish` fails: `permission_denied: write_package` | Repo → Settings → Actions → General → Workflow permissions; allow write, or check the package is not linked to a different repository |
| `publish`: `git push` rejected | `main` is protected. Use a config repo or a PR-based promote (section 10) |
| Pod `ImagePullBackOff` | The GHCR package is private. Make it Public, or create an `imagePullSecret` |
| Application `ComparisonError ... path does not exist` | The overlay path in the Application does not match your repo, or Argo cannot read the repo (private repo needs credentials) |
| Application `Unknown` / cannot find `config/default` | Argo needs the whole repo: `config/` must be committed. Kustomize builds `gitops/base` which references `../../config/default` |
| `webapps-dev` stuck **Progressing** | Argo is waiting for the `Available` condition. Run `kubectl get webapp demo -n webapps-dev -o yaml` and look at `status.conditions`. If your condition type is not `Available`, edit the Lua in `argocd-cm-patch.yaml` |
| `webapps-dev` fails: `no matches for kind WebApp` | CRD not yet registered. Retry runs every few seconds; check `webapp-operator` is Synced |
| `OutOfSync` that never clears (CRD) | Set `ServerSideApply=true` (already set). If a field is defaulted by the API server, add an `ignoreDifferences` block |
| Argo UI cannot be reached | The port-forward terminal was closed. Restart it |
| Sync is slow | Polling is 30s. Use `scripts\refresh.ps1`, or add a GitHub webhook (needs a public URL; a tunnel such as ngrok can provide one for local kind) |
| Reset everything | `kind delete cluster --name gitops`, then run `bootstrap.ps1` again |

Useful diagnostics:

```powershell
kubectl get applications -n argocd
kubectl describe application webapp-operator -n argocd
kubectl -n argocd logs statefulset/argocd-application-controller --tail=50
kubectl -n argocd logs deploy/argocd-repo-server --tail=50
```

---

## 10. Where to take it next

1. **Split into two repos (the production pattern).** App repo = code and CI. Config repo = `gitops/`. CI opens a PR (or pushes with a deploy key) to the config repo. This allows branch protection on the app repo, separate permissions, and a clean audit log of "what was deployed when".
2. **PR-based promotion.** Replace the direct push with `peter-evans/create-pull-request`, so tag bumps are reviewed or auto-merged by rule.
3. **Multiple environments.** Add `overlays/staging` and `overlays/prod` plus a matching `workloads/` folder, then generate the Applications with an **ApplicationSet** instead of copying files. Promote by copying the tag from staging to prod.
4. **Private repo.** Add credentials to Argo: `argocd repo add` with a fine-grained token, or a `Secret` labelled `argocd.argoproj.io/secret-type: repository` in the `argocd` namespace. Pulling a private GHCR image needs an `imagePullSecret`.
5. **Webhook** instead of polling.
6. **Argo CD Image Updater** (alternative to the CI commit): Argo watches the registry itself. The CI-commit approach used here is more explicit and auditable, which is why I chose it.
7. **Signed images and policy:** `cosign` in CI, plus Kyverno or Gatekeeper to require signatures.
8. **Safer rollouts** of the operator with Argo Rollouts; and for secrets, External Secrets or SOPS rather than plain Git.

---

## 11. Talking about this in an interview

Say what you built and be exact about its scope: *a learning setup on local kind, not production.*

> “I built a GitOps loop for my own Kubernetes operator. GitHub Actions does CI only: it checks that generated CRDs are committed, builds the image, and runs an end-to-end test in a throw-away kind cluster using the same manifests that deploy. On main it pushes an image tagged with the commit SHA and commits that tag into a kustomize overlay. Argo CD, running in the cluster, pulls that repo, so CI never has cluster credentials. I used an app-of-apps with sync waves so the operator and CRDs are healthy before the custom resources are applied, and a custom health check so Argo understands my WebApp's status condition. Rollback is a git revert.”

Likely follow-ups and honest answers:

| Question | Answer |
|---|---|
| Why not `kubectl apply` from CI? | Needs cluster credentials in CI, no drift detection, and Git is no longer the record of what is running |
| Why a SHA tag, not `latest`? | Immutable and traceable; makes Git the real record and rollback a revert |
| How do you avoid a CI/CD loop? | Path filters, `[skip ci]`, and commits from `GITHUB_TOKEN` do not trigger workflows |
| Who owns the Deployment? | The operator (ownerReference). Argo owns only the WebApp. Two reconcile loops, one source of truth each |
| What breaks if CRD and CR ship together? | Ordering. Sync waves plus health checks, and SkipDryRunOnMissingResource as a backstop |
| Why protect CRDs with Prune=false? | Deleting a CRD deletes every custom resource of that kind |
| What would you change for production? | Separate config repo, PR-based promotion, ApplicationSets per environment, private registry with pull secrets, signed images, policy, webhooks, and an alert on OutOfSync |

Do not claim production Argo CD experience from this; claim that you understand the model and have built a working example.

---

## 12. Cleanup

```powershell
kind delete cluster --name gitops
```