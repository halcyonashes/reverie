#!/bin/bash
# Engram — Shared helpers for Claude Code hooks
# WARNING: Do not read from stdin here — scripts source this before reading their hook input.

# Reject directories that can never name a project: the home directory, a
# dotfile directory, or a filesystem/volume root. Mirrors isNonProjectDir in
# internal/project/detect.go. Returns 0 (shell true) when dir must be rejected.
is_non_project_dir() {
  local dir="${1//\\//}" # accept Windows separators
  [ -z "$dir" ] && return 1

  # Strip one trailing slash, but keep "/" itself intact.
  case "$dir" in
    /) ;;
    */) dir="${dir%/}" ;;
  esac

  # Filesystem or volume root: "/" or "C:".
  case "$dir" in
    / | [A-Za-z]:) return 0 ;;
  esac

  # Home directory, in either POSIX or Windows form.
  local home_posix="${HOME//\\//}"
  local home_win="${USERPROFILE//\\//}"
  [ -n "$home_posix" ] && [ "$dir" = "${home_posix%/}" ] && return 0
  [ -n "$home_win" ] && [ "$dir" = "${home_win%/}" ] && return 0

  # Dotfile directory.
  case "${dir##*/}" in
    .*) return 0 ;;
  esac

  return 1
}

# Detect project name from git remote, with fallbacks.
# Priority: git remote origin repo name > git root basename > cwd basename
# Prints nothing and returns 1 when no directory can legitimately name a project.
detect_project() {
  local dir="$1"

  # Try git remote origin URL
  local url
  url=$(git -C "$dir" remote get-url origin 2>/dev/null)
  if [ -n "$url" ]; then
    # Handles both SSH (git@github.com:user/repo.git) and HTTPS (https://github.com/user/repo.git)
    local name
    name=$(echo "$url" | sed 's/\.git$//' | sed 's|.*[/:]||' | tr '[:upper:]' '[:lower:]')
    if [ -n "$name" ]; then
      echo "$name"
      return
    fi
  fi

  # Fallback: git root directory name (works in worktrees)
  local root
  root=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null)
  if [ -n "$root" ]; then
    basename "$root" | tr '[:upper:]' '[:lower:]'
    return
  fi

  # Final fallback: cwd basename — but only when the directory can actually name
  # a project. Declining is deliberate: inventing a name here files memories
  # under buckets like the username or ".claude".
  if is_non_project_dir "$dir"; then
    return 1
  fi
  basename "$dir" | tr '[:upper:]' '[:lower:]'
}
