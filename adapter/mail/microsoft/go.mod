module hop.top/uma/adapter/mail/microsoft

go 1.26.1

require (
	hop.top/uma v0.1.0-alpha.0
	hop.top/uma/adapter/mail v0.1.0-alpha.0
	hop.top/uma/testing/xrrtest v0.1.0-alpha.0
)

require (
	github.com/emersion/go-message v0.18.2 // indirect
	github.com/emersion/go-msgauth v0.7.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	hop.top/xrr v0.1.0-alpha.5 // indirect
)

replace (
	hop.top/uma => ../../..
	hop.top/uma/adapter/mail => ..
	hop.top/uma/testing/xrrtest => ../../../testing/xrrtest
)
