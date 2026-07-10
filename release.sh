#!/usr/bin/env bash
# Tag and publish a GitHub release for this repo, with notes generated from
# Conventional Commits since the last release.
#
# Usage: ./release.sh [--publish] [--minor|--major]
#   (no flags)  draft release, patch bump
#   --publish   publish live (default is a draft) — pushes the tag, which
#               triggers the docker-publish workflow
#   --minor     bump the minor version instead of patch (resets patch to 0)
#   --major     bump the major version instead of patch (resets minor/patch to 0)
set -euo pipefail

publish=false
bump=patch
for arg in "$@"; do
  case "$arg" in
    --publish) publish=true ;;
    --minor)   bump=minor ;;
    --major)   bump=major ;;
    *) echo "Unknown argument: $arg" >&2; exit 1 ;;
  esac
done

if ! git rev-parse --is-inside-work-tree > /dev/null 2>&1; then
  echo "Run this from inside a git repo." >&2
  exit 1
fi

repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)

# ── Compute the next tag ──────────────────────────────────────────────────

last_tag=$(gh release list --limit 100 --json tagName,isDraft \
  -q '[.[] | select(.isDraft == false)] | .[0].tagName' 2>/dev/null || echo "")

if [[ -z "$last_tag" ]]; then
  tag="v0.0.1"
else
  if [[ ! "$last_tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "Last release tag '${last_tag}' isn't plain vMAJOR.MINOR.PATCH — bump it manually." >&2
    exit 1
  fi
  major="${BASH_REMATCH[1]}"
  minor="${BASH_REMATCH[2]}"
  patch="${BASH_REMATCH[3]}"
  case "$bump" in
    patch) patch=$((patch + 1)) ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    major) major=$((major + 1)); minor=0; patch=0 ;;
  esac
  tag="v${major}.${minor}.${patch}"
fi

# ── Gather commits since the last release ─────────────────────────────────

if [[ -n "$last_tag" ]]; then
  raw=$(gh api "repos/${repo}/compare/${last_tag}...master" \
    --jq '.commits[] | select(.parents | length == 1) | .sha[0:7] + " " + (.commit.message | split("\n")[0])' \
    2>/dev/null || true)
  range="${last_tag}...master"
else
  raw=$(gh api "repos/${repo}/commits?sha=master&per_page=100" \
    --jq '.[] | .sha[0:7] + " " + (.commit.message | split("\n")[0])' \
    2>/dev/null || true)
  range="all commits"
fi

if [[ -z "$raw" ]]; then
  echo "Nothing to release — no commits since ${last_tag:-the beginning}."
  exit 0
fi

# ── Categorise by Conventional Commits type ───────────────────────────────

re_feat='^feat(\([^)]+\))?:'
re_fix='^fix(\([^)]+\))?:'
re_docs='^docs(\([^)]+\))?:'
feats=""; fixes=""; docs_changes=""; others=""

while IFS= read -r line; do
  hash="${line%% *}"; msg="${line#* }"
  entry="- ${msg} (\`${hash}\`)"
  if [[ "$msg" =~ $re_feat ]];   then feats+="${entry}"$'\n'
  elif [[ "$msg" =~ $re_fix ]];  then fixes+="${entry}"$'\n'
  elif [[ "$msg" =~ $re_docs ]]; then docs_changes+="${entry}"$'\n'
  else others+="${entry}"$'\n'
  fi
done <<< "$raw"

notes="## What's Changed"$'\n\n'
[[ -n "$feats" ]]        && notes+="### Features"$'\n'"${feats}"$'\n'
[[ -n "$fixes" ]]        && notes+="### Bug Fixes"$'\n'"${fixes}"$'\n'
[[ -n "$docs_changes" ]] && notes+="### Documentation"$'\n'"${docs_changes}"$'\n'
[[ -n "$others" ]]       && notes+="### Other Changes"$'\n'"${others}"$'\n'
[[ -n "$last_tag" ]] && notes+="**Full diff:** https://github.com/${repo}/compare/${last_tag}...${tag}"$'\n'

# ── Preview ────────────────────────────────────────────────────────────────

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Repo:   ${repo}"
echo "Tag:    ${tag}"
echo "Range:  ${range}"
echo ""
printf '%s\n' "$notes"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

if [[ "$publish" == true ]]; then
  read -r -p "Publish ${tag}? This pushes the tag and triggers docker-publish CI. [y/N] " confirm
else
  read -r -p "Draft ${tag}? [y/N] " confirm
fi
[[ "$confirm" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 0; }

# ── Create the release ─────────────────────────────────────────────────────

gh_args=(--title "${tag}" --notes "$notes" --target master)
[[ "$publish" == false ]] && gh_args+=(--draft)

gh release create "${tag}" "${gh_args[@]}"

if [[ "$publish" == true ]]; then
  echo "Published: https://github.com/${repo}/releases/tag/${tag}"
else
  echo "Draft:     https://github.com/${repo}/releases"
fi
