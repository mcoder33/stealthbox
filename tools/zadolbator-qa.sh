#!/bin/bash
# Run from a Stealthbox disposable runner checkout, never from LocalRoot.
set -euo pipefail
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
usage() {
    printf '%s\n' 'Usage: bash /path/to/stealthbox/tools/zadolbator-qa.sh ACTION' \
        'Actions: prepare | common | api | frontend | backend | status | down' \
        'Run from ~/.local/share/stealthbox/runners/project-<id>/tree with .stealthbox-runner.' \
        'prepare: initialize Development, fresh isolated services and both DB migrations.' \
        'common/api/frontend/backend: prepare idempotently, run suite, save log/XML.' \
        'status: show only this checkout QA stack; down: delete only its stack/volumes.' \
        'Optional ZADOLBATOR_QA_IMAGE: existing linux/amd64 PHP 7.4 image tag.' \
        'Optional ZADOLBATOR_QA_VENDOR_SOURCE: absolute vendor cache path; copy only if absent.'
}
action=${1:-help}
case "$action" in help|-h|--help) usage; exit 0 ;; prepare|common|api|frontend|backend|status|down) ;; *) usage; exit 2 ;; esac
[ "$#" -eq 1 ] || fail 'Exactly one action is required.'
checkout=$(pwd -P)
runner_base="$HOME/.local/share/stealthbox/runners"
[ -d "$runner_base" ] || fail 'Default Stealthbox RunnerRoot does not exist.'
runner_base=$(cd "$runner_base" && pwd -P)
case "$checkout" in "$runner_base"/project-*/tree) ;; *) fail 'Refusing cwd outside a default disposable RunnerRoot checkout.' ;; esac
checkout_id=${checkout#"$runner_base"/}
checkout_id=${checkout_id%/tree}
checkout_hash=${checkout_id#project-}
[ "${#checkout_hash}" -eq 24 ] || fail 'Invalid checkout ID length.'
case "$checkout_hash" in *[!a-f0-9]*) fail 'Invalid checkout ID.' ;; esac
[ -f .stealthbox-runner ] && [ ! -L .stealthbox-runner ] || fail 'Missing regular .stealthbox-runner marker.'
helper_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/zadolbator-qa" && pwd -P)
qa_dir="$checkout/.stealthbox-qa"
[ ! -L "$qa_dir" ] || fail 'QA state directory must not be a symlink.'
mkdir -p "$qa_dir"
chmod 700 "$qa_dir"
[ "$(cd "$qa_dir" && pwd -P)" = "$qa_dir" ] || fail 'QA state escapes the disposable checkout.'
# Different checkout paths always own different Compose resources.
path_hash=$(printf '%s' "$checkout" | shasum -a 256)
path_hash=${path_hash%% *}
project="stealthbox-zadolbator-qa-${path_hash:0:16}"
image=${ZADOLBATOR_QA_IMAGE:-zadolbator-app:latest}
case "$image" in *[!a-zA-Z0-9_./:-]*|'') fail 'QA image must be a local image tag.' ;; esac
case "$image" in sha256:*) fail 'Use a local image tag instead of an image ID.' ;; esac
mkdir "$qa_dir/lock" 2>/dev/null || fail 'QA helper already running (or interrupted); inspect .stealthbox-qa/lock before retrying.'
trap 'rmdir "$qa_dir/lock"' EXIT
for file in compose.yml dummy.env compose.env reports; do
    [ ! -L "$qa_dir/$file" ] || fail "QA $file must not be a symlink."
done
cp "$helper_dir/compose.yml" "$qa_dir/compose.yml"
cp "$helper_dir/dummy.env" "$qa_dir/dummy.env"
# Paths are quoted in the interpolation env file, not evaluated as shell code.
case "$checkout$helper_dir" in *\'*|*$'\n'*|*$'\r'*) fail 'Unsupported quote/newline in checkout/helper path.' ;; esac
printf "QA_CHECKOUT='%s'\nQA_HELPERS='%s'\nQA_APP_IMAGE='%s'\n" "$checkout" "$helper_dir" "$image" > "$qa_dir/compose.env"
compose() {
    env -i HOME="$HOME" PATH="$PATH" docker compose --project-name "$project" \
        --project-directory "$qa_dir" --env-file "$qa_dir/compose.env" -f "$qa_dir/compose.yml" "$@"
}
printf 'QA checkout: %s\nQA Compose project: %s\n' "$checkout" "$project"
case "$action" in
    status) compose ps --all; exit 0 ;;
    down) compose rm --stop --force app; compose down --volumes --remove-orphans; printf 'Removed only %s; code and vendor preserved.\n' "$project"; exit 0 ;;
esac
[ -f codecept.sh ] && [ -f init ] && [ -f environments/index.php ] || fail 'This checkout is not Zadolbator.'
[ ! -e .env ] && [ ! -e .env.local ] || fail 'Refusing pre-existing .env/.env.local; use a fresh disposable checkout.'
unsafe_links=$(find . -path './vendor' -prune -o -type l -print)
[ -z "$unsafe_links" ] || fail 'Refusing source/config symlinks; re-import without symlinks.'
for template in dev prod prod-tracker stage; do
    [ -d "environments/$template" ] || fail "Missing environments/$template; re-import tracked templates before init."
done
[ ! -L vendor ] || fail 'Vendor cache directory must not be a symlink.'
if [ -d vendor ]; then
    [ "$(cd vendor && pwd -P)" = "$checkout/vendor" ] || fail 'Vendor cache escapes the disposable checkout.'
fi
if [ ! -f vendor/autoload.php ]; then
    vendor_source=${ZADOLBATOR_QA_VENDOR_SOURCE:-/Users/coder33/Projects/itfinance/zadolbator/vendor}
    [ -d "$vendor_source" ] && [ -f "$vendor_source/autoload.php" ] || fail 'No vendor cache. Set ZADOLBATOR_QA_VENDOR_SOURCE to a local vendor directory.'
    vendor_source=$(cd "$vendor_source" && pwd -P)
    [ "$vendor_source" != "$checkout/vendor" ] || fail 'Vendor source and destination are the same.'
    printf 'Copying vendor cache only: %s -> %s/vendor\n' "$vendor_source" "$checkout"
    mkdir -p vendor
    rsync -a --no-links --exclude=auth.json --exclude=.env --exclude=.env.local -- "$vendor_source/" "$checkout/vendor/"
fi
[ -f vendor/bin/codecept ] || fail 'vendor cache has no Codeception; install dependencies separately in a disposable environment.'
image_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image" 2>/dev/null) || fail "Missing local $image. Load/build a separate PHP 7.4 QA image first; this helper never builds or overwrites images."
[ "$image_platform" = linux/amd64 ] || fail "Expected linux/amd64 PHP image, got $image_platform."
compose config --quiet
printf 'Starting isolated PostgreSQL 15, Redis 6.2.16, ClickHouse 25.8 (no published ports).\n'
compose up -d --wait --wait-timeout 180 pgsql redis clickhouse
compose exec -T pgsql psql -U qa -d postgres -v ON_ERROR_STOP=1 -tAc "SELECT 1 FROM pg_database WHERE datname='sms-service_test'" | \
    /usr/bin/grep -q 1 || compose exec -T pgsql createdb -U qa sms-service_test
mkdir -p "$qa_dir/reports"
printf 'Initializing and migrating disposable test databases. Log: %s/reports/prepare.log\n' "$qa_dir"
compose run --rm -T app bash /qa/prepare.sh 2>&1 | tee "$qa_dir/reports/prepare.log"
printf 'Preparing empty date-grid ClickHouse schema in owned qa database.\n'
compose run --rm -T app php /qa/prepare-clickhouse-schema.php \
    /app/console/migrations-clickhouse/m240202_043836_create_zb_message_queue_wide_cmt_table.php > "$qa_dir/clickhouse-schema.sql"
compose exec -T clickhouse clickhouse-client --user qa --password qa-only-not-secret \
    --database qa --multiquery < "$qa_dir/clickhouse-schema.sql" 2>&1 | tee "$qa_dir/reports/clickhouse-prepare.log"
[ "$action" != prepare ] || exit 0
printf 'Running all %s suites. Reports: %s/reports\n' "$action" "$qa_dir"
report="$checkout/$action/tests/_output/stealthbox-qa.xml"
rm -f "$report" "$qa_dir/reports/$action.xml"
set +e
compose run --rm -T app bash -e ./codecept.sh "$action" --xml=stealthbox-qa.xml --no-colors 2>&1 | tee "$qa_dir/reports/$action.log"
result=${PIPESTATUS[0]}
set -e
if [ "$result" -ne 0 ]; then
    for diagnostic in "$checkout/$action/tests/_output/"*.fail.html; do
        [ -f "$diagnostic" ] && [ ! -L "$diagnostic" ] || continue
        diagnostic_target="$qa_dir/reports/$(basename "$diagnostic")"
        [ ! -L "$diagnostic_target" ] || fail 'Diagnostic report target must not be a symlink.'
        [ ! -e "$diagnostic_target" ] || [ -f "$diagnostic_target" ] || fail 'Diagnostic report target must be a regular file.'
        cp "$diagnostic" "$diagnostic_target"
    done
fi
if [ -f "$report" ] && [ ! -L "$report" ]; then
    cp "$report" "$qa_dir/reports/$action.xml"
    if [ "$result" -eq 0 ] && ! /usr/bin/grep -Eq '<testcase([[:space:]]|>)' "$report"; then
        fail "Suite $action returned success but XML contains no test cases; inspect $qa_dir/reports/$action.log."
    fi
elif [ "$result" -eq 0 ]; then
    fail 'Suite returned success without expected XML report.'
fi
printf 'Suite %s exit: %s\n' "$action" "$result"
exit "$result"
