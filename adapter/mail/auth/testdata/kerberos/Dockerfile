# MIT Kerberos KDC and Dovecot IMAP (GSSAPI and PLAIN over implicit TLS)
# in one container, for the auth package's integration tests only.
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      krb5-kdc krb5-admin-server krb5-user dovecot-imapd dovecot-gssapi \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --home-dir /var/mail vmail \
 && chown vmail:vmail /var/mail

COPY krb5.conf /etc/krb5.conf
COPY kdc.conf /etc/krb5kdc/kdc.conf
COPY dovecot.conf /etc/dovecot/dovecot.conf
COPY start.sh /start.sh

EXPOSE 88/tcp 993/tcp
ENTRYPOINT ["/bin/sh", "/start.sh"]
