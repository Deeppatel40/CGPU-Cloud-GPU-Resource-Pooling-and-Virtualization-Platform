#!/usr/bin/env bash
# Build a kubeconfig for the cgpu-api ServiceAccount (deploy/k8s/cgpu-api-rbac.yaml).
# Run on the control plane with admin kubectl access. Writes ./api-kubeconfig (mode 600);
# copy it to the API host as /etc/cgpu/api-kubeconfig.
#
#   scripts/make-api-kubeconfig.sh [server-url]   (default https://10.20.24.31:6443)
set -euo pipefail

SERVER="${1:-https://10.20.24.31:6443}"
NS=cgpu-jobs
OUT="${OUT:-api-kubeconfig}"

for _ in $(seq 1 20); do
  TOKEN=$(kubectl -n "$NS" get secret cgpu-api-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
  [ -n "$TOKEN" ] && break
  sleep 1
done
[ -n "${TOKEN:-}" ] || { echo "token for cgpu-api not issued yet; did you apply deploy/k8s/cgpu-api-rbac.yaml?" >&2; exit 1; }
CA=$(kubectl -n "$NS" get secret cgpu-api-token -o jsonpath='{.data.ca\.crt}')

umask 077
cat > "$OUT" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: cgpu
    cluster:
      server: ${SERVER}
      certificate-authority-data: ${CA}
users:
  - name: cgpu-api
    user:
      token: ${TOKEN}
contexts:
  - name: cgpu-api
    context:
      cluster: cgpu
      user: cgpu-api
      namespace: ${NS}
current-context: cgpu-api
EOF
echo "wrote $OUT"
