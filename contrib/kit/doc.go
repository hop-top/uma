// Package kit is the root of hop.top/uma/contrib/kit, the optional module
// that plugs hop.top/kit into uma. uma core stays free of kit; each package
// here adapts one kit facility to one uma port:
//
//	secret  ports.Credentials over a host-built kit storage/secret Store
//	blob    ports.BlobStore over a host-built kit storage/blob Store
//
// The packages import kit's interfaces only, never a kit backend. The host
// picks, configures and constructs the backend and passes it in, so
// importing this module registers nothing, reads no environment variable
// and starts nothing.
package kit
