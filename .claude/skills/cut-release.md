---
name: cut-release
description: Cut stable releases from verified merged work and confirm preview releases continue
user_invocable: true
---

# Cut Stable Release

Cut a stable release when meaningful verified work has accumulated since the
last stable release. Patch releases are the maintainer's lever; minor
`1.x.0` releases are Sachin's lever and require asking him first.

## Steps

1. **Find the latest releases** — compare the last stable release with preview
   releases:
   ```bash
   gh release list --limit 20
   ```

   Identify the latest stable `1.0.<patch>` release and the newest preview
   release. Use the stable release date as the starting point for the merge
   count.

2. **Count merged PRs since the last stable**:
   ```bash
   gh pr list --state merged --base master --limit 100 \
     --search "merged:>YYYY-MM-DD" \
     --json number,title,mergedAt,author
   ```

   Cut a stable release when the merged work is meaningful and already
   verified. Security fixes and data-loss fixes warrant a prompt stable
   release even if the batch is small.

3. **Choose the next version** — increment only the patch component:
   ```bash
   gh workflow run "Stable Release" -f version=1.0.<next>
   ```

   Do not cut a `1.x.0` minor release without Sachin's explicit direction.

4. **Watch the stable workflow to completion**:
   ```bash
   gh run list --workflow "Stable Release" --limit 5
   gh run watch <run-id>
   ```

   If the run fails, inspect the logs and fix or escalate before announcing
   the release.

5. **Confirm preview releases still cut** — check the `Auto Preview Release`
   workflow and the latest preview release:
   ```bash
   gh run list --workflow "Auto Preview Release" --limit 5
   gh release list --limit 20
   ```

   A stable release is not done until the stable workflow has completed and
   the preview auto-release path still looks healthy.
