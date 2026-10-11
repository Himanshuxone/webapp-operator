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
