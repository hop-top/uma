# Dovecot IMAP over implicit TLS and STARTTLS, for the live mail tests only.
# One account, desk@example.test, with a throwaway password.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends dovecot-imapd \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --home-dir /var/mail vmail \
 && chown vmail:vmail /var/mail

COPY dovecot.conf /etc/dovecot/dovecot.conf
COPY start.sh /start.sh

EXPOSE 143/tcp 993/tcp
ENTRYPOINT ["/bin/sh", "/start.sh"]
