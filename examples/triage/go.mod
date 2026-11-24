module hop.top/uma/examples/triage

go 1.26.1

require (
	hop.top/uma v0.1.0-alpha.0
	hop.top/uma/adapter/mail v0.1.0-alpha.0
	hop.top/uma/adapter/mail/google v0.1.0-alpha.0
	hop.top/uma/examples/fakeprovider v0.0.0-00010101000000-000000000000
)

require (
	blitiri.com.ar/go/spf v1.6.0 // indirect
	github.com/Azure/go-ntlmssp v0.1.1 // indirect
	github.com/emersion/go-imap/v2 v2.0.0-beta.8 // indirect
	github.com/emersion/go-message v0.18.2 // indirect
	github.com/emersion/go-msgauth v0.7.0 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/emersion/go-smtp v0.26.0 // indirect
	github.com/hashicorp/go-uuid v1.0.4 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/goidentity/v6 v6.0.1 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace (
	hop.top/uma => ../..
	hop.top/uma/adapter/mail => ../../adapter/mail
	hop.top/uma/adapter/mail/google => ../../adapter/mail/google
	hop.top/uma/examples/fakeprovider => ../fakeprovider
	hop.top/uma/testing/xrrtest => ../../testing/xrrtest
)
