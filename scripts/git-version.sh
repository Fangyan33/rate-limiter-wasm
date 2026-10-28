#!/usr/bin/env bash
set -euo pipefail

# 只考虑 HEAD 可达的 tag，避免其他分支的发布影响当前分支。
commit=$(git rev-parse --short=12 HEAD)
tag=$(git describe --tags --abbrev=0 HEAD 2>/dev/null || true)
if [[ -z "$tag" ]]; then
  printf '%s\n' "$commit"
  exit 0
fi

# annotated tag 的创建时间可能晚于后续提交，因此比较所指提交的时间。
commit_time=$(git log -1 --format=%ct HEAD)
tag_time=$(git log -1 --format=%ct "refs/tags/$tag")
if (( commit_time > tag_time )); then
  printf '%s\n' "$commit"
else
  printf '%s\n' "$tag"
fi
