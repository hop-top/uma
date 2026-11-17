# Writes the account, then runs Dovecot in the foreground. up.sh waits for
# the ready line and installs /etc/dovecot/tls/*.pem before starting.
set -eu
printf 'desk@example.test:{PLAIN}desk-live-pw\n' >/etc/dovecot/users
echo "live dovecot ready" >&2
exec dovecot -F
