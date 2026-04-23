#!/usr/bin/env bash
set -euo pipefail

# Rebuild main from upstream + fork patches + PR branches.
# Run this whenever upstream moves, a PR is rebased, or a new PR opens.
#
# Usage: scripts/sync-main.sh [--push]

UPSTREAM_REMOTE="upstream"
ORIGIN_REMOTE="origin"
FORK_BRANCH="desired-fork"

# PR branches to merge into main. Update this list as PRs open/close.
#
# CHANGELOG of this list (relative to the previous version that had 7 entries):
#
# REMOVED (superseded upstream):
#   pr/sandbox-pod-annotation-propagation  # #517 — superseded by upstream #514 (KEP-0174, strict superset with deletion tracking + domain validation at claim layer)
#   pr/podip-status                        # upstream #482 closed without merge but equivalent feature landed via upstream #518
#   pr/fix-stale-pod-annotation            # #521 — superseded by upstream #613 (strict superset with clearPodNameAnnotation helper)
#
# ADDED (new fork-only patch):
#   pr/warmpool-requeue-after              # New fork-only: RequeueAfter 10s so warm pool replenishes after adoption ownership transfer
#
# Kept unchanged: claim-identity-labels (#455), pr/workspace-resources-only (#459),
# pr/claim-skip-not-ready (#519), pr/template-volume-claim-templates.
#
# NOTE: pr/warm-adoption-preserve-podtemplate-metadata was never in PR_BRANCHES;
# its content was bundled into pr/workspace-resources-only (commit fa34c14) and
# is now dropped during that branch's rebase because KEP-0174 supersedes it.
PR_BRANCHES=(
  claim-identity-labels              # #455 — Propagate SandboxIDLabel (claim-uid) to Sandbox.metadata.labels AND Pod labels (KEP-0174 only covers pod labels, not top-level Sandbox labels; load-bearing for platform informer)
  pr/workspace-resources-only        # #459 — Per-claim workspace container resource overrides + in-place resize on running sandboxes
  pr/claim-skip-not-ready            # #519 — Skip not-ready sandboxes during warm pool adoption (now implemented in verifySandboxCandidate after upstream queue refactor)
  pr/template-volume-claim-templates # Propagate VolumeClaimTemplates from SandboxTemplate to Sandbox for PVC workspace persistence (fork-only, no upstream PR)
  pr/warmpool-requeue-after          # Warm pool replenishment after adoption: return RequeueAfter 10s so owner-ref change is not missed (fork-only, no upstream PR)
)

PUSH=false
if [[ "${1:-}" == "--push" ]]; then
  PUSH=true
fi

echo "==> Fetching remotes..."
git fetch "$UPSTREAM_REMOTE"
git fetch "$ORIGIN_REMOTE"

# Save current branch to restore later
ORIGINAL_BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || git rev-parse HEAD)"

echo "==> Resetting main to ${UPSTREAM_REMOTE}/main..."
git checkout -B main "${UPSTREAM_REMOTE}/main"

echo "==> Merging ${FORK_BRANCH} (fork-only patches)..."
git merge --no-ff "$FORK_BRANCH" -m "Merge ${FORK_BRANCH} patches"

for branch in "${PR_BRANCHES[@]}"; do
  # Strip inline comments
  branch="${branch%%#*}"
  branch="$(echo "$branch" | xargs)"
  [[ -z "$branch" ]] && continue

  echo "==> Merging ${branch}..."
  git merge --no-ff "$branch" -m "Merge branch '${branch}'"
done

echo ""
echo "main is now:"
git log --oneline "${UPSTREAM_REMOTE}/main..main"

if $PUSH; then
  echo ""
  echo "==> Pushing main to ${ORIGIN_REMOTE}..."
  git push "$ORIGIN_REMOTE" main --force-with-lease
  echo "Done. Image build should trigger if paths changed."
else
  echo ""
  echo "Run 'git push origin main --force-with-lease' to publish, or re-run with --push."
fi

# Restore original branch if it wasn't main
if [[ "$ORIGINAL_BRANCH" != "main" ]]; then
  git checkout "$ORIGINAL_BRANCH" 2>/dev/null || true
fi
