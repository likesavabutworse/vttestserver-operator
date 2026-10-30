#!/usr/bin/env bash
# Fails when the Helm chart differs from the files that controller-gen generates:
#   - the CRDs in config/crd/bases
#   - the rules of the ClusterRole in config/rbac/role.yaml
# Needs helm and yq. Run `make manifests` first. When this check fails, copy the generated content into
# dist/chart/templates (the CRD spec, or the rules of templates/rbac/manager-role.yaml).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
rendered="$(helm template check "$root/dist/chart" -n check)"
status=0

for crd in "$root"/config/crd/bases/*.yaml; do
  name="$(yq '.metadata.name' "$crd")"
  want="$(yq '.spec' "$crd")"
  got="$(yq "select(.kind == \"CustomResourceDefinition\" and .metadata.name == \"$name\") | .spec" <<<"$rendered")"
  if [ "$want" != "$got" ]; then
    echo "CRD $name in dist/chart differs from config/crd/bases" >&2
    status=1
  fi
done

# Order the rules by their content, so that the order in the files does not matter.
rules='.rules | map(.apiGroups |= sort | .resources |= sort | .verbs |= sort) | sort_by(.apiGroups[0] + "/" + .resources[0] + "/" + .verbs[0])'
want="$(yq -o=json -I=0 "$rules" "$root/config/rbac/role.yaml")"
# The chart has one copy of the rules for the cluster wide install and one for the namespaced install.
namespaced="$(helm template check "$root/dist/chart" -n check --set rbac.namespaced=true)"
for kind in ClusterRole:"$rendered" Role:"$namespaced"; do
  name="${kind%%:*}"
  got="$(yq -o=json -I=0 "select(.kind == \"$name\" and .metadata.name == \"check-vttestserver-operator-manager-role\") | $rules" <<<"${kind#*:}")"
  if [ "$want" != "$got" ]; then
    echo "The rules of the manager $name in dist/chart differ from config/rbac/role.yaml" >&2
    status=1
  fi
done
exit $status
