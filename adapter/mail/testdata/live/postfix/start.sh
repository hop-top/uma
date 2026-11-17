# Configures Postfix, then runs it in the foreground. Nothing is delivered
# anywhere until the test sets default_transport: until then every message
# is refused by the error transport. No DNS lookup is ever made.
set -eu
postconf -F '*/*/chroot=n'
postconf -e \
  myhostname=mta.example.test mydomain=example.test mydestination= \
  mynetworks=0.0.0.0/0 inet_interfaces=all inet_protocols=ipv4 \
  maillog_file=/dev/stdout \
  disable_dns_lookups=yes \
  smtp_tls_security_level=may smtp_tls_loglevel=1 \
  smtpd_relay_restrictions=permit_mynetworks,reject \
  default_transport='error:live test relay not set' \
  minimal_backoff_time=5s maximal_backoff_time=10s queue_run_delay=5s \
  maximal_queue_lifetime=2m bounce_queue_lifetime=2m \
  message_size_limit=52428800
echo "live postfix ready" >&2
exec postfix start-fg
