#!/usr/bin/env bash
# Render kustomize app directories the way Argo CD will, to catch a broken
# manifest before it reaches homelab-cicd-config.
#
#   scripts/render-deploy.sh deploy/apps/garage-obsync deploy/apps/obsync-worker
#   RENDER_OUT=/tmp/out scripts/render-deploy.sh ...   # also write <app>.yaml there
#
# An app whose SealedSecret has not been generated yet is still rendered: a
# placeholder stands in for sealed-secret.yaml in a temporary copy, so the rest
# of the manifests are checked without anything fake being committed. Argo CD
# still fails loudly on the real directory until the secret exists, which is the
# intended failure mode.
set -euo pipefail

build() {
  if command -v kustomize >/dev/null 2>&1; then
    kustomize build "$1"
  else
    kubectl kustomize "$1"
  fi
}

[ "$#" -gt 0 ] || { echo "usage: $0 <app-dir>..." >&2; exit 2; }

for app in "$@"; do
  name="$(basename "$app")"
  tmp="$(mktemp -d)"
  cp -R "$app/." "$tmp/"

  if grep -qE '^[[:space:]]*-[[:space:]]*sealed-secret\.yaml' "$tmp/kustomization.yaml" \
     && [ ! -f "$tmp/sealed-secret.yaml" ]; then
    echo "::warning::$app/sealed-secret.yaml is not generated yet; rendered with a placeholder. Argo CD shows $name as Unknown until it is committed." >&2
    printf 'apiVersion: bitnami.com/v1alpha1\nkind: SealedSecret\nmetadata:\n  name: placeholder\n' \
      > "$tmp/sealed-secret.yaml"
  fi

  out="$(build "$tmp")"
  rm -rf "$tmp"

  echo "$name: rendered $(grep -c '^kind:' <<<"$out") resources ($(grep '^kind:' <<<"$out" | sort | uniq -c | awk '{printf "%s%s x%s", sep, $3, $1; sep=", "}'))" >&2
  if [ -n "${RENDER_OUT:-}" ]; then
    mkdir -p "$RENDER_OUT"
    printf '%s\n' "$out" > "$RENDER_OUT/$name.yaml"
  fi
done
