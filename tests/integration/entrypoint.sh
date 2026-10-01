#!/bin/sh
set -eu
cp /test-key.pub /home/developer/.ssh/authorized_keys
chown developer:developer /home/developer/.ssh/authorized_keys
chmod 700 /home/developer/.ssh
chmod 600 /home/developer/.ssh/authorized_keys
ssh-keygen -A >/dev/null
exec /usr/sbin/sshd -D -e
