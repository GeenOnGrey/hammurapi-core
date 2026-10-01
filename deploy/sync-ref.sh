#!/usr/bin/env bash
# sync-ref.sh [--check] — подставляет INFRA_WORKFLOW_REF из deploy/versions.env в
# `uses:` переиспользуемого workflow в .github/workflows/release.yml.
# С --check только сверяет и завершается с ошибкой при расхождении (проверка PR).
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ref=$(sed -n 's/^INFRA_WORKFLOW_REF=//p' "$root/deploy/versions.env")
[[ -n $ref ]] || { echo "в deploy/versions.env нет INFRA_WORKFLOW_REF" >&2; exit 1; }
wf=$root/.github/workflows/release.yml
pattern='hammurapi-infra/\.github/workflows/deploy-component\.yml@'
current=$(grep -oE "${pattern}[^[:space:]]+" "$wf" | sed "s#.*@##" | sort -u)
if [[ ${1:-} == --check ]]; then
  if [[ $current != "$ref" ]]; then
    echo "::error file=.github/workflows/release.yml::uses указывает на ${current:-?}, а INFRA_WORKFLOW_REF=$ref — выполните deploy/sync-ref.sh" >&2
    exit 1
  fi
  echo "INFRA_WORKFLOW_REF=$ref совпадает с release.yml"
  exit 0
fi
sed -i -E "s#(${pattern})[^[:space:]]+#\1${ref}#" "$wf"
echo "release.yml: deploy-component.yml@$ref"
