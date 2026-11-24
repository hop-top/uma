module hop.top/uma/adapter/ticket/github

go 1.26.1

require (
	github.com/google/go-github/v92 v92.0.0
	hop.top/uma v0.1.0-alpha.0
	hop.top/uma/testing/xrrtest v0.1.0-alpha.0
)

require (
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	hop.top/xrr v0.1.0-alpha.5 // indirect
)

replace (
	hop.top/uma => ../../..
	hop.top/uma/testing/xrrtest => ../../../testing/xrrtest
)
