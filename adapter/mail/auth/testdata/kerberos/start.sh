# Creates the realm, the user and service principals and their keytabs,
# then runs the KDC and Dovecot. The test copies /out/alice.keytab and
# /out/alice.ccache from the container and installs /etc/dovecot/tls/*.pem.
set -eu
realm=EXAMPLE.TEST
mkdir -p /out
kdb5_util create -s -r "$realm" -P stash-only-test-master-key >/dev/null
kadmin.local -q "addprinc -pw alice-kerberos-pw alice" >/dev/null
kadmin.local -q "addprinc -randkey imap/mail.example.test" >/dev/null
kadmin.local -q "ktadd -k /etc/dovecot/imap.keytab imap/mail.example.test" >/dev/null
kadmin.local -q "ktadd -k /out/alice.keytab -norandkey alice" >/dev/null
chmod 0644 /etc/dovecot/imap.keytab /out/alice.keytab
krb5kdc
kinit -k -t /out/alice.keytab -c FILE:/out/alice.ccache alice
chmod 0644 /out/alice.ccache
printf 'alice:{PLAIN}alice-mail-pw\n' >/etc/dovecot/users
echo "kerberos test server ready" >&2
exec dovecot -F
