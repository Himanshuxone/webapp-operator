# Ask Argo to re-read Git right now instead of waiting for the next poll.
param([string]$App = "root")
kubectl -n argocd annotate application $App argocd.argoproj.io/refresh=hard --overwrite
foreach ($a in "webapp-operator", "webapps-dev") {
    kubectl -n argocd annotate application $a argocd.argoproj.io/refresh=hard --overwrite 2>$null
}
kubectl get applications -n argocd
