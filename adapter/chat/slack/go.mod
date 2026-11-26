module hop.top/uma/adapter/chat/slack

go 1.26.1

require (
	github.com/coder/websocket v1.8.15
	hop.top/uma v0.1.0-alpha.0
	hop.top/uma/testing/xrrtest v0.1.0-alpha.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/text v0.43.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	hop.top/xrr v0.1.0-alpha.5 // indirect
)

replace (
	hop.top/uma => ../../..
	hop.top/uma/testing/xrrtest => ../../../testing/xrrtest
)
