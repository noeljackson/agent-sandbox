#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Rebuild the mirror main branch from upstream + fork overlay + PR branches.
# Run this whenever upstream moves, a PR is rebased, or a new PR opens.
#
# Usage:
#   scripts/sync-main.sh [--push] [--skip-verify] [--keep-worktree]
#
# The generated main branch is built in a temporary worktree so the caller's
# checkout is not reset or left mid-merge if a conflict needs human attention.

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
ORIGIN_REMOTE="${ORIGIN_REMOTE:-origin}"
TARGET_BRANCH="${TARGET_BRANCH:-main}"
FORK_OVERLAY_BRANCH="${FORK_OVERLAY_BRANCH:-fork-overlay}"

# PR branches to merge into main. Update this list as PRs open/close.
PR_BRANCHES=(
  pr/claim-skip-not-ready-v2         # #683: Briefly wait for rotating warm-pool candidates to report PodIPs
  pr/workspace-resources-only        # #459: Per-claim workspace container resource overrides + in-place resize on running sandboxes
  pr/fake-newclientset               # #795: Add applyconfig-backed fake clientsets with NewClientset
)

PUSH=false
VERIFY=true
KEEP_WORKTREE=false
ALLOW_DIRTY=false
WORKTREE_DIR="${WORKTREE_DIR:-}"
SYNC_WORKTREE_BASE="${SYNC_WORKTREE_BASE:-}"

usage() {
  cat <<EOF
Usage: scripts/sync-main.sh [flags]

Flags:
  --push            Push the generated HEAD to ${ORIGIN_REMOTE}/${TARGET_BRANCH} with --force-with-lease.
  --skip-verify     Skip the verification commands before reporting or pushing.
  --keep-worktree   Keep the temporary worktree after a successful run for inspection.
  --allow-dirty     Allow running when the caller's worktree has uncommitted changes.
  --worktree DIR    Build in DIR instead of creating a temporary directory.
  -h, --help        Show this help.

Environment:
  UPSTREAM_REMOTE       Default: upstream
  ORIGIN_REMOTE         Default: origin
  TARGET_BRANCH         Default: main
  FORK_OVERLAY_BRANCH   Default: fork-overlay
  WORKTREE_DIR          Optional default for --worktree.
  SYNC_WORKTREE_BASE    Default: ../.agent-sandbox-sync outside this repo
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

run_check() {
  echo "==> $*"
  "$@"
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push)
      PUSH=true
      ;;
    --skip-verify)
      VERIFY=false
      ;;
    --keep-worktree)
      KEEP_WORKTREE=true
      ;;
    --allow-dirty)
      ALLOW_DIRTY=true
      ;;
    --worktree)
      [[ $# -ge 2 ]] || die "--worktree requires a directory"
      WORKTREE_DIR="$2"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
  shift
done

REPO_ROOT="$(git rev-parse --show-toplevel)"
UPSTREAM_REF="${UPSTREAM_REMOTE}/${TARGET_BRANCH}"
TARGET_REMOTE_REF="refs/heads/${TARGET_BRANCH}"
if [[ -z "$SYNC_WORKTREE_BASE" ]]; then
  SYNC_WORKTREE_BASE="$(dirname "$REPO_ROOT")/.agent-sandbox-sync"
fi

if ! $ALLOW_DIRTY; then
  git -C "$REPO_ROOT" diff --quiet || die "worktree has unstaged changes; commit/stash them or pass --allow-dirty"
  git -C "$REPO_ROOT" diff --cached --quiet || die "worktree has staged changes; commit/stash them or pass --allow-dirty"
fi

echo "==> Fetching remotes..."
git -C "$REPO_ROOT" fetch "$UPSTREAM_REMOTE"
git -C "$REPO_ROOT" fetch "$ORIGIN_REMOTE"

require_ref() {
  local ref="$1"
  git -C "$REPO_ROOT" rev-parse --verify --quiet "${ref}^{commit}" >/dev/null ||
    die "missing required ref: ${ref}"
}

require_ref "$UPSTREAM_REF"

OVERLAY_REF="${ORIGIN_REMOTE}/${FORK_OVERLAY_BRANCH}"
OVERLAY_NAME="$FORK_OVERLAY_BRANCH"
require_ref "$OVERLAY_REF"

for raw_branch in "${PR_BRANCHES[@]}"; do
  branch="$(trim "${raw_branch%%#*}")"
  [[ -z "$branch" ]] && continue
  require_ref "${ORIGIN_REMOTE}/${branch}"
done

TMP_PARENT=""
WORKTREE_CREATED=false
if [[ -z "$WORKTREE_DIR" ]]; then
  mkdir -p "$SYNC_WORKTREE_BASE"
  TMP_PARENT="$(mktemp -d "${SYNC_WORKTREE_BASE}/sync-main.XXXXXX")"
  WORKTREE_DIR="${TMP_PARENT}/worktree"
elif [[ -e "$WORKTREE_DIR" ]]; then
  die "--worktree path already exists: ${WORKTREE_DIR}"
fi

cleanup() {
  local status=$?
  if $WORKTREE_CREATED; then
    if [[ $status -eq 0 && "$KEEP_WORKTREE" == "false" ]]; then
      git -C "$REPO_ROOT" worktree remove -f "$WORKTREE_DIR" >/dev/null 2>&1 || true
      [[ -z "$TMP_PARENT" ]] || rm -rf "$TMP_PARENT"
    else
      echo "Sync worktree left at: ${WORKTREE_DIR}" >&2
      [[ -z "$TMP_PARENT" ]] || echo "Temporary parent left at: ${TMP_PARENT}" >&2
    fi
  fi
}
trap cleanup EXIT

echo "==> Creating temporary worktree from ${UPSTREAM_REF}..."
git -C "$REPO_ROOT" worktree add --detach "$WORKTREE_DIR" "$UPSTREAM_REF"
WORKTREE_CREATED=true

echo "==> Merging ${OVERLAY_REF} (fork overlay patches)..."
git -C "$WORKTREE_DIR" merge --no-ff "$OVERLAY_REF" -m "Merge ${OVERLAY_NAME} patches"

for raw_branch in "${PR_BRANCHES[@]}"; do
  branch="$(trim "${raw_branch%%#*}")"
  [[ -z "$branch" ]] && continue

  branch_ref="${ORIGIN_REMOTE}/${branch}"
  echo "==> Merging ${branch_ref}..."
  git -C "$WORKTREE_DIR" merge --no-ff "$branch_ref" -m "Merge branch '${branch}'"
done

GENERATED_PATHS=(
  api/v1alpha1/zz_generated.deepcopy.go
  api/v1beta1/zz_generated.deepcopy.go
  extensions/api/v1alpha1/zz_generated.deepcopy.go
  extensions/api/v1beta1/zz_generated.deepcopy.go
  clients/k8s
  k8s/crds
  k8s/rbac.generated.yaml
  k8s/extensions-rbac.generated.yaml
  helm/crds
  helm/templates/rbac.generated.yaml
  helm/templates/extensions-rbac.generated.yaml
)

mkdir -p "${WORKTREE_DIR}/dev/tools/tmp"

echo "==> Regenerating composed API and client artifacts..."
run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" make -C "$WORKTREE_DIR" fix-go-generate
git -C "$WORKTREE_DIR" add -- "${GENERATED_PATHS[@]}"
git -C "$WORKTREE_DIR" diff --quiet ||
  die "generation changed tracked files outside the generator-owned path allowlist"
if ! git -C "$WORKTREE_DIR" diff --cached --quiet; then
  run_check git -C "$WORKTREE_DIR" diff --cached --check
  git -C "$WORKTREE_DIR" commit -m "chore: regenerate composed API and client artifacts"
fi

echo "==> Verifying generation is stable..."
run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" make -C "$WORKTREE_DIR" fix-go-generate
git -C "$WORKTREE_DIR" diff --quiet || die "generation is not stable after the integration commit"

if $VERIFY; then
  echo "==> Verifying generated ${TARGET_BRANCH}..."
  run_check git -C "$WORKTREE_DIR" diff --check "${UPSTREAM_REF}..HEAD"
  run_check git -C "$WORKTREE_DIR" diff --quiet
  run_check env GOLANGCI_LINT_CACHE="${WORKTREE_DIR}/dev/tools/tmp/golangci-lint-cache" make -C "$WORKTREE_DIR" lint-api
  run_check env GOLANGCI_LINT_CACHE="${WORKTREE_DIR}/dev/tools/tmp/golangci-lint-cache" make -C "$WORKTREE_DIR" lint-go
  run_check make -C "$WORKTREE_DIR" build
  run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" make -C "$WORKTREE_DIR" test-unit
  run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" make -C "$WORKTREE_DIR" toc-verify
  run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" make -C "$WORKTREE_DIR" verify-olm
  run_check env TMPDIR="${WORKTREE_DIR}/dev/tools/tmp" "${WORKTREE_DIR}/dev/tools/lint-olm"
else
  echo "==> Verification skipped."
fi

HEAD_SHA="$(git -C "$WORKTREE_DIR" rev-parse HEAD)"
SHORT_SHA="$(git -C "$WORKTREE_DIR" rev-parse --short=12 HEAD)"
COMMIT_EPOCH="$(git -C "$WORKTREE_DIR" show -s --format=%ct HEAD)"
COMMIT_TIME="$(date -u -d "@${COMMIT_EPOCH}" +%Y%m%d%H%M%S)"
PSEUDO_VERSION="v0.0.0-${COMMIT_TIME}-${SHORT_SHA}"
DESCRIBE="$(git -C "$WORKTREE_DIR" describe --tags --always HEAD)"
EXPECTED_REMOTE_SHA="$(git -C "$REPO_ROOT" rev-parse "${ORIGIN_REMOTE}/${TARGET_BRANCH}")"

echo ""
echo "${TARGET_BRANCH} would be:"
git -C "$WORKTREE_DIR" log --oneline "${UPSTREAM_REF}..HEAD"

echo ""
echo "Pin output:"
echo "  sha:            ${HEAD_SHA}"
echo "  short_sha:      ${SHORT_SHA}"
echo "  describe:       ${DESCRIBE}"
echo "  go_pseudo_ver:  ${PSEUDO_VERSION}"
echo "  old_remote_sha: ${EXPECTED_REMOTE_SHA}"

if $PUSH; then
  echo ""
  echo "==> Pushing ${TARGET_BRANCH} to ${ORIGIN_REMOTE}..."
  git -C "$WORKTREE_DIR" push "$ORIGIN_REMOTE" "HEAD:${TARGET_REMOTE_REF}" \
    --force-with-lease="${TARGET_REMOTE_REF}:${EXPECTED_REMOTE_SHA}"
  echo "Done. Image build should trigger if controller paths changed."
else
  echo ""
  echo "Dry run only. Re-run with --push to publish ${ORIGIN_REMOTE}/${TARGET_BRANCH}."
fi
