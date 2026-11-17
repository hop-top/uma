// Package meta holds what the adapters for Meta messaging platforms share:
// webhook subscription verification, X-Hub-Signature-256 payload signature
// validation and a small Graph API client with error and rate-limit
// decoding. Adapters live in subpackages; whatsapp is the WhatsApp Cloud
// API adapter.
//
// Everything here is standard library only. HTTP goes through the
// ports.HTTPClient the host injects; credentials are passed in by the
// caller per request and never logged, stored or put in errors.
package meta
