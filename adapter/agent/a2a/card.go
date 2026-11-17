package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// ProtocolVersion is the A2A protocol version (major.minor) the adapter
// speaks; a request naming another is refused with VersionNotSupportedError.
const ProtocolVersion = "1.0"

// cardPaths are the last path elements serving the Agent Card, under the
// host's mount.
const cardPath = "agent-card.json"

type wireCard struct {
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	SupportedInterfaces  []wireInterface   `json:"supportedInterfaces"`
	Provider             *wireProvider     `json:"provider,omitempty"`
	Version              string            `json:"version"`
	DocumentationURL     string            `json:"documentationUrl,omitempty"`
	Capabilities         wireCapabilities  `json:"capabilities"`
	SecuritySchemes      map[string]any    `json:"securitySchemes,omitempty"`
	SecurityRequirements []wireRequirement `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string          `json:"defaultInputModes"`
	DefaultOutputModes   []string          `json:"defaultOutputModes"`
	Skills               []wireSkill       `json:"skills"`
	IconURL              string            `json:"iconUrl,omitempty"`
}

type wireInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type wireProvider struct {
	URL          string `json:"url"`
	Organization string `json:"organization"`
}

// wireCapabilities are always explicit: this endpoint streams nothing,
// pushes nothing and has no extended card.
type wireCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard"`
}

type wireRequirement struct {
	Schemes map[string]wireScopes `json:"schemes"`
}

type wireScopes struct {
	List []string `json:"list"`
}

type wireSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// card is the rendered Agent Card with its validators.
type card struct {
	body []byte
	etag string
	// challenges are the WWW-Authenticate challenges of a refused request.
	challenges []string
}

var defaultModes = []string{"text/plain"}

func buildCard(c config) (card, error) {
	wc := wireCard{
		Name: c.Name, Description: c.Description, Version: c.Version,
		SupportedInterfaces: []wireInterface{{URL: c.PublicURL, ProtocolBinding: "JSONRPC", ProtocolVersion: ProtocolVersion}},
		DocumentationURL:    c.DocumentURL, IconURL: c.IconURL,
		DefaultInputModes: orModes(c.InputModes), DefaultOutputModes: orModes(c.OutputModes),
	}
	if c.Provider != (Provider{}) {
		wc.Provider = &wireProvider{URL: c.Provider.URL, Organization: c.Provider.Organization}
	}
	for _, s := range c.Skills {
		wc.Skills = append(wc.Skills, wireSkill(s))
	}
	var challenges []string
	if len(c.SecuritySchemes) > 0 {
		wc.SecuritySchemes = map[string]any{}
		for _, name := range sortedKeys(c.SecuritySchemes) {
			wc.SecuritySchemes[name] = c.SecuritySchemes[name]
			challenges = appendChallenge(challenges, c.SecuritySchemes[name])
		}
	}
	for _, req := range c.SecurityRequirements {
		wr := wireRequirement{Schemes: map[string]wireScopes{}}
		for name, scopes := range req {
			wr.Schemes[name] = wireScopes{List: slices.Clone(scopes)}
		}
		wc.SecurityRequirements = append(wc.SecurityRequirements, wr)
	}
	if len(challenges) == 0 {
		challenges = []string{"Bearer"}
	}
	body, err := json.Marshal(wc)
	if err != nil {
		return card{}, err
	}
	sum := sha256.Sum256(body)
	return card{body: body, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, challenges: challenges}, nil
}

func orModes(m []string) []string {
	if len(m) == 0 {
		return defaultModes
	}
	return m
}

// appendChallenge adds the HTTP challenge a declared scheme implies: an
// HTTP scheme its own name, OAuth 2 and OpenID Connect a bearer token.
// API keys and mutual TLS have no challenge.
func appendChallenge(out []string, raw json.RawMessage) []string {
	kind, ok := schemeKind(raw)
	if !ok {
		return out
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body) // #nosec G104 -- schemeKind parsed it
	var ch string
	switch kind {
	case "httpAuthSecurityScheme":
		var h struct {
			Scheme string `json:"scheme"`
		}
		if json.Unmarshal(body[kind], &h) != nil || h.Scheme == "" || strings.ContainsAny(h.Scheme, " \t\r\n\",") {
			return out
		}
		ch = h.Scheme
	case "oauth2SecurityScheme", "openIdConnectSecurityScheme":
		ch = "Bearer"
	default:
		return out
	}
	if slices.ContainsFunc(out, func(c string) bool { return strings.EqualFold(c, ch) }) {
		return out
	}
	return append(out, ch)
}

// serveCard answers a card request: the card, cacheable, with a validator.
func (x *Instance) serveCard(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("ETag", x.card.etag)
	h.Set("Cache-Control", "public, max-age=300")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatch(match, x.card.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(x.card.body)
}

func etagMatch(header, etag string) bool {
	for _, c := range strings.Split(header, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}
