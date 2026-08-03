#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
ENV_FILE="$SCRIPT_DIR/.env"
COMPOSE_FILE="$SCRIPT_DIR/compose.yml"
RUNTIME_DIR="$SCRIPT_DIR/runtime"

usage() {
    cat <<'EOF'
usage: testenv/lab.sh <command> [arguments]

  up       render config, build this checkout, start services, and seed Lidarr
  reset    delete all containers/volumes/runtime state, then run up
  seed     recreate the deterministic Lidarr wanted set without resetting
  down     stop the lab but preserve its state
  destroy  stop the lab and delete all volumes/runtime state
  info     print the lab's addresses, accounts, and listen ports
  logs     follow logs (optionally: logs slusk)
  ps       show container state
  config   validate the rendered Compose and slusk configuration
EOF
}

need_env() {
    if [ ! -f "$ENV_FILE" ]; then
        echo "missing $ENV_FILE" >&2
        echo "copy testenv/.env.example to testenv/.env and fill both Soulseek accounts" >&2
        exit 2
    fi
}

compose() {
    docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"
}

render() {
    need_env
    ensure_share_dir
    python3 "$SCRIPT_DIR/render_config.py" --env "$ENV_FILE" --runtime "$RUNTIME_DIR"
}

# Docker silently creates a missing bind-mount source as a root-owned empty
# directory, which then shares nothing and looks like a broken share index
# rather than a typo. Fail loudly on a bad SLUSK_SHARE_DIR instead, and only
# create the default when the user has not pointed the lab elsewhere.
ensure_share_dir() {
    share_dir=$(sed -n 's/^[[:space:]]*SLUSK_SHARE_DIR[[:space:]]*=[[:space:]]*//p' "$ENV_FILE" | tail -n 1 | tr -d '"'"'"'')
    if [ -z "$share_dir" ]; then
        mkdir -p "$SCRIPT_DIR/share"
        return
    fi
    case "$share_dir" in
        */CHANGE_ME/*)
            echo "set SLUSK_SHARE_DIR in $ENV_FILE to a real directory, or remove the line to use testenv/share" >&2
            exit 2
            ;;
    esac
    if [ ! -d "$share_dir" ]; then
        echo "SLUSK_SHARE_DIR=$share_dir is not a directory" >&2
        exit 2
    fi
}

up() {
    render
    # Force-recreate both Soulseek clients so credential/backend-only .env
    # changes cannot leave an old process running with stale configuration.
    compose up --detach --build --force-recreate slskd slusk
    if ! python3 "$SCRIPT_DIR/wait_ready.py" --env "$ENV_FILE"; then
        compose logs --tail 100 slusk >&2
        return 1
    fi
    compose --profile tools run --rm lidarr-seed
    summary
}

summary() {
    python3 "$SCRIPT_DIR/summary.py" --env "$ENV_FILE"
}

command=${1:-}
if [ -n "$command" ]; then
    shift
fi

case "$command" in
    up)
        up
        ;;
    reset)
        need_env
        compose down --volumes --remove-orphans
        rm -rf "$RUNTIME_DIR"
        up
        ;;
    seed)
        render
        compose --profile tools run --rm lidarr-seed
        ;;
    down)
        need_env
        compose down --remove-orphans
        ;;
    destroy)
        need_env
        compose down --volumes --remove-orphans
        rm -rf "$RUNTIME_DIR"
        ;;
    info)
        need_env
        summary
        ;;
    logs)
        need_env
        compose logs --follow "$@"
        ;;
    ps)
        need_env
        compose ps "$@"
        ;;
    config)
        render
        compose config --quiet
        (cd "$ROOT_DIR" && go run ./testenv/validate_config.go testenv/runtime/slusk/config.toml)
        echo "Compose and slusk configuration are valid"
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac
