#!/usr/bin/env bash
# Decides docs.yml's `docs_changed` output (#5659). Prints `docs_changed=<bool>`
# on stdout; the workflow appends it to $GITHUB_OUTPUT.
#
# Publishing is not monotonic the way verification is: docs.yml cancels a
# superseded run, and a cancelled run may have been the one carrying a docs
# change. Diffing only this run's own push range would then strand the docs. So
# 'auto' diffs against the LAST SUCCESSFULLY DEPLOYED commit (the newest
# github-pages deployment whose latest status is success): whichever run
# survives deploys everything still pending. When no deployment is found, the
# API errors, or the base is not an ancestor of the commit under examination,
# it falls back to the old push-range / own-commit diff.
#
# Inputs (environment, never argv — verify_sha is free-form dispatch input):
#   EVENT_NAME DEPLOY_DOCS VERIFY_SHA EVENT_BEFORE EVENT_SHA
#   GITHUB_REPOSITORY GH_TOKEN (for `gh api`)
set -euo pipefail

if [[ -n "${VERIFY_SHA:-}" && ! "$VERIFY_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::verify_sha must be a full 40-character commit SHA"
  exit 1
fi

if [[ "${EVENT_NAME:-}" == "workflow_dispatch" && "${DEPLOY_DOCS:-auto}" != "auto" ]]; then
  echo "docs_changed=$DEPLOY_DOCS"
  exit 0
fi

# `built` is the tree this run builds and publishes (#5688). The deployed-base
# comparison must reach it: diffing up to an older verify_sha would compare the
# base against itself (verify_sha already deployed, a newer docs commit pending)
# and skip a needed deploy. verify_sha only pins the FALLBACK diff below.
built="$EVENT_SHA"
after="${VERIFY_SHA:-$EVENT_SHA}"
before="${EVENT_BEFORE:-}"

# The newest successfully deployed commit, or empty.
last_deployed() {
  local rows id sha state
  rows="$(gh api "repos/${GITHUB_REPOSITORY}/deployments?environment=github-pages&per_page=20" \
    --jq '.[] | "\(.id) \(.sha)"' 2>/dev/null)" || return 0
  while read -r id sha; do
    [[ -n "$id" ]] || continue
    state="$(gh api "repos/${GITHUB_REPOSITORY}/deployments/${id}/statuses?per_page=1" \
      --jq '.[0].state // empty' 2>/dev/null)" || continue
    if [[ "$state" == "success" ]]; then
      echo "$sha"
      return 0
    fi
  done <<< "$rows"
}

base="$(last_deployed || true)"
if [[ "$base" =~ ^[0-9a-f]{40}$ ]] \
  && git cat-file -e "${base}^{commit}" 2>/dev/null \
  && git merge-base --is-ancestor "$base" "$built" 2>/dev/null; then
  echo "::notice::Diffing docs paths against the last deployed commit ${base:0:12}." >&2
  changed="$(git diff --name-only "$base" "$built")"
else
  echo "::notice::No usable last-deployed commit (got '${base:-none}'); falling back to the push range." >&2
  # An explicitly named commit is always diffed on its own, never against a
  # `before` that belongs to a different event.
  if [[ -n "${VERIFY_SHA:-}" || -z "$before" || "$before" =~ ^0+$ ]]; then
    changed="$(git diff-tree --no-commit-id --name-only -r "$after")"
  else
    changed="$(git diff --name-only "$before" "$after")"
  fi
fi

docs_changed=false
while IFS= read -r path; do
  case "$path" in
    docs/*|mkdocs.yml|README.md|commands/docs_gen.go|requirements-docs.txt|scripts/gen-docs.sh)
      docs_changed=true
      break
      ;;
  esac
done <<< "$changed"

echo "docs_changed=$docs_changed"
