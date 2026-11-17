# Postfix as the sending MTA of the live smtp.mail test: it accepts mail from
# the test, queues it, and relays everything to the one address the test sets
# with postconf. Retries use short, fixed timers so a deferral is retried in
# seconds. For the live mail tests only.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

RUN echo "postfix postfix/main_mailer_type select Internet Site" | debconf-set-selections \
 && echo "postfix postfix/mailname string mta.example.test" | debconf-set-selections \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends postfix \
 && rm -rf /var/lib/apt/lists/*

COPY start.sh /start.sh

EXPOSE 25/tcp
ENTRYPOINT ["/bin/sh", "/start.sh"]
