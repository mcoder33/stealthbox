#!/bin/bash
set -euo pipefail
cd /app
test -f .stealthbox-runner || { echo 'ERROR: missing disposable runner marker'; exit 1; }
php -r 'if (PHP_MAJOR_VERSION !== 7 || PHP_MINOR_VERSION !== 4) {fwrite(STDERR, "ERROR: QA image must use PHP 7.4\n"); exit(1);}'
test -f vendor/autoload.php && test -f vendor/bin/codecept || {
    echo 'ERROR: vendor cache is missing; copy dependencies to this disposable runner first.'
    exit 1
}
for app in common console api backend frontend tracker; do
    mkdir -p "$app/runtime/logs" "$app/tests/_output" "$app/tests/_support"
done
for app in api backend frontend tracker; do mkdir -p "$app/web/assets"; done
# The init tool enumerates every environment, including those not selected.
for template in dev prod prod-tracker stage; do
    test -d "environments/$template" || { echo "ERROR: missing tracked environments/$template; re-import source templates"; exit 1; }
done
php init --env=Development --overwrite=All </dev/null
test -f yii && test -f yii_test && test -f common/config/main-local.php || {
    echo 'ERROR: Development init did not generate the expected files'; exit 1;
}
# Refresh path-dependent autoload entries offline; never fetch packages/auth.
COMPOSER_DISABLE_NETWORK=1 composer dump-autoload --no-scripts --no-plugins --no-interaction
php yii migrate --interactive=0
php yii_test migrate --interactive=0
echo 'QA preparation completed: both PostgreSQL databases migrated.'
