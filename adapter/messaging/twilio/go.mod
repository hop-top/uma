module hop.top/uma/adapter/messaging/twilio

go 1.26.1

require (
	github.com/twilio/twilio-go v1.33.0
	gopkg.in/yaml.v3 v3.0.1
	hop.top/uma v0.1.0-alpha.0
	hop.top/uma/testing/xrrtest v0.1.0-alpha.0
)

require (
	github.com/golang/mock v1.6.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.49.0 // indirect
	hop.top/xrr v0.1.0-alpha.5 // indirect
)

replace (
	hop.top/uma => ../../..
	hop.top/uma/testing/xrrtest => ../../../testing/xrrtest
)
